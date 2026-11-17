package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// Tests for the interactive bridge's less travelled branches: queue order, a stop of a queued
// turn, refusals that must leave the node claimable, and orphan closing.

// parallelInteractive runs interactive A beside automatic C in one wave; A is dispatched first.
const parallelInteractive = `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"聊","interactive":true}}},
 {"id":"c","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"并行","retry":{"enabled":false,"maxRetries":0,"initialDelaySeconds":1}}}}
],"edges":[{"source":"start","target":"a"},{"source":"start","target":"c"}]}`

// holdDispatchOf makes the attempt of nodeID wait, before it does anything, until release is
// called. It returns a channel closed once the attempt is waiting; release also runs at cleanup,
// so a failing test never strands the attempt.
func (f *fixture) holdDispatchOf(t *testing.T, nodeID string) (<-chan struct{}, func()) {
	t.Helper()
	var once, entered sync.Once
	waiting, released := make(chan struct{}), make(chan struct{})
	release := func() { once.Do(func() { close(released) }) }
	t.Cleanup(release)
	f.host.dispatching = func(d workflow.AgentDispatch) {
		if d.Node.ID == nodeID {
			entered.Do(func() { close(waiting) })
			<-released
		}
	}
	return waiting, release
}

func (f *fixture) nodeRow(t *testing.T, runID, id string) store.WorkflowNodeRun {
	t.Helper()
	nodes, err := f.s.WorkflowEngineNodeRuns(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return node(nodes, id)
}

// R2: a follow-up the starter sent runs at once, beside a sibling's executing turn: each node's
// turns are its own session's, and the run's sessions execute concurrently.
func TestFollowUpRunsBesideASiblingTurn(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, parallelInteractive, nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	started := f.exec.awaitStarts(t, 2)
	a, c := started["a"], started["c"]
	f.exec.finish(a, completed("在"))
	f.await(t, runID, parked("a"))
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "先回我"); err != nil {
		t.Fatal(err)
	}
	if id := f.exec.awaitAppend(t); id != a {
		t.Fatalf("follow-up went to %s", id)
	}
	if f.exec.isClosed(c) {
		t.Fatal("the follow-up disturbed C's executing turn")
	}
	f.exec.finish(a, completed("回你了"))
	f.await(t, runID, parked("a"))
	f.exec.finish(c, completed("好"))
	_, nodes := f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "c").Status == store.WorkflowStatusSucceeded
	})
	if p := interactivePayload(t, node(nodes, "a")); p["session_ended"] != false {
		t.Fatalf("A after both turns = %v, want parked with its session open", p)
	}
}

// R3b, R7: while C is still being prepared (no session yet), the starter's follow-up to A runs at
// once. Stopping it ends the session, and the node parks for completion with its first answer.
func TestStopEndsAFollowUpWhileASiblingIsPrepared(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	waiting, release := f.holdDispatchOf(t, "c")
	runID := f.run(t, parallelInteractive, nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	f.exec.finish(a, completed("在"))
	f.await(t, runID, parked("a"))
	<-waiting
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "不用等 C"); err != nil {
		t.Fatal(err)
	}
	if id := f.exec.awaitAppend(t); id != a {
		t.Fatalf("follow-up went to %s", id)
	}
	if _, err := f.host.StopNodeTurn(ctx, f.scope, runID, "a"); err != nil {
		t.Fatalf("stopping the follow-up = %v", err)
	}
	_, nodes := f.await(t, runID, parked("a"))
	if p := interactivePayload(t, node(nodes, "a")); p["session_ended"] != true || p["completable"] != true || !f.exec.isClosed(a) {
		t.Fatalf("summary = %v closed = %v", p, f.exec.isClosed(a))
	}
	release()
	c := f.exec.awaitStart(t)
	f.exec.finish(c, completed("好"))
}

func TestTurnRefusedByAStoppedRunLeavesTheNodeClaimable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	waiting, release := f.holdDispatchOf(t, "c")
	runID := f.run(t, parallelInteractive, nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	f.exec.finish(a, completed("在"))
	f.await(t, runID, parked("a"))
	<-waiting
	// C fails the run while its attempt is still held, before the attempt's sweep.
	c := f.nodeRow(t, runID, "c")
	if err := f.host.engine.FailNode(ctx, runID, c.ID, workflow.Failure{Kind: workflow.FailureSession, Message: "旁支失败"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "还在吗"); !errors.Is(err, server.ErrWorkflowNodeNotAwaitingInput) {
		t.Fatalf("turn under a failed run = %v", err)
	}
	// The refused move gave the claim back, so the completion can still settle the node at once:
	// its accepted answer is recorded under the failed run, as any root attempt that settles after
	// a sibling failed the run is, and a resume from failure reuses it.
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); err != nil {
		t.Fatalf("completion under a failed run = %v", err)
	}
	if r := f.nodeRow(t, runID, "a"); r.Status != store.WorkflowStatusSucceeded || !f.exec.isClosed(a) {
		t.Fatalf("A = %+v closed = %v, want settled by the completion", r, f.exec.isClosed(a))
	}
	release()
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
}

// R6: a run that stopped while the node's turn ran refuses the park; the node must not stay
// running under the stopped run.
func TestRefusedParkClosesTheNode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, parallelInteractive, nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	started := f.exec.awaitStarts(t, 2)
	a := started["a"]
	c := f.nodeRow(t, runID, "c")
	if err := f.host.engine.FailNode(ctx, runID, c.ID, workflow.Failure{Kind: workflow.FailureSession, Message: "旁支失败"}); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(a, completed("在"))
	_, nodes := f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status != store.WorkflowStatusRunning
	})
	if node(nodes, "a").Status != store.WorkflowStatusCancelled || !f.exec.isClosed(a) {
		t.Fatalf("A = %+v closed = %v", node(nodes, "a"), f.exec.isClosed(a))
	}
	f.exec.finish(started["c"], completed("晚了")) // C executed beside A; its late answer ends it
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
}

// R8: a follow-up whose session ended where the bridge could not see it is refused by Append; the
// node then says its session ended instead of offering another message that would be lost too.
func TestAppendToAnEndedSessionMarksItEnded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	f.exec.mu.Lock()
	f.exec.closed[a] = true
	f.exec.mu.Unlock()
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "继续"); err != nil {
		t.Fatal(err)
	}
	_, nodes := f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusPending && interactivePayload(t, node(n, "a"))["session_ended"] == true
	})
	if p := interactivePayload(t, node(nodes, "a")); p["completable"] != true || p["turns"] != float64(1) {
		t.Fatalf("summary = %v", p)
	}
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再试"); !errors.Is(err, server.ErrWorkflowNodeSessionEnded) {
		t.Fatalf("turn after the refused append = %v", err)
	}
}

// R10: the starter's request ending (a closed tab) after the completion claimed the node does not
// abandon it: the output is written and the successor starts.
func TestCompletionOutlivesItsRequest(t *testing.T) {
	f := newFixture(t)
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	ctx, cancel := context.WithCancel(context.Background())
	end := f.exec.EndFunc
	f.exec.EndFunc = func(c context.Context, scope execution.Scope, id string) error {
		if id == a {
			cancel() // the client goes away while the session ends
		}
		return end(c, scope, id)
	}
	_, _ = f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human")
	b := f.exec.awaitStart(t)
	if f.exec.request(b).Caller.NodeID != "b" {
		t.Fatalf("successor = %s", f.exec.request(b).Caller.NodeID)
	}
	if r := f.nodeRow(t, runID, "a"); r.Status != store.WorkflowStatusSucceeded || r.Output == nil || *r.Output != "初稿一" {
		t.Fatalf("A = %+v", r)
	}
	if f.host.state(runID) == nil {
		t.Fatal("the run state was dropped while B runs")
	}
	st := f.host.state(runID)
	st.mu.Lock()
	_, tracked := st.interactive["a"]
	st.mu.Unlock()
	if tracked {
		t.Fatal("the completed node is still tracked as parked")
	}
}

// Own mutant: a stop always ends the session, even when the turn ended just before the stop
// reached it (the execution service then returns that turn's result and keeps the session).
func TestStopEndsTheSessionEvenWhenTheTurnJustEnded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "继续"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.exec.onCancel = func(ref execution.Ref) error {
		f.exec.finish(ref.SessionID, completed("刚好写完"))
		return nil
	}
	if _, err := f.host.StopNodeTurn(ctx, f.scope, runID, "a"); err != nil {
		t.Fatal(err)
	}
	_, nodes := f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusPending && interactivePayload(t, node(n, "a"))["turns"] == float64(2)
	})
	if p := interactivePayload(t, node(nodes, "a")); p["session_ended"] != true || p["answer"] != "刚好写完" || !f.exec.isClosed(a) {
		t.Fatalf("summary = %v closed = %v", p, f.exec.isClosed(a))
	}
}

// waitUntil polls cond for a short while, for state no event announces (the hub is held).
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// cancelBeforeStart is the host's repository with one fault: when armed, the next successor
// start first cancels the run, the interleaving where a completion commits and the cancellation
// lands before its successor wave starts.
type cancelBeforeStart struct {
	repository
	tenant string
	armed  *bool
}

func (r cancelBeforeStart) StartNodes(ctx context.Context, runID string, wave []workflow.NodeStart) ([]workflow.NodeRun, error) {
	if *r.armed {
		*r.armed = false
		if _, err := r.s.CancelWorkflowRun(ctx, r.tenant, runID); err != nil {
			return nil, err
		}
	}
	return r.repository.StartNodes(ctx, runID, wave)
}

// R4: the row decides who won. A completion that committed wins even when the successor wave then
// meets the cancellation and the engine reports that error.
func TestCompletionThatCommittedWinsOverALateCancellation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	armed := false
	f.host.engine = workflow.NewEngine(cancelBeforeStart{repository{f.s}, f.scope.TenantID, &armed}, f.host, f.host.hub.Publish, f.host.now)
	runID, _ := f.startParked(t, interactiveChain, "初稿一")
	armed = true // only the completion's successor wave starts nodes from here on
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); err != nil {
		t.Fatalf("a committed completion = %v, want its win reported", err)
	}
	if r := f.nodeRow(t, runID, "a"); r.Status != store.WorkflowStatusSucceeded || r.Output == nil || *r.Output != "初稿一" {
		t.Fatalf("A = %+v", r)
	}
	if run, _ := f.s.WorkflowRun(ctx, f.scope.TenantID, runID); run.Status != store.WorkflowStatusCancelled {
		t.Fatalf("run = %s, want the cancellation kept", run.Status)
	}
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
}

// A resume from failure restarts a node that was parked when a sibling failed the run: the sweep
// closed its row as cancelled and ended its session, so the resumed run gives it a new attempt
// with a new session, and the starter talks to and completes that one.
func TestResumeRestartsANodeParkedWhenTheRunFailed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, parallelInteractive, nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	started := f.exec.awaitStarts(t, 2)
	a, c := started["a"], started["c"]
	f.exec.finish(a, completed("在"))
	f.await(t, runID, parked("a"))
	f.exec.finish(c, execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	_, nodes := f.await(t, runID, func(r store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return r.Status == store.WorkflowStatusFailed && node(n, "a").Status == store.WorkflowStatusCancelled
	})
	first := node(nodes, "a").ID
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })

	if _, err := f.host.ResumeRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	// The resume restarts the cancelled A and the failed C, which execute concurrently again.
	resumed := f.exec.awaitStarts(t, 2)
	again := resumed["a"]
	if again == "" || again == a || !f.exec.isClosed(a) {
		t.Fatalf("resumed A runs in %s (was %s, closed = %v), node %s", again, a, f.exec.isClosed(a), f.exec.request(again).Caller.NodeID)
	}
	f.exec.finish(again, completed("重来"))
	_, nodes = f.await(t, runID, parked("a"))
	if row := node(nodes, "a"); row.ID == first || interactivePayload(t, row)["turns"] != float64(1) {
		t.Fatalf("resumed A = %+v, want a new attempt on its first turn", row)
	}
	f.exec.finish(resumed["c"], completed("旁支好了"))
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); err != nil {
		t.Fatal(err)
	}
	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusSucceeded || *node(nodes, "a").Output != "重来" {
		t.Fatalf("run %s, A %+v", run.Status, node(nodes, "a"))
	}
}

// The window the sweep leaves open: the run has failed, but nothing has closed the parked node yet
// (the sibling's attempt is still in flight). A resume in that window must not keep a row
// that reads as awaiting input while its session is about to be ended. ResumeRun closes the
// parked node itself first, so the resumed run restarts it; the worker's late sweep then finds
// the run running and leaves the new attempt alone.
func TestResumeBeforeTheSweepRestartsTheParkedNode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	waiting, release := f.holdDispatchOf(t, "c")
	runID := f.run(t, parallelInteractive, nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	f.exec.finish(a, completed("在"))
	_, nodes := f.await(t, runID, parked("a"))
	first := node(nodes, "a").ID
	<-waiting
	// C fails the run while its attempt is still held, before the attempt's sweep.
	c := f.nodeRow(t, runID, "c")
	if err := f.host.engine.FailNode(ctx, runID, c.ID, workflow.Failure{Kind: workflow.FailureSession, Message: "旁支失败"}); err != nil {
		t.Fatal(err)
	}
	if row := f.nodeRow(t, runID, "a"); row.Status != store.WorkflowStatusPending || f.exec.isClosed(a) {
		t.Fatalf("before the sweep A = %s, closed = %v; the test lost its window", row.Status, f.exec.isClosed(a))
	}
	if _, err := f.host.ResumeRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	if !f.exec.isClosed(a) {
		t.Fatal("the resume kept the parked node's old session")
	}
	// C's stale dispatch is dropped; the resumed attempts start.
	release()
	again := f.exec.awaitStart(t)
	if f.exec.request(again).Caller.NodeID != "a" || again == a {
		t.Fatalf("after the resume session %s of node %s started, want a new session for A", again, f.exec.request(again).Caller.NodeID)
	}
	f.exec.finish(again, completed("重来"))
	_, nodes = f.await(t, runID, parked("a"))
	row := node(nodes, "a")
	if row.ID == first || f.exec.isClosed(again) || interactivePayload(t, row)["session_ended"] != false {
		t.Fatalf("resumed A = %+v, session closed = %v; want a new attempt parked with its session open", row, f.exec.isClosed(again))
	}
	st := f.host.state(runID)
	st.mu.Lock()
	tracked := st.interactive["a"]
	st.mu.Unlock()
	if tracked == nil || tracked.session != again {
		t.Fatalf("the run tracks %+v for A, want the resumed attempt's session %s", tracked, again)
	}
}

// A follow-up executing beside a sibling that fails the run does not park the node again under the
// failed run: its session ends and the row closes instead of staying running under the failed run
// (where it would also keep a resume from failure refused).
func TestFollowUpBesideAFailingSiblingClosesWithTheRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, parallelInteractive, nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	started := f.exec.awaitStarts(t, 2)
	a, c := started["a"], started["c"]
	f.exec.finish(a, completed("在"))
	f.await(t, runID, parked("a"))
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "并行着"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.exec.finish(c, execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	f.await(t, runID, func(r store.WorkflowRun, _ []store.WorkflowNodeRun) bool {
		return r.Status == store.WorkflowStatusFailed
	})
	// C's attempt has returned and swept the run's orphans; A's follow-up still executes, so the
	// sweep leaves it to its turn instead of ending the session underneath it.
	st := f.host.state(runID)
	waitUntil(t, func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return len(st.attempts) == 1 && st.dispatched == 1
	})
	f.host.forgetFinished(runID, st)
	if row := f.nodeRow(t, runID, "a"); row.Status != store.WorkflowStatusRunning || f.exec.isClosed(a) {
		t.Fatalf("A = %s, closed = %v while its follow-up executes", row.Status, f.exec.isClosed(a))
	}
	f.exec.finish(a, completed("晚了"))
	run, nodes := f.await(t, runID, func(r store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusCancelled
	})
	if run.Status != store.WorkflowStatusFailed || node(nodes, "c").Status != store.WorkflowStatusFailed || !f.exec.isClosed(a) {
		t.Fatalf("run %s, C = %s, A closed = %v", run.Status, node(nodes, "c").Status, f.exec.isClosed(a))
	}
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
}

// A follow-up accepted just before a sibling failed the run does not run: its goroutine sees the
// stopped run, and the refused park ends the session and closes the row.
func TestAcceptedFollowUpDoesNotRunAfterTheRunFailed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var once, entered sync.Once
	waiting, released := make(chan struct{}), make(chan struct{})
	release := func() { once.Do(func() { close(released) }) }
	t.Cleanup(release)
	f.host.followingUp = func(nodeID string) {
		if nodeID == "a" {
			entered.Do(func() { close(waiting) })
			<-released
		}
	}
	runID := f.run(t, parallelInteractive, nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	started := f.exec.awaitStarts(t, 2)
	a, c := started["a"], started["c"]
	f.exec.finish(a, completed("在"))
	f.await(t, runID, parked("a"))
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "刚发出"); err != nil {
		t.Fatal(err)
	}
	<-waiting
	f.exec.finish(c, execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	f.await(t, runID, func(r store.WorkflowRun, _ []store.WorkflowNodeRun) bool {
		return r.Status == store.WorkflowStatusFailed
	})
	release()
	run, nodes := f.await(t, runID, func(r store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusCancelled
	})
	if run.Status != store.WorkflowStatusFailed || node(nodes, "c").Status != store.WorkflowStatusFailed || !f.exec.isClosed(a) {
		t.Fatalf("run %s, C = %s, A closed = %v", run.Status, node(nodes, "c").Status, f.exec.isClosed(a))
	}
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
	f.exec.mu.Lock()
	appended := len(f.exec.appended)
	f.exec.mu.Unlock()
	if appended != 0 {
		t.Fatal("the follow-up ran under the failed run")
	}
}

// Cancelling a run stops an executing follow-up beside an automatic sibling. The follow-up's stop is
// confirmed and its turn ends first, but the sibling's is not: the run stays open until that last
// turn ends, then closes as cancelled.
func TestCancelledFollowUpDoesNotCloseTheRunBeforeItsSibling(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, parallelInteractive, nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	started := f.exec.awaitStarts(t, 2)
	a, c := started["a"], started["c"]
	f.exec.finish(a, completed("在"))
	f.await(t, runID, parked("a"))
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "说着"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.exec.onCancel = func(ref execution.Ref) error {
		if ref.SessionID == c {
			return execution.ErrStopUnconfirmed
		}
		return nil
	}
	if _, err := f.host.CancelRun(ctx, f.scope, runID); !errors.Is(err, server.ErrWorkflowRunStopUnconfirmed) {
		t.Fatalf("cancel with one unconfirmed stop = %v", err)
	}
	// A's follow-up has ended and settled; C still executes, so the run stays open.
	st := f.host.state(runID)
	waitUntil(t, func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return len(st.attempts) == 1 && st.dispatched == 1
	})
	if run, _ := f.s.WorkflowRun(ctx, f.scope.TenantID, runID); run.Status != store.WorkflowStatusRunning {
		t.Fatalf("the follow-up closed the run while a sibling's stop is unconfirmed: %s", run.Status)
	}
	f.exec.finish(c, execution.Result{Status: execution.StatusCancelled, StopReason: "cancelled"})
	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusCancelled || node(nodes, "a").Status != store.WorkflowStatusCancelled || node(nodes, "c").Status != store.WorkflowStatusCancelled {
		t.Fatalf("run %s nodes %+v", run.Status, nodes)
	}
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
}

// A stop whose cancellation execution does not confirm still ends the session once the turn ends:
// the node parks for completion and takes no further turn.
func TestUnconfirmedStopOfAFollowUpStillEndsTheSession(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	waiting, release := f.holdDispatchOf(t, "c")
	runID := f.run(t, parallelInteractive, nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	f.exec.finish(a, completed("在"))
	f.await(t, runID, parked("a"))
	<-waiting
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "说着"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.exec.onCancel = func(ref execution.Ref) error {
		if ref.SessionID == a {
			return execution.ErrStopUnconfirmed
		}
		return nil
	}
	if _, err := f.host.StopNodeTurn(ctx, f.scope, runID, "a"); !errors.Is(err, server.ErrWorkflowRunStopUnconfirmed) {
		t.Fatalf("stop with an unconfirmed cancel = %v, want stop unconfirmed", err)
	}
	f.exec.onCancel = nil
	f.exec.finish(a, completed("说完了"))
	_, nodes := f.await(t, runID, parked("a"))
	if p := interactivePayload(t, node(nodes, "a")); p["session_ended"] != true || p["completable"] != true || !f.exec.isClosed(a) {
		t.Fatalf("summary = %v closed = %v, want parked for completion", p, f.exec.isClosed(a))
	}
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "还能说吗"); !errors.Is(err, server.ErrWorkflowNodeSessionEnded) {
		t.Fatalf("turn after the stop = %v, want session ended", err)
	}
	release()
	f.exec.finish(f.exec.awaitStart(t), completed("好"))
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); err != nil {
		t.Fatalf("completion after the stop = %v", err)
	}
	if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("run %s", run.Status)
	}
}

// An interactive node whose first turn never had a session fails as a session that could not be
// set up (resumable, never auto-retried) instead of parking with nothing to talk to or accept:
// Node refused the session over the checkout's limit, or the Agent never started because its
// checkout or plugin could not be prepared. (A session lost after its Agent started still parks,
// see TestLostFirstTurnParksWithoutAnAnswer.)
func TestFirstTurnWithoutASessionFailsTheInteractiveNode(t *testing.T) {
	for name, tc := range map[string]struct {
		result  execution.Result
		message string
	}{
		"Node refused the session": {execution.Result{Status: execution.StatusFailed, FailureCode: execution.FailureCheckoutSessionLimit, AgentLost: true}, "Node refused the Agent session on the run's directory: checkout_session_limit"},
		"preparation failed":       {execution.Result{Status: execution.StatusFailed, FailureCode: "agent_prepare_failed", AgentLost: true, NotStarted: true}, "the Agent session could not be set up: agent_prepare_failed"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			runID := f.run(t, interactiveChain, str("支付重构"))
			if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
				t.Fatal(err)
			}
			a := f.exec.awaitStart(t)
			f.exec.finish(a, tc.result)
			run, nodes := f.await(t, runID, terminal)
			var p struct {
				ErrorDetail struct {
					Kind          string `json:"kind"`
					Message       string `json:"message"`
					AutoRetryable bool   `json:"auto_retryable"`
					Resumable     bool   `json:"resumable"`
				} `json:"error_detail"`
			}
			row := node(nodes, "a")
			if err := json.Unmarshal(row.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if run.Status != store.WorkflowStatusFailed || row.Status != store.WorkflowStatusFailed || p.ErrorDetail.Kind != "interactive_session" || p.ErrorDetail.AutoRetryable || !p.ErrorDetail.Resumable || !strings.Contains(p.ErrorDetail.Message, tc.message) {
				t.Fatalf("run %s, A %s, detail %+v; want a resumable interactive_session failure saying %q", run.Status, row.Status, p.ErrorDetail, tc.message)
			}
			f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
			if starts(f) != 1 {
				t.Fatalf("starts = %d, want no retry", starts(f))
			}
		})
	}
}

// A completion whose End is not confirmed leaves the node parked, but its session takes no
// further turn (the execution service was asked to end it). The node says so at once: the next
// message is refused instead of being accepted and lost, and the completion can be retried.
func TestUnconfirmedEndDuringCompletionClosesTheConversation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿")
	end := f.exec.EndFunc
	f.exec.EndFunc = func(context.Context, execution.Scope, string) error { return execution.ErrStopUnconfirmed }
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); !errors.Is(err, server.ErrWorkflowRunStopUnconfirmed) {
		t.Fatalf("completion with an unconfirmed End = %v", err)
	}
	_, nodes := f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusPending && interactivePayload(t, node(n, "a"))["session_ended"] == true
	})
	if p := interactivePayload(t, node(nodes, "a")); p["completable"] != true || node(nodes, "a").Output != nil {
		t.Fatalf("summary = %v, output = %v; want still parked and completable", p, node(nodes, "a").Output)
	}
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "还能说吗"); !errors.Is(err, server.ErrWorkflowNodeSessionEnded) {
		t.Fatalf("turn after the unconfirmed End = %v, want session ended", err)
	}
	f.exec.mu.Lock()
	appended := len(f.exec.appended)
	f.exec.mu.Unlock()
	if appended != 0 {
		t.Fatal("a turn was accepted for a session that was asked to end")
	}
	f.exec.EndFunc = end
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); err != nil {
		t.Fatalf("retried completion = %v", err)
	}
	b := f.exec.awaitStart(t)
	if f.exec.request(b).Caller.NodeID != "b" || a == b {
		t.Fatalf("after the completion session %s of node %s started", b, f.exec.request(b).Caller.NodeID)
	}
}

// A stop that catches the first turn while its session is still being prepared comes back from
// the execution service as a turn that never started. It is still the starter's stop, not a
// setup failure: the node parks like any stopped turn instead of failing the run.
func TestStopDuringPreparationParksInsteadOfFailing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, interactiveChain, str("支付重构"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusRunning && interactivePayload(t, node(n, "a")) != nil
	})
	f.exec.onCancel = func(execution.Ref) error {
		f.exec.finish(a, execution.Result{Status: execution.StatusFailed, FailureCode: "agent_stop_unconfirmed", AgentLost: true, NotStarted: true})
		return nil
	}
	if _, err := f.host.StopNodeTurn(ctx, f.scope, runID, "a"); err != nil {
		t.Fatal(err)
	}
	run, nodes := f.await(t, runID, awaitingInput("a"))
	row := node(nodes, "a")
	if p := interactivePayload(t, row); run.Status != store.WorkflowRunAwaitingInput || errorKind(t, row) != "" || p["session_ended"] != true || p["blocker"].(map[string]any)["reason"] != blockerNoCompletedTurn {
		t.Fatalf("run %s, A %+v, summary %v; want parked by the stop, never failed", run.Status, row, p)
	}
}

// A parked interactive node keeps its session, so it keeps its slot: a sibling over the limit waits
// until the node completes and its session ends.
func TestParkedInteractiveSessionHoldsItsSlot(t *testing.T) {
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
	f.exec.noStart(t, "C started beside A's first turn")
	f.exec.finish(a, completed("在"))
	f.await(t, runID, parked("a"))
	f.exec.noStart(t, "C started beside A's parked session")
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

// A completion whose End is not confirmed keeps the node's slot, since Node may still hold the
// session: the retried completion ends the session again, and only then does the waiting
// sibling start.
func TestUnconfirmedEndKeepsTheInteractiveSlot(t *testing.T) {
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
	confirm := f.exec.EndFunc
	f.exec.EndFunc = func(ctx context.Context, scope execution.Scope, id string) error {
		if id == a {
			return execution.ErrStopUnconfirmed
		}
		return confirm(ctx, scope, id)
	}
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); err == nil {
		t.Fatal("completion with an unconfirmed End succeeded")
	}
	f.exec.noStart(t, "C started while A's session may still be live")
	f.exec.EndFunc = confirm
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); err != nil {
		t.Fatalf("retried completion: %v", err)
	}
	if !f.exec.isClosed(a) {
		t.Fatal("the retried completion freed the slot without ending A's session")
	}
	c := f.exec.awaitStart(t)
	f.exec.finish(c, completed("好"))
	if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("run %s", run.Status)
	}
}

// A stopped follow-up gives its slot back only once End returned: until then Node may still hold
// the session, and the waiting sibling stays queued.
func TestStoppedTurnKeepsTheSlotUntilEndReturns(t *testing.T) {
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
	ending, finishEnd := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(finishEnd) }) })
	confirm := f.exec.EndFunc
	f.exec.EndFunc = func(ctx context.Context, scope execution.Scope, id string) error {
		if id == a {
			close(ending)
			<-finishEnd
		}
		return confirm(ctx, scope, id)
	}
	f.exec.onCancel = func(ref execution.Ref) error {
		f.exec.finish(ref.SessionID, completed("停"))
		return nil
	}
	if _, err := f.host.StopNodeTurn(ctx, f.scope, runID, "a"); err != nil {
		t.Fatal(err)
	}
	<-ending
	f.exec.noStart(t, "C started before A's End returned")
	once.Do(func() { close(finishEnd) })
	c := f.exec.awaitStart(t)
	if f.exec.request(c).Caller.NodeID != "c" {
		t.Fatalf("started %s, want C", f.exec.request(c).Caller.NodeID)
	}
}

// A sibling waiting behind a parked interactive session only (no executing turn whose end would
// wake it) is dropped by a cancellation, and the run's state is forgotten.
func TestCancelDropsASiblingWaitingBehindAParkedSession(t *testing.T) {
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
	f.exec.noStart(t, "C started beside A's parked session")
	if _, err := f.host.CancelRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusCancelled {
		t.Fatalf("run %s", run.Status)
	}
	f.exec.noStart(t, "the waiting sibling started after the cancellation")
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
}
