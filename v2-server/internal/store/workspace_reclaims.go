package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrInvalidReclaimMode reports a reclaim mode other than the two below.
var ErrInvalidReclaimMode = errors.New("invalid workspace reclaim mode")

// ReclaimMode is what the person chose for a deleted conversation's checkout. It never changes
// once recorded.
type ReclaimMode string

const (
	// ReclaimRecoverable removes the checkout only when its deliveries hold its work, and keeps it
	// otherwise. It is the default.
	ReclaimRecoverable ReclaimMode = "reclaim"
	// ReclaimDiscard removes the checkout regardless: the person chose to drop undelivered work.
	ReclaimDiscard ReclaimMode = "discard"
)

// ReclaimStatus is where a workspace reclaim stands: pending until judged, deleting once judged
// (only the path's identity is checked again from then on), reclaimed or kept when final.
type ReclaimStatus string

// Reclaim statuses.
const (
	ReclaimPending   ReclaimStatus = "pending"
	ReclaimDeleting  ReclaimStatus = "deleting"
	ReclaimReclaimed ReclaimStatus = "reclaimed"
	ReclaimKept      ReclaimStatus = "kept"
)

// SessionDelivery is what a session's turns and deliveries say about its checkout. It is read by
// session ID alone, so it still works after the conversation row is gone.
type SessionDelivery struct {
	// Busy reports a turn not ended yet or delivery work not settled (HasWork): the session's
	// delivery outcome is still to come.
	Busy bool
	// Delivered reports any delivery record of the session; without one the session predates
	// turn delivery or never ended a turn.
	Delivered bool
	// LastTurnID is the session's last turn (highest position); empty without turns.
	LastTurnID string
	// LastStatus is the delivery outcome of the last turn, empty when it has none.
	LastStatus DeliveryStatus
	// GoodDeliveryID and GoodCommit identify the newest delivered (not empty) delivery of the
	// session, the one a patch is downloaded from; GoodCommit is also what undelivered changes are
	// listed against.
	GoodDeliveryID, GoodCommit string
	// LastDeliveryID and LastCommit identify the last turn's delivery when it is good; LastCommit
	// is empty for an empty delivery.
	LastDeliveryID, LastCommit string
}

// Checkout is a session's recorded checkout and what judging it needs to read.
type Checkout struct {
	SessionID, WorkspaceID, ProjectID string
	Path, BaseCommit, CloneExecution  string
	// MirrorPath is the project's mirror, where the session's deliveries live.
	MirrorPath string
	Delivery   SessionDelivery
}

// ChatCheckout is what deleting one of the user's conversations is about. Registered is false
// when the conversation has no checkout recorded (no session yet, its first turn's outcome is
// unknown, or its checkout never became ready).
type ChatCheckout struct {
	Registered bool
	Checkout
}

// WorkspaceReclaim is one deleted conversation's checkout to remove.
type WorkspaceReclaim struct {
	WorkspaceID, TenantID, ProjectID, SessionID, ConversationID string
	Path, CloneExecution                                        string
	Mode                                                        ReclaimMode
	Status                                                      ReclaimStatus
	Reason, LastError                                           string
	Attempts                                                    int
	LastAttemptAt, RequestedAt, FinishedAt                      *time.Time
	// The delivery a reclaimed checkout relied on, for an operator to restore its work from.
	DeliveryID, DeliveryCommit, BaseCommit string
	// RemovedBytes and RemovedFiles are what the checkout held when it was removed.
	RemovedBytes, RemovedFiles int64
}

// ReclaimBasis is what a removal records: the delivery it relied on and what it released.
type ReclaimBasis struct {
	DeliveryID, DeliveryCommit, BaseCommit string
	Bytes, Files                           int64
	// Reason explains a removal that did not follow the person's choice, e.g. an operator's.
	Reason string
}

// ReclaimIdentity is the database's view of a reclaim's checkout when the reclaim is executed.
type ReclaimIdentity struct {
	// Recorded reports that v2_workspaces still records the reclaim's path and clone execution
	// for its workspace, and no other workspace records the same path.
	Recorded bool
	// Live reports an execution session pointing at the checkout that has not ended.
	Live bool
	// Shared reports another execution session that used the checkout.
	Shared bool
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// sessionDelivery reads the session's turns, deliveries and delivery work directly.
func sessionDelivery(ctx context.Context, q queryRower, sessionID string) (SessionDelivery, error) {
	var d SessionDelivery
	var turn, status, lastID, lastCommit, goodID, goodCommit *string
	err := q.QueryRow(ctx, `
		SELECT
			EXISTS (SELECT 1 FROM v2_execution_turns WHERE session_id = $1 AND ended_at IS NULL)
			OR EXISTS (SELECT 1 FROM v2_turn_delivery_work w JOIN v2_execution_turns t ON t.id = w.turn_id
				WHERE t.session_id = $1),
			EXISTS (SELECT 1 FROM v2_turn_deliveries WHERE session_id = $1),
			last.turn_id, last.status, last.id::text, last.delivery_commit, good.id::text, good.delivery_commit
		FROM (SELECT 1) one
		LEFT JOIN LATERAL (
			SELECT t.id AS turn_id, d.status, d.id, d.delivery_commit FROM v2_execution_turns t
			LEFT JOIN v2_turn_deliveries d ON d.turn_id = t.id
			WHERE t.session_id = $1 ORDER BY t.position DESC LIMIT 1) last ON true
		LEFT JOIN LATERAL (
			SELECT d.id, d.delivery_commit FROM v2_turn_deliveries d
			WHERE d.session_id = $1 AND d.status = 'delivered' ORDER BY d.sequence DESC LIMIT 1) good ON true`, sessionID).
		Scan(&d.Busy, &d.Delivered, &turn, &status, &lastID, &lastCommit, &goodID, &goodCommit)
	if err != nil {
		return d, fmt.Errorf("read session deliveries: %w", err)
	}
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	d.LastTurnID, d.LastStatus = str(turn), DeliveryStatus(str(status))
	if d.LastStatus == DeliveryDelivered || d.LastStatus == DeliveryEmpty {
		d.LastDeliveryID, d.LastCommit = str(lastID), str(lastCommit)
	}
	d.GoodDeliveryID, d.GoodCommit = str(goodID), str(goodCommit)
	return d, nil
}

const checkoutColumns = `COALESCE(s.session_id, ''), COALESCE(e.data->>'WorkspaceID', ''), COALESCE(e.data->'Target'->>'ProjectID', ''),
	COALESCE(w.path, ''), COALESCE(w.base_commit, ''), COALESCE(w.clone_execution, ''), COALESCE(p.data->>'MirrorPath', '')`

const checkoutJoins = `FROM v2_chat_conversations c
	LEFT JOIN v2_chat_sessions s ON s.conversation_id = c.id
	LEFT JOIN v2_execution_sessions e ON e.id = s.session_id
	LEFT JOIN v2_workspaces w ON w.id = e.data->>'WorkspaceID'
	LEFT JOIN v2_projects p ON p.id = e.data->'Target'->>'ProjectID' AND p.tenant_id = c.tenant_id
	WHERE c.tenant_id = $1 AND c.user_id = $2 AND c.id = $3`

func scanChatCheckout(row pgx.Row) (ChatCheckout, error) {
	var c ChatCheckout
	err := row.Scan(&c.SessionID, &c.WorkspaceID, &c.ProjectID, &c.Path, &c.BaseCommit, &c.CloneExecution, &c.MirrorPath)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, fmt.Errorf("read conversation checkout: %w", err)
	}
	c.Registered = c.SessionID != "" && c.WorkspaceID != "" && c.ProjectID != "" && c.Path != ""
	return c, nil
}

// ChatCheckout reads the checkout of one of the user's conversations, or ErrNotFound.
func (s *Store) ChatCheckout(ctx context.Context, tenant, user, conversationID string) (ChatCheckout, error) {
	if !accessUUID.MatchString(conversationID) {
		return ChatCheckout{}, ErrNotFound
	}
	c, err := scanChatCheckout(s.pool.QueryRow(ctx, `SELECT `+checkoutColumns+` `+checkoutJoins, tenant, user, conversationID))
	if err != nil || !c.Registered {
		return c, err
	}
	c.Delivery, err = sessionDelivery(ctx, s.pool, c.SessionID)
	return c, err
}

// DeleteChatConversationReclaiming is the person deleting one of their conversations: it deletes
// the conversation and, in the same transaction, records a reclaim of its checkout under mode.
// Deleting the conversation cuts the only link to its checkout, so the reclaim must be written
// first or the checkout would be left with no owner. It returns the checkout's workspace ID,
// empty when the conversation had none recorded. DeleteChatConversation, which removes a
// conversation whose first turn was refused, records nothing: that is not a person's deletion.
func (s *Store) DeleteChatConversationReclaiming(ctx context.Context, tenant, user, id string, mode ReclaimMode) (string, error) {
	if mode != ReclaimRecoverable && mode != ReclaimDiscard {
		return "", ErrInvalidReclaimMode
	}
	if !accessUUID.MatchString(id) {
		return "", ErrNotFound
	}
	var workspace string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		c, err := scanChatCheckout(tx.QueryRow(ctx, `SELECT `+checkoutColumns+` `+checkoutJoins+` FOR UPDATE OF c`, tenant, user, id))
		if err != nil {
			return err
		}
		if c.Registered {
			workspace = c.WorkspaceID
			if _, err := tx.Exec(ctx, `
				INSERT INTO v2_workspace_reclaims (workspace_id, tenant_id, project_id, session_id, conversation_id, path, clone_execution, mode)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (workspace_id) DO NOTHING`,
				c.WorkspaceID, tenant, c.ProjectID, c.SessionID, id, c.Path, c.CloneExecution, string(mode)); err != nil {
				return fmt.Errorf("record workspace reclaim: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM v2_chat_conversations WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete conversation: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return workspace, nil
}

const reclaimColumns = `workspace_id, tenant_id::text, project_id, session_id, conversation_id::text, path, clone_execution,
	mode, status, reason, last_error, attempts, last_attempt_at, requested_at, finished_at,
	COALESCE(delivery_id::text, ''), COALESCE(delivery_commit, ''), COALESCE(base_commit, ''),
	COALESCE(removed_bytes, 0), COALESCE(removed_files, 0)`

func scanReclaim(row pgx.Row) (WorkspaceReclaim, error) {
	var r WorkspaceReclaim
	var requested time.Time
	err := row.Scan(&r.WorkspaceID, &r.TenantID, &r.ProjectID, &r.SessionID, &r.ConversationID, &r.Path, &r.CloneExecution,
		&r.Mode, &r.Status, &r.Reason, &r.LastError, &r.Attempts, &r.LastAttemptAt, &requested, &r.FinishedAt,
		&r.DeliveryID, &r.DeliveryCommit, &r.BaseCommit, &r.RemovedBytes, &r.RemovedFiles)
	r.RequestedAt = &requested
	return r, err
}

// WorkspaceReclaim reads one reclaim by workspace ID, or ErrNotFound.
func (s *Store) WorkspaceReclaim(ctx context.Context, workspaceID string) (WorkspaceReclaim, error) {
	r, err := scanReclaim(s.pool.QueryRow(ctx, `SELECT `+reclaimColumns+` FROM v2_workspace_reclaims WHERE workspace_id = $1`, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, fmt.Errorf("read workspace reclaim: %w", err)
	}
	return r, nil
}

// WorkspaceReclaims lists reclaims, oldest first, for operators; statuses narrows the list and
// none lists all.
func (s *Store) WorkspaceReclaims(ctx context.Context, statuses ...ReclaimStatus) ([]WorkspaceReclaim, error) {
	filter := make([]string, len(statuses))
	for i, st := range statuses {
		filter[i] = string(st)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+reclaimColumns+` FROM v2_workspace_reclaims
		WHERE cardinality($1::text[]) = 0 OR status = ANY($1::text[]) ORDER BY requested_at, workspace_id`, filter)
	if err != nil {
		return nil, fmt.Errorf("list workspace reclaims: %w", err)
	}
	defer rows.Close()
	var out []WorkspaceReclaim
	for rows.Next() {
		r, err := scanReclaim(rows)
		if err != nil {
			return nil, fmt.Errorf("list workspace reclaims: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workspace reclaims: %w", err)
	}
	return out, nil
}

// ReclaimCheckout reads the reclaim's checkout as judging it needs: the session's deliveries by
// the recorded session ID, the base commit and the project mirror.
func (s *Store) ReclaimCheckout(ctx context.Context, r WorkspaceReclaim) (Checkout, error) {
	c := Checkout{SessionID: r.SessionID, WorkspaceID: r.WorkspaceID, ProjectID: r.ProjectID, Path: r.Path, CloneExecution: r.CloneExecution}
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT base_commit FROM v2_workspaces WHERE id = $1), ''),
			COALESCE((SELECT data->>'MirrorPath' FROM v2_projects WHERE id = $2 AND tenant_id = $3::uuid), '')`,
		r.WorkspaceID, r.ProjectID, r.TenantID).Scan(&c.BaseCommit, &c.MirrorPath)
	if err != nil {
		return c, fmt.Errorf("read reclaim checkout: %w", err)
	}
	c.Delivery, err = sessionDelivery(ctx, s.pool, r.SessionID)
	return c, err
}

// CheckReclaimIdentity checks that the reclaim's path still is its checkout's: v2_workspaces
// records the same path and clone execution for the workspace and no other workspace records
// that path; and whether any session pointing at the checkout is live, or another session used
// it. Chat workspace IDs are new for every session, so only a workflow run's sessions share one.
func (s *Store) CheckReclaimIdentity(ctx context.Context, r WorkspaceReclaim) (ReclaimIdentity, error) {
	var id ReclaimIdentity
	err := s.pool.QueryRow(ctx, `
		SELECT
			EXISTS (SELECT 1 FROM v2_workspaces WHERE id = $1 AND path = $3 AND clone_execution = $4)
			AND NOT EXISTS (SELECT 1 FROM v2_workspaces WHERE path = $3 AND id <> $1),
			EXISTS (SELECT 1 FROM v2_execution_sessions WHERE data->>'WorkspaceID' = $1 AND ended_at IS NULL),
			EXISTS (SELECT 1 FROM v2_execution_sessions WHERE data->>'WorkspaceID' = $1 AND id <> $2)`,
		r.WorkspaceID, r.SessionID, r.Path, r.CloneExecution).Scan(&id.Recorded, &id.Live, &id.Shared)
	if err != nil {
		return id, fmt.Errorf("check workspace reclaim: %w", err)
	}
	return id, nil
}

// MarkWorkspaceDeleting moves a pending reclaim to deleting, recording the delivery it relied on
// (empty for discard). It reports whether this call moved it.
func (s *Store) MarkWorkspaceDeleting(ctx context.Context, workspaceID string, basis ReclaimBasis) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE v2_workspace_reclaims SET status = 'deleting', delivery_id = NULLIF($2, '')::uuid,
			delivery_commit = NULLIF($3, ''), base_commit = NULLIF($4, '')
		WHERE workspace_id = $1 AND status = 'pending'`, workspaceID, basis.DeliveryID, basis.DeliveryCommit, basis.BaseCommit)
	if err != nil {
		return false, fmt.Errorf("mark workspace reclaim deleting: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// FinishWorkspaceReclaimed records that the checkout was removed and how much it held. A kept
// reclaim is finished too when the removal is an operator's (basis.Reason says so).
func (s *Store) FinishWorkspaceReclaimed(ctx context.Context, workspaceID string, basis ReclaimBasis) error {
	from := []string{string(ReclaimDeleting)}
	if basis.Reason != "" {
		from = append(from, string(ReclaimKept))
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE v2_workspace_reclaims SET status = 'reclaimed', reason = $2, removed_bytes = $3, removed_files = $4,
			finished_at = now(), last_attempt_at = now()
		WHERE workspace_id = $1 AND status = ANY($5::text[])`, workspaceID, basis.Reason, basis.Bytes, basis.Files, from)
	if err != nil {
		return fmt.Errorf("finish workspace reclaim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// FinishWorkspaceKept records that the checkout is kept for good, and why.
func (s *Store) FinishWorkspaceKept(ctx context.Context, workspaceID, reason string) error {
	if reason == "" {
		return errors.New("a kept workspace needs a reason")
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE v2_workspace_reclaims SET status = 'kept', reason = $2, finished_at = now(), last_attempt_at = now()
		WHERE workspace_id = $1 AND status IN ('pending', 'deleting')`, workspaceID, reason)
	if err != nil {
		return fmt.Errorf("finish workspace reclaim: %w", err)
	}
	return nil
}

// NoteWorkspaceReclaimAttempt records an attempt that left the reclaim open: lastError is empty
// when it only waited (for delivery work), otherwise the attempt failed and is counted.
func (s *Store) NoteWorkspaceReclaimAttempt(ctx context.Context, workspaceID, lastError string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE v2_workspace_reclaims SET last_attempt_at = now(),
			attempts = attempts + CASE WHEN $2 = '' THEN 0 ELSE 1 END,
			last_error = CASE WHEN $2 = '' THEN last_error ELSE $2 END
		WHERE workspace_id = $1 AND status IN ('pending', 'deleting')`, workspaceID, lastError)
	if err != nil {
		return fmt.Errorf("note workspace reclaim attempt: %w", err)
	}
	return nil
}
