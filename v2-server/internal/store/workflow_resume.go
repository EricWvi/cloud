package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// WorkflowResumeState is what resume planning reads about one run: its stored status, every live
// attempt of every scope, the nodes holding a frozen review form and those whose form was
// submitted, and how a task owns the run (TaskLink: TaskLinkHandle, TaskLinkDecomposition or empty).
type WorkflowResumeState struct {
	Status               string
	Nodes                []WorkflowNodeRun
	FormNodeIDs          []string
	SubmittedFormNodeIDs []string
	TaskLink             string
}

// queryer is what both the pool and a transaction offer for reads.
type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func readWorkflowResumeState(ctx context.Context, q queryer, runID, status string) (WorkflowResumeState, error) {
	st := WorkflowResumeState{Status: status, Nodes: []WorkflowNodeRun{}, FormNodeIDs: []string{}, SubmittedFormNodeIDs: []string{}}
	rows, err := q.Query(ctx, `
		SELECT `+workflowNodeRunColumns+` FROM v2_workflow_node_runs
		WHERE run_id = $1 AND deleted_at IS NULL
		ORDER BY created_at, id`, runID)
	if err != nil {
		return st, fmt.Errorf("list resume node runs: %w", err)
	}
	st.Nodes, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (WorkflowNodeRun, error) { return scanNodeRun(row) })
	if err != nil {
		return st, fmt.Errorf("list resume node runs: %w", err)
	}
	forms, err := q.Query(ctx, `SELECT node_id, status = 'submitted' FROM v2_workflow_forms WHERE run_id = $1`, runID)
	if err != nil {
		return st, fmt.Errorf("list run forms: %w", err)
	}
	type form struct {
		NodeID    string
		Submitted bool
	}
	all, err := pgx.CollectRows(forms, pgx.RowToStructByPos[form])
	if err != nil {
		return st, fmt.Errorf("list run forms: %w", err)
	}
	for _, f := range all {
		st.FormNodeIDs = append(st.FormNodeIDs, f.NodeID)
		if f.Submitted {
			st.SubmittedFormNodeIDs = append(st.SubmittedFormNodeIDs, f.NodeID)
		}
	}
	if st.TaskLink, err = runTaskLink(ctx, q, runID); err != nil {
		return st, err
	}
	return st, nil
}

// WorkflowRunTaskLinkKind returns how a task owns the run, as runTaskLink does, for texts that must
// not offer a reopening D25, D30 and D32 refuse. It does not lock: the run is reopened only when it
// has failed or was cancelled, and by then its link can no longer appear (see runTaskLink).
func (s *Store) WorkflowRunTaskLinkKind(ctx context.Context, runID string) (string, error) {
	return runTaskLink(ctx, s.pool, runID)
}

// runTaskLink returns how a task owns the run: TaskLinkHandle for a task's handling run
// (v2_task_workflow_runs), TaskLinkDecomposition for a task decomposition's run
// (v2_task_decompositions.workflow_run_id), empty for any other run, inspection and patrol sources
// included. A run a task owns either way is never returned to running in place, by resume or by
// the form's task creation retry (owner decisions D25, D30 and D32): its link settled when the run
// failed or was cancelled, and nothing reads the run's outcome again. Callers that reopen a run
// read it in their transaction, under the run lock. A handling link is inserted together with its
// run; a decomposition link right after its run is created pending and before it starts, so a run
// that can be reopened, one with a failed or cancelled attempt, has its link already.
func runTaskLink(ctx context.Context, q queryer, runID string) (string, error) {
	var kind string
	if err := q.QueryRow(ctx, `SELECT CASE
		WHEN EXISTS (SELECT 1 FROM v2_task_workflow_runs WHERE run_id = $1) THEN 'handle'
		WHEN EXISTS (SELECT 1 FROM v2_task_decompositions WHERE workflow_run_id = $1) THEN 'decomposition'
		ELSE '' END`, runID).Scan(&kind); err != nil {
		return "", fmt.Errorf("read task link: %w", err)
	}
	return kind, nil
}

// WorkflowResumeState reads a run's resume inputs without locking, for the preview. The resume
// itself replans from the locked rows, so a stale preview cannot bypass a rule.
func (s *Store) WorkflowResumeState(ctx context.Context, runID string) (WorkflowResumeState, error) {
	var status string
	err := s.pool.QueryRow(ctx, `SELECT status FROM v2_workflow_runs WHERE id = $1 AND deleted_at IS NULL`, runID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowResumeState{}, ErrWorkflowRunNotFound
	}
	if err != nil {
		return WorkflowResumeState{}, fmt.Errorf("read run status: %w", err)
	}
	return readWorkflowResumeState(ctx, s.pool, runID, status)
}

// ResumeWorkflowEngineRun is desktop's resume transaction (desktop@ebd5b725
// crates/db/src/repository/workflow_run_engine/resume.rs:12-146) on v2's rows. Under the run lock
// it hands the live state to plan, which returns the node ids to clear or an error that aborts
// the transaction unchanged. It then closes the rounds of every cleared Loop row and soft-deletes
// their attempts, soft-deletes every live attempt of a cleared node in any scope, applies clear to
// the run payload and returns the run to running with no current node, no error and no finish
// instant. When starter is nonempty and the run has no started_by yet, it records starter in the
// same transaction so a refused resume never alters started_by. Soft-deleted attempts keep counting
// toward attempt numbers, so the next attempt of a cleared node continues the numbering. The
// workspace is not touched (keep-files only).
func (s *Store) ResumeWorkflowEngineRun(
	ctx context.Context, runID, starter string,
	plan func(WorkflowResumeState) ([]string, error),
	clear func(payload []byte, cleared []string) ([]byte, error),
) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := lockEngineRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		st, err := readWorkflowResumeState(ctx, tx, runID, r.status)
		if err != nil {
			return err
		}
		ids, err := plan(st)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return ErrWorkflowRunState
		}
		// A cleared Loop takes its rounds with it, so the rerun starts from round 1 and no round
		// of the old Loop row stays reserved by v2_workflow_scopes_one_active_round.
		const clearedOwners = `SELECT id FROM v2_workflow_node_runs
			WHERE run_id = $1 AND deleted_at IS NULL AND node_id = ANY($2)`
		if _, err := tx.Exec(ctx, `
			UPDATE v2_workflow_node_runs SET deleted_at = now(), updated_at = now()
			WHERE run_id = $1 AND deleted_at IS NULL AND scope_id IN (
				SELECT id FROM v2_workflow_scopes WHERE parent_loop_node_run_id IN (`+clearedOwners+`))`,
			runID, ids); err != nil {
			return fmt.Errorf("clear loop round attempts: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE v2_workflow_scopes SET status = 'cancelled', updated_at = now()
			WHERE status IN ('pending', 'running') AND parent_loop_node_run_id IN (`+clearedOwners+`)`,
			runID, ids); err != nil {
			return fmt.Errorf("close loop rounds: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE v2_workflow_node_runs SET deleted_at = now(), updated_at = now()
			WHERE run_id = $1 AND deleted_at IS NULL AND node_id = ANY($2)`, runID, ids); err != nil {
			return fmt.Errorf("clear node attempts: %w", err)
		}
		payload, err := clear(r.payload, ids)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE v2_workflow_runs
			SET status = 'running', payload = $2, error = NULL, finished_at = NULL,
				state = '{"current_nodes":[]}'::jsonb,
				started_by = COALESCE(started_by, NULLIF($3, '')::uuid),
				updated_at = now()
			WHERE id = $1`, runID, payload, starter); err != nil {
			return fmt.Errorf("resume run: %w", err)
		}
		return nil
	})
}
