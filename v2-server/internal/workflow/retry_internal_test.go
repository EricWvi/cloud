package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// retryRepo adds the retry port to memoryRepo, with fault injection for the retry decision.
type retryRepo struct {
	*memoryRepo
	retryErr error
	retries  int // RetryNode calls, successful or not
}

func (r *retryRepo) RetryNode(_ context.Context, _ string, row NodeRun, _ Failure, _, next func(int) ([]byte, error)) (NodeRun, error) {
	r.retries++
	if r.retryErr != nil {
		return NodeRun{}, r.retryErr
	}
	payload, err := next(2)
	if err != nil {
		return NodeRun{}, err
	}
	r.next++
	n := NodeRun{ID: fmt.Sprintf("nr%d", r.next), NodeID: row.NodeID, NodeType: row.NodeType, Status: StatusRunning, Payload: payload}
	*r.find(row.ID) = n // the replaced attempt leaves the live rows, as soft deletion does
	return n, nil
}

func (r *retryRepo) WakeNode(_ context.Context, _ string, row NodeRun, payload []byte) (NodeRun, error) {
	n := r.find(row.ID)
	now := time.UnixMilli(0)
	n.StartedAt, n.Payload = &now, payload
	return *n, nil
}

func (r *retryRepo) AbandonRetry(context.Context, string, string) (bool, error) { return false, nil }

func retryEngine(t *testing.T) (*Engine, *retryRepo, *recorder, *atomic.Int64) {
	t.Helper()
	repo, agents := &retryRepo{memoryRepo: newMemoryRepo(chain, str("kickoff"))}, &recorder{}
	clock := &atomic.Int64{}
	clock.Store(1_700_000_000_000)
	e := NewEngine(repo, agents, nil, func() time.Time { return time.UnixMilli(clock.Load()) })
	t.Cleanup(e.Close)
	if err := e.UpdateInput(context.Background(), "run", str("kickoff"), map[string]json.RawMessage{"topic": json.RawMessage(`"payments"`)}); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background(), "run"); err != nil {
		t.Fatal(err)
	}
	if len(agents.dispatched) != 1 {
		t.Fatalf("dispatched = %d", len(agents.dispatched))
	}
	return e, repo, agents, clock
}

// agentRow is the live attempt of Agent a.
func agentRow(repo *retryRepo) NodeRun {
	for _, r := range repo.runs {
		if r.NodeID == "a" {
			return r
		}
	}
	return NodeRun{}
}

func failureKind(t *testing.T, raw []byte) FailureKind {
	t.Helper()
	var p struct {
		Detail struct {
			Kind FailureKind `json:"kind"`
		} `json:"error_detail"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p.Detail.Kind
}

// An error while deciding a retry fails the attempt: the session already ended and nothing else
// would drive the running row.
func TestRetryDecisionErrorFailsTheAttempt(t *testing.T) {
	e, repo, _, _ := retryEngine(t)
	repo.retryErr = errors.New("database unavailable")
	id := agentRow(repo).ID
	if err := e.FailNode(context.Background(), "run", id, Failure{Kind: FailureSession, Message: "lost"}); err != nil {
		t.Fatal(err)
	}
	if got := repo.status("a"); got != StatusFailed || repo.run.Status != StatusFailed {
		t.Fatalf("attempt = %s run = %s, want both failed", got, repo.run.Status)
	}
	if kind := failureKind(t, repo.payload[id]); kind != FailureSession {
		t.Fatalf("failure kind = %s", kind)
	}
}

// A retryable failure of an attempt whose run already ended is a late callback: the engine does
// not ask the store to schedule a retry for it.
func TestRetryNotScheduledOnceTheRunEnded(t *testing.T) {
	e, repo, agents, _ := retryEngine(t)
	repo.run.Status = StatusFailed // another branch failed the run while a was executing
	id := agentRow(repo).ID
	if err := e.FailNode(context.Background(), "run", id, Failure{Kind: FailureSession, Message: "lost"}); err != nil {
		t.Fatal(err)
	}
	if repo.retries != 0 || len(agents.dispatched) != 1 {
		t.Fatalf("retry under an ended run: %d RetryNode calls, %d dispatches", repo.retries, len(agents.dispatched))
	}
	if row := agentRow(repo); row.ID != id {
		t.Fatalf("the attempt was replaced: %+v", row)
	}
}

// Once a wake commits the start, an error dispatching it fails the started attempt instead of
// leaving a started row that nothing executes.
func TestWakeDispatchErrorFailsTheStartedAttempt(t *testing.T) {
	e, repo, agents, clock := retryEngine(t)
	ctx := context.Background()
	if err := e.FailNode(ctx, "run", agentRow(repo).ID, Failure{Kind: FailureSession, Message: "lost"}); err != nil {
		t.Fatal(err)
	}
	waiting := agentRow(repo)
	var p retryPayload
	if err := json.Unmarshal(waiting.Payload, &p); err != nil || p.Wait == nil {
		t.Fatalf("wait payload %s: %v", waiting.Payload, err)
	}
	// The wake does not read the run payload before it commits the start; dispatch does.
	repo.run.Payload = []byte(`{"locale":5}`)
	clock.Store(p.Wait.DueAt)
	if err := e.WakeRetry(ctx, "run", waiting.ID); err != nil {
		t.Fatal(err)
	}
	if len(agents.dispatched) != 1 {
		t.Fatalf("an undispatchable retry was dispatched: %d", len(agents.dispatched))
	}
	row := repo.find(waiting.ID)
	if row.StartedAt == nil || row.Status != StatusFailed || repo.run.Status != StatusFailed {
		t.Fatalf("started attempt = %+v run = %s, want failed", row, repo.run.Status)
	}
	if kind := failureKind(t, repo.payload[waiting.ID]); kind != FailureInvalidRunPayload {
		t.Fatalf("failure kind = %s", kind)
	}
}

// A woken attempt is dispatched with its own row as started, not the waiting row read before.
func TestWakeDispatchesTheStartedRow(t *testing.T) {
	e, repo, agents, clock := retryEngine(t)
	ctx := context.Background()
	if err := e.FailNode(ctx, "run", agentRow(repo).ID, Failure{Kind: FailureSession, Message: "lost"}); err != nil {
		t.Fatal(err)
	}
	waiting := agentRow(repo)
	var p retryPayload
	_ = json.Unmarshal(waiting.Payload, &p)
	clock.Store(p.Wait.DueAt)
	if err := e.WakeRetry(ctx, "run", waiting.ID); err != nil {
		t.Fatal(err)
	}
	if len(agents.dispatched) != 2 {
		t.Fatalf("dispatched = %d", len(agents.dispatched))
	}
	d := agents.dispatched[1]
	if d.NodeRunID != waiting.ID {
		t.Fatalf("dispatched %s, want %s", d.NodeRunID, waiting.ID)
	}
	for _, r := range d.NodeRuns {
		if r.ID == waiting.ID {
			var started retryPayload
			_ = json.Unmarshal(r.Payload, &started)
			if r.StartedAt == nil || started.Wait != nil {
				t.Fatalf("dispatched row is the stale waiting row: %+v %s", r, r.Payload)
			}
			return
		}
	}
	t.Fatal("dispatch omitted the started row")
}

// The cancellation fence is released once the run is closed, so it does not grow with every
// cancellation over the process lifetime.
func TestReleaseRetriesDropsTheCancellationFence(t *testing.T) {
	e := NewEngine(newMemoryRepo(chain, nil), &recorder{}, nil, time.Now)
	for i := range 3 {
		e.CancelRetries(fmt.Sprint("run-", i))
	}
	if !e.retries.stopped("run-0") {
		t.Fatal("cancel did not fence the run")
	}
	for i := range 3 {
		e.ReleaseRetries(fmt.Sprint("run-", i))
	}
	if len(e.retries.cancelled) != 0 || e.retries.stopped("run-0") {
		t.Fatalf("fences after release = %v", e.retries.cancelled)
	}
}
