package workflowhost

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/boot"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
	"github.com/wanglongan587/cloud/v2-server/internal/taskdecomp"
	"github.com/wanglongan587/cloud/v2-server/internal/taskrun"
	"github.com/wanglongan587/cloud/v2-server/internal/taskworkflow"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// idleRuns stands in for main's adapters from task owners to the workflow host. Startup recovery
// only reads and cancels runs through the store, so nothing here is called during a boot.
type idleRuns struct{}

func (idleRuns) Start(context.Context, string, string, string) error { return nil }
func (idleRuns) Subscribe(string) (<-chan struct{}, func())          { return make(chan struct{}), func() {} }
func (idleRuns) Prepare(context.Context, string, string, string, *string, map[string]json.RawMessage) error {
	return nil
}
func (idleRuns) Cancel(context.Context, string, string, string) error { return nil }

// hubRuns lets a decomposition watcher follow runs through the booted host's hub, as main's
// adapter does.
type hubRuns struct {
	idleRuns
	host *Host
}

func (r hubRuns) Subscribe(runID string) (<-chan struct{}, func()) {
	return r.host.Hub().Subscribe(runID)
}

// bootProcess restarts the fixture as main does: the old host stops without reporting its turns,
// then a new host and the task owners recover through boot.Recover, the function main calls, so
// a change to the startup order there is what these tests see.
func bootProcess(t *testing.T, f *fixture) {
	t.Helper()
	f.host.Close()
	f.host = f.newHost(t)
	taskWorkflows := taskworkflow.New(f.s, idleRuns{})
	t.Cleanup(taskWorkflows.Close)
	decompositions := taskdecomp.New(f.s, hubRuns{idleRuns{}, f.host}, nil)
	t.Cleanup(decompositions.Close)
	taskRuns := taskrun.New(f.s, f.exec, f.exec, nil)
	t.Cleanup(taskRuns.Close)
	if err := boot.Recover(context.Background(), boot.Hosts{
		TaskWorkflows: taskWorkflows, Decompositions: decompositions, WorkflowRuns: f.host, TaskRuns: taskRuns,
	}); err != nil {
		t.Fatal(err)
	}
}

// startIterationAndAwaitMember starts the run and waits until round 0's Agent is executing with
// its session bound: the shape the workflow sweep would absorb and resume.
func startIterationAndAwaitMember(t *testing.T, f *fixture, id string) {
	t.Helper()
	if _, err := f.host.StartRun(context.Background(), f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitStart(t)
	f.await(t, id, func(_ store.WorkflowRun, rows []store.WorkflowNodeRun) bool {
		return iterationRow(rows, "worker", 0).SessionID != nil
	})
}

// assertCancelledBeforeTheSweep checks that the task owner cancelled the run before the workflow
// sweep: the executing member is cancelled, not failed as interrupted_by_restart (the sweep's
// mark, which a reordered boot would leave), and no later round started.
func assertCancelledBeforeTheSweep(t *testing.T, f *fixture, id string) {
	t.Helper()
	run, err := f.s.WorkflowRun(context.Background(), f.scope.TenantID, id)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := f.s.WorkflowRunNodes(context.Background(), f.scope.TenantID, id)
	if err != nil {
		t.Fatal(err)
	}
	member := iterationRow(rows, "worker", 0)
	if run.Status != store.WorkflowStatusCancelled || node(rows, "iter").Status != store.WorkflowStatusCancelled ||
		member.Status != store.WorkflowStatusCancelled {
		t.Fatalf("run = %s, iter = %s, member = %s (%s); want all cancelled before the workflow sweep",
			run.Status, node(rows, "iter").Status, member.Status, deref(member.Error))
	}
	if next := iterationRow(rows, "worker", 1); next.ID != "" || starts(f) != 1 {
		t.Fatalf("the run carried on after the restart: round 1 = %+v, starts = %d", next, starts(f))
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Decompositions do not cross processes. A decomposition whose run holds an absorbable
// Iteration fails at boot, and its run is cancelled before the workflow sweep could resume it,
// so the run cannot carry on and succeed after its decomposition failed.
func TestBootFailsADecompositionAndCancelsItsAbsorbableRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.run(t, iterationGraph("continue", []string{"one", "two"}, 50), nil)
	task, err := f.s.CreateTask(ctx, f.scope.TenantID, f.scope.UserID, store.TaskInput{Title: "拆分", ProjectID: f.project})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.s.WorkflowRun(ctx, f.scope.TenantID, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.CreateTaskDecomposition(ctx, store.NewTaskDecomposition{
		TaskID: task.ID, TenantID: f.scope.TenantID, StartedBy: f.scope.UserID, StartKey: "k",
		WorkflowID: run.WorkflowID, WorkflowName: run.Name, WorkflowRunID: id,
	}); err != nil {
		t.Fatal(err)
	}
	startIterationAndAwaitMember(t, f, id)

	bootProcess(t, f)

	ds, err := f.s.TaskDecompositions(ctx, f.scope.TenantID, task.ID)
	if err != nil || len(ds) != 1 {
		t.Fatalf("decompositions = %+v %v", ds, err)
	}
	if ds[0].Status != store.DecompositionFailed || !strings.Contains(ds[0].Failure, "服务重启") {
		t.Fatalf("decomposition = %s %q, want failed by the restart", ds[0].Status, ds[0].Failure)
	}
	assertCancelledBeforeTheSweep(t, f, id)
}

// The contrast with #1036, pinned through the same boot: a task's handling run holding an absorbable
// Iteration is cancelled before the sweep, the task returns to pending and the link reads
// interrupted.
func TestBootInterruptsAHandlingRunBeforeTheSweep(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	w, _, err := f.s.CreateWorkflow(ctx, f.scope.TenantID, "处理", iterationGraph("continue", []string{"one", "two"}, 50))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PublishWorkflow(ctx, f.scope.TenantID, w.ID, "v1", nil); err != nil {
		t.Fatal(err)
	}
	published, err := f.s.PublishedWorkflowSnapshot(ctx, f.scope.TenantID, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	task, err := f.s.CreateTask(ctx, f.scope.TenantID, f.scope.UserID, store.TaskInput{
		Title: "处理", ProjectID: f.project, Assignee: &store.TaskAssigneeRef{Type: "human", ID: f.scope.UserID},
	})
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := f.s.CreateTaskWorkflowRun(ctx, store.NewTaskWorkflow{
		TenantID: f.scope.TenantID, TaskID: task.ID, UserID: f.scope.UserID, Key: "k", WorkflowID: w.ID, SnapshotID: published.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	startIterationAndAwaitMember(t, f, id)

	bootProcess(t, f)

	got, err := f.s.Task(ctx, f.scope.TenantID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	links, err := f.s.TaskWorkflowRuns(ctx, f.scope.TenantID, task.ID)
	if err != nil || len(links) != 1 {
		t.Fatalf("links = %+v %v", links, err)
	}
	if got.Status != store.TaskPending || links[0].LinkStatus != "interrupted" {
		t.Fatalf("task = %s, link = %s, want pending and interrupted", got.Status, links[0].LinkStatus)
	}
	assertCancelledBeforeTheSweep(t, f, id)
}

// A root Agent failing the run leaves the Iteration owner beside it running. The boot sweep
// must not absorb a run that already ended, since no runtime would settle the round: the owner
// fails on the first boot and the second boot finds nothing to interrupt.
func TestBootFailsALeftoverIterationOwnerOfAFailedRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	graph := iterationGraph("continue", []string{"one"}, 50)
	graph = strings.Replace(graph, `{"id":"iter"`, `{"id":"outer","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"outer",`+containerRetryDisabled+`}}},
	 {"id":"iter"`, 1)
	graph = strings.Replace(graph, `"edges":[`, `"edges":[{"source":"start","target":"outer"},`, 1)
	id := f.run(t, graph, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	// The outer Agent and the Iteration member execute concurrently.
	outer := f.exec.awaitStarts(t, 2)["outer"]
	f.await(t, id, func(_ store.WorkflowRun, rows []store.WorkflowNodeRun) bool {
		return iterationRow(rows, "worker", 0).ID != ""
	})
	f.exec.finish(outer, execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	run, rows := f.await(t, id, terminal)
	if run.Status != store.WorkflowStatusFailed || node(rows, "iter").Status != store.WorkflowStatusRunning {
		t.Fatalf("before restart: run = %s, iter = %s; want a failed run with its Iteration owner running", run.Status, node(rows, "iter").Status)
	}
	for boot := 1; boot <= 2; boot++ {
		restartHost(t, f)
		run, rows = f.await(t, id, terminal)
		if run.Status != store.WorkflowStatusFailed || node(rows, "iter").Status != store.WorkflowStatusFailed {
			t.Fatalf("boot %d: run = %s, iter = %s; want the leftover owner failed", boot, run.Status, node(rows, "iter").Status)
		}
	}
	// The second sweep had nothing left: the run was not reported or absorbed again.
	again, err := f.s.InterruptWorkflowRunsWithRecovery(ctx, interruptedMessage,
		func(int, []byte) ([]byte, error) { return []byte(`{}`), nil }, workflowInterrupts)
	if err != nil || len(again) != 0 {
		t.Fatalf("a later sweep found %v %v", again, err)
	}
}

// A decomposition run whose review form was submitted before the restart is the exception to
// "decompositions do not cross processes": it is not cancelled before the sweep, the host's review
// recovery replays its deterministic task creation, and the decomposition then settles from the
// run's real output rather than failing as interrupted.
func TestBootRecoversADecompositionWithASubmittedReview(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	report := "Inspection report"
	id := f.run(t, formGraph, &report)
	parent, err := f.s.CreateTask(ctx, f.scope.TenantID, f.scope.UserID, store.TaskInput{Title: "表单拆分", ProjectID: f.project})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.s.WorkflowRun(ctx, f.scope.TenantID, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.CreateTaskDecomposition(ctx, store.NewTaskDecomposition{
		TaskID: parent.ID, TenantID: f.scope.TenantID, StartedBy: f.scope.UserID, StartKey: "k",
		WorkflowID: run.WorkflowID, WorkflowName: run.Name, WorkflowRunID: id,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.await(t, id, func(_ store.WorkflowRun, nodes []store.WorkflowNodeRun) bool {
		return node(nodes, "review").Status == "pending"
	})
	a := storetest.Access(t, f.s, f.scope.TenantID, f.scope.UserID)
	form, err := f.host.ReadForm(ctx, a, id)
	if err != nil {
		t.Fatal(err)
	}
	values := []any{map[string]any{"key": "disk:root", "decision": "confirmed", "assignee": map[string]string{"type": "human", "id": f.scope.UserID}, "values": map[string]string{}}}
	answers, _ := json.Marshal(values)
	// Only the store half of SaveForm: the submission is durable, the advance never happened.
	if _, err := f.s.SaveWorkflowForm(ctx, a, id, form.Version, "submit", answers, true, func(store.WorkflowForm) error { return nil },
		func(raw []byte, fm store.WorkflowForm) ([]byte, error) {
			p, _, err := workflow.DecodeRunPayload(raw)
			if err != nil {
				return nil, err
			}
			text := string(answers)
			if err := p.Complete(fm.NodeID, "form", nil, &text, map[string]any{"answers": values}); err != nil {
				return nil, err
			}
			return p.Encode()
		}); err != nil {
		t.Fatal(err)
	}
	before, err := f.s.Tasks(ctx, f.scope.TenantID)
	if err != nil {
		t.Fatal(err)
	}

	bootProcess(t, f)

	// The review recovery resumes the run; its deterministic tail finishes after the boot.
	final, _ := f.await(t, id, terminal)
	if final.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("run = %s (%s), want the submitted review recovered to success", final.Status, deref(final.Error))
	}
	after, err := f.s.Tasks(ctx, f.scope.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("tasks = %d, want %d: the confirmed finding's task was not created", len(after), len(before)+1)
	}
	// Adopt's watcher settles the decomposition once the run's end is published.
	ds := awaitDecomposition(t, f, parent.ID)
	// Settled from the run's output, exactly as a watcher would have, not as interrupted.
	subtasks, failure := taskdecomp.Subtasks(final.Output)
	t.Logf("decomposition settled %s %q from output %s", ds[0].Status, ds[0].Failure, deref(final.Output))
	wantStatus := store.DecompositionFailed
	if failure == "" {
		wantStatus = store.DecompositionSucceeded
	}
	if ds[0].Status != wantStatus || ds[0].Failure != failure || ds[0].CreatedCount != len(subtasks) || strings.Contains(ds[0].Failure, "服务重启") {
		t.Fatalf("decomposition = %s %q (%d created), want %s %q (%d) from the run's output %s",
			ds[0].Status, ds[0].Failure, ds[0].CreatedCount, wantStatus, failure, len(subtasks), deref(final.Output))
	}
}

// awaitDecomposition waits until the task's only decomposition is no longer running.
func awaitDecomposition(t *testing.T, f *fixture, taskID string) []store.TaskDecomposition {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		ds, err := f.s.TaskDecompositions(context.Background(), f.scope.TenantID, taskID)
		if err != nil || len(ds) != 1 {
			t.Fatalf("decompositions = %+v %v", ds, err)
		}
		if ds[0].Status != store.DecompositionRunning {
			return ds
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-timeout:
			t.Fatalf("decomposition %s still running after its run ended", ds[0].ID)
		}
	}
}
