package workflowhost

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// blockEnd holds every End of session until the returned release is called, and reports on the
// returned channel when the first one began.
func (f *fixture) blockEnd(t *testing.T, session string) (began <-chan struct{}, release func()) {
	t.Helper()
	called, gate := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	end := f.exec.EndFunc
	f.exec.EndFunc = func(ctx context.Context, scope execution.Scope, id string) error {
		if id == session {
			once.Do(func() { close(called) })
			<-gate
		}
		return end(ctx, scope, id)
	}
	release = func() { releaseOnce.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return called, release
}

// inUse reads the run's occupied session slots.
func (f *fixture) inUse(runID string) int {
	st := f.host.state(runID)
	if st == nil {
		return -1
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.sessionsInUse()
}

// A recovered node whose checkout turned out to be gone keeps its slot until its End returns, as a
// first turn that could not be set up does, and a resume of the failed run then starts A again
// under a limit of one session.
func TestAMissingWorkspaceKeepsItsSlotUntilItsEndReturns(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	_, nodes := f.await(t, runID, parked("a"))
	f.exec.recordedTurns(a, node(nodes, "a").ID, completed("初稿一"))
	f.restart(t)
	f.host.sessionLimit = 1

	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再改一版"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	began, release := f.blockEnd(t, a)
	f.exec.finish(a, execution.Result{Status: execution.StatusFailed, FailureCode: execution.FailureWorkspaceMissing, AgentLost: true})
	receive(t, began)
	if used := f.inUse(runID); used != 1 {
		t.Fatalf("sessions in use while A's End is unconfirmed = %d, want A's slot kept", used)
	}
	release()
	if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusFailed {
		t.Fatalf("run %s", run.Status)
	}
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
	if _, err := f.host.ResumeRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	if again := f.exec.awaitStart(t); f.exec.request(again).Caller.NodeID != "a" {
		t.Fatalf("resumed %s, want A", f.exec.request(again).Caller.NodeID)
	}
}

// A parked node the restart fails keeps its session's slot until its End returns, like any
// session no node tracks, and the End is retried while it is not confirmed.
func TestAParkedNodeFailedOnRestartKeepsItsSlotUntilItsEndReturns(t *testing.T) {
	f := newFixture(t)
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	_, nodes := f.await(t, runID, parked("a"))
	f.exec.recordedTurns(a, node(nodes, "a").ID, completed("初稿一"))
	f.exec.ListExecutorsFunc = func(context.Context, execution.Scope) ([]execution.Executor, error) { return nil, nil }
	began, release := f.blockEnd(t, a)

	f.host.Close()
	f.host = f.newHost(t)
	interrupted := make(chan error, 1)
	go func() { interrupted <- f.host.Interrupt(context.Background()) }()
	receive(t, began)
	if used := f.inUse(runID); used != 1 {
		t.Fatalf("sessions in use while A's End is unconfirmed = %d, want A's slot kept", used)
	}
	release()
	if err := receive(t, interrupted); err != nil {
		t.Fatal(err)
	}
	if run, nodes := f.await(t, runID, terminal); run.Status != store.WorkflowStatusFailed || node(nodes, "a").Status != store.WorkflowStatusFailed {
		t.Fatalf("run %s nodes %+v, want A failed with its run", run.Status, nodes)
	}
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
	if !f.exec.isClosed(a) {
		t.Fatal("the session of the failed node was never ended")
	}
}

// A node whose session ended before the restart without a confirmed End (its Agent was lost and
// Node never confirmed the End) has nothing retrying that End after the restart: the recovery ends
// it, so the node gives its slot back and still awaits completion with its last answer.
func TestARecoveredNodeWhoseSessionEndedIsEndedAgain(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	_, nodes := f.await(t, runID, parked("a"))
	rowA := node(nodes, "a")
	end := f.exec.EndFunc
	f.exec.EndFunc = func(ctx context.Context, scope execution.Scope, id string) error {
		if id == a {
			return execution.ErrStopUnconfirmed
		}
		return end(ctx, scope, id)
	}
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再改一版"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.exec.finish(a, execution.Result{Status: execution.StatusFailed, FailureCode: "agent_transport_closed", AgentLost: true})
	f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusPending && interactivePayload(t, node(n, "a"))["session_ended"] == true
	})
	// The store still reads the session as open: its End was never confirmed.
	f.exec.recordedTurns(a, rowA.ID, completed("初稿一"), execution.Result{Status: execution.StatusFailed, FailureCode: "agent_transport_closed", AgentLost: true})
	f.exec.EndFunc = end
	ends := f.countEndsOf(func() string { return a })

	f.restart(t)
	f.await(t, runID, awaitingInput("a"))
	if ends() == 0 {
		t.Fatal("the restart left the ended session's End unretried")
	}
	st := f.host.state(runID)
	st.mu.Lock()
	n := st.interactive["a"]
	gone := n != nil && n.sessionGone
	used := st.sessionsInUse()
	st.mu.Unlock()
	if !gone || used != 0 {
		t.Fatalf("recovered node gone=%v, sessions in use %d, want its slot given back", gone, used)
	}
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); err != nil {
		t.Fatalf("complete with the last answer: %v", err)
	}
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
		var zero T
		return zero
	}
}
