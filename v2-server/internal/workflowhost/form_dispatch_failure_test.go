package workflowhost

import (
	"context"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/taskworkflow"
)

// L1 (review 4): a run's first task creation fails in the engine's dispatch, not in the replay,
// so the dispatch must record the handling text too. The confirmed finding's assignee is a real
// member retired after the submission and before the dispatch; no restart is involved. A run no
// task handles keeps the dispatch's own text, and the handling text is also what the task page's
// workflow history shows as the run's failure.
func TestFirstTaskCreationFailureTextFollowsTheTaskLink(t *testing.T) {
	for _, handling := range []bool{true, false} {
		name := "unowned"
		if handling {
			name = "handling"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			tw := taskworkflow.New(f.s, taskRuns{f.host})
			t.Cleanup(tw.Close)
			var id, taskID string
			if handling {
				task, err := f.s.CreateTask(ctx, f.scope.TenantID, f.scope.UserID, store.TaskInput{Title: "T", ProjectID: f.project, Assignee: &store.TaskAssigneeRef{Type: "human", ID: f.scope.UserID}})
				if err != nil {
					t.Fatal(err)
				}
				taskID = task.ID
				w, _, err := f.s.CreateWorkflow(ctx, f.scope.TenantID, "handling-dispatch", formGraph)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.s.PublishWorkflow(ctx, f.scope.TenantID, w.ID, "v1", nil); err != nil {
					t.Fatal(err)
				}
				if id, err = tw.Start(ctx, store.NewTaskWorkflow{TenantID: f.scope.TenantID, TaskID: task.ID, UserID: f.scope.UserID, Key: "k", WorkflowID: w.ID, Input: str("Evidence")}, nil); err != nil {
					t.Fatal(err)
				}
			} else {
				id = f.run(t, formGraph, str("Evidence"))
				if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
					t.Fatal(err)
				}
			}
			const email = "dispatch-retired@demo.test"
			submitToAssignee(t, f, id, email)
			if err := f.s.RetireUsers(ctx, []store.RetiredUser{{Email: email}}); err != nil {
				t.Fatal(err)
			}
			// What host.SaveForm runs after a final submission: the engine dispatches createTasks.
			if err := f.host.resumeReviewRun(ctx, id); err != nil {
				t.Fatal(err)
			}
			run, rows := f.await(t, id, terminal)
			want := formFailed
			if handling {
				want = formTasksFailedHandling
			}
			if got := node(rows, "tasks"); run.Status != store.WorkflowStatusFailed || got.Status != store.WorkflowStatusFailed || got.Error == nil || *got.Error != want {
				t.Fatalf("run %s, createTasks %s with %v, want the failure %q", run.Status, got.Status, got.Error, want)
			}
			if handling {
				if _, link := handlingLink(t, f, tw, taskID, id); link.Failure != want {
					t.Fatalf("task history failure = %q, want %q", link.Failure, want)
				}
			}
		})
	}
}

// L2 (review 4): a failed handling-link read keeps the caller's own text, so a run that may well
// be retried is never told it is a task's handling run, and returns the read error to report.
func TestFormTasksFailureKeepsTheCallersTextWhenTheLookupFails(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	id := f.run(t, formGraph, str("Evidence"))
	if text, err := f.host.formTasksFailure(ctx, id, formTasksFailed); err == nil || text != formTasksFailed {
		t.Fatalf("failed lookup = %q %v, want the caller's text and the error", text, err)
	}
	if text, err := f.host.formTasksFailure(context.Background(), id, formFailed); err != nil || text != formFailed {
		t.Fatalf("unowned run = %q %v", text, err)
	}
}
