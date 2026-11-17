package workflowhost

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// joinAfterSiblings fans Start out to A and B, which both lead into the Agent J.
const joinAfterSiblings = `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"A"}}},
 {"id":"b","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"B"}}},
 {"id":"j","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"{{#a.output#}} {{#b.output#}}"}}}
],"edges":[{"source":"start","target":"a"},{"source":"start","target":"b"},{"source":"a","target":"j"},{"source":"b","target":"j"}]}`

// interactiveBesideALoop runs a root interactive node A beside a Loop whose round holds the
// Agent writer and an unconnected LoopExit.
func interactiveBesideALoop() string {
	graph := strings.Replace(exitGraph(), `{"id":"loop"`, `{"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"聊","interactive":true}}},{"id":"loop"`, 1)
	return strings.Replace(graph, `{"source":"start","target":"loop"}`, `{"source":"start","target":"loop"},{"source":"start","target":"a"}`, 1)
}

// awaitIdle waits until none of the run's attempt goroutines is left, waiting for a slot or not.
func (f *fixture) awaitIdle(t *testing.T, runID, why string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := f.host.state(runID)
		pending := 0
		if st != nil {
			st.mu.Lock()
			pending = st.dispatched
			st.mu.Unlock()
		}
		if pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d attempts still pending", why, pending)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Two siblings completing at the same moment hand their answers to the join exactly once, and the
// join starts only after both of them completed.
func TestSiblingsCompletingTogetherJoinOnce(t *testing.T) {
	for range 5 {
		f := newFixture(t)
		runID := f.run(t, joinAfterSiblings, nil)
		if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
			t.Fatal(err)
		}
		started := f.exec.awaitStarts(t, 2)
		f.exec.noStart(t, "the join started before its predecessors completed")
		var wg sync.WaitGroup
		for node, answer := range map[string]string{"a": "左", "b": "右"} {
			wg.Add(1)
			go func() { defer wg.Done(); f.exec.finish(started[node], completed(answer)) }()
		}
		wg.Wait()
		j := f.exec.awaitStart(t)
		request := f.exec.request(j)
		if request.Caller.NodeID != "j" || !strings.Contains(promptText(t, request.Prompt), "左 右") {
			t.Fatalf("started %s with %q, want J with both answers", request.Caller.NodeID, promptText(t, request.Prompt))
		}
		f.exec.noStart(t, "the join started twice")
		f.exec.finish(j, completed("合"))
		run, nodes := f.await(t, runID, terminal)
		joins := 0
		for _, n := range nodes {
			if n.NodeID == "j" {
				joins++
			}
		}
		if run.Status != store.WorkflowStatusSucceeded || joins != 1 {
			t.Fatalf("run %s with %d join attempts", run.Status, joins)
		}
	}
}

// Shutting the host down drops a sibling waiting for a session slot that only a parked
// interactive node holds: Close does not wait for that slot forever.
func TestCloseDropsASiblingWaitingForASlot(t *testing.T) {
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
	f.exec.noStart(t, "C started over the session limit")
	closed := make(chan struct{})
	go func() { f.host.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited for a sibling queued behind a parked session")
	}
	f.exec.noStart(t, "C started after the host shut down")
}

// A cancellation whose stop of a parked session is not confirmed leaves the run open, but it still
// drops the sibling waiting for that session's slot at once instead of leaving it queued.
func TestUnconfirmedCancelStillDropsASiblingWaitingForASlot(t *testing.T) {
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
	f.refuseEndOf(t, func() string { return a })
	if _, err := f.host.CancelRun(ctx, f.scope, runID); err == nil {
		t.Fatal("cancellation with an unconfirmed End succeeded")
	}
	f.awaitIdle(t, runID, "the cancelled run kept a sibling waiting for a slot")
	f.exec.noStart(t, "C started after the cancellation")
}

// LoopExit drops a round member still waiting for a session slot that a root interactive node
// holds, instead of leaving it queued until that node ends.
func TestLoopExitDropsAMemberWaitingForASlot(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.host.sessionLimit = 1
	waiting, release := f.holdDispatchOf(t, "writer")
	runID := f.run(t, interactiveBesideALoop(), nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	<-waiting
	release()
	f.exec.finish(a, completed("在"))
	f.await(t, runID, parked("a"))
	f.exec.noStart(t, "the writer started over the session limit")
	rounds, err := f.s.WorkflowEngineLoopRounds(ctx, runID)
	if err != nil || len(rounds) != 1 {
		t.Fatal(rounds, err)
	}
	// A break reached in the same round while the writer waits, as a swift branch would.
	if _, err := f.s.StartWorkflowEngineNodes(ctx, runID, []store.WorkflowEngineStart{{NodeID: "exit", NodeType: "loopExit", ScopeID: rounds[0].ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.host.engine.Resume(ctx, runID); err != nil {
		t.Fatal(err)
	}
	f.awaitIdle(t, runID, "LoopExit left a member waiting for a slot")
	f.exec.noStart(t, "the writer started after LoopExit")
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); err != nil {
		t.Fatal(err)
	}
	f.exec.noStart(t, "the writer started once the slot was free")
	if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("run %s", run.Status)
	}
}

// A run that finished has no use for a slot: a session whose End was not confirmed gives it back
// without further retries, and the run's state is forgotten.
func TestAFinishedRunGivesUpALingeringEnd(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var ended atomic.Value
	ended.Store("")
	f.refuseEndOf(t, func() string { return ended.Load().(string) })
	ends := f.countEndsOf(func() string { return ended.Load().(string) })
	runID := f.run(t, twoAgents, str("输入"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	f.exec.finish(a, completed("初稿"))
	b := f.exec.awaitStart(t)
	ended.Store(b)
	f.exec.finish(b, completed("定稿"))
	if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("run %s", run.Status)
	}
	// B's End runs before its result is recorded, so a retry may still come while the run runs;
	// once the run finished, none does.
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
	settled := ends()
	time.Sleep(10 * f.host.endRetry)
	if n := ends(); n != settled {
		t.Fatalf("B's End was tried %d more times after the run finished", n-settled)
	}
}

// A failure that closes a Loop round stops only that round's turns: a root sibling executing beside
// the Loop finishes on its own, and its result is kept although the run failed.
func TestAFailedRoundLeavesRootSiblingsRunning(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	graph := strings.Replace(loopGraph, `{"id":"loop"`, `{"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"旁支"}}},{"id":"loop"`, 1)
	graph = strings.Replace(graph, `{"source":"start","target":"loop"}`, `{"source":"start","target":"loop"},{"source":"start","target":"a"}`, 1)
	runID := f.run(t, graph, nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	started := f.exec.awaitStarts(t, 2)
	refused := completed("no")
	refused.StopReason = "refusal"
	f.exec.finish(started["writer"], refused)
	f.await(t, runID, func(r store.WorkflowRun, _ []store.WorkflowNodeRun) bool {
		return r.Status == store.WorkflowStatusFailed
	})
	f.exec.noStart(t, "the failed run started another session")
	f.exec.mu.Lock()
	stopped := slices.ContainsFunc(f.exec.cancels, func(ref execution.Ref) bool { return ref.SessionID == started["a"] })
	f.exec.mu.Unlock()
	if stopped || f.exec.isClosed(started["a"]) {
		t.Fatal("the failed round stopped a root sibling")
	}
	f.exec.finish(started["a"], completed("旁支结果"))
	_, nodes := f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusSucceeded
	})
	if out := node(nodes, "a").Output; out == nil || *out != "旁支结果" {
		t.Fatalf("root sibling = %+v", node(nodes, "a"))
	}
}

// A cancellation that gave up waiting for an in-flight Start leaves the run open; when that Start
// is then refused, the attempt is the run's last turn and closes it as cancelled.
func TestARefusedStartAfterAGivenUpCancellationClosesTheRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.host.stopTimeout = 200 * time.Millisecond
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	f.exec.beforeStart = func(execution.StartRequest) error {
		close(entered)
		<-release
		return errors.New("refused")
	}
	runID := f.run(t, twoAgents, str("输入"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err := f.host.CancelRun(ctx, f.scope, runID); err == nil {
		t.Fatal("a cancellation that could not reach the turn was confirmed")
	}
	if run, _ := f.s.WorkflowRun(ctx, f.scope.TenantID, runID); run.Status != store.WorkflowStatusRunning {
		t.Fatalf("run %s before the Start returned", run.Status)
	}
	once.Do(func() { close(release) })
	if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusCancelled {
		t.Fatalf("run %s, want cancelled", run.Status)
	}
}

// A failure that closes a Loop round stops only that round: the round of another Loop beside it
// keeps executing.
func TestAFailedRoundLeavesAnotherLoopsRoundRunning(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	i, j := strings.Index(loopGraph, `{"id":"loop"`), strings.Index(loopGraph, `{"id":"out"`)
	second := strings.NewReplacer(`"id":"loop"`, `"id":"loop2"`, `"containerId":"loop"`, `"containerId":"loop2"`, `#loop.`, `#loop2.`, `"writer"`, `"writer2"`, `"entry"`, `"entry2"`).Replace(loopGraph[i:j])
	graph := loopGraph[:j] + second + loopGraph[j:]
	graph = strings.Replace(graph, `{"source":"start","target":"loop"}`, `{"source":"start","target":"loop"},{"source":"start","target":"loop2"},{"source":"entry2","target":"writer2"}`, 1)
	runID := f.run(t, graph, str("kickoff"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	started := f.exec.awaitStarts(t, 2)
	refused := completed("no")
	refused.StopReason = "refusal"
	f.exec.finish(started["writer"], refused)
	f.await(t, runID, func(r store.WorkflowRun, _ []store.WorkflowNodeRun) bool {
		return r.Status == store.WorkflowStatusFailed
	})
	f.exec.noStart(t, "the failed run started another session")
	f.exec.mu.Lock()
	stopped := slices.ContainsFunc(f.exec.cancels, func(ref execution.Ref) bool { return ref.SessionID == started["writer2"] })
	f.exec.mu.Unlock()
	if stopped || f.exec.isClosed(started["writer2"]) {
		t.Fatal("the failed round stopped the round of another Loop")
	}
	f.exec.finish(started["writer2"], completed("done"))
}

// An interactive node whose first turn could not be set up gives its slot back once its End is
// confirmed: a resume of the failed run starts it again under a limit of one session.
func TestAFirstTurnThatCouldNotBeSetUpGivesItsSlotBack(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.host.sessionLimit = 1
	runID := f.run(t, interactiveChain, str("支付重构"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	ends := f.countEndsOf(func() string { return a })
	f.exec.finish(a, execution.Result{Status: execution.StatusFailed, FailureCode: "agent_prepare_failed", AgentLost: true, NotStarted: true})
	if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusFailed {
		t.Fatalf("run %s", run.Status)
	}
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
	if ends() == 0 {
		t.Fatal("the session that could not be set up was never ended")
	}
	if _, err := f.host.ResumeRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	if again := f.exec.awaitStart(t); f.exec.request(again).Caller.NodeID != "a" {
		t.Fatalf("resumed %s, want A", f.exec.request(again).Caller.NodeID)
	}
}

// A turn that ends after a sibling failed the run cannot park: its session is ended, its row
// closed and its slot given back, so a resume starts both nodes.
func TestARefusedParkGivesTheSlotBack(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.host.sessionLimit = 2
	runID := f.run(t, parallelInteractive, nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	started := f.exec.awaitStarts(t, 2)
	a, c := started["a"], started["c"]
	f.exec.finish(a, completed("在"))
	f.await(t, runID, parked("a"))
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "继续"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	ends := f.countEndsOf(func() string { return a })
	f.exec.finish(c, execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	f.await(t, runID, func(r store.WorkflowRun, _ []store.WorkflowNodeRun) bool {
		return r.Status == store.WorkflowStatusFailed
	})
	// Let the failure's sweep pass first: it leaves A to its executing turn, so only the refused
	// park can end A's session.
	f.exec.noStart(t, "the failed run started another session")
	f.exec.finish(a, completed("晚了"))
	f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusCancelled
	})
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
	if ends() == 0 {
		t.Fatal("the refused park left A's session open")
	}
	if _, err := f.host.ResumeRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	if resumed := f.exec.awaitStarts(t, 2); resumed["a"] == "" || resumed["c"] == "" {
		t.Fatalf("resumed %v, want A and C", resumed)
	}
}
