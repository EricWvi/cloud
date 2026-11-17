package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// WorkflowSessionSlotWait is what a running Agent attempt records in its payload under
// `session_slot_wait` while it queues for one of its run's session slots: the run already holds
// Limit unfinished sessions on its checkout, so the attempt has no session yet and starts once one
// ends. Since is when it began waiting, in Unix milliseconds.
type WorkflowSessionSlotWait struct {
	Since int64 `json:"since"`
	Limit int   `json:"limit"`
}

// MarkWorkflowNodeAwaitingSessionSlot records wait on a running attempt. An attempt that is no
// longer running (its run was cancelled or failed meanwhile) keeps its payload unchanged.
func (s *Store) MarkWorkflowNodeAwaitingSessionSlot(ctx context.Context, runID, nodeRunID string, wait WorkflowSessionSlotWait) error {
	marker, err := json.Marshal(wait)
	if err != nil {
		return fmt.Errorf("encode session slot wait: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE v2_workflow_node_runs
		SET payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object('session_slot_wait', $3::jsonb), updated_at = now()
		WHERE id = $1 AND run_id = $2 AND status = 'running' AND deleted_at IS NULL`, nodeRunID, runID, marker); err != nil {
		return fmt.Errorf("mark workflow node awaiting a session slot: %w", err)
	}
	return nil
}

// ClearWorkflowNodeSessionSlotWait drops the marker MarkWorkflowNodeAwaitingSessionSlot left,
// whatever the attempt's status: one cancelled while it waited must not keep reading as queued.
// Binding the attempt's session drops it as well (see BindWorkflowNodeSession).
func (s *Store) ClearWorkflowNodeSessionSlotWait(ctx context.Context, runID, nodeRunID string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE v2_workflow_node_runs
		SET payload = payload - 'session_slot_wait', updated_at = now()
		WHERE id = $1 AND run_id = $2 AND payload ? 'session_slot_wait'`, nodeRunID, runID); err != nil {
		return fmt.Errorf("clear the session slot wait of a workflow node: %w", err)
	}
	return nil
}
