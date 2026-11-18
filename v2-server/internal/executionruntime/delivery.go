package executionruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workspacefs"
)

// DeliveryFilesystem is the workspace snapshot writer used by the execution service.
type DeliveryFilesystem interface {
	LockWorkspace(ctx context.Context, root string) (func(), error)
	Deliver(ctx context.Context, req workspacefs.DeliveryRequest) (workspacefs.Snapshot, error)
}

// DeliveryEnabled reports whether this runtime owns delivery for eligible page turns.
func (s *Service) DeliveryEnabled() bool { return s.delivery != nil }

const (
	deliveryGitTimeout   = 2 * time.Minute
	deliveryWriteTimeout = 10 * time.Second
)

func retryDeliveryWrite(ctx context.Context, run func(context.Context) error) error {
	for {
		write, cancel := context.WithTimeout(ctx, deliveryWriteTimeout)
		err := run(write)
		cancel()
		if err == nil || errors.Is(err, execution.ErrInvalidRequest) || errors.Is(err, execution.ErrNotFound) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// finishDelivery keeps the execution worker and its admission slot until the record and public
// terminal result commit. Capture time and the computed ref survive transient database failures.
func (s *Service) finishDelivery(scope execution.Scope, ref execution.Ref, result execution.Result, agentErr error) (execution.Result, bool, error) {
	var work store.DeliveryWork
	var enrolled bool
	err := retryDeliveryWrite(s.ctx, func(ctx context.Context) error {
		var err error
		work, err = s.db.DeliveryWork(ctx, ref)
		enrolled = err == nil
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil || !enrolled {
		return result, enrolled || err != nil, err
	}
	err = retryDeliveryWrite(s.ctx, func(ctx context.Context) error {
		actual, err := s.db.StageDeliveryTerminal(ctx, scope, ref, result)
		if errors.Is(err, execution.ErrInvalidRequest) && result.Status != execution.StatusFailed {
			result.Status, result.FailureCode = execution.StatusFailed, "execution_failed"
			actual, err = s.db.StageDeliveryTerminal(ctx, scope, ref, result)
		}
		if err != nil {
			return err
		}
		result = actual
		work, err = s.db.DeliveryWork(ctx, ref)
		return err
	})
	if err != nil {
		return result, true, err
	}
	// A protocol failure can report a terminal outcome before the Agent stopped writing. A
	// successful protocol terminal has no such ambiguity. Do not take a misleading snapshot.
	unstable := false
	if agentErr != nil && !result.NotStarted && !errors.Is(agentErr, execution.ErrSessionEnded) {
		unstable = s.stopAgent(s.ctx, ref.SessionID) != nil
	}
	return result, true, s.publishDelivery(s.ctx, work, unstable)
}

func deliveryFailure(record store.DeliveryRecord, code, reason string) store.DeliveryRecord {
	record.Status, record.ReasonCode, record.Reason = store.DeliveryFailed, code, reason
	return record
}

func (s *Service) publishDelivery(ctx context.Context, work store.DeliveryWork, unstable bool) error {
	key := work.Workspace.Path
	if key == "" {
		key = work.WorkspaceID
	}
	unlock, lockErr := s.delivery.LockWorkspace(ctx, key)
	if lockErr == nil {
		defer unlock()
	} else if ctx.Err() != nil {
		return ctx.Err()
	}
	// Another finalizer can win before this one acquires the lock. Its PostgreSQL outcome is
	// authoritative and avoids creating an orphan during ordinary duplicate notifications.
	var current store.DeliveryWork
	var found bool
	err := retryDeliveryWrite(ctx, func(ctx context.Context) error {
		var err error
		current, err = s.db.DeliveryWork(ctx, work.Ref)
		found = err == nil
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil || !found {
		return err
	}
	record := store.DeliveryRecord{ID: newID(), BaseCommit: work.Workspace.BaseCommit}
	switch {
	case unstable || current.Workspace != work.Workspace:
		record = deliveryFailure(record, "workspace_unstable", "工作区状态无法确认")
	case work.Workspace.Path == "":
		record = deliveryFailure(record, "workspace_not_ready", "工作区尚未就绪")
	case lockErr != nil || work.MirrorPath == "":
		record = deliveryFailure(record, "delivery_unavailable", "无法保存本轮交付")
	default:
		message := fmt.Sprintf("Ora delivery: %s %s turn %d (%s)", work.Kind, work.SourceID, work.Position, work.Ref.TurnID)
		if work.NodeRunID != "" {
			message += fmt.Sprintf(" node %s attempt %s", work.NodeID, work.NodeRunID)
		}
		req := workspacefs.DeliveryRequest{
			Workspace:  workspacefs.Workspace{Root: work.Workspace.Path, BaseCommit: work.Workspace.BaseCommit},
			MirrorPath: work.MirrorPath, ProjectID: work.ProjectID, DeliveryID: record.ID, Message: message,
		}
		if work.ObservedAt != nil {
			req.ObservedAt = *work.ObservedAt
		}
		budget, stop := context.WithTimeout(ctx, deliveryGitTimeout)
		snapshot, err := s.delivery.Deliver(budget, req)
		stop()
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, workspacefs.ErrGone):
			record = deliveryFailure(record, "workspace_removed", "工作区已移除")
		case errors.Is(err, workspacefs.ErrDeliveryTooLarge):
			record = deliveryFailure(record, "changes_too_large", "改动过大")
		case err != nil:
			slog.Warn("turn delivery Git operation failed", "turn", work.Ref.TurnID, "error", err)
			record = deliveryFailure(record, "delivery_unavailable", "无法保存本轮交付")
		case snapshot.Commit == "":
			record.Status = store.DeliveryEmpty
		default:
			record.Status, record.DeliveryCommit = store.DeliveryDelivered, snapshot.Commit
		}
	}
	if !validDeliveryBase(record.BaseCommit) {
		record.BaseCommit = ""
	}
	return retryDeliveryWrite(ctx, func(ctx context.Context) error {
		_, err := s.db.CommitTurnDelivery(ctx, work, record)
		if errors.Is(err, store.ErrDeliveryWorkspaceChanged) {
			// Leave an already created ref orphaned: it no longer describes the turn's binding.
			record = deliveryFailure(store.DeliveryRecord{ID: newID(), BaseCommit: record.BaseCommit}, "workspace_unstable", "工作区状态无法确认")
			_, err = s.db.CommitTurnDelivery(ctx, work, record)
		}
		return err
	})
}

func validDeliveryBase(base string) bool {
	return (len(base) == 40 || len(base) == 64) && strings.Trim(base, "0123456789abcdef") == ""
}

// RecoverTurnDeliveries publishes outstanding snapshots after boot concludes old executions.
// It runs synchronously before resumed workflows or HTTP admission can modify their checkouts.
func (s *Service) RecoverTurnDeliveries(ctx context.Context) error {
	if s.delivery == nil {
		return nil
	}
	work, err := s.db.PendingTurnDeliveries(ctx)
	if err != nil {
		return fmt.Errorf("list interrupted turn deliveries: %w", err)
	}
	for _, turn := range work {
		budget, cancel := context.WithTimeout(ctx, deliveryGitTimeout+30*time.Second)
		err := s.publishDelivery(budget, turn, false)
		cancel()
		if err != nil {
			return fmt.Errorf("recover turn delivery %s: %w", turn.Ref.TurnID, err)
		}
	}
	return nil
}
