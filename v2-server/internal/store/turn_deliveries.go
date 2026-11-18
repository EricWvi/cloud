package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
)

// DeliveryStatus describes the durable outcome of a workspace delivery.
type DeliveryStatus string

const (
	// DeliveryDelivered identifies a snapshot with an immutable mirror ref.
	DeliveryDelivered DeliveryStatus = "delivered"
	// DeliveryEmpty identifies a snapshot whose tree equals its base.
	DeliveryEmpty DeliveryStatus = "empty"
	// DeliveryFailed identifies an unsuccessful snapshot.
	DeliveryFailed DeliveryStatus = "failed"
)

// DeliverySource selects a specific page source within the reader's tenant.
// TaskID is required for a task run.
type DeliverySource struct {
	Kind       execution.CallerKind
	ID, TaskID string
}

// DeliveryRecord is a retained outcome. MirrorPath is for server-side readers only.
type DeliveryRecord struct {
	ID             string         `json:"id"`
	TurnID         string         `json:"turn_id"`
	SessionID      string         `json:"session_id"`
	TurnPosition   int64          `json:"turn_position"`
	NodeID         string         `json:"node_id"`
	NodeRunID      string         `json:"node_run_id"`
	BaseCommit     string         `json:"base_commit"`
	DeliveryCommit string         `json:"delivery_commit"`
	Status         DeliveryStatus `json:"status"`
	ReasonCode     string         `json:"reason_code"`
	Reason         string         `json:"reason"`
	CreatedAt      time.Time      `json:"created_at"`
	ProjectID      string         `json:"project_id"`
	MirrorPath     string         `json:"-"`
}

// DeliveryView is a single database snapshot of the source's delivery state.
type DeliveryView struct {
	Latest, Good, Failed *DeliveryRecord
	InProgress           bool
}

// DeliveryWork captures trusted admission identity, terminal result, and the current checkout.
type DeliveryWork struct {
	Scope                                               execution.Scope
	Ref                                                 execution.Ref
	Kind                                                execution.CallerKind
	SourceID, ProjectID, WorkspaceID, NodeID, NodeRunID string
	Position                                            int64
	Result                                              *execution.Result
	ObservedAt                                          *time.Time
	Workspace                                           WorkspaceLocation
	MirrorPath                                          string
}

func deliveryNodeRun(c execution.Caller) string {
	id, _, _ := strings.Cut(c.AttemptID, "/turn-")
	return id
}

func enrollDelivery(ctx context.Context, tx pgx.Tx, scope execution.Scope, session execution.Session, turn execution.Turn) error {
	c := turn.Caller
	if session.Target == nil || session.WorkspaceID == "" || !accessUUID.MatchString(c.ID) || !accessUUID.MatchString(session.Target.ProjectID) {
		return nil
	}
	var source string
	args := []any{turn.Ref.TurnID, scope.TenantID, session.Target.ProjectID, string(c.Kind), c.ID, session.WorkspaceID}
	switch c.Kind {
	case execution.CallerChat:
		source = `SELECT 1 FROM v2_chat_conversations WHERE id=$5::uuid AND tenant_id=$2 AND project_id=$3::uuid::text AND user_id=$7`
		args = append(args, scope.UserID)
	case execution.CallerTask:
		source = `SELECT 1 FROM v2_task_runs r JOIN v2_tasks t ON t.id=r.task_id AND t.tenant_id=r.tenant_id
			WHERE r.id=$5::uuid AND r.tenant_id=$2 AND r.started_by=$7`
		args = append(args, scope.UserID)
	case execution.CallerWorkflow:
		nodeRun := deliveryNodeRun(c)
		if !accessUUID.MatchString(nodeRun) || c.NodeID == "" {
			return nil
		}
		source = `SELECT 1 FROM v2_workflow_runs r JOIN v2_workflow_node_runs n ON n.run_id=r.id AND n.tenant_id=r.tenant_id
			WHERE r.id=$5::uuid AND r.tenant_id=$2 AND r.project_id=$3::uuid::text AND n.id=$7 AND n.node_id=$8`
		args = append(args, nodeRun, c.NodeID)
	default:
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO v2_turn_delivery_work(turn_id,tenant_id,project_id,source_kind,source_id,workspace_id)
		SELECT $1,$2,$3,$4,$5,$6 WHERE EXISTS (`+source+`) ON CONFLICT (turn_id) DO NOTHING`, args...)
	return err
}

// StageDeliveryTerminal retains the first terminal outcome before its driver event is acknowledged.
// A non-enrolled turn uses the ordinary terminal publication path.
func (s *Store) StageDeliveryTerminal(ctx context.Context, scope execution.Scope, ref execution.Ref, result execution.Result) (execution.Result, error) {
	var actual execution.Result
	err := s.executionRuntimeTransaction(ctx, scope, ref.SessionID, func(tx pgx.Tx, _ execution.Session) error {
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT w.terminal_result FROM v2_turn_delivery_work w
			JOIN v2_execution_turns t ON t.id=w.turn_id WHERE t.id=$1 AND t.session_id=$2`, ref.TurnID, ref.SessionID).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			actual, err = finishExecutionTurn(ctx, tx, ref, result)
			return err
		}
		if err != nil {
			return err
		}
		if raw != nil {
			return json.Unmarshal(raw, &actual)
		}
		if err := validateTerminalResult(&result); err != nil {
			return err
		}
		actual = result
		_, err = tx.Exec(ctx, `UPDATE v2_turn_delivery_work SET terminal_result=$2,observed_at=now() WHERE turn_id=$1`, ref.TurnID, result)
		return err
	})
	return actual, err
}

const deliveryWorkSelect = `SELECT w.tenant_id,s.user_id,t.session_id,t.id,w.source_kind,w.source_id,w.project_id,w.workspace_id,
	t.position,t.caller,w.terminal_result,w.observed_at,COALESCE(p.data->>'MirrorPath',''),
	COALESCE(a.path,''),COALESCE(a.base_commit,''),COALESCE(a.clone_execution,'')
	FROM v2_turn_delivery_work w JOIN v2_execution_turns t ON t.id=w.turn_id
	JOIN v2_execution_sessions s ON s.id=t.session_id AND s.tenant_id=w.tenant_id
	LEFT JOIN v2_projects p ON p.id=w.project_id::text AND p.tenant_id=w.tenant_id
	LEFT JOIN v2_workspaces a ON a.id=w.workspace_id`

func scanDeliveryWork(row pgx.Row) (DeliveryWork, error) {
	var w DeliveryWork
	var caller execution.Caller
	err := row.Scan(&w.Scope.TenantID, &w.Scope.UserID, &w.Ref.SessionID, &w.Ref.TurnID, &w.Kind, &w.SourceID, &w.ProjectID,
		&w.WorkspaceID, &w.Position, &caller, &w.Result, &w.ObservedAt, &w.MirrorPath,
		&w.Workspace.Path, &w.Workspace.BaseCommit, &w.Workspace.CloneExecution)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if err == nil && w.Kind == execution.CallerWorkflow {
		w.NodeID, w.NodeRunID = caller.NodeID, deliveryNodeRun(caller)
	}
	return w, err
}

// DeliveryWork finds an enrolled turn's trusted delivery inputs.
func (s *Store) DeliveryWork(ctx context.Context, ref execution.Ref) (DeliveryWork, error) {
	return scanDeliveryWork(s.pool.QueryRow(ctx, deliveryWorkSelect+` WHERE t.id=$1 AND t.session_id=$2`, ref.TurnID, ref.SessionID))
}

// PendingTurnDeliveries lists obligations whose executions were already concluded by boot.
func (s *Store) PendingTurnDeliveries(ctx context.Context) ([]DeliveryWork, error) {
	rows, err := s.pool.Query(ctx, deliveryWorkSelect+` WHERE t.ended_at IS NOT NULL ORDER BY t.ended_at,t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var work []DeliveryWork
	for rows.Next() {
		w, err := scanDeliveryWork(rows)
		if err != nil {
			return nil, err
		}
		work = append(work, w)
	}
	return work, rows.Err()
}

// ErrDeliveryWorkspaceChanged signals a checkout replacement before delivery settled.
var ErrDeliveryWorkspaceChanged = errors.New("delivery workspace changed")

// CommitTurnDelivery stores the first outcome and publishes the staged execution result atomically.
// A delivered record must be supplied only after its exact ref was published successfully.
func (s *Store) CommitTurnDelivery(ctx context.Context, work DeliveryWork, record DeliveryRecord) (DeliveryRecord, error) {
	var winner DeliveryRecord
	err := s.executionRuntimeTransaction(ctx, work.Scope, work.Ref.SessionID, func(tx pgx.Tx, _ execution.Session) error {
		err := tx.QueryRow(ctx, `SELECT to_jsonb(d) FROM v2_turn_deliveries d WHERE d.turn_id=$1 AND d.session_id=$2`, work.Ref.TurnID, work.Ref.SessionID).Scan(&winner)
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if record.Status != DeliveryFailed && work.Workspace.Path != "" {
			var at WorkspaceLocation
			err := tx.QueryRow(ctx, `SELECT path,base_commit,clone_execution FROM v2_workspaces WHERE id=$1 FOR SHARE`, work.WorkspaceID).
				Scan(&at.Path, &at.BaseCommit, &at.CloneExecution)
			if errors.Is(err, pgx.ErrNoRows) || err == nil && at != work.Workspace {
				return ErrDeliveryWorkspaceChanged
			}
			if err != nil {
				return err
			}
		}
		err = tx.QueryRow(ctx, `INSERT INTO v2_turn_deliveries
			(id,turn_id,session_id,tenant_id,project_id,source_kind,source_id,workspace_id,turn_position,node_id,node_run_id,
			 base_commit,delivery_commit,status,reason_code,reason)
			SELECT $1,w.turn_id,t.session_id,w.tenant_id,w.project_id,w.source_kind,w.source_id,w.workspace_id,t.position,
			 NULLIF($4,''),NULLIF($5,'')::uuid,NULLIF($6,''),NULLIF($7,''),$8,NULLIF($9,''),NULLIF($10,'')
			FROM v2_turn_delivery_work w JOIN v2_execution_turns t ON t.id=w.turn_id
			WHERE t.id=$2 AND t.session_id=$3 AND (w.terminal_result IS NOT NULL OR t.ended_at IS NOT NULL)
			ON CONFLICT (turn_id) DO NOTHING RETURNING created_at`,
			record.ID, work.Ref.TurnID, work.Ref.SessionID, work.NodeID, work.NodeRunID,
			record.BaseCommit, record.DeliveryCommit, record.Status, record.ReasonCode, record.Reason).Scan(&winner.CreatedAt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err = tx.QueryRow(ctx, `SELECT to_jsonb(d) FROM v2_turn_deliveries d WHERE d.turn_id=$1 AND d.session_id=$2`, work.Ref.TurnID, work.Ref.SessionID).Scan(&winner); err != nil {
			return err
		}
		var result *execution.Result
		err = tx.QueryRow(ctx, `DELETE FROM v2_turn_delivery_work WHERE turn_id=$1 RETURNING terminal_result`, work.Ref.TurnID).Scan(&result)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil || result == nil {
			return err
		}
		_, err = finishExecutionTurn(ctx, tx, work.Ref, *result)
		return err
	})
	return winner, err
}

func deliverySourceQuery(source DeliverySource) (string, error) {
	if !accessUUID.MatchString(source.ID) {
		return "", ErrNotFound
	}
	switch source.Kind {
	case execution.CallerChat:
		return `SELECT id FROM v2_chat_conversations WHERE tenant_id=$1 AND user_id=$2 AND id=$3`, nil
	case execution.CallerTask:
		if accessUUID.MatchString(source.TaskID) {
			return `SELECT r.id FROM v2_task_runs r JOIN v2_tasks t ON t.id=r.task_id AND t.tenant_id=r.tenant_id
				WHERE r.tenant_id=$1 AND r.id=$3 AND t.id=$4 AND $2::uuid IS NOT NULL`, nil
		}
	case execution.CallerWorkflow:
		return `SELECT id FROM v2_workflow_runs WHERE tenant_id=$1 AND id=$3 AND deleted_at IS NULL AND $2::uuid IS NOT NULL`, nil
	}
	return "", ErrNotFound
}

// WorkspaceDeliveryView returns the latest outcome, good outcome, failed outcome, and active or
// delivering flag from one snapshot for an authorized source. Missing outcomes are nil.
func (s *Store) WorkspaceDeliveryView(ctx context.Context, scope execution.Scope, source DeliverySource) (DeliveryView, error) {
	var view DeliveryView
	sourceSQL, err := deliverySourceQuery(source)
	if err != nil {
		return view, err
	}
	args := []any{scope.TenantID, scope.UserID, source.ID, string(source.Kind)}
	if source.Kind == execution.CallerTask {
		sourceSQL = strings.ReplaceAll(sourceSQL, "$4", "$5")
		args = append(args, source.TaskID)
	}
	if _, err := tenantAccess(ctx, s.pool, scope.TenantID, scope.UserID); err != nil {
		return view, err
	}
	lookup := func(condition string) string {
		return `(SELECT to_jsonb(d) FROM v2_turn_deliveries d
			WHERE d.tenant_id=$1 AND d.source_kind=$4 AND d.source_id=src.id ` + condition + `
			ORDER BY d.sequence DESC LIMIT 1)`
	}
	var latest, good, failed *DeliveryRecord
	var latestMirror, goodMirror, failedMirror string
	err = s.pool.QueryRow(ctx, `WITH src AS (`+sourceSQL+`), outcomes AS (
		SELECT `+lookup("")+` AS latest,`+lookup("AND d.status IN ('delivered','empty')")+` AS good,`+
		lookup("AND d.status='failed'")+` AS failed,
		EXISTS (SELECT 1 FROM v2_turn_delivery_work w WHERE w.tenant_id=$1 AND w.source_kind=$4 AND w.source_id=src.id) AS active
		FROM src)
		SELECT o.latest,o.good,o.failed,o.active,COALESCE(l.data->>'MirrorPath',''),
			COALESCE(g.data->>'MirrorPath',''),COALESCE(f.data->>'MirrorPath','')
		FROM outcomes o
		LEFT JOIN v2_projects l ON l.id=o.latest->>'project_id' AND l.tenant_id=$1
		LEFT JOIN v2_projects g ON g.id=o.good->>'project_id' AND g.tenant_id=$1
		LEFT JOIN v2_projects f ON f.id=o.failed->>'project_id' AND f.tenant_id=$1`, args...).Scan(
		&latest, &good, &failed, &view.InProgress, &latestMirror, &goodMirror, &failedMirror)
	if errors.Is(err, pgx.ErrNoRows) {
		return view, ErrNotFound
	}
	if err != nil {
		return view, err
	}
	if latest != nil {
		latest.MirrorPath = latestMirror
	}
	if good != nil {
		good.MirrorPath = goodMirror
	}
	if failed != nil {
		failed.MirrorPath = failedMirror
	}
	view.Latest, view.Good, view.Failed = latest, good, failed
	return view, nil
}

// WorkspaceDeliveryByID returns an individual delivery in the authorized source, including its
// mirror path for internal Git reads. IDs belonging to other sources return ErrNotFound.
func (s *Store) WorkspaceDeliveryByID(ctx context.Context, scope execution.Scope, source DeliverySource, id string) (DeliveryRecord, error) {
	var record DeliveryRecord
	sourceSQL, err := deliverySourceQuery(source)
	if err != nil || !accessUUID.MatchString(id) {
		return record, ErrNotFound
	}
	args := []any{scope.TenantID, scope.UserID, source.ID, string(source.Kind), id}
	if source.Kind == execution.CallerTask {
		sourceSQL = strings.ReplaceAll(sourceSQL, "$4", "$6")
		args = append(args, source.TaskID)
	}
	if _, err := tenantAccess(ctx, s.pool, scope.TenantID, scope.UserID); err != nil {
		return record, err
	}
	var mirror string
	err = s.pool.QueryRow(ctx, `WITH src AS (`+sourceSQL+`) SELECT to_jsonb(d),COALESCE(p.data->>'MirrorPath','')
		FROM src JOIN v2_turn_deliveries d ON d.tenant_id=$1 AND d.source_kind=$4 AND d.source_id=src.id AND d.id=$5
		LEFT JOIN v2_projects p ON p.id=d.project_id::text AND p.tenant_id=d.tenant_id`, args...).Scan(&record, &mirror)
	if errors.Is(err, pgx.ErrNoRows) {
		return record, ErrNotFound
	}
	record.MirrorPath = mirror
	return record, err
}

func recoverDeliveryWork(ctx context.Context, tx pgx.Tx, session execution.Session, ref execution.Ref) (*execution.Result, error) {
	var scope execution.Scope
	var terminal *execution.Result
	if err := tx.QueryRow(ctx, `SELECT t.tenant_id,t.user_id,w.terminal_result FROM v2_execution_turns t
		LEFT JOIN v2_turn_delivery_work w ON w.turn_id=t.id WHERE t.id=$1 AND t.session_id=$2`,
		ref.TurnID, ref.SessionID).Scan(&scope.TenantID, &scope.UserID, &terminal); err != nil {
		return nil, err
	}
	if terminal != nil || session.Target == nil {
		return terminal, nil
	}
	turn, err := readExecutionTurn(ctx, tx, ref)
	if err != nil {
		return nil, err
	}
	return nil, enrollDelivery(ctx, tx, scope, session, turn)
}
