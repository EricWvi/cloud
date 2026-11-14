package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

// interactiveChain parks A for the starter; B reads what completing A accepted.
const interactiveChain = `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"a","data":{"kind":"agent","title":"起草","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"起草：{{#start.input#}}","interactive":true}}},
 {"id":"b","data":{"kind":"agent","title":"审阅","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"审阅：{{#a.output#}}"}}},
 {"id":"out","data":{"kind":"output","outputs":[{"name":"review","variableSelector":["b","output"]}]}}
],"edges":[{"source":"start","target":"a"},{"source":"a","target":"b"},{"source":"b","target":"out"}]}`

// interactiveStructured requires A's accepted answer to be {"approved": bool}.
const interactiveStructured = `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"评审","interactive":true,
  "outputContract":{"type":"structured","schema":{"type":"object","properties":{"approved":{"type":"boolean"}},"required":["approved"]}}}}},
 {"id":"b","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"结论：{{#a.structured_output.approved#}}"}}}
],"edges":[{"source":"start","target":"a"},{"source":"a","target":"b"}]}`

func parked(id string) func(store.WorkflowRun, []store.WorkflowNodeRun) bool {
	return func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, id).Status == store.WorkflowStatusPending
	}
}

func interactivePayload(t *testing.T, n store.WorkflowNodeRun) map[string]any {
	t.Helper()
	var p struct {
		Interactive map[string]any `json:"interactive"`
	}
	if err := json.Unmarshal(n.Payload, &p); err != nil {
		t.Fatalf("payload %s: %v", n.Payload, err)
	}
	return p.Interactive
}

// startParked starts graph and finishes A's first turn with answer, returning A's session.
func (f *fixture) startParked(t *testing.T, graph, answer string) (string, string) {
	t.Helper()
	runID := f.run(t, graph, str("支付重构"))
	if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	f.exec.finish(a, completed(answer))
	f.await(t, runID, parked("a"))
	return runID, a
}

func TestInteractiveNodeParksAfterItsFirstTurn(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")

	run, err := f.s.WorkflowRun(ctx, f.scope.TenantID, runID)
	if err != nil || run.Status != "awaitingInput" {
		t.Fatalf("run = %+v %v, want awaiting input while A is parked", run, err)
	}
	nodes, _ := f.s.WorkflowEngineNodeRuns(ctx, runID)
	rowA := node(nodes, "a")
	if rowA.SessionID == nil || *rowA.SessionID != a || rowA.Output != nil || rowA.FinishedAt != nil {
		t.Fatalf("parked A = %+v, want its session bound and no output yet", rowA)
	}
	if p := interactivePayload(t, rowA); p["turns"] != float64(1) || p["completable"] != true || p["session_ended"] != false || p["answer"] != "初稿一" {
		t.Fatalf("parked summary = %v", p)
	}
	if f.exec.isClosed(a) {
		t.Fatal("parking ended the session the starter talks to")
	}
	if node(nodes, "b").ID != "" {
		t.Fatal("B started before A was completed")
	}
}

func TestFollowUpTurnsRunInTheSameSession(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")

	for i, answer := range []string{"初稿二", "初稿三"} {
		if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再改一版"); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		if id := f.exec.awaitAppend(t); id != a {
			t.Fatalf("turn %d went to session %s, want %s", i, id, a)
		}
		_, nodes := f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
			return node(n, "a").Status == store.WorkflowStatusRunning
		})
		if node(nodes, "a").SessionID == nil || *node(nodes, "a").SessionID != a {
			t.Fatal("a follow-up rebound the attempt")
		}
		// A second turn while one is in flight is refused, not queued twice.
		if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再来"); !errors.Is(err, server.ErrWorkflowNodeNotAwaitingInput) {
			t.Fatalf("turn during a turn = %v", err)
		}
		// Nor can the starter complete the node while its turn runs, even after a refused turn.
		if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); !errors.Is(err, server.ErrWorkflowNodeNotAwaitingInput) {
			t.Fatalf("completion during a turn = %v", err)
		}
		if f.exec.isClosed(a) {
			t.Fatal("a refused completion ended the session mid-turn")
		}
		f.exec.finish(a, completed(answer))
		_, nodes = f.await(t, runID, parked("a"))
		if p := interactivePayload(t, node(nodes, "a")); p["turns"] != float64(i+2) || p["answer"] != answer {
			t.Fatalf("after turn %d summary = %v", i, p)
		}
	}
	f.exec.mu.Lock()
	appended := f.exec.appended
	f.exec.mu.Unlock()
	if len(appended) != 2 || appended[0].Caller.AttemptID == appended[1].Caller.AttemptID || appended[0].Caller.ID != runID || appended[0].Caller.NodeID != "a" {
		t.Fatalf("appended = %+v, want two distinct caller attempts of A", appended)
	}
	if text := promptText(t, appended[0].Prompt); text != "再改一版" {
		t.Fatalf("follow-up prompt = %q, want what the starter typed", text)
	}
	if len(f.exec.started) != 1 {
		t.Fatalf("started %d sessions, want follow-ups in A's one session", len(f.exec.started))
	}
}

func TestCompletionWritesTheOutputOnceAndReleasesTheSuccessor(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "收紧措辞"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.exec.finish(a, completed("定稿"))
	f.await(t, runID, parked("a"))

	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); err != nil {
		t.Fatal(err)
	}
	if !f.exec.isClosed(a) {
		t.Fatal("completion left A's session open")
	}
	b := f.exec.awaitStart(t)
	if text := promptText(t, f.exec.request(b).Prompt); !strings.Contains(text, "审阅：定稿") {
		t.Fatalf("B did not read the accepted answer through the selector:\n%s", text)
	}
	// Completing again, or talking to a completed node, is refused.
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); !errors.Is(err, server.ErrWorkflowNodeNotAwaitingInput) {
		t.Fatalf("second completion = %v", err)
	}
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "还有"); !errors.Is(err, server.ErrWorkflowNodeNotAwaitingInput) {
		t.Fatalf("turn after completion = %v", err)
	}
	f.exec.finish(b, completed("可以"))
	run, nodes := f.await(t, runID, terminal)
	rowA := node(nodes, "a")
	if run.Status != store.WorkflowStatusSucceeded || rowA.Status != store.WorkflowStatusSucceeded || rowA.Output == nil || *rowA.Output != "定稿" {
		t.Fatalf("run %s, A %+v", run.Status, rowA)
	}
	var payload map[string]any
	_ = json.Unmarshal(rowA.Payload, &payload)
	if payload["stop_reason"] != "end_turn" || payload["task"] != "起草：支付重构" || len(payload["unapplied"].([]any)) != 0 {
		t.Fatalf("A payload = %v, want the ordinary completion keys and interactive applied", payload)
	}
}

func TestStructuredCompletionRefusesANonconformingAnswer(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveStructured, "我觉得可以")
	_, nodes := f.await(t, runID, parked("a"))
	if p := interactivePayload(t, node(nodes, "a")); p["completable"] != false || p["blocker"].(map[string]any)["reason"] != blockerStructuredOutput {
		t.Fatalf("summary = %v, want the structured blocker", p)
	}
	var incomplete *server.WorkflowNodeIncomplete
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); !errors.As(err, &incomplete) || incomplete.Reason != blockerStructuredOutput {
		t.Fatalf("completion = %v, want structured_output", err)
	}
	if f.exec.isClosed(a) {
		t.Fatal("a refused completion ended the session")
	}
	_, nodes = f.await(t, runID, parked("a"))
	if r := node(nodes, "a"); r.Status != store.WorkflowStatusPending || r.Output != nil {
		t.Fatalf("A = %+v, want still parked without output (never failed, so never retried)", r)
	}
	// The starter asks again; the conforming answer completes and routes.
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "按格式回答"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.exec.finish(a, completed(`{"approved": true}`))
	f.await(t, runID, parked("a"))
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); err != nil {
		t.Fatal(err)
	}
	b := f.exec.awaitStart(t)
	if text := promptText(t, f.exec.request(b).Prompt); !strings.Contains(text, "结论：true") {
		t.Fatalf("B did not read the structured output:\n%s", text)
	}
}

func TestOnlyTheStarterTalksToTheNode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, _ := f.startParked(t, interactiveChain, "初稿一")
	otherID, _ := storetest.Login(t, f.s, "interactive-reader")
	other := server.WorkflowRunScope{TenantID: f.scope.TenantID, UserID: otherID}
	for name, call := range map[string]func() error{
		"turn":     func() error { _, err := f.host.SendNodeTurn(ctx, other, runID, "a", "我来"); return err },
		"stop":     func() error { _, err := f.host.StopNodeTurn(ctx, other, runID, "a"); return err },
		"complete": func() error { _, err := f.host.CompleteNode(ctx, other, runID, "a", false, "human"); return err },
	} {
		if err := call(); !errors.Is(err, server.ErrWorkflowRunOwned) {
			t.Fatalf("%s by another member = %v, want run owned", name, err)
		}
	}
	if _, nodes := f.await(t, runID, parked("a")); interactivePayload(t, node(nodes, "a"))["turns"] != float64(1) {
		t.Fatal("a refused member changed the node")
	}
}

func TestStopEndsTheSessionAndCompletesWithTheLastNormalTurn(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	if _, err := f.host.StopNodeTurn(ctx, f.scope, runID, "a"); !errors.Is(err, server.ErrWorkflowNodeTurnNotRunning) {
		t.Fatalf("stop while parked = %v", err)
	}
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "继续"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusRunning
	})
	if _, err := f.host.StopNodeTurn(ctx, f.scope, runID, "a"); err != nil {
		t.Fatal(err)
	}
	run, nodes := f.await(t, runID, parked("a"))
	if run.Status != "awaitingInput" {
		t.Fatalf("stopping the turn moved the run to %s", run.Status)
	}
	if p := interactivePayload(t, node(nodes, "a")); p["session_ended"] != true || p["completable"] != true || p["answer"] != "初稿一" {
		t.Fatalf("summary after stop = %v", p)
	}
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再来"); !errors.Is(err, server.ErrWorkflowNodeSessionEnded) {
		t.Fatalf("turn after stop = %v", err)
	}
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); err != nil {
		t.Fatal(err)
	}
	b := f.exec.awaitStart(t)
	if text := promptText(t, f.exec.request(b).Prompt); !strings.Contains(text, "审阅：初稿一") {
		t.Fatalf("B did not read the last normal answer:\n%s", text)
	}
	f.exec.mu.Lock()
	cancels := len(f.exec.cancels)
	f.exec.mu.Unlock()
	if cancels != 1 || !f.exec.isClosed(a) {
		t.Fatalf("cancels = %d closed = %v", cancels, f.exec.isClosed(a))
	}
}

func TestStoppedFirstTurnCannotComplete(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, interactiveChain, str("支付重构"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitStart(t)
	f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool { return node(n, "a").SessionID != nil })
	if _, err := f.host.StopNodeTurn(ctx, f.scope, runID, "a"); err != nil {
		t.Fatal(err)
	}
	_, nodes := f.await(t, runID, parked("a"))
	if p := interactivePayload(t, node(nodes, "a")); p["completable"] != false || p["blocker"].(map[string]any)["reason"] != blockerNoCompletedTurn {
		t.Fatalf("summary = %v", p)
	}
	var incomplete *server.WorkflowNodeIncomplete
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); !errors.As(err, &incomplete) || incomplete.Reason != blockerNoCompletedTurn {
		t.Fatalf("completion = %v", err)
	}
	if r := node(nodes, "a"); r.Status != store.WorkflowStatusPending || errorKind(t, r) != "" {
		t.Fatalf("A = %+v, want parked, never failed", r)
	}
	// Cancelling is the way out; it closes the parked row.
	if _, err := f.host.CancelRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	if _, nodes := f.await(t, runID, terminal); node(nodes, "a").Status != store.WorkflowStatusCancelled {
		t.Fatalf("A = %+v", node(nodes, "a"))
	}
}

func TestCancelRunEndsTheParkedSession(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	run, err := f.host.CancelRun(ctx, f.scope, runID)
	if err != nil || run.Status != store.WorkflowStatusCancelled {
		t.Fatalf("cancel = %+v %v", run, err)
	}
	if !f.exec.isClosed(a) {
		t.Fatal("cancelling the run left the parked session open")
	}
	_, nodes := f.await(t, runID, terminal)
	if node(nodes, "a").Status != store.WorkflowStatusCancelled {
		t.Fatalf("A = %+v", node(nodes, "a"))
	}
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "还在吗"); !errors.Is(err, server.ErrWorkflowNodeNotAwaitingInput) {
		t.Fatalf("turn after cancel = %v", err)
	}
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
}

// race starts both calls together and returns their errors.
func race(first, second func() error) (error, error) {
	var wg sync.WaitGroup
	gate := make(chan struct{})
	var e1, e2 error
	wg.Add(2)
	go func() { defer wg.Done(); <-gate; e1 = first() }()
	go func() { defer wg.Done(); <-gate; e2 = second() }()
	close(gate)
	wg.Wait()
	return e1, e2
}

func TestCompletionAndFollowUpRaceHasOneWinner(t *testing.T) {
	for i := 0; i < 5; i++ {
		f := newFixture(t)
		ctx := context.Background()
		runID, a := f.startParked(t, interactiveChain, "初稿一")
		completeErr, turnErr := race(
			func() error { _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); return err },
			func() error { _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再改"); return err },
		)
		switch {
		case completeErr == nil && errors.Is(turnErr, server.ErrWorkflowNodeNotAwaitingInput):
			// Completion won: B starts with A's first answer and no turn was appended.
			b := f.exec.awaitStart(t)
			if text := promptText(t, f.exec.request(b).Prompt); !strings.Contains(text, "审阅：初稿一") {
				t.Fatalf("B prompt:\n%s", text)
			}
			f.exec.mu.Lock()
			appended := len(f.exec.appended)
			f.exec.mu.Unlock()
			if appended != 0 {
				t.Fatal("a refused turn reached the session")
			}
		case turnErr == nil && errors.Is(completeErr, server.ErrWorkflowNodeNotAwaitingInput):
			// The turn won: A runs it in its session and stays incomplete.
			f.exec.awaitAppend(t)
			f.exec.finish(a, completed("初稿二"))
			_, nodes := f.await(t, runID, parked("a"))
			if node(nodes, "a").Output != nil || f.exec.isClosed(a) {
				t.Fatalf("A = %+v closed=%v", node(nodes, "a"), f.exec.isClosed(a))
			}
		default:
			t.Fatalf("complete = %v, turn = %v: want exactly one winner and a stable refusal", completeErr, turnErr)
		}
	}
}

func TestCompletionAndCancelRaceEndsCancelled(t *testing.T) {
	for i := 0; i < 5; i++ {
		f := newFixture(t)
		ctx := context.Background()
		runID, a := f.startParked(t, interactiveChain, "初稿一")
		completeErr, cancelErr := race(
			func() error { _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); return err },
			func() error { _, err := f.host.CancelRun(ctx, f.scope, runID); return err },
		)
		if cancelErr != nil {
			t.Fatalf("cancel = %v", cancelErr)
		}
		run, nodes := f.await(t, runID, terminal)
		rowA := node(nodes, "a")
		switch {
		case completeErr == nil:
			// Completion committed first, then the run was cancelled with B unstarted or stopped.
			if rowA.Status != store.WorkflowStatusSucceeded || rowA.Output == nil || *rowA.Output != "初稿一" {
				t.Fatalf("completion won but A = %+v", rowA)
			}
		case errors.Is(completeErr, server.ErrWorkflowNodeNotAwaitingInput):
			if rowA.Status != store.WorkflowStatusCancelled || rowA.Output != nil {
				t.Fatalf("cancel won but A = %+v", rowA)
			}
		default:
			t.Fatalf("complete = %v", completeErr)
		}
		if run.Status != store.WorkflowStatusCancelled || !f.exec.isClosed(a) {
			t.Fatalf("run = %s, A session closed = %v", run.Status, f.exec.isClosed(a))
		}
		f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
	}
}

func TestSiblingFailureClosesTheParkedNode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	graph := `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"聊","interactive":true}}},
 {"id":"c","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"并行"}}}
],"edges":[{"source":"start","target":"a"},{"source":"start","target":"c"}]}`
	runID := f.run(t, graph, nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	// The run's turns share one slot in wave order: C starts once A parked.
	a := f.exec.awaitStart(t)
	if f.exec.request(a).Caller.NodeID != "a" {
		t.Fatalf("first turn belongs to %s", f.exec.request(a).Caller.NodeID)
	}
	f.exec.finish(a, completed("在"))
	f.await(t, runID, parked("a"))
	c := f.exec.awaitStart(t)
	f.exec.finish(c, execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	sessions := map[string]string{"a": a}
	_, nodes := f.await(t, runID, func(r store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return r.Status == store.WorkflowStatusFailed && node(n, "a").Status == store.WorkflowStatusCancelled
	})
	if node(nodes, "c").Status != store.WorkflowStatusFailed || !f.exec.isClosed(sessions["a"]) {
		t.Fatalf("c = %+v, A closed = %v", node(nodes, "c"), f.exec.isClosed(sessions["a"]))
	}
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
}

func TestBrokenFirstTurnFailsTheNodeVisibly(t *testing.T) {
	f := newFixture(t)
	runID := f.run(t, interactiveChain, str("支付重构"))
	if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	f.exec.finish(a, execution.Result{Status: execution.StatusFailed, FailureCode: "agent_prepare_failed"})
	run, nodes := f.await(t, runID, terminal)
	rowA := node(nodes, "a")
	if run.Status != store.WorkflowStatusFailed || rowA.Status != store.WorkflowStatusFailed || errorKind(t, rowA) != "session" ||
		rowA.Error == nil || !strings.Contains(*rowA.Error, "agent_prepare_failed") {
		t.Fatalf("run %s, A %+v: a turn that broke with nothing to accept must fail as an automatic Agent does", run.Status, rowA)
	}
	if !f.exec.isClosed(a) {
		t.Fatal("the broken session was left open")
	}
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
}

func TestBrokenFollowUpKeepsTheEarlierAnswer(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "继续"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.exec.finish(a, execution.Result{Status: execution.StatusInterrupted, FailureCode: "session_lost"})
	_, nodes := f.await(t, runID, parked("a"))
	if p := interactivePayload(t, node(nodes, "a")); p["session_ended"] != true || p["completable"] != true || p["answer"] != "初稿一" {
		t.Fatalf("summary = %v", p)
	}
	if !f.exec.isClosed(a) {
		t.Fatal("the broken session was left open")
	}
}

func TestUnfinishedTurnBlocksCompletionButNotTheConversation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, interactiveChain, str("支付重构"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	f.exec.finish(a, execution.Result{Status: execution.StatusCompleted, Answer: "不行", AnswerComplete: true, StopReason: "refusal"})
	_, nodes := f.await(t, runID, parked("a"))
	if p := interactivePayload(t, node(nodes, "a")); p["completable"] != false || p["blocker"].(map[string]any)["reason"] != blockerTurnNotCompleted || p["session_ended"] != false {
		t.Fatalf("summary = %v", p)
	}
	var incomplete *server.WorkflowNodeIncomplete
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", true, "human"); !errors.As(err, &incomplete) || incomplete.Reason != blockerTurnNotCompleted {
		t.Fatalf("completion (even with force) = %v", err)
	}
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "换个说法"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.exec.finish(a, execution.Result{Status: execution.StatusCompleted, Answer: "半截", StopReason: "end_turn"})
	_, nodes = f.await(t, runID, parked("a"))
	if p := interactivePayload(t, node(nodes, "a")); p["completable"] != false {
		t.Fatalf("an unreadable answer was accepted: %v", p)
	}
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再来"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.exec.finish(a, completed("好的"))
	_, nodes = f.await(t, runID, parked("a"))
	if p := interactivePayload(t, node(nodes, "a")); p["completable"] != true || p["turns"] != float64(3) {
		t.Fatalf("summary = %v", p)
	}
}

func TestInteractiveLoopMemberRunsAutomatically(t *testing.T) {
	f := newFixture(t)
	graph := strings.Replace(loopGraph, `"prompt":"Improve {{#loop.draft#}}"}`, `"prompt":"Improve {{#loop.draft#}}","interactive":true}`, 1)
	if graph == loopGraph {
		t.Fatal("fixture graph changed")
	}
	id := f.run(t, graph, str("kickoff"))
	if _, err := f.host.StartRun(context.Background(), f.scope, id); err != nil {
		t.Fatal(err)
	}
	first := f.exec.awaitStart(t)
	f.exec.finish(first, completed("done"))
	run, _ := f.await(t, id, terminal)
	if run.Status != store.WorkflowStatusSucceeded || !f.exec.isClosed(first) {
		t.Fatalf("run %s, session closed %v: a Loop member must not park", run.Status, f.exec.isClosed(first))
	}
	nodes, err := f.s.WorkflowRunNodes(context.Background(), f.scope.TenantID, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.NodeID != "writer" {
			continue
		}
		var p struct {
			Unapplied []string `json:"unapplied"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if len(p.Unapplied) != 1 || p.Unapplied[0] != "interactive" {
			t.Fatalf("writer unapplied = %v, want interactive reported", p.Unapplied)
		}
		return
	}
	t.Fatal("no writer attempt")
}

func TestClaimsAreRefusedOnceCancellationStarted(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	ending, release := make(chan struct{}), make(chan struct{})
	end := f.exec.EndFunc
	f.exec.EndFunc = func(ctx context.Context, scope execution.Scope, id string) error {
		if id == a {
			close(ending)
			<-release
		}
		return end(ctx, scope, id)
	}
	cancelled := make(chan error, 1)
	go func() { _, err := f.host.CancelRun(ctx, f.scope, runID); cancelled <- err }()
	<-ending // the cancellation is ending the parked session; the run still reads running
	_, turnErr := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "抢在取消前")
	close(release)
	if err := <-cancelled; err != nil {
		t.Fatal(err)
	}
	if !errors.Is(turnErr, server.ErrWorkflowNodeNotAwaitingInput) {
		t.Fatalf("turn during the cancellation = %v", turnErr)
	}
	f.exec.mu.Lock()
	appended := len(f.exec.appended)
	f.exec.mu.Unlock()
	if appended != 0 {
		t.Fatal("a turn reached the session of a cancelling run")
	}
}
