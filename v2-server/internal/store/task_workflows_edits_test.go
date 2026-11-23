package store_test

// Regression cases adapted from the second review.

import (
	"context"
	"errors"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
	"github.com/wanglongan587/cloud/v2-server/internal/taskworkflow"
)

// Task metadata changes must not consume the handling admission's status ownership.
func TestTaskWorkflowEditsPreserveSettlement(t *testing.T) {
	for _, tc := range []struct{ name, edit, outcome, want string }{
		{"describe then succeed", "describe", "succeeded", "review"},
		{"describe then cancel", "describe", "cancelled", "pending"},
		{"describe then fail", "describe", "failed", "review"},
		{"reassign then succeed", "reassign", "succeeded", "review"},
		{"reassign then cancel", "reassign", "cancelled", "pending"},
		{"reassign then fail", "reassign", "failed", "review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, user, task, wid := reviewWorkflow(t)
			h := taskworkflow.New(f.store, &reviewPort{taskWorkflowPort: taskWorkflowPort{db: f.store}})
			defer h.Close()
			id, err := h.Start(f.ctx, store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "k", WorkflowID: wid}, nil)
			if err != nil {
				t.Fatal(err)
			}
			cur, _ := f.store.Task(f.ctx, f.tenant, task.ID)
			if cur.Status != "running" {
				t.Fatalf("start status=%s", cur.Status)
			}
			switch tc.edit {
			case "describe":
				if _, err := f.store.UpdateTask(f.ctx, storetest.Access(t, f.store, f.tenant, user), task.ID, store.TaskInput{Title: cur.Title, Description: "clarified while it runs", ProjectID: cur.ProjectID, Version: cur.Version}); err != nil {
					t.Fatalf("edit refused: %v", err)
				}
			case "reassign":
				other := f.login(t, "other", "member")
				if _, err := f.store.SetTaskAssignee(f.ctx, storetest.Access(t, f.store, f.tenant, user), task.ID, &store.TaskAssigneeRef{Type: "human", ID: other}, cur.Version); err != nil {
					t.Fatalf("reassign refused: %v", err)
				}
			}
			if tc.outcome == "cancelled" {
				if _, err := f.store.CancelWorkflowRun(f.ctx, f.tenant, id); err != nil {
					t.Fatal(err)
				}
			} else if _, err := f.store.FinishWorkflowRun(f.ctx, f.tenant, id, tc.outcome, nil, nil); err != nil {
				t.Fatal(err)
			}
			list, err := h.List(f.ctx, f.tenant, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := f.store.Task(f.ctx, f.tenant, task.ID)
			t.Logf("run=%s link=%s task=%s (want %s)", tc.outcome, list[0].LinkStatus, got.Status, tc.want)
			// Agent runs use this same from-status transition after an ordinary task edit.
			agentTask := createTask(t, f, user, store.TaskInput{Title: "Agent comparison", Assignee: &store.TaskAssigneeRef{Type: "human", ID: user}})
			running, e := f.store.TransitionTaskStatus(f.ctx, f.tenant, agentTask.ID, "pending", "running")
			if e != nil {
				t.Fatal(e)
			}
			if _, e = f.store.UpdateTask(f.ctx, storetest.Access(t, f.store, f.tenant, user), agentTask.ID, store.TaskInput{Title: running.Title, Description: "clarified", ProjectID: running.ProjectID, Version: running.Version}); e != nil {
				t.Fatal(e)
			}
			agentResult, e := f.store.TransitionTaskStatus(f.ctx, f.tenant, agentTask.ID, "running", tc.want)
			if e != nil || agentResult.Status != got.Status {
				t.Fatalf("workflow/Agent edit mismatch: %+v vs %+v, %v", got, agentResult, e)
			}
			if got.Status != tc.want {
				t.Errorf("STRANDED STATUS: %s run settled, task left %q instead of %q", tc.outcome, got.Status, tc.want)
			}
		})
	}
}

// Preparation can overlap a metadata edit without refusing or abandoning the admitted start.
func TestTaskWorkflowEditDuringPrepare(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		name := "continues"
		if refuse {
			name = "refused start restores original status"
		}
		t.Run(name, func(t *testing.T) {
			f, user, task, wid := reviewWorkflow(t)
			port := &reviewPort{taskWorkflowPort: taskWorkflowPort{db: f.store}}
			port.prepare = func(ctx context.Context, _ string) error {
				cur, err := f.store.Task(ctx, f.tenant, task.ID)
				if err != nil {
					return err
				}
				_, err = f.store.UpdateTask(ctx, storetest.Access(t, f.store, f.tenant, user), task.ID, store.TaskInput{Title: cur.Title, Description: "edited", ProjectID: cur.ProjectID, Version: cur.Version})
				if err != nil {
					return err
				}
				if refuse {
					return errors.New("injected preparation refusal")
				}
				return nil
			}
			h := taskworkflow.New(f.store, port)
			defer h.Close()
			id, err := h.Start(f.ctx, store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "k", WorkflowID: wid}, nil)
			if (err != nil) != refuse || port.started == refuse {
				t.Fatalf("refuse=%v: start=%v started=%v", refuse, err, port.started)
			}
			wantTask, wantLink := "running", "running"
			if refuse {
				wantTask, wantLink = "pending", "failed"
			}
			got, err := f.store.Task(f.ctx, f.tenant, task.ID)
			if err != nil || got.Status != wantTask {
				t.Fatalf("task=%+v %v; want %s", got, err, wantTask)
			}
			list, err := h.List(f.ctx, f.tenant, task.ID)
			if err != nil || len(list) != 1 || list[0].LinkStatus != wantLink {
				t.Fatalf("links=%+v %v; want %s", list, err, wantLink)
			}
			if !refuse {
				if _, err := f.store.FinishWorkflowRun(f.ctx, f.tenant, id, "succeeded", nil, nil); err != nil {
					t.Fatal(err)
				}
				if _, err := h.List(f.ctx, f.tenant, task.ID); err != nil {
					t.Fatal(err)
				}
				got, err = f.store.Task(f.ctx, f.tenant, task.ID)
				if err != nil || got.Status != "review" {
					t.Fatalf("settlement stranded task: %+v %v", got, err)
				}
			}
		})
	}
}
