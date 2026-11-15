package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/taskworkflow"
)

func TestTaskWorkflowRefusalRetriesCleanup(t *testing.T) {
	f, user, task, wid := reviewWorkflow(t)
	channel := "cleanup_" + strings.ReplaceAll(task.ID, "-", "")
	if _, err := f.db.Exec(f.ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	// Hold cancellation in a failed state until the test explicitly releases it. Notifications
	// acknowledge the eventual settlement transaction, so the assertion never relies on a sleep.
	_, err := f.db.Exec(f.ctx, `CREATE FUNCTION reject_cancel() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.status='cancelled' THEN RAISE EXCEPTION 'injected cancellation failure'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER reject_cancel BEFORE UPDATE ON v2_workflow_runs FOR EACH ROW EXECUTE FUNCTION reject_cancel();
 CREATE FUNCTION notify_settlement() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN PERFORM pg_notify(TG_ARGV[0],NEW.run_id::text); RETURN NEW; END $$;`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(f.ctx, `CREATE TRIGGER settled AFTER UPDATE ON v2_task_workflow_runs FOR EACH ROW WHEN(NEW.status='failed') EXECUTE FUNCTION notify_settlement('`+channel+`')`); err != nil {
		t.Fatal(err)
	}
	port := &reviewPort{taskWorkflowPort: taskWorkflowPort{db: f.store}, startError: errors.New("host refused")}
	h := taskworkflow.New(f.store, port)
	defer h.Close()
	id, err := h.Start(f.ctx, store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "retry", WorkflowID: wid}, nil)
	if err == nil {
		t.Fatal("cleanup failure was hidden")
	}
	var link string
	if err := f.db.QueryRow(f.ctx, `SELECT status FROM v2_task_workflow_runs WHERE run_id=$1`, id).Scan(&link); err != nil || link != "running" {
		t.Fatalf("released before cancellation: %s %v", link, err)
	}
	if _, err := f.db.Exec(f.ctx, `DROP TRIGGER reject_cancel ON v2_workflow_runs`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	notice, err := f.db.WaitForNotification(ctx)
	if err != nil {
		t.Fatalf("cleanup was not retried: %v", err)
	}
	if notice.Payload != id {
		t.Fatalf("settled wrong run: %s", notice.Payload)
	}
	current, err := f.store.Task(f.ctx, f.tenant, task.ID)
	if err != nil || current.Status != "pending" {
		t.Fatalf("restoration=%+v %v", current, err)
	}
	run, err := f.store.WorkflowRun(f.ctx, f.tenant, id)
	if err != nil || run.Status != "cancelled" {
		t.Fatalf("run=%+v %v", run, err)
	}
}

// A pending admitted run can be deleted before its first scheduling pass. It is a refused start:
// history records the failed admission while the task returns to the status it had before it.
func TestTaskWorkflowDeletedAdmissionRestoresOriginalStatus(t *testing.T) {
	for _, initial := range []string{store.TaskPending, store.TaskReview} {
		t.Run(initial, func(t *testing.T) {
			f, user, task, wid := reviewWorkflow(t)
			if _, err := f.db.Exec(f.ctx, `UPDATE v2_tasks SET status=$2 WHERE id=$1`, task.ID, initial); err != nil {
				t.Fatal(err)
			}
			id, fresh, err := f.store.CreateTaskWorkflowRun(f.ctx, store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "deleted-admission", WorkflowID: wid})
			if err != nil || !fresh {
				t.Fatalf("admission: %s %v %v", id, fresh, err)
			}
			if err := f.store.DeleteWorkflowRun(f.ctx, f.tenant, id); err != nil {
				t.Fatal(err)
			}
			h := taskworkflow.New(f.store, &reviewPort{taskWorkflowPort: taskWorkflowPort{db: f.store}})
			defer h.Close()
			rows, err := h.List(f.ctx, f.tenant, task.ID)
			if err != nil || len(rows) != 1 || !rows[0].Deleted || rows[0].Status != store.WorkflowStatusFailed || rows[0].Failure != "Workflow run was deleted." {
				t.Fatalf("deleted admission: %+v %v", rows, err)
			}
			current, err := f.store.Task(f.ctx, f.tenant, task.ID)
			if err != nil || current.Status != initial {
				t.Fatalf("unexecuted deletion moved task from %s: %+v %v", initial, current, err)
			}
		})
	}
}

// moveToReview walks a fresh task through its legal transitions to 待验收, the status whose
// start shows a wrong restore most clearly when it is mistaken for a cancellation.
func moveToReview(t *testing.T, f *squadFixture, taskID string) {
	t.Helper()
	if _, err := f.store.TransitionTaskStatus(f.ctx, f.tenant, taskID, store.TaskPending, store.TaskRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.TransitionTaskStatus(f.ctx, f.tenant, taskID, store.TaskRunning, store.TaskReview); err != nil {
		t.Fatal(err)
	}
}

// A run that executed, was cancelled, and then had its history deleted still settles as a
// cancellation: the task returns to pending, from the status stored on the soft-deleted row.
func TestTaskWorkflowCancelledThenDeletedRunSettlesPending(t *testing.T) {
	f, user, task, wid := reviewWorkflow(t)
	h := taskworkflow.New(f.store, &reviewPort{taskWorkflowPort: taskWorkflowPort{db: f.store}})
	defer h.Close()
	id, err := h.Start(f.ctx, store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "k", WorkflowID: wid}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Model a watcher that has not observed the cancellation when deletion commits.
	h.Close()
	h = taskworkflow.New(f.store, &reviewPort{taskWorkflowPort: taskWorkflowPort{db: f.store}})
	defer h.Close()
	if _, err := f.store.CancelWorkflowRun(f.ctx, f.tenant, id); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DeleteWorkflowRun(f.ctx, f.tenant, id); err != nil {
		t.Fatal(err)
	}
	list, err := h.List(f.ctx, f.tenant, task.ID)
	if err != nil || len(list) != 1 || list[0].LinkStatus != store.WorkflowStatusCancelled {
		t.Fatalf("cancelled deleted history=%+v %v", list, err)
	}
	got, err := f.store.Task(f.ctx, f.tenant, task.ID)
	if err != nil || got.Status != store.TaskPending {
		t.Fatalf("cancelled-then-deleted run left task %+v: %v", got, err)
	}
}

// A refused start is atomic against history reads: the failed cancellation/restoration transaction
// rolls back whole, an intermediate history read never reinterprets it as a cancellation, and the
// process-owned retry restores the original status.
func TestTaskWorkflowRefusalIsAtomicAgainstHistoryReads(t *testing.T) {
	f, user, task, wid := reviewWorkflow(t)
	moveToReview(t, f, task.ID)
	// Reject the combined cancellation/restoration transaction once. A history read must not see
	// half of it and reinterpret the refused start as an ordinary cancellation.
	if _, err := f.db.Exec(f.ctx, `CREATE FUNCTION reject_failed_link() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.status='failed' THEN RAISE EXCEPTION 'injected transient failure'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER reject_failed_link BEFORE UPDATE ON v2_task_workflow_runs FOR EACH ROW EXECUTE FUNCTION reject_failed_link();`); err != nil {
		t.Fatal(err)
	}
	h := taskworkflow.New(f.store, &reviewPort{taskWorkflowPort: taskWorkflowPort{db: f.store}, startError: errors.New("host refused")})
	defer h.Close()
	id, err := h.Start(f.ctx, store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "k", WorkflowID: wid}, nil)
	if err == nil {
		t.Fatal("start should be refused")
	}
	mid, err := f.store.Task(f.ctx, f.tenant, task.ID)
	if err != nil || mid.Status != store.TaskRunning {
		t.Fatalf("rolled-back refusal moved task: %+v %v", mid, err)
	}
	run, err := f.store.WorkflowRun(f.ctx, f.tenant, id)
	if err != nil || run.Status != store.WorkflowStatusPending {
		t.Fatalf("rolled-back refusal partially cancelled run: %+v %v", run, err)
	}
	if _, err := h.List(f.ctx, f.tenant, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(f.ctx, `DROP TRIGGER reject_failed_link ON v2_task_workflow_runs`); err != nil {
		t.Fatal(err)
	}
	// Apply the same retry synchronously; the host's process-owned worker is an additional caller.
	if err := f.store.RefuseTaskWorkflow(f.ctx, f.tenant, id, "host refused"); err != nil {
		t.Fatal(err)
	}
	list, err := h.List(f.ctx, f.tenant, task.ID)
	if err != nil || len(list) != 1 || list[0].LinkStatus != store.WorkflowStatusFailed {
		t.Fatalf("refusal history=%+v %v", list, err)
	}
	got, err := f.store.Task(f.ctx, f.tenant, task.ID)
	if err != nil || got.Status != store.TaskReview {
		t.Fatalf("refused start from review left task %+v: %v", got, err)
	}
}

// A handling run cancelled before it ever executed (a member clicks 停止 on the pending row while
// the start is still preparing) is a start that did not happen: the link records the stop as
// failed and the task returns to its original status, whichever of Start's own refusal or a
// history read settles first.
func TestTaskWorkflowCancelledBeforeExecutionRestoresOriginalStatus(t *testing.T) {
	for _, historyReadFirst := range []bool{false, true} {
		name := "start refusal settles"
		if historyReadFirst {
			name = "history read settles"
		}
		t.Run(name, func(t *testing.T) {
			f, user, task, wid := reviewWorkflow(t)
			moveToReview(t, f, task.ID)
			var h *taskworkflow.Host
			port := &reviewPort{taskWorkflowPort: taskWorkflowPort{db: f.store}, prepare: func(ctx context.Context, id string) error {
				if _, err := f.store.CancelWorkflowRun(ctx, f.tenant, id); err != nil {
					return err
				}
				if historyReadFirst {
					_, err := h.List(ctx, f.tenant, task.ID)
					return err
				}
				return nil
			}}
			h = taskworkflow.New(f.store, port)
			defer h.Close()
			_, err := h.Start(f.ctx, store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "k", WorkflowID: wid}, nil)
			if err == nil {
				t.Fatal("cancelled start reported success")
			}
			got, err := f.store.Task(f.ctx, f.tenant, task.ID)
			if err != nil || got.Status != store.TaskReview {
				t.Fatalf("task=%+v %v; want the original review", got, err)
			}
			list, err := h.List(f.ctx, f.tenant, task.ID)
			if err != nil || len(list) != 1 || list[0].LinkStatus != store.WorkflowStatusFailed || list[0].Failure != "Workflow run was stopped before it started." {
				t.Fatalf("stopped-before-start history=%+v %v", list, err)
			}
			run, err := f.store.WorkflowRun(f.ctx, f.tenant, list[0].WorkflowRunID)
			if err != nil || run.StartedAt != nil {
				t.Fatalf("run recorded execution it never did: %+v %v", run, err)
			}
		})
	}
}

// startedThenFails models a dispatch that committed the run as running and then still returned an
// error, for example the host's final read after StartAs. Its Cancel records every host-side stop.
type startedThenFails struct {
	reviewPort
	hostCancels []string
}

func (p *startedThenFails) Start(ctx context.Context, tenant, _, id string) error {
	if _, err := p.db.StartWorkflowRun(ctx, tenant, id); err != nil {
		return err
	}
	return errors.New("read after start failed")
}

func (p *startedThenFails) Cancel(ctx context.Context, tenant, _, id string) error {
	p.hostCancels = append(p.hostCancels, id)
	_, err := p.db.CancelWorkflowRun(ctx, tenant, id)
	return err
}

// A start whose run already began executing is not a refusal: it is stopped through the workflow
// host — the dispatched Agent turn included — and settlement records the real outcome, so the task
// returns to pending instead of being restored as if nothing had run.
func TestTaskWorkflowStartFailureAfterExecutionCancelsThroughHost(t *testing.T) {
	f, user, task, wid := reviewWorkflow(t)
	moveToReview(t, f, task.ID)
	p := &startedThenFails{reviewPort: reviewPort{taskWorkflowPort: taskWorkflowPort{db: f.store}}}
	h := taskworkflow.New(f.store, p)
	defer h.Close()
	id, err := h.Start(f.ctx, store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "k", WorkflowID: wid}, nil)
	if err == nil || err.Error() != "read after start failed" {
		t.Fatalf("start error=%v", err)
	}
	if len(p.hostCancels) != 1 || p.hostCancels[0] != id {
		t.Fatalf("host-side cancel not reached: %v", p.hostCancels)
	}
	run, err := f.store.WorkflowRun(f.ctx, f.tenant, id)
	if err != nil || run.Status != store.WorkflowStatusCancelled || run.StartedAt == nil {
		t.Fatalf("run=%+v %v", run, err)
	}
	list, err := h.List(f.ctx, f.tenant, task.ID)
	if err != nil || len(list) != 1 || list[0].LinkStatus != store.WorkflowStatusCancelled {
		t.Fatalf("link=%+v %v; want the real cancelled outcome", list, err)
	}
	got, err := f.store.Task(f.ctx, f.tenant, task.ID)
	if err != nil || got.Status != store.TaskPending {
		t.Fatalf("executed start was restored as a refusal: %+v %v", got, err)
	}
}

// A refusal only covers a start that never executed: a run that already began executing — still
// running, or already finished — keeps its link for normal settlement, and restoring the task
// would misreport work that really ran. The cancellation inside the refused transaction rolls back.
func TestTaskWorkflowRefusalRejectsExecutedRun(t *testing.T) {
	for _, tc := range []struct{ name, status string }{
		{name: "still running", status: store.WorkflowStatusRunning},
		{name: "already finished", status: store.WorkflowStatusSucceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, user, task, wid := reviewWorkflow(t)
			id, _, err := f.store.CreateTaskWorkflowRun(f.ctx, store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "ran", WorkflowID: wid})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.StartWorkflowRun(f.ctx, f.tenant, id); err != nil {
				t.Fatal(err)
			}
			if tc.status == store.WorkflowStatusSucceeded {
				if _, err := f.store.FinishWorkflowRun(f.ctx, f.tenant, id, store.WorkflowStatusSucceeded, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.store.RefuseTaskWorkflow(f.ctx, f.tenant, id, "should not settle"); !errors.Is(err, store.ErrWorkflowRunState) {
				t.Fatalf("executed run was refuseable: %v", err)
			}
			var link string
			if err := f.db.QueryRow(f.ctx, `SELECT status FROM v2_task_workflow_runs WHERE run_id=$1`, id).Scan(&link); err != nil || link != "running" {
				t.Fatalf("refusal settled an executed link: %s %v", link, err)
			}
			run, err := f.store.WorkflowRun(f.ctx, f.tenant, id)
			if err != nil || run.Status != tc.status || run.StartedAt == nil {
				t.Fatalf("refusal cancelled an executed run: %+v %v", run, err)
			}
			got, err := f.store.Task(f.ctx, f.tenant, task.ID)
			if err != nil || got.Status != store.TaskRunning {
				t.Fatalf("refusal moved an executed task: %+v %v", got, err)
			}
		})
	}
}
