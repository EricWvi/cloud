package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

func seedCommentWorkflowAdmission(t *testing.T, f *squadFixture, user, taskID, wid, key string) store.TaskComment {
	t.Helper()
	comment, fresh, err := f.store.CreateTaskWorkflowComment(f.ctx, store.NewTaskWorkflowComment{
		TenantID:   f.tenant,
		TaskID:     taskID,
		AuthorID:   user,
		CreateKey:  key,
		Body:       "@Workflow",
		WorkflowID: wid,
	})
	if err != nil || !fresh || comment.WorkflowRun == nil {
		t.Fatalf("seed comment workflow: fresh=%t comment=%+v err=%v", fresh, comment, err)
	}
	return comment
}

func TestDiscardUnstartedCommentWorkflowGuards(t *testing.T) {
	f := newSquadFixture(t)
	user := f.login(t, "owner", "owner")
	addProject(t, f, user)
	task := createTask(t, f, user, store.TaskInput{Title: "Comment guards"})
	wf, _, err := f.store.CreateWorkflow(f.ctx, f.tenant, "Workflow", handlingGraph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PublishWorkflow(f.ctx, f.tenant, wf.ID, "v1", nil); err != nil {
		t.Fatal(err)
	}

	t.Run("discards unstarted pending admission", func(t *testing.T) {
		comment := seedCommentWorkflowAdmission(t, f, user, task.ID, wf.ID, "discard-ok")
		discarded, err := f.store.DiscardUnstartedCommentWorkflow(f.ctx, f.tenant, task.ID, user, "discard-ok", comment.WorkflowRun.ID)
		if err != nil || !discarded {
			t.Fatalf("discard = (%t, %v), want (true, nil)", discarded, err)
		}
		if _, err := f.store.TaskCommentByKey(f.ctx, f.tenant, task.ID, user, "discard-ok"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("comment survived discard: %v", err)
		}
		if _, err := f.store.WorkflowRun(f.ctx, f.tenant, comment.WorkflowRun.ID); !errors.Is(err, store.ErrWorkflowRunNotFound) {
			t.Fatalf("run survived discard: %v", err)
		}
	})

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, runID string)
	}{
		{
			name: "retains cancelled run with null started_at",
			setup: func(t *testing.T, runID string) {
				if _, err := f.store.CancelWorkflowRun(f.ctx, f.tenant, runID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "retains failed run with null started_at",
			setup: func(t *testing.T, runID string) {
				if _, err := f.db.Exec(f.ctx, `UPDATE v2_workflow_runs SET status = 'failed' WHERE id = $1`, runID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "retains pending run with started_at stamped",
			setup: func(t *testing.T, runID string) {
				if _, err := f.db.Exec(f.ctx, `UPDATE v2_workflow_runs SET started_at = now() WHERE id = $1`, runID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "retains soft-deleted pending run",
			setup: func(t *testing.T, runID string) {
				if err := f.store.DeleteWorkflowRun(f.ctx, f.tenant, runID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "retains pending run with node execution rows",
			setup: func(t *testing.T, runID string) {
				if _, err := f.store.CreateWorkflowNodeRun(f.ctx, f.tenant, runID, store.WorkflowNodeRunInput{
					NodeID: "start", NodeType: "start",
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := "guard-" + tc.name
			comment := seedCommentWorkflowAdmission(t, f, user, task.ID, wf.ID, key)
			tc.setup(t, comment.WorkflowRun.ID)
			discarded, err := f.store.DiscardUnstartedCommentWorkflow(f.ctx, f.tenant, task.ID, user, key, comment.WorkflowRun.ID)
			if err != nil || discarded {
				t.Fatalf("discard = (%t, %v), want (false, nil)", discarded, err)
			}
			if _, err := f.store.TaskCommentByKey(f.ctx, f.tenant, task.ID, user, key); err != nil {
				t.Fatalf("guarded comment missing: %v", err)
			}
			var runCount int
			if err := f.db.QueryRow(f.ctx, `SELECT COUNT(*) FROM v2_workflow_runs WHERE id = $1`, comment.WorkflowRun.ID).Scan(&runCount); err != nil || runCount != 1 {
				t.Fatalf("guarded run count=%d err=%v", runCount, err)
			}
		})
	}

	t.Run("missing comment identity returns ErrNotFound and keeps run", func(t *testing.T) {
		comment := seedCommentWorkflowAdmission(t, f, user, task.ID, wf.ID, "wrong-key")
		discarded, err := f.store.DiscardUnstartedCommentWorkflow(f.ctx, f.tenant, task.ID, user, "other-key", comment.WorkflowRun.ID)
		if !errors.Is(err, store.ErrNotFound) || discarded {
			t.Fatalf("wrong key = (%t, %v), want (false, ErrNotFound)", discarded, err)
		}
		if _, err := f.store.WorkflowRun(f.ctx, f.tenant, comment.WorkflowRun.ID); err != nil {
			t.Fatalf("rollback failed to preserve run: %v", err)
		}
	})
}

func TestDiscardUnstartedCommentWorkflowRowLock(t *testing.T) {
	f := newSquadFixture(t)
	user := f.login(t, "owner", "owner")
	addProject(t, f, user)
	task := createTask(t, f, user, store.TaskInput{Title: "Row lock"})
	wf, _, err := f.store.CreateWorkflow(f.ctx, f.tenant, "Workflow", handlingGraph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PublishWorkflow(f.ctx, f.tenant, wf.ID, "v1", nil); err != nil {
		t.Fatal(err)
	}
	comment := seedCommentWorkflowAdmission(t, f, user, task.ID, wf.ID, "lock-key")

	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err := tx.Exec(f.ctx, `SELECT id FROM v2_workflow_runs WHERE id = $1 FOR UPDATE`, comment.WorkflowRun.ID); err != nil {
		t.Fatal(err)
	}

	type discardResult struct {
		discarded bool
		err       error
	}
	done := make(chan discardResult, 1)
	go func() {
		discarded, err := f.store.DiscardUnstartedCommentWorkflow(f.ctx, f.tenant, task.ID, user, "lock-key", comment.WorkflowRun.ID)
		done <- discardResult{discarded, err}
	}()

	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		var blocked bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pg_backend_pid() = ANY(pg_blocking_pids(pid)))`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case res := <-done:
			t.Fatalf("discard bypassed workflow run row lock: %+v", res)
		case <-ctx.Done():
			t.Fatal("discard never waited on workflow run row lock")
		case <-tick.C:
		}
	}

	if _, err := tx.Exec(f.ctx, `UPDATE v2_workflow_runs SET status = 'running', started_at = now() WHERE id = $1`, comment.WorkflowRun.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	res := <-done
	if res.err != nil || res.discarded {
		t.Fatalf("discard after locked start = %+v, want (false, nil)", res)
	}
	if _, err := f.store.WorkflowRun(f.ctx, f.tenant, comment.WorkflowRun.ID); err != nil {
		t.Fatalf("started run deleted after unlock: %v", err)
	}
}

func TestRecordCommentWorkflowStartFailureGuards(t *testing.T) {
	f := newSquadFixture(t)
	user := f.login(t, "owner", "owner")
	addProject(t, f, user)
	task := createTask(t, f, user, store.TaskInput{Title: "Record failure guards"})
	wf, _, err := f.store.CreateWorkflow(f.ctx, f.tenant, "Workflow", handlingGraph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PublishWorkflow(f.ctx, f.tenant, wf.ID, "v1", nil); err != nil {
		t.Fatal(err)
	}

	newRun := func(key string) string {
		return seedCommentWorkflowAdmission(t, f, user, task.ID, wf.ID, key).WorkflowRun.ID
	}

	// Only cancelled + NULL error + matching tenant receives the safe message.
	cancelledID := newRun("cancelled-empty")
	if _, err := f.store.CancelWorkflowRun(f.ctx, f.tenant, cancelledID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordCommentWorkflowStartFailure(f.ctx, "00000000-0000-4000-8000-000000000099", cancelledID, "wrong tenant"); err != nil {
		t.Fatal(err)
	}
	if run, err := f.store.WorkflowRun(f.ctx, f.tenant, cancelledID); err != nil || run.Error != nil {
		t.Fatalf("wrong tenant wrote error: %+v %v", run, err)
	}
	if err := f.store.RecordCommentWorkflowStartFailure(f.ctx, f.tenant, cancelledID, "safe failure"); err != nil {
		t.Fatal(err)
	}
	if run, err := f.store.WorkflowRun(f.ctx, f.tenant, cancelledID); err != nil || run.Error == nil || *run.Error != "safe failure" {
		t.Fatalf("cancelled run error = %+v %v", run, err)
	}
	if err := f.store.RecordCommentWorkflowStartFailure(f.ctx, f.tenant, cancelledID, "overwrite"); err != nil {
		t.Fatal(err)
	}
	if run, err := f.store.WorkflowRun(f.ctx, f.tenant, cancelledID); err != nil || run.Error == nil || *run.Error != "safe failure" {
		t.Fatalf("existing error overwritten: %+v %v", run, err)
	}

	// Non-cancelled runs must never have an error stamped by start-failure fallback.
	for _, status := range []string{store.WorkflowStatusPending, store.WorkflowStatusRunning, store.WorkflowStatusSucceeded, store.WorkflowStatusFailed} {
		id := newRun("status-" + status)
		if status != store.WorkflowStatusPending {
			if _, err := f.store.StartWorkflowRun(f.ctx, f.tenant, id); err != nil {
				t.Fatal(err)
			}
			if status != store.WorkflowStatusRunning {
				if _, err := f.store.FinishWorkflowRun(f.ctx, f.tenant, id, status, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := f.store.RecordCommentWorkflowStartFailure(f.ctx, f.tenant, id, "should not write"); err != nil {
			t.Fatal(err)
		}
		if run, err := f.store.WorkflowRun(f.ctx, f.tenant, id); err != nil || run.Error != nil {
			t.Fatalf("%s run unexpectedly got error: %+v %v", status, run, err)
		}
	}
}

func TestTaskWorkflowRunsClearsDeletedMentionAndSourceFailure(t *testing.T) {
	f := newSquadFixture(t)
	user := f.login(t, "owner", "owner")
	addProject(t, f, user)
	task := createTask(t, f, user, store.TaskInput{Title: "Deleted run failure"})
	wf, _, err := f.store.CreateWorkflow(f.ctx, f.tenant, "Workflow", handlingGraph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PublishWorkflow(f.ctx, f.tenant, wf.ID, "v1", nil); err != nil {
		t.Fatal(err)
	}

	comment := seedCommentWorkflowAdmission(t, f, user, task.ID, wf.ID, "deleted-mention")
	mentionID := comment.WorkflowRun.ID
	if _, err := f.store.StartWorkflowRun(f.ctx, f.tenant, mentionID); err != nil {
		t.Fatal(err)
	}
	msg := "private mention error"
	if _, err := f.store.FinishWorkflowRun(f.ctx, f.tenant, mentionID, store.WorkflowStatusFailed, nil, &msg); err != nil {
		t.Fatal(err)
	}

	source, err := f.store.CreateWorkflowRun(f.ctx, f.tenant, wf.ID, "", store.WorkflowRunInput{ProjectID: taskProject})
	if err != nil {
		t.Fatal(err)
	}
	node, err := f.store.CreateWorkflowNodeRun(f.ctx, f.tenant, source.ID, store.WorkflowNodeRunInput{NodeID: "review", NodeType: "form"})
	if err != nil {
		t.Fatal(err)
	}
	results, _ := json.Marshal([]map[string]string{{"taskId": task.ID}})
	if _, err := f.db.Exec(f.ctx, `INSERT INTO v2_workflow_forms(id,tenant_id,run_id,node_id,reviewer_id,config,findings,report,results) VALUES($1,$2,$3,'review',$4,'{}','[]','Report',$5)`,
		node.ID, f.tenant, source.ID, user, results); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.StartWorkflowRun(f.ctx, f.tenant, source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CompleteWorkflowNodeRun(f.ctx, f.tenant, source.ID, node.ID, store.WorkflowNodeResult{}); err != nil {
		t.Fatal(err)
	}
	srcMsg := "private source error"
	if _, err := f.store.FinishWorkflowRun(f.ctx, f.tenant, source.ID, store.WorkflowStatusFailed, nil, &srcMsg); err != nil {
		t.Fatal(err)
	}

	before, err := f.store.TaskWorkflowRuns(f.ctx, f.tenant, task.ID)
	if err != nil || len(before) != 2 {
		t.Fatalf("before delete = %+v %v", before, err)
	}
	for _, item := range before {
		if item.Failure == "" || item.Deleted {
			t.Fatalf("expected non-deleted failure before delete: %+v", item)
		}
	}

	for _, id := range []string{mentionID, source.ID} {
		if err := f.store.DeleteWorkflowRun(f.ctx, f.tenant, id); err != nil {
			t.Fatal(err)
		}
	}
	after, err := f.store.TaskWorkflowRuns(f.ctx, f.tenant, task.ID)
	if err != nil || len(after) != 2 {
		t.Fatalf("after delete = %+v %v", after, err)
	}
	for _, item := range after {
		if !item.Deleted || item.Failure != "" {
			t.Fatalf("deleted %s run exposed failure: %+v", item.Kind, item)
		}
	}
}

func TestTaskWorkflowRunsClearsDeletedHandleFailure(t *testing.T) {
	f := newSquadFixture(t)
	user := f.login(t, "owner", "owner")
	addProject(t, f, user)
	task := createTask(t, f, user, store.TaskInput{
		Title:    "Deleted handle failure",
		Assignee: &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: user},
	})
	wf, _, err := f.store.CreateWorkflow(f.ctx, f.tenant, "Workflow", handlingGraph)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := f.store.PublishWorkflow(f.ctx, f.tenant, wf.ID, "v1", nil)
	if err != nil {
		t.Fatal(err)
	}

	runID, fresh, err := f.store.CreateTaskWorkflowRun(f.ctx, store.NewTaskWorkflow{
		TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "deleted-handle",
		WorkflowID: wf.ID, SnapshotID: snap.ID,
	})
	if err != nil || !fresh {
		t.Fatalf("create task workflow run = %s %t %v", runID, fresh, err)
	}
	if _, err := f.store.StartWorkflowRun(f.ctx, f.tenant, runID); err != nil {
		t.Fatal(err)
	}
	msg := "private run error"
	if _, err := f.store.FinishWorkflowRun(f.ctx, f.tenant, runID, store.WorkflowStatusFailed, nil, &msg); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SettleTaskWorkflow(f.ctx, f.tenant, runID, "failed", "private handle error"); err != nil {
		t.Fatal(err)
	}

	before, err := f.store.TaskWorkflowRuns(f.ctx, f.tenant, task.ID)
	if err != nil || len(before) != 1 || before[0].Failure != "private handle error" || before[0].Deleted {
		t.Fatalf("before delete = %+v %v", before, err)
	}
	if err := f.store.DeleteWorkflowRun(f.ctx, f.tenant, runID); err != nil {
		t.Fatal(err)
	}
	after, err := f.store.TaskWorkflowRuns(f.ctx, f.tenant, task.ID)
	if err != nil || len(after) != 1 || !after[0].Deleted || after[0].Failure != "" {
		t.Fatalf("after delete = %+v %v", after, err)
	}
}
