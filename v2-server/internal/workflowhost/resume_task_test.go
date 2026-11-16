package workflowhost

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/taskworkflow"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// taskRuns is the task workflow host's run port over the real workflow host.
type taskRuns struct{ h *Host }

func (p taskRuns) Prepare(ctx context.Context, tenant, user, id string, input *string, values map[string]json.RawMessage) error {
	_, err := p.h.UpdateRunInput(ctx, server.WorkflowRunScope{TenantID: tenant, UserID: user}, id, input, values)
	return err
}
func (p taskRuns) Start(ctx context.Context, tenant, user, id string) error {
	_, err := p.h.StartRun(ctx, server.WorkflowRunScope{TenantID: tenant, UserID: user}, id)
	return err
}
func (p taskRuns) Cancel(ctx context.Context, tenant, user, id string) error {
	_, err := p.h.CancelRun(ctx, server.WorkflowRunScope{TenantID: tenant, UserID: user}, id)
	return err
}
func (p taskRuns) Subscribe(id string) (<-chan struct{}, func()) { return p.h.Hub().Subscribe(id) }

// handlingLink returns the task and its link for run id, as the task's history lists it.
func handlingLink(t *testing.T, f *fixture, tw *taskworkflow.Host, taskID, id string) (store.Task, store.TaskWorkflowRun) {
	t.Helper()
	items, err := tw.List(context.Background(), f.scope.TenantID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	task, err := f.s.Task(context.Background(), f.scope.TenantID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.WorkflowRunID == id {
			return task, it
		}
	}
	t.Fatalf("run %s is not in the task's history: %+v", id, items)
	return task, store.TaskWorkflowRun{}
}

// H1 (owner decision D25): a run a task started as its handling run cannot resume, failed or
// cancelled; the refusal comes from the resume transaction, changes neither the run nor its task
// link, starts nothing and leaves no process state behind (X7).
func TestResumeRefusesATaskHandlingRun(t *testing.T) {
	for _, mode := range []string{"failed", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			tw := taskworkflow.New(f.s, taskRuns{f.host})
			t.Cleanup(tw.Close)
			task, err := f.s.CreateTask(ctx, f.scope.TenantID, f.scope.UserID, store.TaskInput{Title: "T", ProjectID: f.project, Assignee: &store.TaskAssigneeRef{Type: "human", ID: f.scope.UserID}})
			if err != nil {
				t.Fatal(err)
			}
			w, _, err := f.s.CreateWorkflow(ctx, f.scope.TenantID, "handling-"+mode, twoAgents)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.PublishWorkflow(ctx, f.scope.TenantID, w.ID, "v1", nil); err != nil {
				t.Fatal(err)
			}
			id, err := tw.Start(ctx, store.NewTaskWorkflow{TenantID: f.scope.TenantID, TaskID: task.ID, UserID: f.scope.UserID, Key: "k", WorkflowID: w.ID, Input: str("x")}, nil)
			if err != nil {
				t.Fatal(err)
			}
			f.exec.finish(expectAgent(t, f, "a"), completed("A"))
			b := expectAgent(t, f, "b")
			if mode == "failed" {
				_, rows := f.await(t, id, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool { return node(n, "b").SessionID != nil })
				if err := f.host.engine.FailNode(ctx, id, node(rows, "b").ID, workflow.Failure{Kind: workflow.FailureMissingAgentRef, Message: "x"}); err != nil {
					t.Fatal(err)
				}
				f.exec.finish(b, sessionFailed)
			} else if _, err := f.host.CancelRun(ctx, f.scope, id); err != nil {
				t.Fatal(err)
			}
			run, _ := f.await(t, id, terminal)
			awaitIdle(t, f, id)
			taskBefore, linkBefore := handlingLink(t, f, tw, task.ID, id)
			if owner, kind, err := f.s.WorkflowRunTaskLink(ctx, f.scope.TenantID, id); err != nil || owner != task.ID || kind != store.TaskLinkHandle {
				t.Fatalf("task link = %q %q %v", owner, kind, err)
			}

			preview, err := f.host.PreviewRunResume(ctx, f.scope, id)
			if err != nil || preview.Reason != workflow.ResumeRefusedTaskHandling {
				t.Fatalf("preview = %+v %v", preview, err)
			}
			if _, err := f.host.ResumeRun(ctx, f.scope, id); refusal(t, err) != workflow.ResumeRefusedTaskHandling {
				t.Fatalf("resume = %v", err)
			}
			if f.host.state(id) != nil {
				t.Fatal("a refused resume left the run's process state behind")
			}
			after, err := f.s.WorkflowRun(ctx, f.scope.TenantID, id)
			if err != nil || after.Status != run.Status {
				t.Fatalf("a refused resume changed the run: %+v %v", after, err)
			}
			taskAfter, linkAfter := handlingLink(t, f, tw, task.ID, id)
			if taskAfter.Status != taskBefore.Status || linkAfter.LinkStatus != linkBefore.LinkStatus || linkAfter.Status != mode {
				t.Fatalf("task %s→%s, link %s→%s, history %s", taskBefore.Status, taskAfter.Status, linkBefore.LinkStatus, linkAfter.LinkStatus, linkAfter.Status)
			}
			if started := startedAttempts(f); len(started) != 2 {
				t.Fatalf("a refused resume started an Agent: %v", started)
			}
		})
	}
}

// D32 (owner decision, 2026-11-16; merge review M1): a task decomposition settles once, when its
// run ends, so its run cannot resume, failed or cancelled. The refusal comes from the resume
// transaction and leaves the run, the decomposition and the task unchanged and starts nothing;
// a restart afterwards (the boot sweep, which would absorb a detached running run) does not
// revive it either. This is the reviewer's TestMergeProbeResumedDecompositionAcrossBoot inverted.
func TestResumeRefusesATaskDecompositionRun(t *testing.T) {
	for _, mode := range []string{"failed", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			task, err := f.s.CreateTask(ctx, f.scope.TenantID, f.scope.UserID, store.TaskInput{Title: "拆分", ProjectID: f.project})
			if err != nil {
				t.Fatal(err)
			}
			strategy := "continue"
			if mode == "failed" {
				strategy = "fail" // the member's failure fails the Iteration and the run
			}
			id := f.run(t, iterationGraph(strategy, []string{"one", "two"}, 50), nil)
			run, err := f.s.WorkflowRun(ctx, f.scope.TenantID, id)
			if err != nil {
				t.Fatal(err)
			}
			d, err := f.s.CreateTaskDecomposition(ctx, store.NewTaskDecomposition{TaskID: task.ID, TenantID: f.scope.TenantID, StartedBy: f.scope.UserID, StartKey: "split", WorkflowID: run.WorkflowID, WorkflowName: "拆分", WorkflowRunID: id})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
				t.Fatal(err)
			}
			member := f.exec.awaitStart(t)
			failure := "工作流运行已取消。"
			if mode == "failed" {
				f.exec.finish(member, sessionFailed)
				failure = "工作流运行失败。"
			} else if _, err := f.host.CancelRun(ctx, f.scope, id); err != nil {
				t.Fatal(err)
			}
			ended, _ := f.await(t, id, terminal)
			awaitIdle(t, f, id)
			// As the decomposition's watcher settles it from the ended run.
			if _, _, err := f.s.SettleTaskDecomposition(ctx, d.ID, nil, failure); err != nil {
				t.Fatal(err)
			}
			taskBefore, err := f.s.Task(ctx, f.scope.TenantID, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if owner, kind, err := f.s.WorkflowRunTaskLink(ctx, f.scope.TenantID, id); err != nil || owner != task.ID || kind != store.TaskLinkDecomposition {
				t.Fatalf("task link = %q %q %v", owner, kind, err)
			}
			before := starts(f)

			preview, err := f.host.PreviewRunResume(ctx, f.scope, id)
			if err != nil || preview.Reason != workflow.ResumeRefusedTaskDecomposition {
				t.Fatalf("preview = %+v %v", preview, err)
			}
			if _, err := f.host.ResumeRun(ctx, f.scope, id); refusal(t, err) != workflow.ResumeRefusedTaskDecomposition {
				t.Fatalf("resume = %v", err)
			}
			if f.host.state(id) != nil {
				t.Fatal("a refused resume left the run's process state behind")
			}
			bootProcess(t, f)
			after, err := f.s.WorkflowRun(ctx, f.scope.TenantID, id)
			if err != nil || after.Status != ended.Status {
				t.Fatalf("run %s after a refused resume and a restart: %+v %v", ended.Status, after, err)
			}
			if n := starts(f); n != before {
				t.Fatalf("a refused resume started %d Agents", n-before)
			}
			items, err := f.s.TaskDecompositions(ctx, f.scope.TenantID, task.ID)
			if err != nil || len(items) != 1 || items[0].Status != store.DecompositionFailed || items[0].Failure != failure {
				t.Fatalf("decomposition after the refusal = %+v %v", items, err)
			}
			if taskAfter, err := f.s.Task(ctx, f.scope.TenantID, task.ID); err != nil || taskAfter.Status != taskBefore.Status || taskAfter.Version != taskBefore.Version {
				t.Fatalf("task %+v → %+v %v", taskBefore, taskAfter, err)
			}
		})
	}
}

// D25 and D32 cover task-owned runs only: an unlinked run (as a patrol or inspection source run
// is, having no handling or decomposition link) stays resumable; the resume advances the run's
// process generation (X15), which retires dispatches queued before it.
func TestResumeKeepsAnUnlinkedRunResumable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.run(t, twoAgents, str("x"))
	if _, kind, err := f.s.WorkflowRunTaskLink(ctx, f.scope.TenantID, id); err != nil || kind != "" {
		t.Fatalf("link = %q %v", kind, err)
	}
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	expectAgent(t, f, "a")
	if _, err := f.host.CancelRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, f, id)
	if preview, err := f.host.PreviewRunResume(ctx, f.scope, id); err != nil || preview.Reason != "" {
		t.Fatalf("preview = %+v %v", preview, err)
	}
	if _, err := f.host.ResumeRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	expectAgent(t, f, "a")
	st := f.host.state(id)
	if st == nil {
		t.Fatal("the resumed run has no process state")
	}
	st.mu.Lock()
	generation := st.generation
	st.mu.Unlock()
	if generation == 0 {
		t.Fatal("the resume did not advance the run's generation")
	}
}
