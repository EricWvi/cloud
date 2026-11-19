package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// DiscardUnstartedCommentWorkflow releases a refused admission's key only when execution never
// started. The run lock fences engine admission; anything with execution history or soft-deletion
// is retained.
func (s *Store) DiscardUnstartedCommentWorkflow(ctx context.Context, tenant, taskID, author, key, runID string) (bool, error) {
	discarded := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var eligible bool
		err := tx.QueryRow(ctx, `SELECT status = 'pending' AND started_at IS NULL AND deleted_at IS NULL
			FROM v2_workflow_runs WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenant, runID).Scan(&eligible)
		if err != nil {
			return err
		}
		if !eligible {
			return nil
		}
		var hasNodes bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM v2_workflow_node_runs WHERE run_id = $1)`, runID).Scan(&hasNodes); err != nil {
			return err
		}
		if hasNodes {
			return nil
		}
		tag, err := tx.Exec(ctx, `DELETE FROM v2_task_comments
			WHERE tenant_id = $1 AND task_id = $2 AND author_id = $3 AND create_key = $4 AND workflow_run_id = $5`,
			tenant, taskID, author, key, runID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrNotFound
		}
		for _, query := range []string{
			`DELETE FROM v2_workflow_run_roots WHERE run_id = $1`,
			`DELETE FROM v2_workflow_scopes WHERE run_id = $1`,
			`DELETE FROM v2_workflow_runs WHERE id = $1`,
		} {
			if _, err := tx.Exec(ctx, query, runID); err != nil {
				return err
			}
		}
		discarded = true
		return nil
	})
	return discarded && err == nil, err
}

// RecordCommentWorkflowStartFailure keeps a safe public reason on a cancelled admission that
// already entered execution. It never replaces an authoritative engine outcome or error.
func (s *Store) RecordCommentWorkflowStartFailure(ctx context.Context, tenant, runID, message string) error {
	_, err := s.pool.Exec(ctx, `UPDATE v2_workflow_runs SET error = $3, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND status = 'cancelled' AND error IS NULL`, tenant, runID, message)
	return err
}

// PendingCommentWorkflow identifies a comment-initiated workflow admission whose run has not yet
// started executing, so startup recovery can discard crash-window leftovers.
type PendingCommentWorkflow struct {
	TenantID  string
	TaskID    string
	AuthorID  string
	CreateKey string
	RunID     string
}

// PendingCommentWorkflows lists live comment-initiated workflow admissions still waiting to start.
func (s *Store) PendingCommentWorkflows(ctx context.Context) ([]PendingCommentWorkflow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.tenant_id::text, c.task_id::text, c.author_id::text, c.create_key, c.workflow_run_id::text
		FROM v2_task_comments c
		JOIN v2_workflow_runs r ON r.id = c.workflow_run_id AND r.tenant_id = c.tenant_id
		WHERE c.workflow_run_id IS NOT NULL
			AND r.deleted_at IS NULL
			AND r.status = 'pending'
			AND r.started_at IS NULL
		ORDER BY c.created_at, c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PendingCommentWorkflow{}
	for rows.Next() {
		var item PendingCommentWorkflow
		if err := rows.Scan(&item.TenantID, &item.TaskID, &item.AuthorID, &item.CreateKey, &item.RunID); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
