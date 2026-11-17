package workflowhost

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
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
		f.exec.finish(ref.SessionID, completed("停"))
		return nil
	}
	if _, err := f.host.StopNodeTurn(ctx, f.scope, runID, "a"); err != nil {
		t.Fatal(err)
	}
	f.await(t, runID, parked("a"))
	f.exec.noStart(t, "C started while A's session may still be live")
	confirm()
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); err != nil {
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

// A sibling failing the run releases the parked interactive node; when its session's End is not
// confirmed, the session keeps its slot. A resume admitted right away therefore starts only as many
// sessions as Node has room for, instead of being refused at the checkout's limit, and the rest
// starts once a retry confirms the End.
func TestOrphanWithAnUnconfirmedEndKeepsItsSlotAcrossAResume(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.host.sessionLimit = 2
	runID := f.run(t, parallelInteractive, nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	started := f.exec.awaitStarts(t, 2)
	a, c := started["a"], started["c"]
	confirm := f.refuseEndOf(t, func() string { return a })
	f.exec.finish(a, completed("在"))
	f.await(t, runID, parked("a"))
	f.exec.finish(c, execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	f.await(t, runID, func(r store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return r.Status == store.WorkflowStatusFailed && node(n, "a").Status == store.WorkflowStatusCancelled
	})
	if _, err := f.host.ResumeRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	first := f.exec.awaitStart(t)
	f.exec.noStart(t, "the resume started a second session while A's old session may still be live")
	confirm()
	second := f.exec.awaitStart(t)
	if !f.exec.isClosed(a) {
		t.Fatal("the second session started before A's old session was ended")
	}
	resumed := map[string]string{}
	for _, id := range []string{first, second} {
		resumed[f.exec.request(id).Caller.NodeID] = id
	}
	if resumed["a"] == "" || resumed["c"] == "" {
		t.Fatalf("resumed %v, want A and C", resumed)
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
		f.exec.finish(ref.SessionID, completed("停"))
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
	var last atomic.Value
	last.Store("")
	f.refuseEndOf(t, func() string { return last.Load().(string) })
	f.host.endRetry = time.Hour
	runID := f.run(t, parallelAgents, nil)
	if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(f.exec.awaitStart(t), completed("one"))
	b := f.exec.awaitStart(t)
	last.Store(b)
	f.exec.finish(b, completed("two"))
	if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("run %s", run.Status)
	}
	f.awaitIdle(t, runID, "an attempt outlived the run")
	if st := f.host.state(runID); st == nil {
		t.Fatal("the run's state was dropped while B's End was unconfirmed")
	}
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
		if _, err := f.host.CompleteNode(context.Background(), f.scope, runID, "a", false, "human"); err != nil {
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
