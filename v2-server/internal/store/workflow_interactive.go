package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// MoveWorkflowEngineNode moves one attempt of a running run between running and pending, merging
// merge's result into its payload in the same transaction. An interactive Agent node parks
// (running → pending) after each turn and takes a human turn (pending → running); the payload
// keys describing the park travel with the status flip so a pending row always carries them. It
// returns false, changing nothing, when the run is no longer running or the attempt is not in
// from, so a park racing a cancellation or a completion never resurrects the node.
func (s *Store) MoveWorkflowEngineNode(ctx context.Context, runID, nodeRunID, from, to string, merge func(node []byte) ([]byte, error)) (bool, error) {
	if !(from == WorkflowStatusRunning && to == WorkflowStatusPending) && !(from == WorkflowStatusPending && to == WorkflowStatusRunning) {
		return false, fmt.Errorf("move workflow node from %s to %s: %w", from, to, ErrWorkflowNodeRunState)
	}
	moved := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := lockEngineRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		node, err := lockWorkflowNodeRun(ctx, tx, r.tenant, runID, nodeRunID)
		if errors.Is(err, ErrWorkflowNodeRunNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if r.status != WorkflowStatusRunning || node.Status != from {
			return nil
		}
		payload, err := merge(node.Payload)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE v2_workflow_node_runs SET status = $2, payload = $3, updated_at = now()
			WHERE id = $1`, nodeRunID, to, payload); err != nil {
			return fmt.Errorf("move workflow node: %w", err)
		}
		moved = true
		return nil
	})
	return moved, err
}

// CloseOrphanedWorkflowNode cancels an attempt still pending or running under a run that is no
// longer running, e.g. an interactive node parked while a sibling failed the run. Failing a run does
// not settle its other attempts, and a pending row under a finished run would both read as awaiting
// input and keep the run from being deleted. It returns false while the run is still running.
func (s *Store) CloseOrphanedWorkflowNode(ctx context.Context, runID, nodeRunID string) (bool, error) {
	closed := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := lockEngineRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if r.status == WorkflowStatusRunning || r.status == WorkflowStatusPending {
			return nil
		}
		tag, err := tx.Exec(ctx, `
			UPDATE v2_workflow_node_runs SET status = 'cancelled', finished_at = now(), updated_at = now()
			WHERE id = $1 AND run_id = $2 AND status IN ('pending', 'running') AND deleted_at IS NULL`, nodeRunID, runID)
		if err != nil {
			return fmt.Errorf("close orphaned workflow node: %w", err)
		}
		closed = tag.RowsAffected() == 1
		return nil
	})
	return closed, err
}
