package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RetryAbandoned is the error a waiting retry records when its run ends before the retry starts
// (desktop@ebd5b725 crates/db/src/repository/workflow_run_engine/retry.rs:19).
const RetryAbandoned = `{"reason":"retry_abandoned"}`

// waitingRetry selects a run's attempts that still wait for their retry deadline: running,
// never started, and carrying the wait marker the engine armed.
const waitingRetry = `status = 'running' AND started_at IS NULL AND deleted_at IS NULL AND payload ? 'retry_wait'`

// abandonRetryWaits settles every waiting retry of a run that leaves running inside tx, so no
// pending retry reads as live under a terminal run (desktop@ebd5b725
// crates/db/src/repository/workflow_run_engine/scoped_node.rs:180-187, retry.rs:221-243). The wait
// marker is dropped; other payload members stay for diagnosis.
func abandonRetryWaits(ctx context.Context, tx pgx.Tx, runID string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE v2_workflow_node_runs
		SET status = 'cancelled', error = $2, payload = payload - 'retry_wait', finished_at = now(), updated_at = now()
		WHERE run_id = $1 AND `+waitingRetry, runID, RetryAbandoned); err != nil {
		return fmt.Errorf("abandon retry waits: %w", err)
	}
	return nil
}

// AbandonWorkflowRetryWait cancels one waiting retry whose run is no longer running and reports
// whether it did. It is the wake-time safety net for a run that left running without settling its
// waits (desktop@ebd5b725 crates/db/src/repository/workflow_run_engine/retry.rs:174-191); a
// running run, or a row that no longer waits, is left untouched.
func (s *Store) AbandonWorkflowRetryWait(ctx context.Context, runID, nodeRunID string) (bool, error) {
	abandoned := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := lockEngineRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if r.status == WorkflowStatusRunning {
			return nil
		}
		tag, err := tx.Exec(ctx, `
			UPDATE v2_workflow_node_runs
			SET status = 'cancelled', error = $3, payload = payload - 'retry_wait', finished_at = now(), updated_at = now()
			WHERE run_id = $1 AND id = $2 AND `+waitingRetry, runID, nodeRunID, RetryAbandoned)
		if err != nil {
			return fmt.Errorf("abandon retry wait: %w", err)
		}
		abandoned = tag.RowsAffected() > 0
		return nil
	})
	return abandoned, err
}
