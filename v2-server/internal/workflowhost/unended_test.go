package workflowhost

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// refuseEndOf makes End of the session reported by which() unconfirmed until the returned confirm
// is called; End of every other session behaves as before. It also shortens the retry delay so
// that a retry follows soon after confirm.
func (f *fixture) refuseEndOf(t *testing.T, which func() string) (confirm func()) {
	t.Helper()
	var confirmed atomic.Bool
	f.host.endRetry = 20 * time.Millisecond
	end := f.exec.EndFunc
	f.exec.EndFunc = func(ctx context.Context, scope execution.Scope, id string) error {
		if id == which() && !confirmed.Load() {
			return execution.ErrStopUnconfirmed
		}
		return end(ctx, scope, id)
	}
	return func() { confirmed.Store(true) }
}

// countEndsOf counts the End calls of the session reported by which(), whatever their outcome.
func (f *fixture) countEndsOf(which func() string) func() int64 {
	var n atomic.Int64
	end := f.exec.EndFunc
	f.exec.EndFunc = func(ctx context.Context, scope execution.Scope, id string) error {
		if id == which() {
			n.Add(1)
		}
		return end(ctx, scope, id)
	}
	return n.Load
}

// An automatic attempt gives its slot back only once its session's End returned: until then Node
// may still hold the session, and a sibling waiting for the slot stays queued.
func TestAnAttemptKeepsItsSlotUntilEndReturns(t *testing.T) {
	f := newFixture(t)
	f.host.sessionLimit = 1
	var first atomic.Value
	first.Store("")
	ending, finishEnd := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(finishEnd) }) }
	t.Cleanup(release)
	end := f.exec.EndFunc
	f.exec.EndFunc = func(ctx context.Context, scope execution.Scope, id string) error {
		if id == first.Load().(string) {
			close(ending)
			<-finishEnd
		}
		return end(ctx, scope, id)
	}
	runID := f.run(t, parallelAgents, nil)
	if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	first.Store(a)
	f.exec.finish(a, completed("one"))
	<-ending
	f.exec.noStart(t, "a sibling started before the first session's End returned")
	release()
	b := f.exec.awaitStart(t)
	f.exec.finish(b, completed("two"))
	if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("run %s", run.Status)
	}
}

// An End the execution service did not confirm keeps the attempt's slot after the attempt is gone:
// the session is ended again in the background, and only a confirmed End lets the waiting sibling
// start. The run's state is kept until then.
func TestAnUnconfirmedEndKeepsTheSlotUntilARetryConfirmsIt(t *testing.T) {
	f := newFixture(t)
	f.host.sessionLimit = 1
	var first atomic.Value
	first.Store("")
	confirm := f.refuseEndOf(t, func() string { return first.Load().(string) })
	runID := f.run(t, parallelAgents, nil)
	if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	first.Store(a)
	f.exec.finish(a, completed("one"))
	f.exec.noStart(t, "a sibling started while the first session may still be live")
	if f.exec.isClosed(a) {
		t.Fatal("the fake recorded an End it refused")
	}
	confirm()
	b := f.exec.awaitStart(t)
	if !f.exec.isClosed(a) {
		t.Fatal("the sibling started before the first session's End was confirmed")
	}
	f.exec.finish(b, completed("two"))
	if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("run %s", run.Status)
	}
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
}

// A stopped interactive turn whose End is not confirmed keeps the node's slot: the waiting sibling
// starts only after completing the node ends the session for certain.
func TestUnconfirmedEndOfAStoppedTurnKeepsTheSlot(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.host.sessionLimit = 1
	waiting, release := f.holdDispatchOf(t, "c")
	runID := f.run(t, parallelInteractive, nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	<-waiting
	release()
	f.exec.finish(a, completed("在"))
	f.await(t, runID, parked("a"))
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "继续"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	confirm := f.refuseEndOf(t, func() string { return a })
	f.exec.onCancel = func(ref execution.Ref) error {
		f.exec.finish(ref.SessionID, stopEnded())
		return nil
	}
	if _, err := f.host.StopNodeTurn(ctx, f.scope, runID, "a"); err != nil {
		t.Fatal(err)
	}
	f.await(t, runID, parked("a"))
	f.exec.noStart(t, "C started while A's session may still be live")
	confirm()
	// The stopped follow-up blocks a plain completion; forcing it takes the answer anyway.
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", server.WorkflowNodeCompletion{Force: true}); err != nil {
		t.Fatal(err)
	}
	c := f.exec.awaitStart(t)
	if f.exec.request(c).Caller.NodeID != "c" {
		t.Fatalf("started %s, want C", f.exec.request(c).Caller.NodeID)
	}
	f.exec.finish(c, completed("好"))
	if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("run %s", run.Status)
	}
}

// A sibling failing the run releases the parked interactive node. Its End is tried once; when it
// is not confirmed, the failed run has no use for the slot, so the session gives it back without
// a retry (a retry could make the execution service rebuild the project's Node early). A resume
// admitted right away starts both nodes.
func TestAnOrphanOfAFailedRunGivesItsSlotBackWithoutARetry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.host.sessionLimit = 2
	runID := f.run(t, parallelInteractive, nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	started := f.exec.awaitStarts(t, 2)
	a, c := started["a"], started["c"]
	f.refuseEndOf(t, func() string { return a })
	f.host.endRetry = time.Hour // the slot must come back without waiting for a retry
	ends := f.countEndsOf(func() string { return a })
	f.exec.finish(a, completed("在"))
	f.await(t, runID, parked("a"))
	f.exec.finish(c, execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	f.await(t, runID, func(r store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return r.Status == store.WorkflowStatusFailed && node(n, "a").Status == store.WorkflowStatusCancelled
	})
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
	if n := ends(); n != 1 {
		t.Fatalf("A's session was ended %d times, want once and no retry", n)
	}
	if _, err := f.host.ResumeRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	resumed := f.exec.awaitStarts(t, 2)
	if resumed["a"] == "" || resumed["c"] == "" {
		t.Fatalf("resumed %v, want A and C", resumed)
	}
}

// While the run runs, an unconfirmed End of an automatic attempt is retried; once the run fails or
// is cancelled the retries stop and the slot is given back.
func TestALingeringEndIsRetriedOnlyWhileTheRunRuns(t *testing.T) {
	graph := strings.Replace(parallelAgents, `"prompt":"B"}`, `"prompt":"B","retry":{"enabled":false,"maxRetries":0,"initialDelaySeconds":1}}`, 1)
	for name, stop := range map[string]func(t *testing.T, f *fixture, runID, b string){
		"failed": func(_ *testing.T, f *fixture, _, b string) {
			f.exec.finish(b, execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
		},
		"cancelled": func(t *testing.T, f *fixture, runID, _ string) {
			if _, err := f.host.CancelRun(context.Background(), f.scope, runID); err != nil {
				t.Error(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			var first atomic.Value
			first.Store("")
			f.refuseEndOf(t, func() string { return first.Load().(string) })
			ends := f.countEndsOf(func() string { return first.Load().(string) })
			runID := f.run(t, graph, nil)
			if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
				t.Fatal(err)
			}
			started := f.exec.awaitStarts(t, 2)
			first.Store(started["a"])
			f.exec.finish(started["a"], completed("A"))
			deadline := time.Now().Add(5 * time.Second)
			for ends() < 2 {
				if time.Now().After(deadline) {
					t.Fatalf("A's End was tried %d times while the run ran, want retries", ends())
				}
				time.Sleep(f.host.endRetry)
			}
			stop(t, f, runID, started["b"])
			f.await(t, runID, terminal)
			f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
			settled := ends()
			// Retries back off: the window covers several of them.
			time.Sleep(30 * f.host.endRetry)
			if n := ends(); n > settled+1 {
				t.Fatalf("A's End was tried %d more times after the run stopped", n-settled)
			}
		})
	}
}

// A stopped turn may leave nothing to complete, so no completion would end its session again: an
// unconfirmed End is retried in the background, and the waiting sibling starts once it is
// confirmed while the node is still parked.
func TestARetryConfirmsTheEndOfAStoppedTurn(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.host.sessionLimit = 1
	waiting, release := f.holdDispatchOf(t, "c")
	runID := f.run(t, parallelInteractive, nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	<-waiting
	release()
	f.exec.finish(a, completed("在"))
	f.await(t, runID, parked("a"))
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "继续"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	confirm := f.refuseEndOf(t, func() string { return a })
	f.exec.onCancel = func(ref execution.Ref) error {
		f.exec.finish(ref.SessionID, stopEnded())
		return nil
	}
	if _, err := f.host.StopNodeTurn(ctx, f.scope, runID, "a"); err != nil {
		t.Fatal(err)
	}
	f.await(t, runID, parked("a"))
	f.exec.noStart(t, "C started while A's session may still be live")
	confirm()
	c := f.exec.awaitStart(t)
	if f.exec.request(c).Caller.NodeID != "c" || !f.exec.isClosed(a) {
		t.Fatalf("started %s (A closed = %v), want C after A's End", f.exec.request(c).Caller.NodeID, f.exec.isClosed(a))
	}
	if row := f.nodeRow(t, runID, "a"); row.Status != store.WorkflowStatusPending {
		t.Fatalf("A = %s, want it still parked", row.Status)
	}
}

// An End reporting the session unknown or already ended proves Node no longer holds it: the slot
// is free at once, without a retry.
func TestAnEndOfAGoneSessionFreesTheSlot(t *testing.T) {
	for _, gone := range []error{execution.ErrNotFound, execution.ErrSessionEnded} {
		t.Run(gone.Error(), func(t *testing.T) {
			f := newFixture(t)
			f.host.sessionLimit = 1
			f.host.endRetry = time.Hour // a retry would never come in time
			end := f.exec.EndFunc
			f.exec.EndFunc = func(ctx context.Context, scope execution.Scope, id string) error {
				_ = end(ctx, scope, id)
				return gone
			}
			runID := f.run(t, parallelAgents, nil)
			if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
				t.Fatal(err)
			}
			f.exec.finish(f.exec.awaitStart(t), completed("one"))
			f.exec.finish(f.exec.awaitStart(t), completed("two"))
			if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusSucceeded {
				t.Fatalf("run %s", run.Status)
			}
			f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
		})
	}
}

// Shutting the host down does not wait for the next retry of a lingering End: the retry leaves at
// once, and Node is left to end the session itself.
func TestCloseDoesNotWaitForTheRetryOfALingeringEnd(t *testing.T) {
	f := newFixture(t)
	f.host.sessionLimit = 1
	var first atomic.Value
	first.Store("")
	f.refuseEndOf(t, func() string { return first.Load().(string) })
	f.host.endRetry = time.Hour
	runID := f.run(t, parallelAgents, nil)
	if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	first.Store(a)
	f.exec.finish(a, completed("one"))
	// The sibling waits for A's slot, so the run keeps running and A's End keeps being retried.
	f.exec.noStart(t, "the sibling started while A's session may still be live")
	closed := make(chan struct{})
	go func() { f.host.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close waited for the retry of a lingering End")
	}
}

// An End reporting the session of a parked node unknown or already ended proves it gone: the node
// can still be completed, and its run cancelled.
func TestAParkedNodeWhoseSessionIsGoneCompletesAndCancels(t *testing.T) {
	gone := func(f *fixture, session string) {
		end := f.exec.EndFunc
		f.exec.EndFunc = func(ctx context.Context, scope execution.Scope, id string) error {
			err := end(ctx, scope, id)
			if id == session {
				return execution.ErrNotFound
			}
			return err
		}
	}
	t.Run("complete", func(t *testing.T) {
		f := newFixture(t)
		runID, a := f.startParked(t, interactiveChain, "初稿")
		gone(f, a)
		if _, err := f.host.CompleteNode(context.Background(), f.scope, runID, "a", confirm); err != nil {
			t.Fatal(err)
		}
		f.exec.finish(f.exec.awaitStart(t), completed("可以"))
		if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusSucceeded {
			t.Fatalf("run %s", run.Status)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		f := newFixture(t)
		runID, a := f.startParked(t, interactiveChain, "初稿")
		gone(f, a)
		if _, err := f.host.CancelRun(context.Background(), f.scope, runID); err != nil {
			t.Fatal(err)
		}
		if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusCancelled {
			t.Fatalf("run %s", run.Status)
		}
	})
}

// A parked node whose End keeps failing after a stopped turn is retried while the run runs. A
// cancellation stops the retries, also while the run stays open because the stop was not
// confirmed.
func TestACancellationStopsTheRetryOfAParkedNodesEnd(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿")
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "继续"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.refuseEndOf(t, func() string { return a })
	ends := f.countEndsOf(func() string { return a })
	f.exec.onCancel = func(ref execution.Ref) error {
		f.exec.finish(ref.SessionID, stopEnded())
		return nil
	}
	if _, err := f.host.StopNodeTurn(ctx, f.scope, runID, "a"); err != nil {
		t.Fatal(err)
	}
	f.await(t, runID, parked("a"))
	deadline := time.Now().Add(5 * time.Second)
	for ends() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("A's End was tried %d times while the run ran, want retries", ends())
		}
		time.Sleep(f.host.endRetry)
	}
	if _, err := f.host.CancelRun(ctx, f.scope, runID); !errors.Is(err, server.ErrWorkflowRunStopUnconfirmed) {
		t.Fatalf("cancel = %v, want the stop unconfirmed", err)
	}
	settled := ends()
	// Retries back off: the window covers several of them.
	time.Sleep(30 * f.host.endRetry)
	if n := ends(); n > settled+1 {
		t.Fatalf("A's End was tried %d more times after the cancellation", n-settled)
	}
}
