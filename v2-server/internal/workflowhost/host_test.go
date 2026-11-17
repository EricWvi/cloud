package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// twoAgents is the demo chain: B reads A's answer through the variable pool.
const twoAgents = `{"nodes":[
 {"id":"start","data":{"kind":"start","title":"开始"}},
 {"id":"a","data":{"kind":"agent","title":"起草","description":"写一段初稿","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"Other Model"},"prompt":"起草：{{#start.input#}}","roleId":"reviewer"}}},
 {"id":"b","data":{"kind":"agent","title":"审阅","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"审阅这段初稿：{{#a.output#}}"}}},
 {"id":"out","data":{"kind":"output","outputs":[{"name":"review","variableSelector":["b","output"]}]}}
],"edges":[{"source":"start","target":"a"},{"source":"a","target":"b"},{"source":"b","target":"out"}]}`

func str(s string) *string { return &s }

func promptText(t *testing.T, p execution.Prompt) string {
	t.Helper()
	text, err := p.Text()
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func TestTwoAgentChainHandsTheAnswerDownstream(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, twoAgents, str("支付重构"))

	run, err := f.host.StartRun(ctx, f.scope, runID)
	if err != nil || run.Status != store.WorkflowStatusRunning {
		t.Fatalf("start = %+v %v", run, err)
	}
	a := f.exec.awaitStart(t)
	reqA := f.exec.request(a)
	if c := reqA.Caller; c.Kind != execution.CallerWorkflow || c.ID != runID || c.NodeID != "a" || c.AttemptID == "" || len(c.Rounds) != 0 {
		t.Fatalf("caller = %+v", reqA.Caller)
	}
	if reqA.ExecutorID != "opencode:other" || reqA.Target.ProjectID != f.project {
		t.Fatalf("A request = %+v", reqA)
	}
	textA := promptText(t, reqA.Prompt)
	for _, want := range []string{
		"<workspace_root>\n你的工作区根目录是：\n.\n", "当前步骤: \"起草\" (`a`)", "步骤说明:\n写一段初稿", "任务要求:\n起草：支付重构",
		"直接前置节点: \"开始\" (`start`)", "- [已成功] \"开始\" (`start`) -> \"起草\" (`a`)", "- [运行中，当前节点] \"起草\" (`a`)",
		"- [未开始] \"审阅\" (`b`)", "## 工作流原始请求\n", "</current_workflow_step>\n\n<workflow_context>",
	} {
		if !strings.Contains(textA, want) {
			t.Fatalf("A prompt lacks %q:\n%s", want, textA)
		}
	}
	if strings.Contains(textA, "reviewer") || strings.Contains(textA, "system_instructions") {
		t.Fatalf("A prompt applied the role:\n%s", textA)
	}
	_, nodes := f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").SessionID != nil
	})
	if *node(nodes, "a").SessionID != a {
		t.Fatalf("A bound to %v", *node(nodes, "a").SessionID)
	}

	f.exec.finish(a, completed("初稿：先拆网关"))
	b := f.exec.awaitStart(t)
	reqB := f.exec.request(b)
	if reqB.ExecutorID != "opencode:fixture" {
		t.Fatalf("B executor = %s, want the requested model", reqB.ExecutorID)
	}
	if textB := promptText(t, reqB.Prompt); !strings.Contains(textB, "任务要求:\n审阅这段初稿：初稿：先拆网关") {
		t.Fatalf("B did not read A's answer:\n%s", textB)
	}
	f.exec.finish(b, completed("可以合并"))

	run, nodes = f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusSucceeded || run.Output == nil || *run.Output != `{"review":"可以合并"}` {
		t.Fatalf("run = %+v", run)
	}
	var payloadA, payloadB map[string]any
	_ = json.Unmarshal(node(nodes, "a").Payload, &payloadA)
	_ = json.Unmarshal(node(nodes, "b").Payload, &payloadB)
	if payloadA["stop_reason"] != "end_turn" || payloadA["executor"].(map[string]any)["id"] != "opencode:other" ||
		payloadA["model"].(map[string]any)["fallback"] != false || !reflect.DeepEqual(payloadA["unapplied"], []any{"role"}) {
		t.Fatalf("A payload = %v", payloadA)
	}
	if payloadB["task"] != "审阅这段初稿：初稿：先拆网关" {
		t.Fatalf("B task = %v, want the rendered template", payloadB["task"])
	}
	if m := payloadB["model"].(map[string]any); m["fallback"] != false || m["requested"] != "fixture" || m["id"] != "fixture" {
		t.Fatalf("B model = %v", m)
	}
	if len(f.exec.ended) != 2 {
		t.Fatalf("sessions ended = %v", f.exec.ended)
	}
	// A late duplicate terminal event changes nothing.
	f.host.complete(workflow.AgentDispatch{RunID: runID, NodeRunID: node(nodes, "b").ID}, workflow.Completion{Output: str("迟到")})
	if again, _ := f.s.WorkflowRun(ctx, f.scope.TenantID, runID); *again.Output != *run.Output {
		t.Fatalf("duplicate completion rewrote the run: %+v", again)
	}
}

func errorKind(t *testing.T, n store.WorkflowNodeRun) string {
	t.Helper()
	var p struct {
		ErrorDetail struct {
			Kind string `json:"kind"`
		} `json:"error_detail"`
	}
	if err := json.Unmarshal(n.Payload, &p); err != nil {
		t.Fatalf("payload %s: %v", n.Payload, err)
	}
	return p.ErrorDetail.Kind
}

func TestAgentOutcomesFailTheRun(t *testing.T) {
	cases := map[string]struct {
		result execution.Result
		kind   string
	}{
		"turn failed":       {execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"}, "session"},
		"answer incomplete": {execution.Result{Status: execution.StatusCompleted, Answer: "半截", StopReason: "end_turn"}, "session"},
		"refusal":           {execution.Result{Status: execution.StatusCompleted, Answer: "不行", AnswerComplete: true, StopReason: "refusal"}, "agent_refusal"},
		"no stop reason":    {execution.Result{Status: execution.StatusCompleted, AnswerComplete: true}, "session_ended_without_stop_reason"},
		"unknown reason":    {execution.Result{Status: execution.StatusCompleted, AnswerComplete: true, StopReason: "paused"}, "unknown_stop_reason"},
	}
	f := newFixture(t)
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			runID := f.run(t, strings.Replace(twoAgents, `"roleId":"reviewer"`, `"roleId":"reviewer","retry":{"enabled":false,"maxRetries":2,"initialDelaySeconds":10}`, 1), str("输入"))
			if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
				t.Fatal(err)
			}
			f.exec.finish(f.exec.awaitStart(t), c.result)
			run, nodes := f.await(t, runID, terminal)
			a := node(nodes, "a")
			if run.Status != store.WorkflowStatusFailed || a.Status != store.WorkflowStatusFailed || errorKind(t, a) != c.kind {
				t.Fatalf("run %s, node %+v kind %s", run.Status, a, errorKind(t, a))
			}
			if node(nodes, "b").ID != "" {
				t.Fatal("the run advanced past a failed node")
			}
			if c.result.Answer != "" && (a.Output == nil || *a.Output != c.result.Answer) {
				t.Fatalf("failed attempt lost its raw answer: %+v", a)
			}
		})
	}
}

func TestNodesFailBeforeStartingAnAgent(t *testing.T) {
	cases := map[string]struct{ agent, kind string }{
		"unknown agent":       {`{"executor":{"agentCli":"official/ora-space.codex","modelId":"x"},"prompt":"hi"}`, "missing_agent_ref"},
		"no agent":            {`{"prompt":"hi"}`, "missing_agent_ref"},
		"unassigned template": {`{"executor":{"agentCli":"official/ora-space.claude","modelId":"messages:fixture"},"prompt":"{{#start.topic#}}"}`, "prompt_template"},
	}
	f := newFixture(t)
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			graph := `{"nodes":[{"id":"start","data":{"kind":"start","inputVariables":[{"name":"topic","valueType":"string"}]}},{"id":"a","data":{"kind":"agent","agentConfig":` + c.agent + `}}],"edges":[{"source":"start","target":"a"}]}`
			runID := f.run(t, graph, nil)
			if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
				t.Fatal(err)
			}
			run, nodes := f.await(t, runID, terminal)
			if run.Status != store.WorkflowStatusFailed || errorKind(t, node(nodes, "a")) != c.kind {
				t.Fatalf("run %s, node %+v", run.Status, node(nodes, "a"))
			}
		})
	}
	if len(f.exec.started) != 0 {
		t.Fatalf("an Agent started: %+v", f.exec.started)
	}
}

func TestCancelStopsTheActiveTurnFirst(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, twoAgents, str("输入"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	run, err := f.host.CancelRun(ctx, f.scope, runID)
	if err != nil || run.Status != store.WorkflowStatusCancelled {
		t.Fatalf("cancel = %+v %v", run, err)
	}
	if len(f.exec.cancels) != 1 || f.exec.cancels[0].SessionID != a {
		t.Fatalf("cancelled refs = %+v", f.exec.cancels)
	}
	_, nodes := f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusCancelled
	})
	if node(nodes, "b").ID != "" {
		t.Fatal("a cancelled run dispatched its successor")
	}
	// Cancelling again is idempotent and asks execution for nothing new.
	if again, err := f.host.CancelRun(ctx, f.scope, runID); err != nil || again.Status != store.WorkflowStatusCancelled || len(f.exec.cancels) != 1 {
		t.Fatalf("repeat cancel = %+v %v (%d cancels)", again, err, len(f.exec.cancels))
	}
}

func TestUnconfirmedStopKeepsTheRunOpenUntilTheTurnEnds(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.exec.onCancel = func(execution.Ref) error { return execution.ErrStopUnconfirmed }
	runID := f.run(t, twoAgents, str("输入"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	if _, err := f.host.CancelRun(ctx, f.scope, runID); !errors.Is(err, server.ErrWorkflowRunStopUnconfirmed) {
		t.Fatalf("cancel = %v", err)
	}
	if run, _ := f.s.WorkflowRun(ctx, f.scope.TenantID, runID); run.Status != store.WorkflowStatusRunning {
		t.Fatalf("an unconfirmed stop closed the run: %s", run.Status)
	}
	// The turn later succeeds anyway: the cancellation request still wins and nothing advances.
	f.exec.finish(a, completed("赶在取消前完成"))
	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusCancelled || node(nodes, "a").Status != store.WorkflowStatusCancelled || node(nodes, "b").ID != "" {
		t.Fatalf("run %s nodes %+v", run.Status, nodes)
	}
}

func TestCancelWaitsForAnInFlightStart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	f.exec.beforeStart = func(execution.StartRequest) error {
		close(entered)
		<-release
		return nil
	}
	runID := f.run(t, twoAgents, str("输入"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	<-entered
	var wg sync.WaitGroup
	var cancelled store.WorkflowRun
	var cancelErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		cancelled, cancelErr = f.host.CancelRun(ctx, f.scope, runID)
	}()
	// The cancellation cannot finish while Start has not returned the turn to stop.
	for !f.host.state(runID).isCancelling() {
		runtime.Gosched()
	}
	close(release)
	wg.Wait()
	if cancelErr != nil || cancelled.Status != store.WorkflowStatusCancelled {
		t.Fatalf("cancel = %+v %v", cancelled, cancelErr)
	}
	a := f.exec.awaitStart(t)
	if len(f.exec.cancels) != 1 || f.exec.cancels[0].SessionID != a {
		t.Fatalf("cancel did not stop the turn Start returned: %+v", f.exec.cancels)
	}
}

func (st *runState) isCancelling() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.cancelling
}

func TestRefusedBindingEndsTheSession(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, twoAgents, str("输入"))
	// Something outside the host closes the run while the Agent starts, so the attempt is no
	// longer running when its session arrives.
	f.exec.beforeStart = func(execution.StartRequest) error {
		_, err := f.s.CancelWorkflowRun(ctx, f.scope.TenantID, runID)
		return err
	}
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool {
		f.exec.mu.Lock()
		defer f.exec.mu.Unlock()
		return len(f.exec.ended) == 1
	})
	if f.exec.ended[0] != a {
		t.Fatalf("ended %v, want %s", f.exec.ended, a)
	}
	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusCancelled || node(nodes, "a").SessionID != nil {
		t.Fatalf("run %s node %+v", run.Status, node(nodes, "a"))
	}
}

// Sibling Agents of one run execute concurrently in its shared directory, as desktop's: both
// start before either finishes, each in its own session, and each session ends on its own.
func TestSiblingAgentsExecuteConcurrently(t *testing.T) {
	f := newFixture(t)
	runID := f.run(t, parallelAgents, nil)
	if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	sessions := f.exec.awaitStarts(t, 2)
	if sessions["a"] == "" || sessions["b"] == "" || sessions["a"] == sessions["b"] {
		t.Fatalf("siblings did not start concurrently in their own sessions: %v", sessions)
	}
	if f.exec.request(sessions["a"]).Target != f.exec.request(sessions["b"]).Target {
		t.Fatal("siblings target different projects")
	}
	f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").SessionID != nil && node(n, "b").SessionID != nil
	})
	f.exec.finish(sessions["b"], completed("B"))
	f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "b").Status == store.WorkflowStatusSucceeded
	})
	f.exec.mu.Lock()
	ended := append([]string(nil), f.exec.ended...)
	f.exec.mu.Unlock()
	if len(ended) != 1 || ended[0] != sessions["b"] {
		t.Fatalf("finishing B ended %v, want only B's session", ended)
	}
	f.exec.finish(sessions["a"], completed("A"))
	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusSucceeded || *node(nodes, "a").SessionID != sessions["a"] || *node(nodes, "b").SessionID != sessions["b"] {
		t.Fatalf("run %s nodes %+v", run.Status, nodes)
	}
}

// Cancelling a run stops every executing sibling turn and closes the run once all are confirmed.
// A sibling whose stop was confirmed does not close the run while another's is not: the run closes
// once that last turn ends.
func TestCancelStopsEverySiblingTurn(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, parallelAgents, nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	sessions := f.exec.awaitStarts(t, 2)
	f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").SessionID != nil && node(n, "b").SessionID != nil
	})
	f.exec.onCancel = func(ref execution.Ref) error {
		if ref.SessionID == sessions["a"] {
			return execution.ErrStopUnconfirmed
		}
		return nil
	}
	if _, err := f.host.CancelRun(ctx, f.scope, runID); !errors.Is(err, server.ErrWorkflowRunStopUnconfirmed) {
		t.Fatalf("cancel with one unconfirmed stop = %v", err)
	}
	f.exec.mu.Lock()
	stopped := map[string]bool{}
	for _, ref := range f.exec.cancels {
		stopped[ref.SessionID] = true
	}
	f.exec.mu.Unlock()
	if !stopped[sessions["a"]] || !stopped[sessions["b"]] {
		t.Fatalf("cancel stopped %v, want both siblings", stopped)
	}
	// B's stop was confirmed and its attempt has returned; A still executes, so the run stays open.
	st := f.host.state(runID)
	waitUntil(t, func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return len(st.attempts) == 1 && st.dispatched == 1
	})
	if run, _ := f.s.WorkflowRun(ctx, f.scope.TenantID, runID); run.Status != store.WorkflowStatusRunning {
		t.Fatalf("a confirmed sibling closed the run while another stop is unconfirmed: %s", run.Status)
	}
	// A's turn ends after all: the last turn gone closes the run as cancelled.
	f.exec.finish(sessions["a"], execution.Result{Status: execution.StatusCancelled, StopReason: "cancelled"})
	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusCancelled || node(nodes, "a").Status != store.WorkflowStatusCancelled || node(nodes, "b").Status != store.WorkflowStatusCancelled {
		t.Fatalf("run %s nodes %+v", run.Status, nodes)
	}
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
}

func TestRestartInterruptsExecutingRuns(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, twoAgents, str("输入"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitStart(t)
	f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool { return node(n, "a").SessionID != nil })
	f.host.Close() // the process stops while A executes

	next := f.newHost(t)
	if err := next.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	run, _ := f.s.WorkflowRun(ctx, f.scope.TenantID, runID)
	nodes, _ := f.s.WorkflowEngineNodeRuns(ctx, runID)
	if run.Status != store.WorkflowStatusFailed || errorKind(t, node(nodes, "a")) != "interrupted_by_restart" {
		t.Fatalf("run %s node %+v", run.Status, node(nodes, "a"))
	}
	if _, err := next.StartRun(ctx, f.scope, runID); !errors.Is(err, server.ErrWorkflowRunNotStartable) {
		t.Fatalf("restart an interrupted run = %v", err)
	}
}

func TestLifecycleAuthorizesAndTranslatesRefusals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	graph := `{"nodes":[{"id":"start","data":{"kind":"start","inputVariables":[{"name":"topic","valueType":"string","required":true}]}},{"id":"out","data":{"kind":"output"}}],"edges":[{"source":"start","target":"out"}]}`
	runID := f.run(t, graph, nil)
	stranger := server.WorkflowRunScope{TenantID: "00000000-0000-0000-0000-000000000000", UserID: f.scope.UserID}
	if _, err := f.host.StartRun(ctx, stranger, runID); !errors.Is(err, store.ErrWorkflowRunNotFound) {
		t.Fatalf("foreign start = %v", err)
	}
	if _, err := f.host.CancelRun(ctx, stranger, runID); !errors.Is(err, store.ErrWorkflowRunNotFound) {
		t.Fatalf("foreign cancel = %v", err)
	}

	var invalid *server.WorkflowRunInputInvalid
	if _, err := f.host.UpdateRunInput(ctx, f.scope, runID, nil, map[string]json.RawMessage{"nope": json.RawMessage(`"x"`)}); !errors.As(err, &invalid) || invalid.Variable != "nope" || invalid.Reason != "undeclared" {
		t.Fatalf("undeclared input = %v", err)
	}
	if _, err := f.host.StartRun(ctx, f.scope, runID); !errors.As(err, &invalid) || invalid.Variable != "topic" {
		t.Fatalf("missing required input = %v", err)
	}
	if _, err := f.host.UpdateRunInput(ctx, f.scope, runID, str("开工"), map[string]json.RawMessage{"topic": json.RawMessage(`"支付"`)}); err != nil {
		t.Fatal(err)
	}
	run, err := f.host.StartRun(ctx, f.scope, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run, _ = f.await(t, runID, terminal); run.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("swift-only run = %+v", run)
	}
	if _, err := f.host.UpdateRunInput(ctx, f.scope, runID, str("太晚"), nil); !errors.Is(err, server.ErrWorkflowRunNotEditable) {
		t.Fatalf("late input = %v", err)
	}
	if _, err := f.host.StartRun(ctx, f.scope, runID); !errors.Is(err, server.ErrWorkflowRunNotStartable) {
		t.Fatalf("second start = %v", err)
	}
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "out", false, "human"); !errors.Is(err, server.ErrWorkflowNodeNotAwaitingInput) {
		t.Fatalf("complete = %v", err)
	}
	if _, err := f.host.CancelRun(ctx, f.scope, runID); !errors.Is(err, store.ErrWorkflowRunState) {
		t.Fatalf("cancel a succeeded run = %v", err)
	}
	if f.host.state(runID) != nil {
		t.Fatal("a finished run kept its process state")
	}
}

// parallelAgents fans Start out to two Agents, which execute concurrently.
const parallelAgents = `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"A"}}},
 {"id":"b","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"B"}}}
],"edges":[{"source":"start","target":"a"},{"source":"start","target":"b"}]}`

// fourAgents fans Start out to four Agents.
const fourAgents = `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"A"}}},
 {"id":"b","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"B"}}},
 {"id":"c","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"C"}}},
 {"id":"d","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"D"}}}
],"edges":[{"source":"start","target":"a"},{"source":"start","target":"b"},{"source":"start","target":"c"},{"source":"start","target":"d"}]}`

// While C's Start is stuck, D's Start returning is enough for the cancellation to stop D's turn:
// it never waits for one Start to see another. The cancellation tracks several Starts in flight
// with no order between them, so the scenario runs a few times.
func TestCancelStopsEachStartAsItReturns(t *testing.T) {
	for round := range 6 {
		t.Run(fmt.Sprint(round), func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			gates := map[string]chan struct{}{"c": make(chan struct{}), "d": make(chan struct{})}
			entered := make(chan string, 2)
			var once sync.Once
			releaseC := func() { once.Do(func() { close(gates["c"]) }) }
			t.Cleanup(releaseC)
			f.exec.beforeStart = func(req execution.StartRequest) error {
				if gate := gates[req.Caller.NodeID]; gate != nil {
					entered <- req.Caller.NodeID
					<-gate
				}
				return nil
			}
			runID := f.run(t, fourAgents, nil)
			if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
				t.Fatal(err)
			}
			started := f.exec.awaitStarts(t, 2)
			<-entered
			<-entered
			f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
				return node(n, "a").SessionID != nil && node(n, "b").SessionID != nil
			})
			cancelled := make(chan error, 1)
			go func() { _, err := f.host.CancelRun(ctx, f.scope, runID); cancelled <- err }()
			stopped := func(session string) bool {
				f.exec.mu.Lock()
				defer f.exec.mu.Unlock()
				return slices.ContainsFunc(f.exec.cancels, func(r execution.Ref) bool { return r.SessionID == session })
			}
			waitUntil(t, func() bool { return stopped(started["a"]) && stopped(started["b"]) })
			close(gates["d"])
			d := f.exec.awaitStart(t)
			// Well inside StopTimeout (a second here), so only waking on D's Start can explain it.
			deadline := time.Now().Add(300 * time.Millisecond)
			for !stopped(d) {
				if time.Now().After(deadline) {
					t.Fatal("D's turn was not stopped while C's Start was still in flight")
				}
				time.Sleep(5 * time.Millisecond)
			}
			select {
			case err := <-cancelled:
				t.Fatalf("cancel returned before C's Start did: %v", err)
			default:
			}
			releaseC()
			f.exec.awaitStart(t)
			if err := <-cancelled; err != nil {
				t.Fatalf("cancel = %v", err)
			}
			if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusCancelled {
				t.Fatalf("run %s", run.Status)
			}
		})
	}
}

// threeAgents fans Start out to three Agents.
const threeAgents = `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"A"}}},
 {"id":"b","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"B"}}},
 {"id":"c","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"C"}}}
],"edges":[{"source":"start","target":"a"},{"source":"start","target":"b"},{"source":"start","target":"c"}]}`

// A slow Start does not hold back the stop of siblings already executing: their turns are
// cancelled at once, and the slow one as soon as its Start returns. When the cancellation times
// out waiting for it, the late Start ends its own session and closes the run.
func TestCancelDoesNotWaitForASlowStartToStopTheOthers(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprint("late=", late), func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			f.exec.beforeStart = func(req execution.StartRequest) error {
				if req.Caller.NodeID == "c" {
					close(entered)
					<-release
				}
				return nil
			}
			runID := f.run(t, threeAgents, nil)
			if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
				t.Fatal(err)
			}
			started := f.exec.awaitStarts(t, 2)
			<-entered
			f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
				return node(n, "a").SessionID != nil && node(n, "b").SessionID != nil
			})
			cancelled := make(chan error, 1)
			go func() { _, err := f.host.CancelRun(ctx, f.scope, runID); cancelled <- err }()
			stoppedOf := func() map[string]bool {
				f.exec.mu.Lock()
				defer f.exec.mu.Unlock()
				stopped := map[string]bool{}
				for _, ref := range f.exec.cancels {
					stopped[ref.SessionID] = true
				}
				return stopped
			}
			waitUntil(t, func() bool { s := stoppedOf(); return s[started["a"]] && s[started["b"]] })
			if late {
				// The cancellation gives up on C's Start (StopTimeout is a second here).
				if err := <-cancelled; !errors.Is(err, server.ErrWorkflowRunStopUnconfirmed) {
					t.Fatalf("cancel past a stuck Start = %v", err)
				}
				once.Do(func() { close(release) })
				c := f.exec.awaitStart(t)
				run, _ := f.await(t, runID, terminal)
				f.exec.mu.Lock()
				ended := slices.Contains(f.exec.ended, c)
				f.exec.mu.Unlock()
				if run.Status != store.WorkflowStatusCancelled || !ended {
					t.Fatalf("after the late Start: run %s, C's session ended = %v", run.Status, ended)
				}
				return
			}
			select {
			case err := <-cancelled:
				t.Fatalf("cancel returned before C's Start did: %v", err)
			default:
			}
			once.Do(func() { close(release) })
			c := f.exec.awaitStart(t)
			if err := <-cancelled; err != nil {
				t.Fatalf("cancel = %v", err)
			}
			// Whichever comes first, the cancellation or C's attempt itself, ends C's turn.
			f.exec.mu.Lock()
			ended := slices.Contains(f.exec.ended, c)
			f.exec.mu.Unlock()
			if !stoppedOf()[c] && !ended {
				t.Fatal("nothing stopped the turn C's late Start returned")
			}
			if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusCancelled {
				t.Fatalf("run %s", run.Status)
			}
		})
	}
}

// A sibling already executing when another node fails the run keeps running and records its
// result, as desktop does; the run stays failed and is released once the sibling ends.
func TestSiblingOfAFailedNodeKeepsItsResult(t *testing.T) {
	f := newFixture(t)
	runID := f.run(t, strings.Replace(parallelAgents, `"prompt":"A"`, `"prompt":"A","retry":{"enabled":false,"maxRetries":2,"initialDelaySeconds":10}`, 1), nil)
	if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	sessions := f.exec.awaitStarts(t, 2)
	f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").SessionID != nil && node(n, "b").SessionID != nil
	})
	f.exec.finish(sessions["a"], execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	run, nodes := f.await(t, runID, func(r store.WorkflowRun, _ []store.WorkflowNodeRun) bool {
		return r.Status == store.WorkflowStatusFailed
	})
	if node(nodes, "a").Status != store.WorkflowStatusFailed || node(nodes, "b").Status != store.WorkflowStatusRunning {
		t.Fatalf("run %s, a %+v, b %+v", run.Status, node(nodes, "a"), node(nodes, "b"))
	}
	if f.host.state(runID) == nil {
		t.Fatal("the run's state was dropped while its sibling still executes")
	}
	f.exec.finish(sessions["b"], completed("B"))
	run, nodes = f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "b").Status == store.WorkflowStatusSucceeded
	})
	if run.Status != store.WorkflowStatusFailed || node(nodes, "b").Output == nil || *node(nodes, "b").Output != "B" {
		t.Fatalf("run %s, b %+v", run.Status, node(nodes, "b"))
	}
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
}

func TestRefusedStartForgetsTheStarter(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	graph := `{"nodes":[{"id":"start","data":{"kind":"start","inputVariables":[{"name":"topic","valueType":"string","required":true}]}},` +
		`{"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"{{#start.topic#}}"}}}],` +
		`"edges":[{"source":"start","target":"a"}]}`
	runID := f.run(t, graph, nil)
	var invalid *server.WorkflowRunInputInvalid
	if _, err := f.host.StartRun(ctx, f.scope, runID); !errors.As(err, &invalid) {
		t.Fatalf("start without the required input = %v", err)
	}
	if f.host.state(runID) != nil {
		t.Fatal("a refused start kept its starter as the run's owner")
	}

	// A teammate fixes the input and starts the run: the Agent executes under their scope.
	teammateID, _ := storetest.Login(t, f.s, "review-teammate")
	teammate := server.WorkflowRunScope{TenantID: f.scope.TenantID, UserID: teammateID}
	if _, err := f.host.UpdateRunInput(ctx, teammate, runID, nil, map[string]json.RawMessage{"topic": json.RawMessage(`"支付"`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.host.StartRun(ctx, teammate, runID); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitStart(t)
	f.exec.mu.Lock()
	scopes := append([]execution.Scope{}, f.exec.scopes...)
	f.exec.mu.Unlock()
	if want := (execution.Scope{TenantID: teammate.TenantID, UserID: teammate.UserID}); len(scopes) != 1 || scopes[0] != want {
		t.Fatalf("Agent scopes = %+v, want %+v", scopes, want)
	}
}

// reviewBranches is the S2 demo shape: a structured review decides which Agent runs, and the
// Aggregator joins whichever answered.
const reviewBranches = `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"review","data":{"kind":"agent","title":"评审","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"评审：{{#start.input#}}",
  "outputContract":{"type":"structured","schema":{"type":"object","properties":{"approved":{"type":"boolean"}},"required":["approved"]}}}}},
 {"id":"cond","data":{"kind":"condition","cases":[{"id":"ok","logic":"and","conditions":[{"variableSelector":["review","structured_output","approved"],"operator":"is","value":true}]}]}},
 {"id":"ship","data":{"kind":"agent","title":"发布","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"发布"}}},
 {"id":"fix","data":{"kind":"agent","title":"返工","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"返工"}}},
 {"id":"join","data":{"kind":"aggregator","aggregatorConfig":{"variables":[["ship","output"],["fix","output"]]}}},
 {"id":"out","data":{"kind":"output","outputs":[{"name":"result","variableSelector":["join","output"]}]}}
],"edges":[{"source":"start","target":"review"},{"source":"review","target":"cond"},
 {"source":"cond","sourceHandle":"ok","target":"ship"},{"source":"cond","sourceHandle":"else","target":"fix"},
 {"source":"ship","target":"join"},{"source":"fix","target":"join"},{"source":"join","target":"out"}]}`

func TestStructuredReviewRoutesOneBranchThroughTheAggregator(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, reviewBranches, str("支付重构"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	review := f.exec.awaitStart(t)
	text := promptText(t, f.exec.request(review).Prompt)
	if !strings.HasSuffix(text, "</structured_output_contract>") || !strings.Contains(text, "\"approved\": true") {
		t.Fatalf("review prompt does not end with the output contract:\n%s", text)
	}
	f.exec.finish(review, completed("评审结论：\n{\"approved\": false}"))
	fix := f.exec.awaitStart(t)
	if f.exec.request(fix).Caller.NodeID != "fix" {
		t.Fatalf("started %s, want the else branch", f.exec.request(fix).Caller.NodeID)
	}
	f.exec.finish(fix, completed("已返工"))
	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusSucceeded || run.Output == nil || *run.Output != `{"result":"已返工"}` {
		t.Fatalf("run = %s %v", run.Status, run.Output)
	}
	if node(nodes, "ship").ID != "" {
		t.Fatal("the branch not taken created a node row")
	}
	if c := node(nodes, "cond"); c.Status != store.WorkflowStatusSucceeded || c.Output != nil {
		t.Fatalf("condition row = %+v (decisions are not node output)", c)
	}
	if j := node(nodes, "join"); j.Output == nil || *j.Output != `"已返工"` {
		t.Fatalf("aggregator row = %+v", j)
	}
	if r := node(nodes, "review"); r.Output == nil || !strings.HasPrefix(*r.Output, "评审结论") {
		t.Fatalf("the structured node lost its raw answer: %+v", r)
	}
	c, err := f.s.WorkflowEngineContext(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		ConditionDecisions map[string]string `json:"conditionDecisions"`
		VariablePool       struct {
			Values map[string]any `json:"values"`
		} `json:"variablePool"`
	}
	if err := json.Unmarshal(c.Run.RuntimePayload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ConditionDecisions["cond"] != "else" || payload.VariablePool.Values["review.structured_output"] == nil {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestNonconformingStructuredAnswerFailsTheNode(t *testing.T) {
	f := newFixture(t)
	runID := f.run(t, strings.Replace(reviewBranches, `"outputContract":`, `"retry":{"enabled":false,"maxRetries":2,"initialDelaySeconds":10},"outputContract":`, 1), str("支付重构"))
	if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(f.exec.awaitStart(t), completed(`{"verdict": "ok"}`))
	run, nodes := f.await(t, runID, terminal)
	r := node(nodes, "review")
	if run.Status != store.WorkflowStatusFailed || errorKind(t, r) != "structured_output" || r.Output == nil || *r.Output != `{"verdict": "ok"}` {
		t.Fatalf("run %s review %+v kind %s", run.Status, r, errorKind(t, r))
	}
	if node(nodes, "cond").ID != "" {
		t.Fatal("the run advanced past a failed structured node")
	}
}

// noStart fails the test if another Agent session starts within a short wait.
func (f *fakeExecution) noStart(t *testing.T, why string) {
	t.Helper()
	select {
	case id := <-f.startedCh:
		t.Fatalf("%s: session %s started", why, id)
	case <-time.After(300 * time.Millisecond):
	}
}

// A fan-out wider than the run's free session slots waits for one instead of asking Node for a
// session over its checkout's limit: the waiting sibling starts once a session ends.
func TestSiblingsBeyondTheSessionLimitWaitForASlot(t *testing.T) {
	f := newFixture(t)
	f.host.sessionLimit = 1
	runID := f.run(t, parallelAgents, nil)
	if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	first := f.exec.awaitStart(t)
	f.exec.noStart(t, "a sibling started over the session limit")
	firstNode := f.exec.request(first).Caller.NodeID
	waiting := "a"
	if firstNode == "a" {
		waiting = "b"
	}
	f.await(t, runID, func(_ store.WorkflowRun, nodes []store.WorkflowNodeRun) bool {
		return slotWait(t, node(nodes, waiting)) != nil
	})
	if _, nodes := f.await(t, runID, always); slotWait(t, node(nodes, firstNode)) != nil {
		t.Fatal("the executing node reads as waiting for a session slot")
	}
	f.exec.finish(first, completed("one"))
	second := f.exec.awaitStart(t)
	if f.exec.request(second).Caller.NodeID != waiting {
		t.Fatal("the waiting sibling did not start next")
	}
	if _, nodes := f.await(t, runID, always); slotWait(t, node(nodes, waiting)) != nil {
		t.Fatal("a node that took its session slot still reads as waiting")
	}
	f.exec.finish(second, completed("two"))
	if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("run %s", run.Status)
	}
}

// Cancelling a run drops a sibling still waiting for a session slot: it never starts, and the run
// closes as cancelled once the executing turn is stopped.
func TestCancelDropsASiblingWaitingForASlot(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.host.sessionLimit = 1
	runID := f.run(t, parallelAgents, nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	first := f.exec.awaitStart(t)
	waiting := "a"
	if f.exec.request(first).Caller.NodeID == "a" {
		waiting = "b"
	}
	f.await(t, runID, func(_ store.WorkflowRun, nodes []store.WorkflowNodeRun) bool {
		return slotWait(t, node(nodes, waiting)) != nil
	})
	if _, err := f.host.CancelRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusCancelled {
		t.Fatalf("run %s", run.Status)
	}
	f.exec.noStart(t, "a waiting sibling started after the cancellation")
	_, nodes := f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
	for _, n := range nodes {
		if slotWait(t, n) != nil {
			t.Fatalf("cancelled node %s still reads as waiting for a session slot", n.NodeID)
		}
	}
}

// always ends an await at once, for reading the run's current rows.
func always(store.WorkflowRun, []store.WorkflowNodeRun) bool { return true }

// slotWait decodes the attempt's `session_slot_wait` marker, nil when it has none.
func slotWait(t *testing.T, n store.WorkflowNodeRun) *store.WorkflowSessionSlotWait {
	t.Helper()
	var p struct {
		Wait *store.WorkflowSessionSlotWait `json:"session_slot_wait"`
	}
	if len(n.Payload) != 0 {
		if err := json.Unmarshal(n.Payload, &p); err != nil {
			t.Fatalf("payload of %s: %v", n.NodeID, err)
		}
	}
	if p.Wait != nil && (p.Wait.Since == 0 || p.Wait.Limit == 0) {
		t.Fatalf("incomplete session slot wait on %s: %+v", n.NodeID, *p.Wait)
	}
	return p.Wait
}
