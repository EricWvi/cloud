package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrTaskNotAssignee reports workflow admission by a caller outside the task assignee scope.
var ErrTaskNotAssignee = errors.New("not task assignee")

// TaskWorkflowRun is the unified, identity-based task workflow history.
type TaskWorkflowRun struct {
	ID            string    `json:"id"`
	TaskID        string    `json:"taskId"`
	WorkflowRunID string    `json:"workflowRunId"`
	WorkflowID    string    `json:"workflowId"`
	WorkflowName  string    `json:"workflowName"`
	Version       string    `json:"version"`
	StartedBy     string    `json:"startedBy"`
	StartedByName string    `json:"startedByName"`
	Status        string    `json:"status"`
	Failure       string    `json:"failure"`
	Kind          string    `json:"kind"`
	CreatedCount  int       `json:"createdCount"`
	Deleted       bool      `json:"deleted"`
	LinkStatus    string    `json:"-"`
	CreatedAt     time.Time `json:"createdAt"`
}

// NewTaskWorkflow freezes caller input and its idempotency identity before execution.
type NewTaskWorkflow struct {
	TenantID, TaskID, UserID, Key, WorkflowID, SnapshotID string
	Input                                                 *string
	ExpectedVersion                                       int64
}

// CreateTaskWorkflowRun reserves a run and link atomically with Agent admission on the task row.
// An existing key returns its run even after settlement; authorization is always checked first.
func (s *Store) CreateTaskWorkflowRun(ctx context.Context, in NewTaskWorkflow) (string, bool, error) {
	var id string
	fresh := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var kind, assignee *string
		var status, project string
		var version int64
		err := tx.QueryRow(ctx, `SELECT assignee_type,assignee_id::text,status,project_id,version FROM v2_tasks WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, in.TenantID, in.TaskID).Scan(&kind, &assignee, &status, &project, &version)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		task := Task{TenantID: in.TenantID}
		if kind != nil && assignee != nil {
			task.Assignee = &TaskAssignee{Type: *kind, ID: *assignee}
		}
		if err := AuthorizeTaskRunner(ctx, task, in.UserID, func(ctx context.Context, tenant, squad, user string) (bool, error) {
			return inSquad(ctx, tx, tenant, squad, user)
		}); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT run_id::text FROM v2_task_workflow_runs WHERE tenant_id=$1 AND task_id=$2 AND started_by=$3 AND start_key=$4`, in.TenantID, in.TaskID, in.UserID, in.Key).Scan(&id)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if in.ExpectedVersion != 0 && version != in.ExpectedVersion {
			return ErrConflict
		}
		if status == TaskDone || status == TaskCancelled {
			return ErrInvalidTransition
		}
		var live bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM v2_task_runs WHERE tenant_id=$1 AND task_id=$2 AND ended_at IS NULL) OR EXISTS(SELECT 1 FROM v2_task_workflow_runs WHERE tenant_id=$1 AND task_id=$2 AND status='running')`, in.TenantID, in.TaskID).Scan(&live); err != nil {
			return err
		}
		if live {
			return ErrRunActive
		}
		run, err := createWorkflowRun(ctx, tx, in.TenantID, in.WorkflowID, in.SnapshotID, WorkflowRunInput{ProjectID: project, Input: in.Input})
		if err != nil {
			return err
		}
		original := status
		if CanRunTransitionTask(status, TaskRunning) {
			tag, err := tx.Exec(ctx, `UPDATE v2_tasks SET status='running',version=version+1,updated_at=now() WHERE tenant_id=$1 AND id=$2 AND status=$3 AND version=$4`, in.TenantID, in.TaskID, status, version)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return ErrConflict
			}
		}
		id = run.ID
		if _, err = tx.Exec(ctx, `INSERT INTO v2_task_workflow_runs(tenant_id,task_id,run_id,started_by,start_key,task_status,original_status) VALUES($1,$2,$3,$4,$5,$6,$7)`, in.TenantID, in.TaskID, id, in.UserID, in.Key, TaskRunning, original); err != nil {
			return err
		}
		fresh = true
		return nil
	})
	return id, fresh, err
}

// TaskWorkflowOutcome is the stored execution outcome used by task-link settlement. Deleted stays
// true for soft-deleted history so cancellation and unexecuted admission retain their semantics,
// and Executed reports whether the run ever left its pending admission (started_at is stamped by
// the first dispatch and never cleared).
type TaskWorkflowOutcome struct {
	Status   string
	Failure  string
	Deleted  bool
	Executed bool
}

// TaskWorkflowOutcome reads a handling run for settlement without hiding soft-deleted history.
func (s *Store) TaskWorkflowOutcome(ctx context.Context, tenant, runID string) (TaskWorkflowOutcome, error) {
	var outcome TaskWorkflowOutcome
	err := s.pool.QueryRow(ctx, `SELECT status,COALESCE(error,''),deleted_at IS NOT NULL,started_at IS NOT NULL FROM v2_workflow_runs WHERE tenant_id=$1 AND id=$2`, tenant, runID).Scan(&outcome.Status, &outcome.Failure, &outcome.Deleted, &outcome.Executed)
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskWorkflowOutcome{}, ErrWorkflowRunNotFound
	}
	return outcome, err
}

// SettleTaskWorkflow records the outcome and fenced task move once in a database-only transaction.
// The link stays live until settlement, preventing another run from racing the status flow-back.
func (s *Store) SettleTaskWorkflow(ctx context.Context, tenant, runID, status, failure string) error {
	to := TaskReview
	switch status {
	case "succeeded", "failed":
	case "cancelled", "interrupted":
		to = TaskPending
	default:
		return ErrInvalidTransition
	}
	return s.settleTaskWorkflow(ctx, tenant, runID, status, failure, to, false)
}

// RefuseTaskWorkflow cancels an admitted run that never executed and releases its link in one
// transaction, restoring the original task status only while this admission still owns the running
// status, so a history read can never observe the cancellation before the refusal settlement. A
// run that already executed is refused: its outcome belongs to normal settlement, and restoring
// the task would misreport work that really ran.
func (s *Store) RefuseTaskWorkflow(ctx context.Context, tenant, runID, failure string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		run, err := cancelWorkflowRun(ctx, tx, tenant, runID)
		if err != nil {
			if !errors.Is(err, ErrWorkflowRunNotFound) {
				return err
			}
			// A deleted admission never executed; its reservation is still released below.
			return settleTaskWorkflow(ctx, tx, tenant, runID, "failed", failure, "", true)
		}
		if run.StartedAt != nil {
			return ErrWorkflowRunState
		}
		return settleTaskWorkflow(ctx, tx, tenant, runID, "failed", failure, "", true)
	})
}

func (s *Store) settleTaskWorkflow(ctx context.Context, tenant, runID, status, failure, to string, restore bool) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return settleTaskWorkflow(ctx, tx, tenant, runID, status, failure, to, restore)
	})
}

func settleTaskWorkflow(ctx context.Context, tx pgx.Tx, tenant, runID, status, failure, to string, restore bool) error {
	var taskID string
	var original string
	if err := tx.QueryRow(ctx, `SELECT task_id::text,original_status FROM v2_task_workflow_runs WHERE tenant_id=$1 AND run_id=$2`, tenant, runID).Scan(&taskID, &original); err != nil {
		return err
	}
	// Match admission's lock order: task before link.
	if _, err := tx.Exec(ctx, `SELECT id FROM v2_tasks WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenant, taskID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE v2_task_workflow_runs SET status=$3,failure=$4 WHERE tenant_id=$1 AND run_id=$2 AND status='running'`, tenant, runID, status, failure)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	if restore {
		to = original
	}
	if !restore && !CanRunTransitionTask(TaskRunning, to) {
		return ErrInvalidTransition
	}
	_, err = tx.Exec(ctx, `UPDATE v2_tasks SET status=$3,version=version+1,updated_at=now() WHERE tenant_id=$1 AND id=$2 AND status=$4
 AND EXISTS(SELECT 1 FROM v2_task_workflow_runs l WHERE l.tenant_id=$1 AND l.task_id=$2 AND l.run_id=$5 AND l.status_revision=1 AND l.task_status=$4)`, tenant, taskID, to, TaskRunning, runID)
	return err
}

// LiveTaskWorkflows lists unsettled handling runs for startup recovery.
func (s *Store) LiveTaskWorkflows(ctx context.Context) ([]struct{ Tenant, TaskID, RunID string }, error) {
	rows, err := s.pool.Query(ctx, `SELECT tenant_id::text,task_id::text,run_id::text FROM v2_task_workflow_runs WHERE status='running'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []struct{ Tenant, TaskID, RunID string }{}
	for rows.Next() {
		var r struct{ Tenant, TaskID, RunID string }
		if err := rows.Scan(&r.Tenant, &r.TaskID, &r.RunID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TaskWorkflowRuns lists all three kinds using durable identities, never task titles.
func (s *Store) TaskWorkflowRuns(ctx context.Context, tenant, taskID string) ([]TaskWorkflowRun, error) {
	if _, err := s.Task(ctx, tenant, taskID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `WITH links AS (
 SELECT run_id,started_by,'handle' AS kind,status,failure,0 AS created_count,created_at FROM v2_task_workflow_runs WHERE tenant_id=$1 AND task_id=$2
 UNION ALL
 SELECT workflow_run_id,started_by,'decomposition',status,failure,created_count,created_at FROM v2_task_decompositions WHERE tenant_id=$1 AND task_id=$2
 UNION ALL
 SELECT f.run_id,r.started_by,'source',r.status,COALESCE(r.error,''),0,COALESCE(f.submitted_at,r.created_at) FROM v2_workflow_forms f JOIN v2_workflow_runs r ON r.id=f.run_id AND r.tenant_id=f.tenant_id
 WHERE f.tenant_id=$1 AND f.results @> jsonb_build_array(jsonb_build_object('taskId',$2::text))
 ) SELECT r.id::text,r.workflow_id::text,w.name,s.version,COALESCE(l.started_by::text,''),COALESCE(u.display_name,''),
 CASE WHEN l.kind IN ('handle','decomposition') AND l.status<>'running' THEN l.status ELSE `+derivedRunStatus+` END,
 CASE WHEN l.failure<>'' THEN l.failure ELSE COALESCE(r.error,'') END,l.kind,l.created_count,l.created_at,r.deleted_at IS NOT NULL,l.status
 FROM links l JOIN v2_workflow_runs r ON r.id=l.run_id AND r.tenant_id=$1
 JOIN v2_workflows w ON w.id=r.workflow_id JOIN v2_workflow_snapshots s ON s.id=r.snapshot_id
 LEFT JOIN users u ON u.id=l.started_by ORDER BY l.created_at DESC,r.id DESC`, tenant, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskWorkflowRun{}
	for rows.Next() {
		var r TaskWorkflowRun
		r.TaskID = taskID
		if err := rows.Scan(&r.WorkflowRunID, &r.WorkflowID, &r.WorkflowName, &r.Version, &r.StartedBy, &r.StartedByName, &r.Status, &r.Failure, &r.Kind, &r.CreatedCount, &r.CreatedAt, &r.Deleted, &r.LinkStatus); err != nil {
			return nil, err
		}
		r.ID = r.Kind + ":" + r.WorkflowRunID
		out = append(out, r)
	}
	return out, rows.Err()
}

// Task link kinds WorkflowRunTaskLink reports.
const (
	TaskLinkHandle        = "handle"
	TaskLinkDecomposition = "decomposition"
)

// WorkflowRunTaskLink returns the owning task and how it owns the run: as its handling run
// (TaskLinkHandle) or as its decomposition (TaskLinkDecomposition). Both are empty for a run no
// task owns, or one outside the tenant. Inspection can create several tasks, so it has no single
// owning task and remains visible through each task's history.
func (s *Store) WorkflowRunTaskLink(ctx context.Context, tenant, runID string) (string, string, error) {
	var id, kind string
	err := s.pool.QueryRow(ctx, `SELECT task_id::text, 'handle' FROM v2_task_workflow_runs WHERE tenant_id=$1 AND run_id=$2 UNION ALL SELECT task_id::text, 'decomposition' FROM v2_task_decompositions WHERE tenant_id=$1 AND workflow_run_id=$2 LIMIT 1`, tenant, runID).Scan(&id, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	return id, kind, err
}

// TaskWorkflowRunByKey replays a caller's durable admission identity before preflight.
func (s *Store) TaskWorkflowRunByKey(ctx context.Context, in NewTaskWorkflow) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT run_id::text FROM v2_task_workflow_runs WHERE tenant_id=$1 AND task_id=$2 AND started_by=$3 AND start_key=$4`, in.TenantID, in.TaskID, in.UserID, in.Key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// CheckTaskWorkflowStart rejects an admission superseded during input preparation. Manual moves
// are blocked while handling is live, but internal lifecycle writers still fence this boundary.
func (s *Store) CheckTaskWorkflowStart(ctx context.Context, tenant, id string) error {
	var status string
	var revision int64
	var linkStatus, admittedStatus string
	err := s.pool.QueryRow(ctx, `SELECT t.status,l.status_revision,l.status,l.task_status FROM v2_task_workflow_runs l JOIN v2_tasks t ON t.id=l.task_id AND t.tenant_id=l.tenant_id WHERE l.tenant_id=$1 AND l.run_id=$2`, tenant, id).Scan(&status, &revision, &linkStatus, &admittedStatus)
	if err != nil {
		return err
	}
	if status == TaskDone || status == TaskCancelled {
		return ErrInvalidTransition
	}
	if status != admittedStatus || revision != 1 || linkStatus != "running" {
		return ErrConflict
	}
	return nil
}
