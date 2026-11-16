package workflowhost

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// startedAttempts lists the attempt ids every accepted Start served.
func startedAttempts(f *fixture) []string {
	f.exec.mu.Lock()
	defer f.exec.mu.Unlock()
	ids := []string{}
	for _, r := range f.exec.started {
		ids = append(ids, r.Caller.AttemptID)
	}
	return ids
}

// awaitIdle waits until the host no longer holds the run's process state, i.e. its worker drained.
func awaitIdle(t *testing.T, f *fixture, runID string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); f.host.state(runID) != nil; {
		if time.Now().After(deadline) {
			t.Fatal("the run's worker never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// blockFirstExecutorList makes the first ListExecutors call wait until the returned release is
// called, reporting on entered once it is waiting.
func blockFirstExecutorList(f *fixture) (entered <-chan struct{}, release func()) {
	var once sync.Once
	var calls atomic.Int32
	gate := make(chan struct{})
	in := make(chan struct{}, 1)
	list := f.exec.ListExecutorsFunc
	f.exec.ListExecutorsFunc = func(c context.Context, s execution.Scope) ([]execution.Executor, error) {
		if calls.Add(1) == 1 {
			in <- struct{}{}
			<-gate
		}
		return list(c, s)
	}
	return in, func() { once.Do(func() { close(gate) }) }
}

// M1: a dispatch already past its abandon check and resolving its executor when the run is
// cancelled and resumed must never start a turn for the attempt the resume soft-deleted, and the
// resumed attempt must still run (the cancellation request is cleared by the resume, N5).
func TestResumeNeverStartsADispatchQueuedBeforeTheCancel(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	entered, release := blockFirstExecutorList(f)
	defer release()
	id := f.run(t, twoAgents, str("x"))
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	<-entered
	_, rows := f.await(t, id, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool { return node(n, "a").ID != "" })
	stale := node(rows, "a").ID
	if run, err := f.host.CancelRun(ctx, f.scope, id); err != nil || run.Status != "cancelled" {
		t.Fatalf("cancel = %+v %v", run, err)
	}
	if _, err := f.host.ResumeRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	release()
	// The worker is serial: the resumed attempt starts only after the stale dispatch was handled.
	resumed := expectAgent(t, f, "a")
	if got := f.exec.request(resumed).Caller.AttemptID; got == stale {
		t.Fatal("the resumed run reused the cancelled attempt")
	}
	if slices.Contains(startedAttempts(f), stale) {
		t.Fatalf("the cancelled attempt %s started an Agent turn after the resume", stale)
	}
	f.exec.finish(resumed, completed("A"))
	f.exec.finish(expectAgent(t, f, "b"), completed("B"))
	if run, _ := f.await(t, id, terminal); run.Status != "succeeded" {
		t.Fatalf("resumed run = %+v", run)
	}
}

// M1: a dispatch whose run a sibling failed while it resolved its executor must not start: the
// abandon check is repeated once the start slot is reserved.
func TestADispatchWhoseRunEndedWhileItResolvedDoesNotStart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	entered, release := blockFirstExecutorList(f)
	defer release()
	id := f.run(t, siblings, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	<-entered // D is resolving its executor; A is queued behind it
	_, rows := f.await(t, id, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool { return node(n, "a").ID != "" })
	if err := f.host.engine.FailNode(ctx, id, node(rows, "a").ID, workflow.Failure{Kind: workflow.FailureSession, Message: "boom"}); err != nil {
		t.Fatal(err)
	}
	release()
	awaitIdle(t, f, id)
	if started := startedAttempts(f); len(started) != 0 {
		t.Fatalf("an Agent started under the failed run: %v", started)
	}
	_, rows = f.await(t, id, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool { return node(n, "d").Status == "cancelled" })
	if node(rows, "d").SessionID != nil {
		t.Fatalf("d = %+v", node(rows, "d"))
	}
}

// N3 / L5.1: while a cancelled turn is still ending in this process, the preview and the resume
// both say the run is settling, with no node list.
func TestResumeWaitsForTheCancelledTurnToFinishEnding(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var block atomic.Bool
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	f.exec.EndFunc = func(context.Context, execution.Scope, string) error {
		if block.Load() {
			entered <- struct{}{}
			<-release
		}
		return nil
	}
	id := f.run(t, twoAgents, str("x"))
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitStart(t)
	block.Store(true)
	if run, err := f.host.CancelRun(ctx, f.scope, id); err != nil || run.Status != "cancelled" {
		t.Fatalf("cancel = %+v %v", run, err)
	}
	<-entered
	preview, perr := f.host.PreviewRunResume(ctx, f.scope, id)
	_, err := f.host.ResumeRun(ctx, f.scope, id)
	block.Store(false)
	close(release)
	if perr != nil || preview.Reason != workflow.ResumeRefusedSettling {
		t.Fatalf("preview while the turn ends = %+v %v", preview, perr)
	}
	refused := refusalOf(t, err)
	if refused.Reason != workflow.ResumeRefusedSettling || refused.RunningNodeIDs == nil || len(refused.RunningNodeIDs) != 0 {
		t.Fatalf("resume while the turn ends = %+v", refused)
	}
	awaitIdle(t, f, id)
	if _, err := f.host.ResumeRun(ctx, f.scope, id); err != nil {
		t.Fatalf("resume once the turn ended: %v", err)
	}
	expectAgent(t, f, "a")
}

// M2: a run cancelled while its review form waited cannot resume, and the form retry does not
// apply either, so the refusal names the case that leaves only a new run.
func TestResumeOfARunCancelledAtItsOpenReviewPointsToANewRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	report := "Inspection report"
	id := f.run(t, formGraph, &report)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.await(t, id, func(_ store.WorkflowRun, nodes []store.WorkflowNodeRun) bool {
		return node(nodes, "review").Status == "pending"
	})
	if run, err := f.host.CancelRun(ctx, f.scope, id); err != nil || run.Status != "cancelled" {
		t.Fatalf("cancel = %+v %v", run, err)
	}
	preview, err := f.host.PreviewRunResume(ctx, f.scope, id)
	if err != nil || preview.Reason != workflow.ResumeRefusedForm {
		t.Fatalf("preview = %+v %v", preview, err)
	}
	if _, err := f.host.ResumeRun(ctx, f.scope, id); refusal(t, err) != workflow.ResumeRefusedForm {
		t.Fatalf("resume = %v", err)
	}
	if _, err := f.host.RetryForm(ctx, storetest.Access(t, f.s, f.scope.TenantID, f.scope.UserID), id); err == nil {
		t.Fatal("the form retry applied to an unsubmitted form of a cancelled run")
	}
}

// M2: a run whose task creation failed after the form was submitted keeps its form and resumes
// normally, rerunning only the task creation; only an unsubmitted form refuses (above).
func TestResumeOfAFailedTaskCreationRerunsOnlyTheTasks(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	report := "Evidence"
	id := f.run(t, formGraph, &report)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.await(t, id, func(_ store.WorkflowRun, nodes []store.WorkflowNodeRun) bool {
		return node(nodes, "review").Status == "pending"
	})
	access := storetest.Access(t, f.s, f.scope.TenantID, f.scope.UserID)
	form, err := f.host.ReadForm(ctx, access, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.UpsertUser(ctx, store.PresetUser{Email: "resume-retired-assignee@demo.test", DisplayName: "Gone", Role: store.RoleMember}); err != nil {
		t.Fatal(err)
	}
	assignee, err := f.s.UserByEmail(ctx, "resume-retired-assignee@demo.test")
	if err != nil {
		t.Fatal(err)
	}
	values := []any{map[string]any{"key": "disk:root", "decision": "confirmed", "assignee": map[string]string{"type": "human", "id": assignee.ID}, "values": map[string]string{}}}
	answers, _ := json.Marshal(values)
	if _, err := f.s.SaveWorkflowForm(ctx, access, id, form.Version, "resume-gap", answers, true, func(store.WorkflowForm) error { return nil }, func(raw []byte, form store.WorkflowForm) ([]byte, error) {
		p, _, err := workflow.DecodeRunPayload(raw)
		if err != nil {
			return nil, err
		}
		text := string(answers)
		if err := p.Complete(form.NodeID, "form", nil, &text, map[string]any{"answers": values}); err != nil {
			return nil, err
		}
		return p.Encode()
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.StartWorkflowEngineNodes(ctx, id, []store.WorkflowEngineStart{{NodeID: "tasks", NodeType: "createTasks"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.RetireUsers(ctx, []store.RetiredUser{{Email: "resume-retired-assignee@demo.test"}}); err != nil {
		t.Fatal(err)
	}
	f.host.Close()
	f.host = f.newHost(t)
	if err := f.host.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	if run, _ := f.s.WorkflowRun(ctx, f.scope.TenantID, id); run.Status != "failed" {
		t.Fatalf("run = %+v", run)
	}
	preview, err := f.host.PreviewRunResume(ctx, f.scope, id)
	if err != nil || preview.Reason != "" || !slices.Equal(preview.ClearNodeIDs, []string{"output", "tasks"}) {
		t.Fatalf("preview = %+v %v", preview, err)
	}
	if _, err := f.host.ResumeRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	// The frozen form stays submitted; task creation runs again and fails again on the retired assignee.
	run, rows := f.await(t, id, terminal)
	if run.Status != "failed" || node(rows, "review").Status != "succeeded" || node(rows, "tasks").Status != "failed" {
		t.Fatalf("run = %s review = %s tasks = %s", run.Status, node(rows, "review").Status, node(rows, "tasks").Status)
	}
	if history, _ := f.s.WorkflowRunFailedAttempts(ctx, f.scope.TenantID, id); len(history) != 1 || history[0].NodeID != "tasks" {
		t.Fatalf("history = %+v", history)
	}
}

// reviewIterLoop is Start → Iteration (continue; one member fails) → Loop{entry → writer} → z.
// z fails, so the Iteration is a resume unit and the Loop is cleared only as its descendant: the
// Loop's body node ids are not in the clear set.
const reviewIterLoop = `{"schemaVersion":2,"nodes":[
 {"id":"start","data":{"kind":"start","inputVariables":[{"name":"items","valueType":"array[string]","value":["one","bad"]}]}},
 {"id":"iter","data":{"kind":"iteration","iterationConfig":{"iteratorSelector":["start","items"],"collectSelector":["worker","output"],"errorStrategy":"continue","maxIterations":50}}},
 {"id":"gate","parentId":"iter","data":{"kind":"condition","cases":[{"id":"process","logic":"and","conditions":[{"variableSelector":["iter","item"],"operator":"not_equals","value":"skip"}]}]}},
 {"id":"worker","parentId":"iter","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"item {{#iter.index#}}: {{#iter.item#}}","retry":{"enabled":false,"maxRetries":0,"initialDelaySeconds":1}}}},
 {"id":"loop","data":{"kind":"loop","loopConfig":{"maxIterations":2,"variables":[],"until":{"logic":"and","conditions":[{"variableSelector":["writer","output"],"operator":"equals","value":"done"}]},"outputs":[{"name":"result","variableSelector":["writer","output"]}]}}},
 {"id":"entry","data":{"kind":"start","containerId":"loop"}},
 {"id":"writer","data":{"kind":"agent","containerId":"loop","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"write","retry":{"enabled":false,"maxRetries":0,"initialDelaySeconds":1}}}},
 {"id":"z","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"z","retry":{"enabled":false,"maxRetries":0,"initialDelaySeconds":1}}}}
 ],"edges":[{"source":"start","target":"iter"},{"source":"iter","target":"gate"},{"source":"gate","sourceHandle":"process","target":"worker"},{"source":"iter","target":"loop"},{"source":"entry","target":"writer"},{"source":"loop","target":"z"}]}`

// L1 (M8): the round rows of a Loop cleared only as a descendant are soft-deleted by scope.
func TestResumeClearsTheRoundRowsOfALoopClearedAsADescendant(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.run(t, reviewIterLoop, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(expectAgent(t, f, "worker"), completed("W1"))
	f.exec.finish(expectAgent(t, f, "worker"), sessionFailed)
	f.exec.finish(expectAgent(t, f, "writer"), completed("done"))
	f.exec.finish(expectAgent(t, f, "z"), sessionFailed)
	if run, _ := f.await(t, id, terminal); run.Status != "failed" {
		t.Fatalf("run = %+v", run)
	}
	preview, err := f.host.PreviewRunResume(ctx, f.scope, id)
	if err != nil || !slices.Equal(preview.ClearNodeIDs, []string{"gate", "iter", "loop", "worker", "z"}) {
		t.Fatalf("preview = %+v %v", preview, err)
	}
	if _, err := f.host.ResumeRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	expectAgent(t, f, "worker")
	rows, err := f.s.WorkflowRunNodes(ctx, f.scope.TenantID, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.NodeID == "writer" || r.NodeID == "entry" {
			t.Errorf("old Loop round row survived the resume: %s %s scope=%s", r.NodeID, r.Status, r.ScopeID)
		}
	}
}

// reviewLoopBeside runs X beside a Loop; X fails the run while the Loop's writer is queued.
const reviewLoopBeside = `{"schemaVersion":2,"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"x","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"x","retry":{"enabled":false,"maxRetries":0,"initialDelaySeconds":1}}}},
 {"id":"loop","data":{"kind":"loop","loopConfig":{"maxIterations":2,"variables":[],"until":{"logic":"and","conditions":[{"variableSelector":["writer","output"],"operator":"equals","value":"done"}]},"outputs":[{"name":"result","variableSelector":["writer","output"]}]}}},
 {"id":"entry","data":{"kind":"start","containerId":"loop"}},
 {"id":"writer","data":{"kind":"agent","containerId":"loop","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"write"}}}
 ],"edges":[{"source":"start","target":"x"},{"source":"start","target":"loop"},{"source":"entry","target":"writer"}]}`

// N12: the open round of a Loop the resume clears is closed, not left reserved.
func TestResumeClosesTheOpenRoundOfAClearedLoop(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.run(t, reviewLoopBeside, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(expectAgent(t, f, "x"), sessionFailed)
	// The queued writer is abandoned (cancelled) under the failed run; its round stays open.
	var open store.WorkflowScope
	for deadline := time.Now().Add(5 * time.Second); open.ID == ""; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the Loop's round never settled its abandoned writer")
		}
		rounds, err := f.s.WorkflowEngineLoopRounds(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rounds {
			rows, _ := f.s.WorkflowEngineScopeNodes(ctx, id, r.ID)
			if *r.Status == "running" && node(rows, "writer").Status == "cancelled" {
				open = r
			}
		}
	}
	if _, err := f.host.ResumeRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	rounds, err := f.s.WorkflowEngineLoopRounds(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rounds {
		if r.ID == open.ID && *r.Status != "cancelled" {
			t.Fatalf("the cleared Loop's open round stayed %s", *r.Status)
		}
	}
}

// N2 / L3: a late failure under an ended run records the attempt but keeps the run's error and
// its failure anchor on the node that failed it.
func TestALateSiblingFailureKeepsTheRunsFailureAnchor(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.run(t, siblings, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	d := expectAgent(t, f, "d")
	_, rows := f.await(t, id, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool { return node(n, "a").ID != "" })
	if err := f.host.engine.FailNode(ctx, id, node(rows, "a").ID, workflow.Failure{Kind: workflow.FailureSession, Message: "boom"}); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(d, sessionFailed)
	run, rows := f.await(t, id, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool { return node(n, "d").Status == "failed" })
	var state struct {
		Current []string `json:"current_nodes"`
	}
	_ = json.Unmarshal(run.State, &state)
	if run.Status != "failed" || run.Error == nil || *run.Error != "boom" || !slices.Equal(state.Current, []string{"a"}) {
		t.Fatalf("late failure moved the run: status=%s error=%v current=%v", run.Status, run.Error, state.Current)
	}
	if _, retried := waitOf(node(rows, "d")); retried {
		t.Fatal("a late failure under an ended run scheduled a retry")
	}
}

// refusalOf returns the resume refusal err carries, failing the test for any other error.
func refusalOf(t *testing.T, err error) *server.WorkflowRunNotResumable {
	t.Helper()
	refused, ok := err.(*server.WorkflowRunNotResumable)
	if !ok {
		t.Fatalf("resume error = %v, want a refusal", err)
	}
	return refused
}

func waitOf(n store.WorkflowNodeRun) (waitPayload, bool) {
	var p waitPayload
	_ = json.Unmarshal(n.Payload, &p)
	return p, p.Wait != nil
}

// L4: a cancellation that starts while ResumeRun is under way waits for it, sees the run running
// again and stops what the resume dispatched; the run closes cancelled with no resumed turn left
// executing. The resume blocks inside ResumeRun, holding the run's lifecycle lock.
func TestCancelDuringAResumeStopsTheResumedAgent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.run(t, siblings, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(expectAgent(t, f, "d"), completed("D"))
	f.exec.finish(expectAgent(t, f, "a"), sessionFailed)
	_, before := f.await(t, id, terminal)
	awaitIdle(t, f, id)
	failedA := node(before, "a").ID

	inside, release := make(chan struct{}), make(chan struct{})
	f.host.resuming = func(string) {
		close(inside)
		<-release
	}
	resumed := make(chan error, 1)
	go func() { _, err := f.host.ResumeRun(ctx, f.scope, id); resumed <- err }()
	<-inside
	cancelled := make(chan store.WorkflowRun, 1)
	go func() {
		run, err := f.host.CancelRun(ctx, f.scope, id)
		if err != nil {
			t.Error(err)
		}
		cancelled <- run
	}()
	select {
	case <-cancelled:
		t.Fatal("the cancellation did not wait for the resume under way")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-resumed; err != nil {
		t.Fatal(err)
	}
	run := <-cancelled
	if run.Status != "cancelled" {
		t.Fatalf("run = %+v", run)
	}
	awaitIdle(t, f, id)
	// Every turn started for the resumed attempt was stopped by the cancellation.
	f.exec.mu.Lock()
	defer f.exec.mu.Unlock()
	for _, r := range f.exec.started {
		if r.Caller.NodeID != "a" || r.Caller.AttemptID == failedA {
			continue
		}
		session := ""
		for id, s := range f.exec.sessions {
			if s.caller.AttemptID == r.Caller.AttemptID {
				session = id
			}
		}
		if !slices.ContainsFunc(f.exec.cancels, func(c execution.Ref) bool { return c.SessionID == session }) {
			t.Fatalf("the resumed attempt %s kept executing after the cancellation", r.Caller.AttemptID)
		}
	}
	_, rows := f.await(t, id, terminal)
	if a := node(rows, "a"); a.ID == failedA || a.Status != "cancelled" {
		t.Fatalf("resumed a = %+v", a)
	}
}

// L4 / X1: a resume attempted while a cancellation holds the run's lifecycle lock is refused at
// once, without waiting for the cancellation.
func TestResumeDuringACancellationIsRefusedAtOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.run(t, siblings, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(expectAgent(t, f, "d"), completed("D"))
	f.exec.finish(expectAgent(t, f, "a"), sessionFailed)
	f.await(t, id, terminal)
	awaitIdle(t, f, id)
	unlock := f.host.lockLifecycle(id) // a cancellation under way
	done := make(chan error, 1)
	go func() { _, err := f.host.ResumeRun(ctx, f.scope, id); done <- err }()
	select {
	case err := <-done:
		unlock()
		if refusal(t, err) != workflow.ResumeRefusedSettling {
			t.Fatalf("resume during a cancellation = %v", err)
		}
	case <-time.After(2 * time.Second):
		unlock()
		<-done
		t.Fatal("the resume waited for the cancellation instead of refusing")
	}
}

// L4: lifecycle entries exist only while a cancel or resume holds or waits for them, and an
// unknown or foreign run id never creates one.
func TestLifecycleLocksAreReleasedAndNeverCreatedForUnknownRuns(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	entries := func() int {
		f.host.mu.Lock()
		defer f.host.mu.Unlock()
		return len(f.host.lifecycles)
	}
	unknown := "00000000-0000-4000-8000-000000000123"
	if _, err := f.host.CancelRun(ctx, f.scope, unknown); err == nil {
		t.Fatal("cancelled an unknown run")
	}
	if _, err := f.host.ResumeRun(ctx, f.scope, unknown); err == nil {
		t.Fatal("resumed an unknown run")
	}
	if n := entries(); n != 0 {
		t.Fatalf("unknown run ids left %d lifecycle entries", n)
	}
	id := f.run(t, siblings, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	expectAgent(t, f, "d")
	if _, err := f.host.CancelRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, f, id)
	if _, err := f.host.ResumeRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	if n := entries(); n != 0 {
		t.Fatalf("cancel and resume left %d lifecycle entries", n)
	}
}

// M1: the start slot is refused to a dispatch from a generation a resume has replaced, before
// any store read, and a released slot leaves nothing in flight.
func TestBeginStartRefusesADispatchFromAReplacedGeneration(t *testing.T) {
	st := &runState{generation: 2}
	if st.beginStart("", 1) {
		t.Fatal("a dispatch queued before the resume reserved the start slot")
	}
	if !st.beginStart("", 2) {
		t.Fatal("a current dispatch was refused the start slot")
	}
	st.releaseStart()
	if st.starting != nil || st.turnDone != nil || st.active != nil {
		t.Fatalf("released slot left state behind: %+v", st)
	}
}
