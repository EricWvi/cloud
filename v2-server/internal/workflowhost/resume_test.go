package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// resumeChain is Start → A → B → C next to an unrelated Start → D. B keeps the default retry
// policy (two retries); B and C read their predecessor's answer.
const resumeChain = `{"nodes":[
 {"id":"start","data":{"kind":"start","title":"开始"}},
 {"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"写初稿"}}},
 {"id":"b","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"审阅：{{#a.output#}}"}}},
 {"id":"c","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"发布：{{#b.output#}}"}}},
 {"id":"d","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"旁支"}}}
],"edges":[{"source":"start","target":"a"},{"source":"a","target":"b"},{"source":"b","target":"c"},{"source":"start","target":"d"}]}`

var sessionFailed = execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"}

// nextAgent waits for the next Agent session and returns it with the node it serves.
func nextAgent(t *testing.T, f *fixture) (string, string) {
	t.Helper()
	session := f.exec.awaitStart(t)
	return session, f.exec.request(session).Caller.NodeID
}

func expectAgent(t *testing.T, f *fixture, nodeID string) string {
	t.Helper()
	session, got := nextAgent(t, f)
	if got != nodeID {
		t.Fatalf("next Agent = %s, want %s", got, nodeID)
	}
	return session
}

// awaitWait waits until nodeID has a waiting retry and returns its row and payload.
func awaitWait(t *testing.T, f *fixture, runID, nodeID string) (store.WorkflowNodeRun, waitPayload) {
	t.Helper()
	_, rows := f.await(t, runID, func(_ store.WorkflowRun, rows []store.WorkflowNodeRun) bool {
		n := node(rows, nodeID)
		return n.Status == "running" && n.StartedAt == nil
	})
	row := node(rows, nodeID)
	var p waitPayload
	if err := json.Unmarshal(row.Payload, &p); err != nil || p.Wait == nil {
		t.Fatalf("wait payload %s: %v", row.Payload, err)
	}
	return row, p
}

func refusal(t *testing.T, err error) string {
	t.Helper()
	var refused *server.WorkflowRunNotResumable
	if !errors.As(err, &refused) {
		t.Fatalf("resume error = %v, want a refusal", err)
	}
	return refused.Reason
}

func runPayload(t *testing.T, f *fixture, runID string) workflow.RunPayload {
	t.Helper()
	run, err := f.s.WorkflowRun(context.Background(), f.scope.TenantID, runID)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := workflow.DecodeRunPayload(run.RuntimePayload)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResumeFromFailureReusesSucceededWorkAndContinuesAttempts(t *testing.T) {
	f := newFixture(t)
	clock := retryClock(t, f)
	ctx := context.Background()
	id := f.run(t, resumeChain, str("需求"))
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	// A and the unrelated D execute concurrently.
	first := f.exec.awaitStarts(t, 2)
	f.exec.finish(first["a"], completed("初稿A"))
	f.exec.finish(first["d"], completed("旁支D"))
	// B fails its first attempt and both automatic retries.
	f.exec.finish(expectAgent(t, f, "b"), sessionFailed)
	for retry := 1; retry <= 2; retry++ {
		row, p := awaitWait(t, f, id, "b")
		clock.Store(p.Wait.Due)
		if err := f.host.engine.WakeRetry(ctx, id, row.ID); err != nil {
			t.Fatal(err)
		}
		f.exec.finish(expectAgent(t, f, "b"), sessionFailed)
	}
	run, before := f.await(t, id, terminal)
	if run.Status != "failed" || node(before, "c").ID != "" {
		t.Fatalf("run = %s, c = %+v", run.Status, node(before, "c"))
	}
	if _, err := f.host.ResumeRun(ctx, server.WorkflowRunScope{TenantID: f.scope.TenantID, UserID: f.scope.UserID}, "00000000-0000-4000-8000-000000000000"); !errors.Is(err, store.ErrWorkflowRunNotFound) {
		t.Fatalf("missing run = %v", err)
	}

	preview, err := f.host.PreviewRunResume(ctx, f.scope, id)
	if err != nil {
		t.Fatal(err)
	}
	want := server.WorkflowRunResumePreview{Reason: "", RunningNodeIDs: []string{}, RestartContainerNodeIDs: []string{}, ClearNodeIDs: []string{"b", "c"},
		FailedNodes: []server.WorkflowRunResumeFailedNode{{NodeID: "b", NodeRunID: node(before, "b").ID, Status: "failed"}}}
	if !reflect.DeepEqual(preview, want) {
		t.Fatalf("preview = %+v, want %+v", preview, want)
	}

	resumed, err := f.host.ResumeRun(ctx, f.scope, id)
	if err != nil || resumed.Status != "running" || resumed.Error != nil || resumed.FinishedAt != nil {
		t.Fatalf("resume = %+v %v", resumed, err)
	}
	// The failure anchor is gone: the resume emptied current_nodes, and the rescheduled wave
	// anchored only B's new attempt.
	var state struct {
		Current []string `json:"current_nodes"`
	}
	if err := json.Unmarshal(resumed.State, &state); err != nil || !slices.Equal(state.Current, []string{"b"}) {
		t.Fatalf("current_nodes after resume = %v (%s)", state.Current, resumed.State)
	}
	// B reruns with A's kept answer, under a new attempt; A and the unrelated D keep their rows.
	b4 := expectAgent(t, f, "b")
	if text := promptText(t, f.exec.request(b4).Prompt); !strings.Contains(text, "审阅：初稿A") {
		t.Fatalf("B did not reuse A's output:\n%s", text)
	}
	_, live := f.await(t, id, func(_ store.WorkflowRun, rows []store.WorkflowNodeRun) bool { return node(rows, "b").SessionID != nil })
	for _, kept := range []string{"start", "a", "d"} {
		if got, was := node(live, kept), node(before, kept); got.ID != was.ID || got.Status != "succeeded" || !reflect.DeepEqual(got.SessionID, was.SessionID) {
			t.Fatalf("%s changed: %+v, was %+v", kept, got, was)
		}
	}
	var fresh map[string]any
	_ = json.Unmarshal(node(live, "b").Payload, &fresh)
	if _, marked := fresh["auto_retry"]; marked {
		t.Fatalf("the resumed attempt inherited the retry marker: %v", fresh)
	}
	// The resumed attempt is attempt 4, and its failure starts a fresh retry budget.
	f.exec.finish(b4, sessionFailed)
	row, p := awaitWait(t, f, id, "b")
	if p.Wait.Attempt != 5 || p.Wait.Retry != 1 || p.Wait.MaxAttempt != 6 || !slices.Equal(p.Chain, []string{f.exec.request(b4).Caller.AttemptID}) {
		t.Fatalf("wait after resume = %+v chain %v", p.Wait, p.Chain)
	}
	clock.Store(p.Wait.Due)
	if err := f.host.engine.WakeRetry(ctx, id, row.ID); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(expectAgent(t, f, "b"), completed("通过"))
	c := expectAgent(t, f, "c")
	if text := promptText(t, f.exec.request(c).Prompt); !strings.Contains(text, "发布：通过") {
		t.Fatalf("C prompt:\n%s", text)
	}
	f.exec.finish(c, completed("已发布"))
	if run, _ := f.await(t, id, terminal); run.Status != "succeeded" {
		t.Fatalf("run = %+v", run)
	}
	history, err := f.s.WorkflowRunFailedAttempts(ctx, f.scope.TenantID, id)
	if err != nil {
		t.Fatal(err)
	}
	attempts := []int{}
	for _, h := range history {
		var p waitPayload
		_ = json.Unmarshal(h.Payload, &p)
		if h.NodeID != "b" {
			t.Fatalf("history holds %s", h.NodeID)
		}
		attempts = append(attempts, p.Detail.Attempt)
	}
	if !slices.Equal(attempts, []int{1, 2, 3, 4}) {
		t.Fatalf("attempts = %v", attempts)
	}
	// A finished run is not resumable.
	if _, err := f.host.ResumeRun(ctx, f.scope, id); refusal(t, err) != workflow.ResumeRefusedStatus {
		t.Fatalf("succeeded run resumed: %v", err)
	}
}

// siblings runs D and A side by side from Start; A never retries.
const siblings = `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"d","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"慢的旁支"}}},
 {"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"会失败","retry":{"enabled":false,"maxRetries":2,"initialDelaySeconds":10}}}}
],"edges":[{"source":"start","target":"d"},{"source":"start","target":"a"}]}`

func TestResumeRefusedWhileASiblingRunsAndOnALiveRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.run(t, siblings, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	started := f.exec.awaitStarts(t, 2)
	d := started["d"]
	if _, err := f.host.ResumeRun(ctx, f.scope, id); refusal(t, err) != workflow.ResumeRefusedStatus {
		t.Fatalf("running run resumed: %v", err)
	}
	// A fails the run while D's turn is still executing.
	f.exec.finish(started["a"], sessionFailed)
	f.await(t, id, terminal)
	preview, err := f.host.PreviewRunResume(ctx, f.scope, id)
	if err != nil || preview.Reason != workflow.ResumeRefusedRunning || !slices.Equal(preview.RunningNodeIDs, []string{"d"}) {
		t.Fatalf("preview = %+v %v", preview, err)
	}
	err = func() error { _, err := f.host.ResumeRun(ctx, f.scope, id); return err }()
	var refused *server.WorkflowRunNotResumable
	if !errors.As(err, &refused) || refused.Reason != workflow.ResumeRefusedRunning || !slices.Equal(refused.RunningNodeIDs, []string{"d"}) {
		t.Fatalf("resume beside a running sibling = %v", err)
	}
	if run, _ := f.s.WorkflowRun(ctx, f.scope.TenantID, id); run.Status != "failed" {
		t.Fatalf("a refused resume changed the run: %+v", run)
	}
	// D's late result is recorded like desktop's, which opens the gate and is reused.
	f.exec.finish(d, completed("旁支D"))
	_, rows := f.await(t, id, func(_ store.WorkflowRun, rows []store.WorkflowNodeRun) bool {
		return node(rows, "d").Status == "succeeded"
	})
	if v := runPayload(t, f, id).VariablePool.Values["d.output"]; v != "旁支D" {
		t.Fatalf("late answer not folded: %v", v)
	}
	f.await(t, id, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return !f.host.settling(id) })
	// Another member may resume, as they may cancel; the Agents still run as the starter, who
	// owns every session of the run.
	other, tenant := storetest.Login(t, f.s, "resume-member")
	if tenant != f.scope.TenantID {
		t.Fatalf("second member is in tenant %s, want %s", tenant, f.scope.TenantID)
	}
	if _, err := f.host.ResumeRun(ctx, server.WorkflowRunScope{TenantID: tenant, UserID: other}, id); err != nil {
		t.Fatal(err)
	}
	a := expectAgent(t, f, "a")
	f.exec.mu.Lock()
	owner := f.exec.scopes[len(f.exec.scopes)-1].UserID
	f.exec.mu.Unlock()
	if owner != f.scope.UserID {
		t.Fatalf("resumed Agent executed as %s, want the starter %s", owner, f.scope.UserID)
	}
	f.exec.finish(a, completed("好了"))
	run, after := f.await(t, id, terminal)
	if run.Status != "succeeded" || node(after, "d").ID != node(rows, "d").ID {
		t.Fatalf("run = %+v d = %+v", run, node(after, "d"))
	}
}

func TestResumeCancelledRunClearsTheCancellationFence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.run(t, twoAgents, str("支付重构"))
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	expectAgent(t, f, "a")
	if run, err := f.host.CancelRun(ctx, f.scope, id); err != nil || run.Status != "cancelled" {
		t.Fatalf("cancel = %+v %v", run, err)
	}
	preview, err := f.host.PreviewRunResume(ctx, f.scope, id)
	if err != nil || preview.Reason != "" || !slices.Equal(preview.ClearNodeIDs, []string{"a", "b", "out"}) || preview.FailedNodes[0].Status != "cancelled" {
		t.Fatalf("preview = %+v %v", preview, err)
	}
	// The cancelled turn's worker releases the fence once more when it settles; wait for it to
	// finish so the fence planted below is the one the resume must drop.
	for deadline := time.Now().Add(5 * time.Second); f.host.state(id) != nil; {
		if time.Now().After(deadline) {
			t.Fatal("the cancelled run's worker never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// A fence an overlapping cancellation left behind must not swallow the resumed run's retries.
	f.host.engine.CancelRetries(id)
	if fenced, _ := f.host.engine.HeldRetries(id); !fenced {
		t.Fatal("the planted fence is not held")
	}
	if _, err := f.host.ResumeRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	if fenced, armed := f.host.engine.HeldRetries(id); fenced || armed != 0 {
		t.Fatalf("resume kept fence=%v armed=%d", fenced, armed)
	}
	// A retries by default: its failure now schedules a retry instead of hanging.
	f.exec.finish(expectAgent(t, f, "a"), sessionFailed)
	_, p := awaitWait(t, f, id, "a")
	if p.Wait.Attempt != 3 || p.Wait.Retry != 1 {
		t.Fatalf("wait = %+v", p.Wait)
	}
	if _, armed := f.host.engine.HeldRetries(id); armed != 1 {
		t.Fatalf("armed = %d", armed)
	}
}

func TestResumeRestartsAFailedIterationFromItsFirstRound(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.run(t, iterationGraph("fail", []string{"one", "two"}, 50), nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(expectAgent(t, f, "worker"), completed("W1"))
	f.exec.finish(expectAgent(t, f, "worker"), sessionFailed)
	if run, _ := f.await(t, id, terminal); run.Status != "failed" {
		t.Fatalf("run = %+v", run)
	}
	p := runPayload(t, f, id)
	if p.VariablePool.Values["worker.output"] != "W1" || p.IterationConditionDecisions["gate#1"] == "" || len(p.IterationLedger["iter"]) == 0 {
		t.Fatalf("the failed run lacks the round state this test clears: %+v", p)
	}
	preview, err := f.host.PreviewRunResume(ctx, f.scope, id)
	if err != nil || !slices.Equal(preview.RestartContainerNodeIDs, []string{"iter"}) || !slices.Equal(preview.ClearNodeIDs, []string{"gate", "iter", "out", "worker"}) {
		t.Fatalf("preview = %+v %v", preview, err)
	}
	for _, n := range preview.FailedNodes {
		if n.ContainerNodeID != "iter" {
			t.Fatalf("failed node %+v is not promoted to its Iteration", n)
		}
	}
	if _, err := f.host.ResumeRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	first := expectAgent(t, f, "worker")
	// The cleared round state is gone before the container reruns it.
	p = runPayload(t, f, id)
	// Round 0 has already started again, so only its fresh gate decision and an empty ledger exist.
	if _, kept := p.VariablePool.Values["worker.output"]; kept || len(p.IterationConditionDecisions) != 1 || len(p.IterationLedger["iter"]) != 0 {
		t.Fatalf("cleared state survived: values=%v decisions=%v ledger=%v", p.VariablePool.Values, p.IterationConditionDecisions, p.IterationLedger)
	}
	if text := promptText(t, f.exec.request(first).Prompt); !strings.Contains(text, "item 0: one") {
		t.Fatalf("the Iteration did not restart from round 1:\n%s", text)
	}
	f.exec.finish(first, completed("W1"))
	f.exec.finish(expectAgent(t, f, "worker"), completed("W2"))
	run, _ := f.await(t, id, terminal)
	if run.Status != "succeeded" || run.Output == nil || *run.Output != `{"failed":0,"items":["W1","W2"]}` {
		t.Fatalf("run = %+v %v", run.Status, run.Output)
	}
}

func TestResumeRestartsAFailedLoopFromRoundOne(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.run(t, loopGraph, str("kickoff"))
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(expectAgent(t, f, "writer"), completed("draft"))
	f.exec.finish(expectAgent(t, f, "writer"), sessionFailed)
	run, before := f.await(t, id, terminal)
	if run.Status != "failed" {
		t.Fatalf("run = %+v", run)
	}
	preview, err := f.host.PreviewRunResume(ctx, f.scope, id)
	if err != nil || !slices.Equal(preview.RestartContainerNodeIDs, []string{"loop"}) || !slices.Equal(preview.ClearNodeIDs, []string{"entry", "loop", "out", "writer"}) {
		t.Fatalf("preview = %+v %v", preview, err)
	}
	if _, err := f.host.ResumeRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	first := expectAgent(t, f, "writer")
	if text := promptText(t, f.exec.request(first).Prompt); !strings.Contains(text, "Improve seed") {
		t.Fatalf("the Loop did not restart from round 1:\n%s", text)
	}
	f.exec.finish(first, completed("done"))
	run, after := f.await(t, id, terminal)
	if run.Status != "succeeded" || run.Output == nil || *run.Output != `{"result":"done"}` {
		t.Fatalf("run = %+v", run)
	}
	rounds, err := f.s.WorkflowEngineLoopRounds(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	newLoop := node(after, "loop").ID
	if newLoop == node(before, "loop").ID {
		t.Fatal("the Loop kept its old row")
	}
	statuses := map[string][]string{}
	for _, r := range rounds {
		key := "old"
		if *r.ParentLoopNodeRunID == newLoop {
			key = "new"
		}
		statuses[key] = append(statuses[key], *r.Status)
	}
	if !slices.Equal(statuses["old"], []string{"succeeded", "failed"}) || !slices.Equal(statuses["new"], []string{"succeeded"}) {
		t.Fatalf("rounds = %v", statuses)
	}
	if _, ok := f.s.WorkflowEngineScopeNodes(ctx, id, rounds[0].ID); ok != nil {
		t.Fatal(ok)
	}
	if rows, _ := f.s.WorkflowEngineScopeNodes(ctx, id, rounds[1].ID); len(rows) != 0 {
		t.Fatalf("the old round kept live rows: %+v", rows)
	}
}

func TestResumeInjectsPreviousFailureAndRespectsDisabledFlag(t *testing.T) {
	for _, inject := range []bool{true, false} {
		t.Run(string(rune('0'+map[bool]int{true: 1, false: 0}[inject])), func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			graph := strings.Replace(reviewBranches, `"outputContract":`, `"retry":{"enabled":false,"maxRetries":0,"initialDelaySeconds":10},"outputContract":`, 1)
			w, _, err := f.s.CreateWorkflow(ctx, f.scope.TenantID, "恢复注入", graph)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.PublishWorkflow(ctx, f.scope.TenantID, w.ID, "v1", nil); err != nil {
				t.Fatal(err)
			}
			run, err := f.s.CreateWorkflowRun(ctx, f.scope.TenantID, w.ID, "", store.WorkflowRunInput{
				ProjectID:         f.project,
				Input:             str("支付评审"),
				InjectLastFailure: &inject,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.host.StartRun(ctx, f.scope, run.ID); err != nil {
				t.Fatal(err)
			}
			f.exec.finish(expectAgent(t, f, "review"), completed(`{"verdict":"bad"}`))
			failedRun, _ := f.await(t, run.ID, terminal)
			if failedRun.Status != "failed" {
				t.Fatalf("run = %+v", failedRun)
			}
			if _, err := f.host.ResumeRun(ctx, f.scope, run.ID); err != nil {
				t.Fatal(err)
			}
			resumedSession := expectAgent(t, f, "review")
			prompt := promptText(t, f.exec.request(resumedSession).Prompt)
			nodes, err := f.s.WorkflowRunNodes(ctx, f.scope.TenantID, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if len(node(nodes, "review").Payload) > 0 {
				if err := json.Unmarshal(node(nodes, "review").Payload, &payload); err != nil {
					t.Fatal(err)
				}
			}
			if inject {
				if !strings.Contains(prompt, "## 上一次尝试（第 1 次）失败信息") || !strings.Contains(prompt, `{"verdict":"bad"}`) {
					t.Fatalf("missing previous failure in resumed prompt:\n%s", prompt)
				}
				if _, ok := payload["injected_failure_context"].(string); !ok {
					t.Fatalf("resumed row missing injected_failure_context: %s", node(nodes, "review").Payload)
				}
			} else {
				if strings.Contains(prompt, "上一次尝试") {
					t.Fatalf("unexpected previous failure when disabled:\n%s", prompt)
				}
				if _, exists := payload["injected_failure_context"]; exists {
					t.Fatalf("unexpected injected_failure_context when disabled: %s", node(nodes, "review").Payload)
				}
			}
			f.exec.finish(resumedSession, completed(`{"approved":true}`))
			f.exec.finish(expectAgent(t, f, "ship"), completed("已发布"))
			settled, _ := f.await(t, run.ID, terminal)
			if settled.Status != "succeeded" {
				t.Fatalf("settled = %+v", settled)
			}
		})
	}
}
