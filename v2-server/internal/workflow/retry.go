package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// RetryRepository persists replacements without propagating a failure to the run.
type RetryRepository interface {
	// RetryNode fails and replaces a running attempt by a waiting one; failed and next build the
	// two payloads with the attempt numbers the store assigned.
	RetryNode(ctx context.Context, runID string, row NodeRun, f Failure, failed, next func(int) ([]byte, error)) (NodeRun, error)
	// WakeNode stamps the start of a waiting attempt, replaces its payload and returns the row
	// as started.
	WakeNode(ctx context.Context, runID string, row NodeRun, payload []byte) (NodeRun, error)
	// AbandonRetry cancels a waiting attempt whose run is no longer running and reports whether
	// it did; a running run or a row that no longer waits is left untouched.
	AbandonRetry(ctx context.Context, runID, nodeRunID string) (bool, error)
}

// retryPolicy is desktop's effective retry policy of an Agent node.
type retryPolicy struct {
	enabled      bool
	max, initial int
}

// retryPolicyOf reads the policy that budgets and paces retries; retryWouldRun derives its answer
// from it. A missing or null policy is desktop's default, enabled with 2 retries 10 s apart
// (desktop@ebd5b725 crates/application/src/workflow_run/engine/retry.rs:22-31, 66-74). The
// analysis reads unvalidated drafts, so a malformed field counts as zero instead of panicking;
// publication has already rejected such a field for every node a run executes.
func retryPolicyOf(config object) retryPolicy {
	if config["retry"] == nil {
		return retryPolicy{true, 2, 10}
	}
	r, _ := config["retry"].(object)
	budget, _ := uintNumber(r["maxRetries"], 1<<32-1)
	initial, _ := uintNumber(r["initialDelaySeconds"], 1<<31-1)
	return retryPolicy{r["enabled"] == true, int(budget), int(initial)}
}

func (p retryPolicy) delay(retry int) time.Duration {
	// desktop@ebd5b725 engine/retry.rs:77-85: initial * 2^(retry-1), capped at 600s.
	return time.Duration(min(p.initial<<(retry-1), 600)) * time.Second
}

type retryMarker struct {
	Retry      int `json:"retry"`
	MaxRetries int `json:"max_retries"`
}
type retryWait struct {
	Attempt     int    `json:"attempt"`
	MaxAttempt  int    `json:"max_attempt"`
	Retry       int    `json:"retry"`
	MaxRetries  int    `json:"max_retries"`
	DelayMS     int64  `json:"delay_ms"`
	ScheduledAt int64  `json:"scheduled_at"`
	DueAt       int64  `json:"due_at"`
	PreviousID  string `json:"previous_node_run_id"`
	Reason      string `json:"reason"`
}
type retryPayload struct {
	Auto  retryMarker `json:"auto_retry"`
	Wait  *retryWait  `json:"retry_wait,omitempty"`
	Chain []string    `json:"retry_chain"`
}

// retryLocation resolves an attempt without broadening Repository.NodeRuns' root-only contract.
// Iteration rows share the root graph and pool; Loop rows use their exact round's body and pool.
type retryLocation struct {
	roots []NodeRun
	rows  []NodeRun
	index int
	round *LoopRound
}

func (e *Engine) locateRetry(ctx context.Context, runID, id string) (retryLocation, error) {
	roots, err := e.repo.NodeRuns(ctx, runID)
	if err != nil {
		return retryLocation{}, err
	}
	for i := range roots {
		if roots[i].ID == id {
			return retryLocation{roots: roots, rows: roots, index: i}, nil
		}
	}
	location := retryLocation{roots: roots, index: -1}
	repo, ok := e.repo.(ContainerRepository)
	if !ok {
		return location, nil
	}
	rounds, err := repo.LoopRounds(ctx, runID)
	if err != nil {
		return retryLocation{}, err
	}
	for i := range rounds {
		rows, err := repo.ScopeNodes(ctx, runID, rounds[i].ID)
		if err != nil {
			return retryLocation{}, err
		}
		for j := range rows {
			if rows[j].ID == id {
				return retryLocation{roots: roots, rows: rows, index: j, round: &rounds[i]}, nil
			}
		}
	}
	return location, nil
}

// graph returns the executable graph and node in the attempt's own scope.
func (l retryLocation) graph(run RunContext) (*Graph, Node, bool, error) {
	g, err := ParseGraph(run.Graph)
	if err != nil {
		return nil, Node{}, false, err
	}
	row := l.rows[l.index]
	if l.round == nil {
		n, ok := g.Node(row.NodeID)
		return g, n, ok, nil
	}
	for _, parent := range l.roots {
		if parent.ID != l.round.ParentID {
			continue
		}
		body, ok := g.LoopBody(parent.NodeID)
		if !ok {
			return g, Node{}, false, nil
		}
		n, ok := body.Node(row.NodeID)
		return body, n, ok, nil
	}
	return g, Node{}, false, nil
}

// retry replaces a failed Agent attempt by a waiting one in the same scope and round when its
// policy covers the failure, and reports whether the failure was consumed. An error while deciding
// reports false: nothing was persisted and the attempt's session already ended, so failing the
// attempt is the only way to keep the run from waiting on a running row nothing drives
// (desktop@ebd5b725 crates/application/src/workflow_run/engine/engine/retry_scheduler.rs:38-57).
func (e *Engine) retry(ctx context.Context, runID, id string, f Failure) bool {
	consumed, err := e.tryRetry(ctx, runID, id, f)
	if err != nil {
		slog.Warn("could not decide an automatic retry; failing the attempt instead", "run", runID, "node_run", id, "error", err)
		return false
	}
	return consumed
}

func (e *Engine) tryRetry(ctx context.Context, runID, id string, f Failure) (bool, error) {
	repo, ok := e.repo.(RetryRepository)
	if !ok || !f.Kind.autoRetryable() {
		return false, nil
	}
	run, err := e.repo.RunContext(ctx, runID)
	if err != nil {
		return false, err
	}
	// A run that already left running records the failure as a late callback.
	if run.Status != StatusRunning {
		return false, nil
	}
	location, err := e.locateRetry(ctx, runID, id)
	if err != nil {
		return false, err
	}
	// An unknown row is a late callback. A row already settled by cancellation or a duplicate
	// callback is consumed without spending another retry.
	if location.index < 0 {
		return false, nil
	}
	row := location.rows[location.index]
	if row.Status != StatusRunning {
		return true, nil
	}
	_, n, ok, err := location.graph(run)
	if err != nil {
		return false, err
	}
	config, _ := n.data["agentConfig"].(object)
	if !ok || n.Kind != "agent" || !retryApplies(config, n.at) {
		return false, nil
	}
	var previous retryPayload
	_ = json.Unmarshal(row.Payload, &previous)
	if previous.Wait != nil {
		return true, nil
	}
	policy := retryPolicyOf(config)
	if previous.Auto.Retry >= policy.max {
		return false, nil
	}
	// Cancellation or shutdown owns this eligible row from here: cancel closes it as cancelled
	// and the next start interrupts it, so no retry may be scheduled into either.
	if e.retries.stopped(runID) {
		return true, nil
	}
	retry := previous.Auto.Retry + 1
	delay := policy.delay(retry)
	now := e.now()
	inject := true
	if p, ok, _ := DecodeRunPayload(run.Payload); ok {
		inject = p.InjectLastFailure
	}
	next, err := repo.RetryNode(ctx, runID, row, f, func(attempt int) ([]byte, error) {
		return MergePayload(row.Payload, map[string]any{"error_detail": f.Detail(attempt, inject, now)})
	}, func(attempt int) ([]byte, error) {
		return json.Marshal(retryPayload{Auto: retryMarker{retry, policy.max},
			Wait:  &retryWait{attempt, attempt + policy.max - retry, retry, policy.max, delay.Milliseconds(), now.UnixMilli(), now.Add(delay).UnixMilli(), id, f.Message},
			Chain: append(previous.Chain, id)})
	})
	if err != nil {
		return false, err
	}
	e.publish(runID)
	e.armRetry(runID, next.ID, time.UnixMilli(now.Add(delay).UnixMilli()))
	return true, nil
}

// WakeRetry revalidates a deadline under the run gate. Duplicate and cancelled wakes cannot
// dispatch an attempt, an early wake re-arms the timer at the persisted deadline, and a wait
// whose run left running is abandoned. Restart intentionally does not re-arm persisted waits.
func (e *Engine) WakeRetry(ctx context.Context, runID, id string) error {
	defer e.gate(runID)()
	repo, ok := e.repo.(RetryRepository)
	if !ok || e.retries.stopped(runID) {
		return nil
	}
	run, err := e.repo.RunContext(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status != StatusRunning {
		// Every path that ends a run settles its waits; this is the safety net for a run that
		// left running some other way (desktop@ebd5b725 crates/db/src/repository/
		// workflow_run_engine/retry.rs:174-191).
		abandoned, err := repo.AbandonRetry(ctx, runID, id)
		if err == nil && abandoned {
			e.publish(runID)
		}
		return err
	}
	location, err := e.locateRetry(ctx, runID, id)
	if err != nil {
		return err
	}
	if location.index < 0 || location.rows[location.index].Status != StatusRunning || location.rows[location.index].StartedAt != nil {
		return nil
	}
	row := location.rows[location.index]
	var p retryPayload
	if json.Unmarshal(row.Payload, &p) != nil || p.Wait == nil {
		return nil
	}
	if e.now().UnixMilli() < p.Wait.DueAt {
		// For example after a wall-clock step: wake again at the deadline
		// (desktop@ebd5b725 crates/application/src/workflow_run/engine/engine/retry_scheduler.rs:129-131).
		e.armRetry(runID, id, time.UnixMilli(p.Wait.DueAt))
		return nil
	}
	p.Wait = nil
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	started, err := repo.WakeNode(ctx, runID, row, raw)
	if err != nil {
		return err
	}
	e.publish(runID)
	location.rows[location.index] = started
	// The attempt is started now, so an error must fail it: returning the error would leave a
	// running row that no executor and no timer will ever finish (desktop@ebd5b725
	// crates/application/src/workflow_run/engine/engine/retry_scheduler.rs:141-169). The
	// scheduling pass then applies an Iteration's error policy after a final dispatch failure.
	if f := e.dispatchRetry(run, location, started); f != nil {
		if err := e.fail(ctx, runID, id, *f); err != nil {
			return err
		}
		return e.schedule(ctx, runID)
	}
	return nil
}

// dispatchRetry hands a started attempt to the Agent bridge as the original attempt was
// dispatched in its root, Iteration or Loop scope, with rows reflecting the started row. A
// failure means it could not be dispatched.
func (e *Engine) dispatchRetry(run RunContext, location retryLocation, started NodeRun) *Failure {
	undispatched := func(reason string) *Failure {
		return &Failure{Kind: FailureInvalidRunPayload, Message: fmt.Sprintf("retry of node %s could not be dispatched: %s", started.NodeID, reason)}
	}
	g, n, ok, err := location.graph(run)
	if err != nil {
		return undispatched(err.Error())
	}
	if !ok || n.Kind != "agent" {
		return undispatched("it has no executable target in its scope")
	}
	payload, _, err := DecodeRunPayload(run.Payload)
	if err != nil {
		return undispatched(err.Error())
	}
	pool := payload.VariablePool
	if location.round != nil {
		state, err := decodeLoopState(location.round.State)
		if err != nil {
			return undispatched(err.Error())
		}
		pool = state.VariablePool
	}
	e.agents.Dispatch(AgentDispatch{RunID: run.RunID, NodeRunID: started.ID, ProjectID: run.ProjectID, Locale: payload.Locale,
		ScopeID: started.ScopeID, Iteration: started.Iteration, Node: n, Graph: g, Pool: pool, RunInput: run.Input, NodeRuns: location.rows})
	return nil
}

// retryTimers owns every waiting goroutine. The mutex serializes Add against Close's Wait;
// per-run cancellation and shutdown both release timers immediately. cancelled fences a run
// whose cancellation is in progress and is released once the run is closed, so it stays bounded
// by the cancellations still in flight.
type retryTimers struct {
	mu        sync.Mutex
	closed    bool
	cancelled map[string]bool
	waits     map[string]map[string]*retryWaiter
	workers   sync.WaitGroup
}

// retryWaiter is one armed timer; its identity lets a goroutine unregister only itself after a
// re-arm replaced it. ctx reports whether it was cancelled before the goroutine unregistered it.
type retryWaiter struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func newRetryTimers() retryTimers {
	return retryTimers{cancelled: map[string]bool{}, waits: map[string]map[string]*retryWaiter{}}
}
func (t *retryTimers) stopped(runID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed || t.cancelled[runID]
}

// armRetry wakes one waiting attempt at due. Arming the same attempt again replaces its timer,
// as desktop's retry timer does; callers hold the run gate, so a replaced timer is either still
// waiting or is the caller re-arming itself and returns right after.
func (e *Engine) armRetry(runID, id string, due time.Time) {
	t := &e.retries
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.cancelled[runID] {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &retryWaiter{ctx, cancel}
	if t.waits[runID] == nil {
		t.waits[runID] = map[string]*retryWaiter{}
	}
	if old := t.waits[runID][id]; old != nil {
		old.cancel()
	}
	t.waits[runID][id] = w
	// Arm against the persisted deadline, excluding time spent committing the replacement.
	// desktop@ebd5b725 crates/backend/src/workflow/run/retry_timer.rs:44-55.
	timer := time.NewTimer(max(0, due.Sub(e.now())))
	t.workers.Add(1)
	go func() {
		defer t.workers.Done()
		defer cancel()
		defer func() {
			t.mu.Lock()
			if t.waits[runID][id] == w {
				delete(t.waits[runID], id)
				if len(t.waits[runID]) == 0 {
					delete(t.waits, runID)
				}
			}
			t.mu.Unlock()
		}()
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := e.WakeRetry(ctx, runID, id); err != nil && ctx.Err() == nil {
				slog.Error("wake workflow retry", "run", runID, "node_run", id, "error", err)
			}
		}
	}()
}

// CancelRetries fences wakes before the host starts cancelling external execution.
func (e *Engine) CancelRetries(runID string) {
	defer e.gate(runID)()
	t := &e.retries
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cancelled[runID] = true
	for _, w := range t.waits[runID] {
		w.cancel()
	}
}

// ReleaseRetries drops the cancellation fence of a run that is no longer running. Its waits are
// settled in PostgreSQL, so a later wake finds nothing to start.
func (e *Engine) ReleaseRetries(runID string) {
	defer e.gate(runID)()
	t := &e.retries
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.cancelled, runID)
}

// HeldRetries reports what the engine still holds for a run: whether its cancellation fence is
// set and how many of its retry timers are armed and not yet cancelled. A closed run holds
// neither, so the process keeps no per-run state, and no timer goroutine outlives a cancellation.
func (e *Engine) HeldRetries(runID string) (fenced bool, armed int) {
	t := &e.retries
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, w := range t.waits[runID] {
		if w.ctx.Err() == nil {
			armed++
		}
	}
	return t.cancelled[runID], armed
}

// Close stops every retry timer and waits for callbacks to exit before host workers shut down.
func (e *Engine) Close() {
	t := &e.retries
	t.mu.Lock()
	t.closed = true
	for _, waits := range t.waits {
		for _, w := range waits {
			w.cancel()
		}
	}
	t.mu.Unlock()
	t.workers.Wait()
}
