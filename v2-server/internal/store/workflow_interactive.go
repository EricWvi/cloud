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
// keys describing the park travel with the status flip so a pending row always carries them.
// Moving a node to the status it has (pending or running) refreshes its keys in place. It
// returns false, changing nothing, when the run is no longer running or the attempt is not in
// from, so a park racing a cancellation or a completion never resurrects the node.
func (s *Store) MoveWorkflowEngineNode(ctx context.Context, runID, nodeRunID, from, to string, merge func(node []byte) ([]byte, error)) (bool, error) {
	live := func(status string) bool { return status == WorkflowStatusRunning || status == WorkflowStatusPending }
	if !live(from) || !live(to) {
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
// input and keep the run from being deleted. It reports whether it cancelled the attempt; one that
// already ended keeps its outcome. While the run is running or pending the attempt is still its
// run's, not an orphan: nothing changes and the error is ErrWorkflowRunActive, so a caller that
// read the run as stopped learns in the same transaction that it runs again (it was resumed).
func (s *Store) CloseOrphanedWorkflowNode(ctx context.Context, runID, nodeRunID string) (bool, error) {
	closed := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := lockEngineRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if r.status == WorkflowStatusRunning || r.status == WorkflowStatusPending {
			return ErrWorkflowRunActive
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

// OpenWorkflowSession is an execution session still open whose workflow node no longer waits on
// it, with the scope that owns it.
type OpenWorkflowSession struct {
	TenantID, UserID, SessionID string
}

// UnparkedWorkflowSessions lists the open execution sessions bound to a workflow node that is not
// parked awaiting input in a running run. On startup execution recovery keeps the held session of
// every node parked in a running run; the workflow host ends those whose node the restart sweep
// failed, or whose run it failed, so no session outlives the node it served. A non-empty runID
// limits the list to that run's nodes.
func (s *Store) UnparkedWorkflowSessions(ctx context.Context, runID string) ([]OpenWorkflowSession, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT e.tenant_id::text, e.user_id::text, e.id
		FROM v2_execution_sessions e
		JOIN v2_workflow_node_runs n ON n.session_id = e.id
		JOIN v2_workflow_runs r ON r.id = n.run_id
		WHERE e.ended_at IS NULL
			AND NOT (n.status = 'pending' AND n.deleted_at IS NULL AND r.status = 'running' AND r.deleted_at IS NULL)
			AND ($1 = '' OR r.id::text = $1)
		ORDER BY e.id`, runID)
	if err != nil {
		return nil, fmt.Errorf("list unparked workflow sessions: %w", err)
	}
	sessions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (OpenWorkflowSession, error) {
		var o OpenWorkflowSession
		return o, row.Scan(&o.TenantID, &o.UserID, &o.SessionID)
	})
	if err != nil {
		return nil, fmt.Errorf("list unparked workflow sessions: %w", err)
	}
	return sessions, nil
}
