package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

func retryGraph(policy string) string {
	if policy != "" {
		policy = `,"retry":` + policy
	}
	return strings.Replace(twoAgents, `"roleId":"reviewer"`, `"roleId":"reviewer"`+policy, 1)
}

// retryClock replaces the fixture's host by one whose clock stands still until the test moves it.
func retryClock(t *testing.T, f *fixture) *atomic.Int64 {
	t.Helper()
	clock := &atomic.Int64{}
	clock.Store(1_700_000_000_000)
	retryHost(t, f, func() time.Time { return time.UnixMilli(clock.Load()) })
	return clock
}

func retryHost(t *testing.T, f *fixture, now func() time.Time) {
	t.Helper()
	f.host.Close()
	h, err := NewHost(Config{Store: f.s, Execution: f.exec, Catalog: f.exec, StopTimeout: time.Second, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	f.host = h
	t.Cleanup(h.Close)
}

type waitPayload struct {
	Wait *struct {
		Attempt    int    `json:"attempt"`
		MaxAttempt int    `json:"max_attempt"`
		Retry      int    `json:"retry"`
		Delay      int64  `json:"delay_ms"`
		Due        int64  `json:"due_at"`
		Reason     string `json:"reason"`
	} `json:"retry_wait"`
	Detail struct {
		Attempt int `json:"attempt"`
	} `json:"error_detail"`
	Chain []string `json:"retry_chain"`
}

func waiting(t *testing.T, f *fixture, id string) (store.WorkflowNodeRun, waitPayload) {
	t.Helper()
	_, rows := f.await(t, id, func(_ store.WorkflowRun, rows []store.WorkflowNodeRun) bool {
		n := node(rows, "a")
		return n.ID != "" && n.Status == "running" && n.StartedAt == nil
	})
	row := node(rows, "a")
	var p waitPayload
	if err := json.Unmarshal(row.Payload, &p); err != nil || p.Wait == nil {
		t.Fatalf("wait payload %s: %v", row.Payload, err)
	}
	if row.SessionID != nil {
		t.Fatal("waiting attempt inherited a session")
	}
	return row, p
}

func TestAutomaticRetryDefaultBudgetAndSessions(t *testing.T) {
	f := newFixture(t)
	clock := retryClock(t, f)
	ctx := context.Background()
	id := f.run(t, retryGraph(""), str("input"))
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	sessions, replaced := []string{}, []string{}
	for retry := 1; retry <= 2; retry++ {
		session := f.exec.awaitStart(t)
		sessions = append(sessions, session)
		replaced = append(replaced, f.exec.request(session).Caller.AttemptID)
		f.exec.finish(session, execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
		row, p := waiting(t, f, id)
		wantDelay := int64(10_000 << (retry - 1))
		if p.Wait.Attempt != retry+1 || p.Wait.MaxAttempt != 3 || p.Wait.Retry != retry || p.Wait.Delay != wantDelay || p.Wait.Reason == "" {
			t.Fatalf("wait = %+v", p.Wait)
		}
		// The chain lists every replaced attempt in order; the run view labels history from it.
		if !slices.Equal(p.Chain, replaced) {
			t.Fatalf("retry %d chain = %v, want %v", retry, p.Chain, replaced)
		}
		// Early wakes and late callbacks for the replaced attempt cannot advance successors.
		if err := f.host.engine.WakeRetry(ctx, id, row.ID); err != nil {
			t.Fatal(err)
		}
		old := f.exec.request(session).Caller.AttemptID
		if err := f.host.engine.FailNode(ctx, id, old, workflow.Failure{Kind: workflow.FailureSession, Message: "late"}); err != nil {
			t.Fatal(err)
		}
		// A waiting attempt never ran, so a failure reported for it cannot spend the budget.
		if err := f.host.engine.FailNode(ctx, id, row.ID, workflow.Failure{Kind: workflow.FailureSession, Message: "not started"}); err != nil {
			t.Fatal(err)
		}
		if rows, _ := f.s.WorkflowEngineNodeRuns(ctx, id); node(rows, "a").ID != row.ID {
			t.Fatalf("a failure of the waiting attempt replaced it: %+v", node(rows, "a"))
		}
		rows, _ := f.s.WorkflowEngineNodeRuns(ctx, id)
		if node(rows, "b").ID != "" || node(rows, "a").StartedAt != nil {
			t.Fatal("early wake dispatched or successor escaped wait")
		}
		clock.Store(p.Wait.Due)
		if err := f.host.engine.WakeRetry(ctx, id, row.ID); err != nil {
			t.Fatal(err)
		}
		if err := f.host.engine.WakeRetry(ctx, id, row.ID); err != nil {
			t.Fatal(err)
		} // stale wake
	}
	last := f.exec.awaitStart(t)
	sessions = append(sessions, last)
	f.exec.finish(last, execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	run, rows := f.await(t, id, terminal)
	if run.Status != "failed" || node(rows, "b").ID != "" {
		t.Fatalf("run = %s", run.Status)
	}
	history, err := f.s.WorkflowRunFailedAttempts(ctx, f.scope.TenantID, id)
	if err != nil || len(history) != 2 {
		t.Fatalf("history = %+v %v", history, err)
	}
	var final waitPayload
	if json.Unmarshal(node(rows, "a").Payload, &final) != nil || !slices.Equal(final.Chain, replaced) {
		t.Fatalf("final attempt chain = %v, want %v", final.Chain, replaced)
	}
	all := append(history, node(rows, "a"))
	for i, row := range all {
		var p waitPayload
		_ = json.Unmarshal(row.Payload, &p)
		if p.Detail.Attempt != i+1 || row.SessionID == nil || *row.SessionID != sessions[i] {
			t.Fatalf("attempt %d = %+v / %s", i+1, row, row.Payload)
		}
	}
	if sessions[0] == sessions[1] || sessions[1] == sessions[2] {
		t.Fatal("retry reused a session")
	}
}

func TestAutomaticRetryPolicyBackoffAndExclusions(t *testing.T) {
	for _, tc := range []struct {
		name, policy string
		kind         workflow.FailureKind
		delays       []int64
		interactive  bool
	}{
		{name: "disabled", policy: `{"enabled":false,"maxRetries":2,"initialDelaySeconds":10}`, kind: workflow.FailureSession},
		{name: "zero budget", policy: `{"enabled":true,"maxRetries":0,"initialDelaySeconds":10}`, kind: workflow.FailureSession},
		{name: "non retryable", kind: workflow.FailureRepository},
		{name: "interactive", kind: workflow.FailureSession, interactive: true},
		{name: "cap", policy: `{"enabled":true,"maxRetries":5,"initialDelaySeconds":300}`, kind: workflow.FailureSession, delays: []int64{300000, 600000, 600000, 600000, 600000}},
		{name: "doubling", policy: `{"enabled":true,"maxRetries":5,"initialDelaySeconds":50}`, kind: workflow.FailureAgentRefusal, delays: []int64{50000, 100000, 200000, 400000, 600000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			clock := retryClock(t, f)
			ctx := context.Background()
			graph := retryGraph(tc.policy)
			if tc.interactive {
				graph = strings.Replace(graph, `"roleId":"reviewer"`, `"roleId":"reviewer","interactive":true`, 1)
			}
			graph = strings.Replace(graph, `审阅这段初稿：{{#a.output#}}`, `independent`, 1)
			id := f.run(t, graph, str("input"))
			if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt <= len(tc.delays); attempt++ {
				session := f.exec.awaitStart(t)
				// Failure injection crosses the real engine/store boundary while execution is held.
				rowID := f.exec.request(session).Caller.AttemptID
				if err := f.host.engine.FailNode(ctx, id, rowID, workflow.Failure{Kind: tc.kind, Message: fmt.Sprintf("failure %d", attempt+1)}); err != nil {
					t.Fatal(err)
				}
				f.exec.finish(session, completed("late result"))
				if attempt == len(tc.delays) {
					break
				}
				row, p := waiting(t, f, id)
				if p.Wait.Delay != tc.delays[attempt] {
					t.Fatalf("delay = %d, want %d", p.Wait.Delay, tc.delays[attempt])
				}
				clock.Store(p.Wait.Due)
				if err := f.host.engine.WakeRetry(ctx, id, row.ID); err != nil {
					t.Fatal(err)
				}
			}
			run, _ := f.await(t, id, terminal)
			if run.Status != "failed" {
				t.Fatalf("run = %s", run.Status)
			}
		})
	}
}

func TestRetryWaitIndependentBranchCancelRestartAndClose(t *testing.T) {
	for _, action := range []string{"cancel", "restart", "close"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t)
			clock := retryClock(t, f)
			ctx := context.Background()
			graph := strings.Replace(twoAgents, `{"source":"a","target":"b"}`, `{"source":"start","target":"b"}`, 1)
			graph = strings.Replace(graph, `审阅这段初稿：{{#a.output#}}`, `independent`, 1)
			id := f.run(t, graph, str("input"))
			if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
				t.Fatal(err)
			}
			// The independent branches execute concurrently; a's wait does not hold b.
			started := f.exec.awaitStarts(t, 2)
			f.exec.finish(started["a"], execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
			row, p := waiting(t, f, id)
			f.exec.finish(started["b"], completed("independent"))
			f.await(t, id, func(_ store.WorkflowRun, rows []store.WorkflowNodeRun) bool {
				return node(rows, "b").Status == "succeeded"
			})
			switch action {
			case "cancel":
				if fenced, armed := f.host.engine.HeldRetries(id); fenced || armed != 1 {
					t.Fatalf("before cancel: fenced %v, %d armed timers, want an armed wait", fenced, armed)
				}
				if _, err := f.host.CancelRun(ctx, f.scope, id); err != nil {
					t.Fatal(err)
				}
				// A confirmed cancel closes the run, so it stops the wait's timer and drops the fence.
				if fenced, armed := f.host.engine.HeldRetries(id); fenced || armed != 0 {
					t.Fatalf("after cancel: fenced %v, %d armed timers, want neither", fenced, armed)
				}
			case "restart":
				f.host.Close()
				f.host = f.newHost(t)
				if err := f.host.Interrupt(ctx); err != nil {
					t.Fatal(err)
				}
			case "close":
				f.host.Close()
			}
			clock.Store(p.Wait.Due)
			if err := f.host.engine.WakeRetry(ctx, id, row.ID); err != nil {
				t.Fatal(err)
			}
			f.exec.mu.Lock()
			starts := len(f.exec.started)
			f.exec.mu.Unlock()
			if starts != 2 {
				t.Fatalf("stale wake started %d sessions", starts)
			}
			rows, err := f.s.WorkflowEngineNodeRuns(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if a := node(rows, "a"); a.StartedAt != nil {
				t.Fatalf("%s: a late wake started the waiting attempt: %+v", action, a)
			}
			if action != "close" {
				run, rows := f.await(t, id, terminal)
				if action == "cancel" && (run.Status != "cancelled" || node(rows, "a").Status != "cancelled") {
					t.Fatal("cancel did not abandon wait")
				}
				if action == "restart" && errorKind(t, node(rows, "a")) != "interrupted_by_restart" {
					t.Fatal("restart rearmed wait")
				}
				if strings.Contains(string(node(rows, "a").Payload), "retry_wait") {
					t.Fatalf("%s left the wait marker: %s", action, node(rows, "a").Payload)
				}
			}
		})
	}
}

func TestZeroDelayTimerDispatchesNewAttempt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.run(t, retryGraph(`{"enabled":true,"maxRetries":1,"initialDelaySeconds":0}`), str("input"))
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	first := f.exec.awaitStart(t)
	f.exec.finish(first, execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	second := f.exec.awaitStart(t)
	if second == first {
		t.Fatal("timer reused session")
	}
	f.exec.finish(second, completed("done"))
	f.exec.finish(f.exec.awaitStart(t), completed("reviewed"))
	run, _ := f.await(t, id, terminal)
	if run.Status != "succeeded" {
		t.Fatalf("run = %s", run.Status)
	}
}

// independentAgents runs a and b as independent branches; bConfig extends b's agentConfig.
func independentAgents(bConfig string) string {
	graph := strings.Replace(twoAgents, `{"source":"a","target":"b"}`, `{"source":"start","target":"b"}`, 1)
	prompt := `"prompt":"independent"`
	if bConfig != "" {
		prompt += "," + bConfig
	}
	return strings.Replace(graph, `"prompt":"审阅这段初稿：{{#a.output#}}"`, prompt, 1)
}

func starts(f *fixture) int {
	f.exec.mu.Lock()
	defer f.exec.mu.Unlock()
	return len(f.exec.started)
}

// abandoned asserts a wait settled as desktop's retry_abandoned without its wait marker.
func abandoned(t *testing.T, a store.WorkflowNodeRun, waitID string) {
	t.Helper()
	if a.ID != waitID || a.Status != store.WorkflowStatusCancelled || a.StartedAt != nil || a.Error == nil || *a.Error != store.RetryAbandoned {
		t.Fatalf("wait = %+v (error %v), want %s cancelled as retry_abandoned", a, a.Error, waitID)
	}
	if strings.Contains(string(a.Payload), "retry_wait") {
		t.Fatalf("abandoned wait kept its marker: %s", a.Payload)
	}
}

// A run failing through another branch settles the wait in the same transaction, and the timer
// that fires later starts nothing.
func TestRetryWaitAbandonedWhenAnotherBranchFailsTheRun(t *testing.T) {
	f := newFixture(t)
	clock := retryClock(t, f)
	ctx := context.Background()
	id := f.run(t, independentAgents(""), str("input"))
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	started := f.exec.awaitStarts(t, 2)
	f.exec.finish(started["a"], execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	row, p := waiting(t, f, id)
	other := started["b"]
	if err := f.host.engine.FailNode(ctx, id, f.exec.request(other).Caller.AttemptID, workflow.Failure{Kind: workflow.FailureRepository, Message: "b broke"}); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(other, completed("late"))
	run, rows := f.await(t, id, terminal)
	if run.Status != store.WorkflowStatusFailed {
		t.Fatalf("run = %s", run.Status)
	}
	abandoned(t, node(rows, "a"), row.ID)
	clock.Store(p.Wait.Due)
	if err := f.host.engine.WakeRetry(ctx, id, row.ID); err != nil {
		t.Fatal(err)
	}
	rows, _ = f.s.WorkflowEngineNodeRuns(ctx, id)
	abandoned(t, node(rows, "a"), row.ID)
	if n := starts(f); n != 2 {
		t.Fatalf("an abandoned wait started: %d sessions", n)
	}
}

// A wait whose run left running without settling it is abandoned by its wake.
func TestWakeAbandonsTheWaitOfARunThatEndedElsewhere(t *testing.T) {
	f := newFixture(t)
	clock := retryClock(t, f)
	ctx := context.Background()
	id := f.run(t, retryGraph(""), str("input"))
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(f.exec.awaitStart(t), execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	row, p := waiting(t, f, id)
	// Finishing a run as succeeded does not settle waits, standing in for any other exit.
	if _, err := f.s.FinishWorkflowRun(ctx, f.scope.TenantID, id, store.WorkflowStatusSucceeded, nil, nil); err != nil {
		t.Fatal(err)
	}
	clock.Store(p.Wait.Due)
	if err := f.host.engine.WakeRetry(ctx, id, row.ID); err != nil {
		t.Fatal(err)
	}
	rows, _ := f.s.WorkflowEngineNodeRuns(ctx, id)
	abandoned(t, node(rows, "a"), row.ID)
	if n := starts(f); n != 1 {
		t.Fatalf("a wake under an ended run started %d sessions", n)
	}
}

// failHeld fails the attempt of a held session synchronously, so its retry timer is armed when
// it returns, then delivers the session's late result.
func failHeld(t *testing.T, f *fixture, runID, session string) {
	t.Helper()
	if err := f.host.engine.FailNode(context.Background(), runID, f.exec.request(session).Caller.AttemptID, workflow.Failure{Kind: workflow.FailureSession, Message: "lost"}); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(session, completed("late result"))
}

// The armed timer, not a manual wake, starts the attempt, and only once its delay elapsed: the
// engine clock already reads the deadline, so an early timer would start it at once.
func TestArmedTimerStartsTheAttemptAfterItsDelay(t *testing.T) {
	f := newFixture(t)
	clock := retryClock(t, f)
	ctx := context.Background()
	id := f.run(t, retryGraph(`{"enabled":true,"maxRetries":1,"initialDelaySeconds":1}`), str("input"))
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	first := f.exec.awaitStart(t)
	failHeld(t, f, id, first)
	armed := time.Now()
	_, p := waiting(t, f, id)
	clock.Store(p.Wait.Due)
	second := f.exec.awaitStart(t)
	if elapsed := time.Since(armed); elapsed < 500*time.Millisecond {
		t.Fatalf("timer started the retry after %v, before its 1s delay", elapsed)
	}
	if second == first || f.exec.request(second).Caller.NodeID != "a" {
		t.Fatalf("timer started %s (first %s)", second, first)
	}
}

// A timer that fires before the deadline by the engine clock, e.g. after the wall clock stepped
// back, re-arms for the deadline instead of dropping the wait.
func TestEarlyTimerRearmsAtTheDeadline(t *testing.T) {
	f := newFixture(t)
	offset := &atomic.Int64{}
	retryHost(t, f, func() time.Time { return time.Now().Add(time.Duration(offset.Load())) })
	ctx := context.Background()
	id := f.run(t, retryGraph(`{"enabled":true,"maxRetries":1,"initialDelaySeconds":1}`), str("input"))
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	first := f.exec.awaitStart(t)
	failHeld(t, f, id, first)
	offset.Store(int64(-500 * time.Millisecond)) // the wall clock steps back after arming
	second := f.exec.awaitStart(t)
	if second == first {
		t.Fatal("retry reused the session")
	}
	rows, _ := f.s.WorkflowEngineNodeRuns(ctx, id)
	if a := node(rows, "a"); a.StartedAt == nil || strings.Contains(string(a.Payload), "retry_wait") {
		t.Fatalf("re-armed attempt = %+v %s", a, a.Payload)
	}
}

// Between CancelRetries and a confirmed stop the run is still running, yet no wake may start its
// wait. The fence only owns retry-eligible rows: an interactive sibling failing in that window
// fails the run as before, which abandons the wait.
func TestUnconfirmedStopFencesOnlyEligibleRetries(t *testing.T) {
	f := newFixture(t)
	clock := retryClock(t, f)
	ctx := context.Background()
	id := f.run(t, independentAgents(`"interactive":true`), str("input"))
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	started := f.exec.awaitStarts(t, 2)
	f.exec.finish(started["a"], execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	row, p := waiting(t, f, id)
	other := started["b"]
	f.exec.onCancel = func(execution.Ref) error { return execution.ErrStopUnconfirmed }
	if _, err := f.host.CancelRun(ctx, f.scope, id); !errors.Is(err, server.ErrWorkflowRunStopUnconfirmed) {
		t.Fatalf("cancel = %v", err)
	}
	// The open run keeps its fence, but the wait's timer is already stopped.
	if fenced, armed := f.host.engine.HeldRetries(id); !fenced || armed != 0 {
		t.Fatalf("unconfirmed stop: fenced %v, %d armed timers, want the fence alone", fenced, armed)
	}
	clock.Store(p.Wait.Due)
	if err := f.host.engine.WakeRetry(ctx, id, row.ID); err != nil {
		t.Fatal(err)
	}
	run, _ := f.s.WorkflowRun(ctx, f.scope.TenantID, id)
	rows, _ := f.s.WorkflowEngineNodeRuns(ctx, id)
	if a := node(rows, "a"); run.Status != store.WorkflowStatusRunning || a.ID != row.ID || a.StartedAt != nil || starts(f) != 2 {
		t.Fatalf("wake inside an unconfirmed stop: run %s a %+v (%d sessions)", run.Status, a, starts(f))
	}
	if err := f.host.engine.FailNode(ctx, id, f.exec.request(other).Caller.AttemptID, workflow.Failure{Kind: workflow.FailureSession, Message: "b lost"}); err != nil {
		t.Fatal(err)
	}
	run, rows = f.await(t, id, terminal)
	if run.Status != store.WorkflowStatusFailed || node(rows, "b").Status != store.WorkflowStatusFailed {
		t.Fatalf("interactive failure inside the window: run %s b %+v", run.Status, node(rows, "b"))
	}
	abandoned(t, node(rows, "a"), row.ID)
	if fenced, _ := f.host.engine.HeldRetries(id); !fenced {
		t.Fatal("the fence was dropped before the cancelled turn ended")
	}
	// The turn ending closes the cancellation (finishCancel) on its attempt, which releases the
	// fence. Closing the host instead would cancel the attempt's context first.
	f.exec.finish(other, completed("late"))
	awaitReleased(t, f, id)
}

// awaitReleased waits until the engine holds neither a fence nor a timer for the run, as it must
// once an attempt closed the run's cancellation.
func awaitReleased(t *testing.T, f *fixture, runID string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		fenced, armed := f.host.engine.HeldRetries(runID)
		if !fenced && armed == 0 {
			return
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatalf("after the cancellation closed: fenced %v, %d armed timers, want neither", fenced, armed)
		}
	}
}

// Close waits for a timer callback already running, so nothing it does outlives the host.
func TestCloseWaitsForARunningRetryCallback(t *testing.T) {
	f := newFixture(t)
	var trapped atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	frozen := time.UnixMilli(1_700_000_000_000)
	retryHost(t, f, func() time.Time {
		// Hold the first clock read of a wake, i.e. a callback inside WakeRetry.
		if trapped.Load() && strings.Contains(string(debug.Stack()), "(*Engine).WakeRetry") && trapped.CompareAndSwap(true, false) {
			close(entered)
			<-release
		}
		return frozen
	})
	ctx := context.Background()
	id := f.run(t, retryGraph(`{"enabled":true,"maxRetries":1,"initialDelaySeconds":1}`), str("input"))
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	failHeld(t, f, id, f.exec.awaitStart(t))
	trapped.Store(true)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the retry timer never fired")
	}
	closed := make(chan struct{})
	go func() { f.host.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned while a retry callback was still running")
	case <-time.After(200 * time.Millisecond):
	}
	unblock()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return once the callback finished")
	}
	rows, _ := f.s.WorkflowEngineNodeRuns(ctx, id)
	if a := node(rows, "a"); a.StartedAt != nil || starts(f) != 1 {
		t.Fatalf("a callback finishing during Close started the attempt: %+v", a)
	}
}

// An Iteration whose fail strategy fails the run settles the root wait through the container path.
func TestRetryWaitAbandonedWhenAContainerFailsTheRun(t *testing.T) {
	f := newFixture(t)
	retryClock(t, f)
	ctx := context.Background()
	graph := iterationGraph("fail", []string{"one"}, 50)
	graph = strings.Replace(graph, ` {"id":"iter"`, ` {"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"root"}}},
 {"id":"iter"`, 1)
	graph = strings.Replace(graph, `"edges":[`, `"edges":[{"source":"start","target":"a"},`, 1)
	id := f.run(t, graph, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	first := f.exec.awaitStart(t)
	if f.exec.request(first).Caller.NodeID != "a" {
		t.Fatalf("expected the root Agent first, got %s", f.exec.request(first).Caller.NodeID)
	}
	f.exec.finish(first, execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	row, _ := waiting(t, f, id)
	worker := f.exec.awaitStart(t)
	refused := completed("refused")
	refused.StopReason = "refusal"
	f.exec.finish(worker, refused)
	run, rows := f.await(t, id, terminal)
	if run.Status != store.WorkflowStatusFailed || node(rows, "iter").Status != store.WorkflowStatusFailed {
		t.Fatalf("run %s iter %+v", run.Status, node(rows, "iter"))
	}
	abandoned(t, node(rows, "a"), row.ID)
}

// Re-arming replaces the attempt's timer, so Close never waits on a replaced one.
func TestRearmReplacesTheTimer(t *testing.T) {
	f := newFixture(t)
	retryClock(t, f)
	ctx := context.Background()
	id := f.run(t, retryGraph(""), str("input"))
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	failHeld(t, f, id, f.exec.awaitStart(t))
	row, _ := waiting(t, f, id)
	// The frozen clock keeps the 10s wait early, so each wake re-arms it.
	for range 3 {
		if err := f.host.engine.WakeRetry(ctx, id, row.ID); err != nil {
			t.Fatal(err)
		}
	}
	closed := make(chan struct{})
	go func() { f.host.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close waited for a replaced retry timer")
	}
}

// A run that another branch failed while a was executing takes no retry when a then fails
// retryably: the late failure leaves no wait and no history.
func TestRetryableFailureAfterTheRunFailedSchedulesNothing(t *testing.T) {
	f := newFixture(t)
	retryClock(t, f)
	ctx := context.Background()
	id := f.run(t, independentAgents(""), str("input"))
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	started := f.exec.awaitStarts(t, 2)
	session := started["a"]
	aAttempt := f.exec.request(session).Caller.AttemptID
	// b executes beside a; failing it non-retryably fails the run while a executes.
	if err := f.host.engine.FailNode(ctx, id, f.exec.request(started["b"]).Caller.AttemptID, workflow.Failure{Kind: workflow.FailureRepository, Message: "b broke"}); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(started["b"], completed("late"))
	if run, _ := f.await(t, id, terminal); run.Status != store.WorkflowStatusFailed {
		t.Fatalf("run = %s", run.Status)
	}
	if err := f.host.engine.FailNode(ctx, id, aAttempt, workflow.Failure{Kind: workflow.FailureSession, Message: "a lost"}); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(session, execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	f.host.Close() // waits for a's worker, which reports the session's failure too
	rows, err := f.s.WorkflowEngineNodeRuns(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if a := node(rows, "a"); a.ID != aAttempt || strings.Contains(string(a.Payload), "retry_wait") {
		t.Fatalf("a late retryable failure under a failed run was retried: %+v %s", a, a.Payload)
	}
	history, err := f.s.WorkflowRunFailedAttempts(ctx, f.scope.TenantID, id)
	if err != nil || len(history) != 0 {
		t.Fatalf("history = %+v %v, want none", history, err)
	}
	if n := starts(f); n != 2 {
		t.Fatalf("started %d sessions, want only a's and b's", n)
	}
}

// The replaced attempt records whether the run hands its failure to the next prompt, from the
// run's injectLastFailure.
func TestReplacedAttemptRecordsTheRunsInjectSetting(t *testing.T) {
	for _, inject := range []bool{true, false} {
		t.Run(fmt.Sprintf("inject=%v", inject), func(t *testing.T) {
			f := newFixture(t)
			retryClock(t, f)
			ctx := context.Background()
			graph := retryGraph("")
			id := f.run(t, graph, str("input"))
			g, err := workflow.ParseGraph(graph)
			if err != nil {
				t.Fatal(err)
			}
			payload := workflow.NewRunPayload(g, workflow.LocaleZhCN)
			payload.InjectLastFailure = inject
			if err := f.s.UpdateWorkflowEngineRunInput(ctx, id, str("input"), func([]byte) ([]byte, error) { return json.Marshal(payload) }); err != nil {
				t.Fatal(err)
			}
			if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
				t.Fatal(err)
			}
			session := f.exec.awaitStart(t)
			if err := f.host.engine.FailNode(ctx, id, f.exec.request(session).Caller.AttemptID, workflow.Failure{Kind: workflow.FailureAgentRefusal, Message: "refused"}); err != nil {
				t.Fatal(err)
			}
			f.exec.finish(session, completed("late result"))
			waiting(t, f, id)
			history, err := f.s.WorkflowRunFailedAttempts(ctx, f.scope.TenantID, id)
			if err != nil || len(history) != 1 {
				t.Fatalf("history = %+v %v", history, err)
			}
			var p struct {
				Detail map[string]any `json:"error_detail"`
			}
			if err := json.Unmarshal(history[0].Payload, &p); err != nil {
				t.Fatal(err)
			}
			if got := p.Detail["injects_previous_failure"]; got != inject {
				t.Fatalf("replaced attempt injects_previous_failure = %v, want %v (%s)", got, inject, history[0].Payload)
			}
		})
	}
}
