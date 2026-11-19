package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
	"github.com/wanglongan587/cloud/v2-server/internal/taskworkflow"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// failFormTasks drives run id, already started, to a failed run whose review form is submitted
// and whose createTasks failed: the confirmed finding's assignee is retired after the submission,
// and a host restart's recovery fails the task creation. These are exactly the form retry's
// preconditions.
func failFormTasks(t *testing.T, f *fixture, id string) {
	t.Helper()
	ctx := context.Background()
	const email = "form-retry-retired@demo.test"
	submitToAssignee(t, f, id, email)
	if _, err := f.s.StartWorkflowEngineNodes(ctx, id, []store.WorkflowEngineStart{{NodeID: "tasks", NodeType: "createTasks"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.RetireUsers(ctx, []store.RetiredUser{{Email: email}}); err != nil {
		t.Fatal(err)
	}
	f.host.Close()
	f.host = f.newHost(t)
	if err := f.host.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	run, rows := f.await(t, id, terminal)
	if run.Status != store.WorkflowStatusFailed || node(rows, "tasks").Status != store.WorkflowStatusFailed {
		t.Fatalf("setup: run %s, createTasks %s", run.Status, node(rows, "tasks").Status)
	}
}

// submitToAssignee waits for run id's review form and submits it at the store level with one
// confirmed finding assigned to a new member with the given email, as host.SaveForm commits a
// final submission before it continues the run.
func submitToAssignee(t *testing.T, f *fixture, id, email string) {
	t.Helper()
	ctx := context.Background()
	f.await(t, id, func(_ store.WorkflowRun, nodes []store.WorkflowNodeRun) bool {
		return node(nodes, "review").Status == "pending"
	})
	access := storetest.Access(t, f.s, f.scope.TenantID, f.scope.UserID)
	form, err := f.host.ReadForm(ctx, access, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.UpsertUser(ctx, store.PresetUser{Email: email, DisplayName: "Gone", Role: store.RoleMember}); err != nil {
		t.Fatal(err)
	}
	assignee, err := f.s.UserByEmail(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	values := []any{map[string]any{"key": "disk:root", "decision": "confirmed", "assignee": map[string]string{"type": "human", "id": assignee.ID}, "values": map[string]string{}}}
	answers, _ := json.Marshal(values)
	if _, err := f.s.SaveWorkflowForm(ctx, access, id, form.Version, "retry", answers, true, func(store.WorkflowForm) error { return nil }, func(raw []byte, form store.WorkflowForm) ([]byte, error) {
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
}

// M1 (owner decision D30): the review form's 「重试任务创建」 is the other path that returns a
// failed run to running, so a task's handling run refuses it as resume does (D25). The refusal
// comes from the retry transaction and leaves the run failed, its createTasks failed, the task and
// its settled link untouched, and nothing reopened that a second handling admission could run
// beside; handling again from the task stays the way forward.
func TestFormRetryRefusesATaskHandlingRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tw := taskworkflow.New(f.s, taskRuns{f.host})
	t.Cleanup(tw.Close)
	task, err := f.s.CreateTask(ctx, f.scope.TenantID, f.scope.UserID, store.TaskInput{Title: "T", ProjectID: f.project, Assignee: &store.TaskAssigneeRef{Type: "human", ID: f.scope.UserID}})
	if err != nil {
		t.Fatal(err)
	}
	w, _, err := f.s.CreateWorkflow(ctx, f.scope.TenantID, "handling-form", formGraph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PublishWorkflow(ctx, f.scope.TenantID, w.ID, "v1", nil); err != nil {
		t.Fatal(err)
	}
	id, err := tw.Start(ctx, store.NewTaskWorkflow{TenantID: f.scope.TenantID, TaskID: task.ID, UserID: f.scope.UserID, Key: "k", WorkflowID: w.ID, Input: str("Evidence")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	failFormTasks(t, f, id)
	// The recorded failure points back to the task rather than to the retry it refuses.
	if _, rows := f.await(t, id, terminal); node(rows, "tasks").Error == nil || *node(rows, "tasks").Error != formTasksFailedHandling {
		t.Fatalf("handling run's task creation failure = %v, want %q", node(rows, "tasks").Error, formTasksFailedHandling)
	}
	if link, err := f.s.WorkflowRunTaskLinkKind(ctx, id); err != nil || link != store.TaskLinkHandle {
		t.Fatalf("task link = %q %v", link, err)
	}
	taskBefore, linkBefore := handlingLink(t, f, tw, task.ID, id)
	access := storetest.Access(t, f.s, f.scope.TenantID, f.scope.UserID)

	for range 2 {
		if _, err := f.host.RetryForm(ctx, access, id); !errors.Is(err, store.ErrTaskHandlingRun) {
			t.Fatalf("form retry of a handling run = %v, want ErrTaskHandlingRun", err)
		}
	}
	if err := f.s.RetryWorkflowFormTasks(ctx, access, id); !errors.Is(err, store.ErrTaskHandlingRun) {
		t.Fatalf("store form retry of a handling run = %v", err)
	}
	run, rows := f.await(t, id, terminal)
	if run.Status != store.WorkflowStatusFailed || node(rows, "tasks").Status != store.WorkflowStatusFailed {
		t.Fatalf("a refused form retry reopened the run: run %s, createTasks %s", run.Status, node(rows, "tasks").Status)
	}
	if f.host.state(id) != nil {
		t.Fatal("a refused form retry left the run's process state behind")
	}
	taskAfter, linkAfter := handlingLink(t, f, tw, task.ID, id)
	if taskAfter.Status != taskBefore.Status || linkAfter.LinkStatus != linkBefore.LinkStatus || linkAfter.Status != store.WorkflowStatusFailed {
		t.Fatalf("task %s→%s, link %s→%s, history %s", taskBefore.Status, taskAfter.Status, linkBefore.LinkStatus, linkAfter.LinkStatus, linkAfter.Status)
	}
	if tasks, err := f.s.Tasks(ctx, f.scope.TenantID); err != nil || len(tasks) != 1 {
		t.Fatalf("a refused form retry created tasks: %v %v", tasks, err)
	}
	// Handling again from the task is the way forward, and the refused run is not running beside it.
	second, _, err := f.s.CreateTaskWorkflowRun(ctx, store.NewTaskWorkflow{TenantID: f.scope.TenantID, TaskID: task.ID, UserID: f.scope.UserID, Key: "k2", WorkflowID: w.ID, Input: str("Evidence")})
	if err != nil || second == id {
		t.Fatalf("new handling admission = %q %v", second, err)
	}
	if old, err := f.s.WorkflowRun(ctx, f.scope.TenantID, id); err != nil || old.Status != store.WorkflowStatusFailed {
		t.Fatalf("the refused run after a new admission: %+v %v", old, err)
	}
}

// D32: a task decomposition's run settles its decomposition when it fails, so its form retry is
// refused as resume is: the run stays failed with its createTasks failed, the decomposition and
// the task keep their state, nothing is created, and the recorded failure sends the user back to
// the task to decompose again.
func TestFormRetryRefusesATaskDecompositionRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	task, err := f.s.CreateTask(ctx, f.scope.TenantID, f.scope.UserID, store.TaskInput{Title: "拆分", ProjectID: f.project})
	if err != nil {
		t.Fatal(err)
	}
	id := f.run(t, formGraph, str("Evidence"))
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
	failFormTasks(t, f, id)
	_, before := f.await(t, id, terminal)
	if e := node(before, "tasks").Error; e == nil || *e != formTasksFailedDecomposition {
		t.Fatalf("decomposition run's task creation failure = %v, want %q", e, formTasksFailedDecomposition)
	}
	if link, err := f.s.WorkflowRunTaskLinkKind(ctx, id); err != nil || link != store.TaskLinkDecomposition {
		t.Fatalf("task link = %q %v", link, err)
	}
	// As the decomposition's watcher settles it from the failed run.
	if _, _, err := f.s.SettleTaskDecomposition(ctx, d.ID, nil, "工作流运行失败。"); err != nil {
		t.Fatal(err)
	}
	taskBefore, err := f.s.Task(ctx, f.scope.TenantID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	access := storetest.Access(t, f.s, f.scope.TenantID, f.scope.UserID)

	if _, err := f.host.RetryForm(ctx, access, id); !errors.Is(err, store.ErrTaskDecompositionRun) {
		t.Fatalf("form retry of a decomposition run = %v, want ErrTaskDecompositionRun", err)
	}
	after, rows := f.await(t, id, terminal)
	if after.Status != store.WorkflowStatusFailed || node(rows, "tasks").Status != store.WorkflowStatusFailed || node(rows, "tasks").ID != node(before, "tasks").ID {
		t.Fatalf("a refused form retry reopened the run: run %s, createTasks %+v", after.Status, node(rows, "tasks"))
	}
	items, err := f.s.TaskDecompositions(ctx, f.scope.TenantID, task.ID)
	if err != nil || len(items) != 1 || items[0].Status != store.DecompositionFailed || items[0].Failure != "工作流运行失败。" {
		t.Fatalf("decomposition after the refusal = %+v %v", items, err)
	}
	if taskAfter, err := f.s.Task(ctx, f.scope.TenantID, task.ID); err != nil || taskAfter.Status != taskBefore.Status || taskAfter.Version != taskBefore.Version {
		t.Fatalf("task %+v → %+v %v", taskBefore, taskAfter, err)
	}
	if tasks, err := f.s.Tasks(ctx, f.scope.TenantID); err != nil || len(tasks) != 1 {
		t.Fatalf("a refused form retry created tasks: %v %v", tasks, err)
	}
}

// D30 and D32 cover task-owned runs only: an unlinked run's form retry still reopens the run and
// replays task creation (which fails again here only because its assignee stays retired).
func TestFormRetryKeepsAnUnlinkedRunRetryable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.run(t, formGraph, str("Evidence"))
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	failFormTasks(t, f, id)
	_, before := f.await(t, id, terminal)
	if e := node(before, "tasks").Error; e == nil || *e != formTasksFailed {
		t.Fatalf("unlinked run's task creation failure = %v, want the retry text", e)
	}
	if link, err := f.s.WorkflowRunTaskLinkKind(ctx, id); err != nil || link != "" {
		t.Fatalf("task link = %q %v", link, err)
	}
	access := storetest.Access(t, f.s, f.scope.TenantID, f.scope.UserID)

	if err := f.s.RetryWorkflowFormTasks(ctx, access, id); err != nil {
		t.Fatalf("store form retry of an unlinked run = %v", err)
	}
	reopened, rows := f.await(t, id, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return true })
	if reopened.Status != store.WorkflowStatusRunning || node(rows, "tasks").Status != store.WorkflowStatusRunning {
		t.Fatalf("form retry did not reopen the run: run %s, createTasks %s", reopened.Status, node(rows, "tasks").Status)
	}
	if err := f.host.resumeReviewRun(ctx, id); !errors.Is(err, store.ErrUserUnavailable) {
		t.Fatalf("replayed task creation = %v, want the retired assignee's refusal", err)
	}
	if _, err := f.host.RetryForm(ctx, access, id); !errors.Is(err, store.ErrUserUnavailable) {
		t.Fatalf("host form retry of an unlinked run = %v, want it replayed", err)
	}
	after, rows := f.await(t, id, terminal)
	if after.Status != store.WorkflowStatusFailed || node(rows, "tasks").ID != node(before, "tasks").ID {
		t.Fatalf("after the replays: run %s, createTasks %+v", after.Status, node(rows, "tasks"))
	}
}

// A form retry reopens the run under its recorded starter, as a resume does. When that starter
// left the workspace (removed, or the account disabled), the retry is refused as
// ErrStarterUnavailable before anything changes: the run stays failed, its createTasks row and
// started_by stay as they were. Once the starter is back, the same run retries.
func TestFormRetryRefusedWhenTheStarterLeft(t *testing.T) {
	for _, exit := range starterExits {
		t.Run(exit.name, func(t *testing.T) {
			f, admin := newFixtureWithAdmin(t)
			ctx := context.Background()
			starter := signInMember(t, f, "form-retry-left-starter-"+exit.name, "发起人")
			id := f.run(t, formGraph, str("Evidence"))
			if _, err := f.host.StartRun(ctx, server.WorkflowRunScope{TenantID: f.scope.TenantID, UserID: starter}, id); err != nil {
				t.Fatal(err)
			}
			failFormTasks(t, f, id)
			before, rowsBefore := f.await(t, id, terminal)
			if before.StartedBy != starter {
				t.Fatalf("setup: started_by = %q, want %s", before.StartedBy, starter)
			}

			exit.leave(t, f, admin, starter)

			access := storetest.Access(t, f.s, f.scope.TenantID, f.scope.UserID)
			if _, err := f.host.RetryForm(ctx, access, id); !errors.Is(err, store.ErrStarterUnavailable) {
				t.Fatalf("form retry after the starter left = %v, want ErrStarterUnavailable", err)
			}
			after, rows := f.await(t, id, terminal)
			tasks := node(rows, "tasks")
			if after.Status != store.WorkflowStatusFailed || after.StartedBy != starter || tasks.Status != store.WorkflowStatusFailed || tasks.ID != node(rowsBefore, "tasks").ID {
				t.Fatalf("a refused form retry changed the run: run %s started by %s, createTasks %+v", after.Status, after.StartedBy, tasks)
			}

			switch exit.name {
			case "removed":
				if _, err := admin.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'member')`, f.scope.TenantID, starter); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if _, err := admin.Exec(ctx, `UPDATE users SET status = 'active' WHERE id = $1`, starter); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.s.RetryWorkflowFormTasks(ctx, access, id); err != nil {
				t.Fatalf("form retry once the starter is back = %v", err)
			}
			reopened, rows := f.await(t, id, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return true })
			if reopened.Status != store.WorkflowStatusRunning || node(rows, "tasks").Status != store.WorkflowStatusRunning {
				t.Fatalf("form retry did not reopen the run: run %s, createTasks %s", reopened.Status, node(rows, "tasks").Status)
			}
			// The replay fails again only because the confirmed finding's assignee stays retired.
			if err := f.host.resumeReviewRun(ctx, id); !errors.Is(err, store.ErrUserUnavailable) {
				t.Fatalf("replayed task creation = %v, want the retired assignee's refusal", err)
			}
		})
	}
}
