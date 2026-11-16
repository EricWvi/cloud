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

// awaitingInput also waits for the run projection. The fixture reads the run before its
// nodes, so a newly parked node can accompany a preceding running run view. Keep parked
// separate for tests where another branch is still executing beside an interactive node.
func awaitingInput(id string) func(store.WorkflowRun, []store.WorkflowNodeRun) bool {
	return func(run store.WorkflowRun, rows []store.WorkflowNodeRun) bool {
		return run.Status == store.WorkflowRunAwaitingInput && parked(id)(run, rows)
	}
}

func interactivePayload(t *testing.T, n store.WorkflowNodeRun) map[string]any {
	t.Helper()
	var p struct {
		Interactive map[string]any `json:"interactive"`
	}
	if len(n.Payload) == 0 {
		return nil // a row the engine has not written a payload to yet
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
	run, nodes := f.await(t, runID, awaitingInput("a"))
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
	// The run page reads the node as an interactive session from its first turn on, so the
	// starter can stop that turn too.
	_, nodes := f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusRunning && interactivePayload(t, node(n, "a")) != nil
	})
	if p := interactivePayload(t, node(nodes, "a")); p["turns"] != float64(0) || p["session_ended"] != false || p["completable"] != false {
		t.Fatalf("first-turn summary = %v", p)
	}
	if _, err := f.host.StopNodeTurn(ctx, f.scope, runID, "a"); err != nil {
		t.Fatal(err)
	}
	_, nodes = f.await(t, runID, parked("a"))
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
 {"id":"c","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"并行","retry":{"enabled":false,"maxRetries":0,"initialDelaySeconds":1}}}}
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

// The results the Agent driver emits for a turn the Agent itself ended without completing
// (agentruntime turnResult): the held session stays open, so none of them ends or fails the node.
var agentEndedTurns = map[string]execution.Result{
	"refusal":           {Status: execution.StatusFailed, Answer: "不行", StopReason: "refusal", FailureCode: "agent_turn_incomplete"},
	"max_tokens":        {Status: execution.StatusFailed, Answer: "半截", StopReason: "max_tokens", FailureCode: "agent_turn_incomplete"},
	"max_turn_requests": {Status: execution.StatusFailed, Answer: "半截", StopReason: "max_turn_requests", FailureCode: "agent_turn_incomplete"},
	"unknown stop":      {Status: execution.StatusFailed, Answer: "?", FailureCode: "agent_stop_unknown"},
	"unasked cancel":    {Status: execution.StatusCancelled, StopReason: "cancelled"},
	"cut at a limit":    {Status: execution.StatusFailed, StopReason: "cancelled", FailureCode: "model_max_duration"},
}

// Desktop keeps a stopped, truncated, refused or unknown turn awaiting input
// (workflow-runs.md:125-126): a first turn that did not end normally parks with its session open,
// completion is refused with turn_not_completed, and the starter keeps talking until a turn ends
// normally.
func TestAgentEndedFirstTurnParksAndTheConversationContinues(t *testing.T) {
	for name, result := range agentEndedTurns {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			runID := f.run(t, interactiveChain, str("支付重构"))
			if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
				t.Fatal(err)
			}
			a := f.exec.awaitStart(t)
			f.exec.finish(a, result)
			run, nodes := f.await(t, runID, awaitingInput("a"))
			p := interactivePayload(t, node(nodes, "a"))
			if run.Status != "awaitingInput" || p["completable"] != false || p["session_ended"] != false || p["blocker"].(map[string]any)["reason"] != blockerTurnNotCompleted || errorKind(t, node(nodes, "a")) != "" {
				t.Fatalf("run %s, summary %v: want parked, open and not completable", run.Status, p)
			}
			if f.exec.isClosed(a) {
				t.Fatal("a turn the Agent ended closed the held session")
			}
			var incomplete *server.WorkflowNodeIncomplete
			if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", true, "human"); !errors.As(err, &incomplete) || incomplete.Reason != blockerTurnNotCompleted {
				t.Fatalf("completion (even with force) = %v", err)
			}
			if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "换个说法"); err != nil {
				t.Fatal(err)
			}
			if id := f.exec.awaitAppend(t); id != a {
				t.Fatalf("the next turn went to %s, want the held session %s", id, a)
			}
			f.exec.finish(a, completed("好的"))
			_, nodes = f.await(t, runID, parked("a"))
			if p := interactivePayload(t, node(nodes, "a")); p["completable"] != true || p["turns"] != float64(2) || p["answer"] != "好的" {
				t.Fatalf("summary = %v", p)
			}
		})
	}
}

// A follow-up the Agent ends without completing blocks completion on the earlier answer too: the
// last turn decides, as long as the session is open (turn_not_completed), and the next normal
// turn lifts it.
func TestUnfinishedTurnBlocksCompletionButNotTheConversation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	for _, name := range []string{"refusal", "unknown stop", "unasked cancel"} {
		if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再改"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		f.exec.awaitAppend(t)
		f.exec.finish(a, agentEndedTurns[name])
		_, nodes := f.await(t, runID, parked("a"))
		if p := interactivePayload(t, node(nodes, "a")); p["completable"] != false || p["session_ended"] != false || p["answer"] != "初稿一" {
			t.Fatalf("after %s summary = %v", name, p)
		}
		var incomplete *server.WorkflowNodeIncomplete
		if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); !errors.As(err, &incomplete) || incomplete.Reason != blockerTurnNotCompleted {
			t.Fatalf("after %s completion = %v", name, err)
		}
	}
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再来"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.exec.finish(a, completed("好的"))
	_, nodes := f.await(t, runID, parked("a"))
	if p := interactivePayload(t, node(nodes, "a")); p["completable"] != true || p["turns"] != float64(5) {
		t.Fatalf("summary = %v", p)
	}
}

// A first turn that lost its Agent (a crash or a dropped stream: the driver reports AgentLost)
// parks too, never failing the node; with no normal answer it cannot complete, and cancelling the
// run is the way out, as after a stop (D7).
func TestLostFirstTurnParksWithoutAnAnswer(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, interactiveChain, str("支付重构"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	f.exec.finish(a, execution.Result{Status: execution.StatusFailed, FailureCode: "agent_transport_closed", AgentLost: true})
	run, nodes := f.await(t, runID, awaitingInput("a"))
	p := interactivePayload(t, node(nodes, "a"))
	if run.Status != "awaitingInput" || p["session_ended"] != true || p["completable"] != false || p["blocker"].(map[string]any)["reason"] != blockerNoCompletedTurn || errorKind(t, node(nodes, "a")) != "" {
		t.Fatalf("run %s, summary %v: want parked with the session ended", run.Status, p)
	}
	var incomplete *server.WorkflowNodeIncomplete
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); !errors.As(err, &incomplete) || incomplete.Reason != blockerNoCompletedTurn {
		t.Fatalf("completion = %v", err)
	}
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "还在吗"); !errors.Is(err, server.ErrWorkflowNodeSessionEnded) {
		t.Fatalf("turn after a lost session = %v", err)
	}
	if _, err := f.host.CancelRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	if _, nodes := f.await(t, runID, terminal); node(nodes, "a").Status != store.WorkflowStatusCancelled {
		t.Fatalf("A = %+v", node(nodes, "a"))
	}
}

func TestLostFollowUpKeepsTheEarlierAnswer(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "继续"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.exec.finish(a, execution.Result{Status: execution.StatusInterrupted, FailureCode: "session_lost", AgentLost: true})
	_, nodes := f.await(t, runID, parked("a"))
	if p := interactivePayload(t, node(nodes, "a")); p["session_ended"] != true || p["completable"] != true || p["answer"] != "初稿一" {
		t.Fatalf("summary = %v", p)
	}
	if !f.exec.isClosed(a) {
		t.Fatal("the lost session was left open")
	}
}

// A turn whose result cannot be read (the stream broke and no attempt was recorded) counts as a
// lost Agent: the host ends the session itself and the node parks for completion.
func TestUnreadableTurnEndsTheSessionAndParks(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	f.exec.SubscribeFunc = func(context.Context, execution.Scope, string, uint64) (execution.Stream, error) {
		return nil, execution.ErrUnavailable
	}
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "继续"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	_, nodes := f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusPending && interactivePayload(t, node(n, "a"))["session_ended"] == true
	})
	if p := interactivePayload(t, node(nodes, "a")); p["completable"] != true || p["answer"] != "初稿一" || !f.exec.isClosed(a) {
		t.Fatalf("summary = %v, closed = %v", p, f.exec.isClosed(a))
	}
}

// H1: a sibling branch that starts its Agent while an interactive node is parked leaves the
// node's session open: the run's sessions stay live side by side in its directory. A follow-up
// sent while the sibling's turn runs waits for the run's turn slot and then reaches the same
// session, and the node completes with that answer.
func TestSiblingStartLeavesTheParkedSessionOpen(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	graph := `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"draft","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"起草","interactive":true}}},
 {"id":"side","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"旁支"}}}
],"edges":[{"source":"start","target":"draft"},{"source":"start","target":"side"}]}`
	runID := f.run(t, graph, nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	draft := f.exec.awaitStart(t)
	if f.exec.request(draft).Lifetime != execution.LifetimeHeld {
		t.Fatal("the interactive node did not hold its session")
	}
	f.exec.finish(draft, completed("初稿"))
	side := f.exec.awaitStart(t)
	if f.exec.request(side).Lifetime != execution.LifetimeTurn {
		t.Fatal("an automatic node held its session")
	}
	_, nodes := f.await(t, runID, parked("draft"))
	if p := interactivePayload(t, node(nodes, "draft")); f.exec.isClosed(draft) || p["session_ended"] != false || p["completable"] != true || p["turns"] != float64(1) {
		t.Fatalf("the sibling's start touched the parked session: closed = %v, summary = %v", f.exec.isClosed(draft), p)
	}
	// The sibling's turn holds the run's slot, so the follow-up is accepted and waits for it.
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "draft", "继续"); err != nil {
		t.Fatalf("turn while the sibling runs = %v", err)
	}
	select {
	case id := <-f.exec.appendedCh:
		t.Fatalf("the follow-up reached %s while the sibling's turn held the slot", id)
	case <-time.After(100 * time.Millisecond):
	}
	f.exec.finish(side, completed("旁支好了"))
	if id := f.exec.awaitAppend(t); id != draft {
		t.Fatalf("the follow-up went to %s, want the node's own session %s", id, draft)
	}
	if !f.exec.isClosed(side) || f.exec.isClosed(draft) {
		t.Fatalf("after the sibling's turn: side closed = %v, draft closed = %v", f.exec.isClosed(side), f.exec.isClosed(draft))
	}
	f.exec.finish(draft, completed("二稿"))
	_, nodes = f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "draft").Status == store.WorkflowStatusPending && node(n, "side").Status == store.WorkflowStatusSucceeded
	})
	if p := interactivePayload(t, node(nodes, "draft")); p["turns"] != float64(2) || p["answer"] != "二稿" || p["session_ended"] != false {
		t.Fatalf("summary after the follow-up = %v", p)
	}
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "draft", false, "human"); err != nil {
		t.Fatal(err)
	}
	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusSucceeded || *node(nodes, "draft").Output != "二稿" {
		t.Fatalf("run %s, draft %+v", run.Status, node(nodes, "draft"))
	}
	if starts(f) != 2 {
		t.Fatalf("starts = %d, want one session per node", starts(f))
	}
}

// M1: a sibling that fails the run while a completion is ending the node's session must not leave
// the node pending under a failed run (undeletable, its run state kept forever). The completion
// still records its accepted answer, as any root attempt that settles after a sibling failed the
// run does (CompleteWorkflowEngineNode), so a resume from failure reuses it; no successor starts.
func TestSiblingFailureDuringCompletionClosesTheNode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	graph := `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"聊","interactive":true}}},
 {"id":"c","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"并行","retry":{"enabled":false,"maxRetries":0,"initialDelaySeconds":1}}}}
],"edges":[{"source":"start","target":"a"},{"source":"start","target":"c"}]}`
	// C fails before it starts a session (its executor list is unavailable), so A's session stays
	// open; C waits in that list until A's completion is inside End.
	list := f.exec.ListExecutorsFunc
	calls, cListing, failC := 0, make(chan struct{}), make(chan struct{})
	var listMu sync.Mutex
	f.exec.ListExecutorsFunc = func(ctx context.Context, scope execution.Scope) ([]execution.Executor, error) {
		listMu.Lock()
		calls++
		second := calls == 2
		listMu.Unlock()
		if second {
			close(cListing)
			<-failC
			return nil, execution.ErrUnavailable
		}
		return list(ctx, scope)
	}
	runID := f.run(t, graph, nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	f.exec.finish(a, completed("在"))
	f.await(t, runID, parked("a"))
	<-cListing
	ending, releaseEnd := make(chan struct{}), make(chan struct{})
	end := f.exec.EndFunc
	f.exec.EndFunc = func(ctx context.Context, scope execution.Scope, id string) error {
		if id == a {
			select {
			case <-ending:
			default:
				close(ending)
				<-releaseEnd
			}
		}
		return end(ctx, scope, id)
	}
	completed := make(chan error, 1)
	go func() { _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); completed <- err }()
	<-ending
	close(failC)
	f.await(t, runID, func(r store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return r.Status == store.WorkflowStatusFailed && node(n, "c").Status == store.WorkflowStatusFailed
	})
	close(releaseEnd)
	if err := <-completed; err != nil {
		t.Fatalf("completion under a failed run = %v", err)
	}
	run, nodes := f.await(t, runID, terminal)
	if rowA := node(nodes, "a"); run.Status != store.WorkflowStatusFailed || rowA.Status != store.WorkflowStatusSucceeded || rowA.Output == nil || *rowA.Output != "在" || !f.exec.isClosed(a) {
		t.Fatalf("run %s, A = %+v, closed = %v; want the answer recorded under the failed run", run.Status, rowA, f.exec.isClosed(a))
	}
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
	if err := f.s.DeleteWorkflowRun(ctx, f.scope.TenantID, runID); err != nil {
		t.Fatalf("delete the failed run: %v", err)
	}
}

// M2: whatever still fails an interactive node (its session could not be set up) is never
// auto-retryable, since a person drives the node and desktop never retries it.
func TestInteractiveSetupFailuresAreNotAutoRetryable(t *testing.T) {
	for name, inject := range map[string]func(*fixture){
		"executor list unavailable": func(f *fixture) {
			f.exec.ListExecutorsFunc = func(context.Context, execution.Scope) ([]execution.Executor, error) {
				return nil, execution.ErrUnavailable
			}
		},
		"start refused": func(f *fixture) {
			f.exec.beforeStart = func(execution.StartRequest) error { return execution.ErrUnavailable }
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			inject(f)
			runID := f.run(t, interactiveChain, str("支付重构"))
			if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
				t.Fatal(err)
			}
			_, nodes := f.await(t, runID, terminal)
			var p struct {
				ErrorDetail struct {
					Kind          string `json:"kind"`
					AutoRetryable bool   `json:"auto_retryable"`
				} `json:"error_detail"`
			}
			if err := json.Unmarshal(node(nodes, "a").Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.ErrorDetail.Kind != "interactive_session" || p.ErrorDetail.AutoRetryable {
				t.Fatalf("error detail = %+v, want interactive_session and not auto-retryable", p.ErrorDetail)
			}
		})
	}
}

// L3: a follow-up accepted just before the run's cancellation would never run, so it is refused
// with the stable not-awaiting error instead of 200. Holding the hub pauses SendNodeTurn between
// the row move and the enqueue.
func TestTurnRacingACancellationIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	st := f.host.state(runID)
	hub := f.host.Hub()
	hub.mu.Lock()
	turned := make(chan error, 1)
	go func() { _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "抢在取消前"); turned <- err }()
	for {
		nodes, err := f.s.WorkflowEngineNodeRuns(ctx, runID)
		if err != nil {
			hub.mu.Unlock()
			t.Fatal(err)
		}
		if node(nodes, "a").Status == store.WorkflowStatusRunning {
			break // moved; SendNodeTurn now waits on the hub before it queues the turn
		}
	}
	if _, err := st.requestCancel(ctx); err != nil { // CancelRun's first step
		hub.mu.Unlock()
		t.Fatal(err)
	}
	hub.mu.Unlock()
	if err := <-turned; !errors.Is(err, server.ErrWorkflowNodeNotAwaitingInput) {
		t.Fatalf("turn racing the cancellation = %v", err)
	}
	if _, err := f.host.CancelRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	if _, nodes := f.await(t, runID, terminal); node(nodes, "a").Status != store.WorkflowStatusCancelled || !f.exec.isClosed(a) {
		t.Fatalf("A = %+v closed = %v", node(nodes, "a"), f.exec.isClosed(a))
	}
	f.exec.mu.Lock()
	appended := len(f.exec.appended)
	f.exec.mu.Unlock()
	if appended != 0 {
		t.Fatal("the refused turn reached the session")
	}
}

func TestInteractiveLoopMemberRunsAutomatically(t *testing.T) {
	f := newFixture(t)
	graph := strings.Replace(loopGraph, `"prompt":"Improve {{#loop.draft#}}"`, `"prompt":"Improve {{#loop.draft#}}","interactive":true`, 1)
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
			Unapplied   []string        `json:"unapplied"`
			Interactive json.RawMessage `json:"interactive"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if len(p.Unapplied) != 1 || p.Unapplied[0] != "interactive" {
			t.Fatalf("writer unapplied = %v, want interactive reported", p.Unapplied)
		}
		if p.Interactive != nil {
			t.Fatalf("writer carries an interactive summary %s; the run page would offer it a conversation", p.Interactive)
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
