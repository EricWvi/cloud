package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
)

// mcpTargetMember is the value-free desired identity. An unavailable member retains its slot.
type mcpTargetMember struct {
	State    string `json:"state"`
	Version  string `json:"plugin_version,omitempty"`
	Revision int64  `json:"configuration_revision,omitempty"`
	Code     string `json:"code,omitempty"`
}

// initialMCPTarget fences the resolver's snapshot against concurrent configuration changes.
// Share locks are taken in canonical plugin order and held until generation zero and its
// first turn commit. A writer that wins first causes a stale start to retry resolution.
func initialMCPTarget(ctx context.Context, tx pgx.Tx, scope execution.Scope, session execution.Session, kind execution.CallerKind) ([]byte, error) {
	if session.MCP == nil || len(session.MCP.Bindings) == 0 {
		return nil, nil
	}
	if kind != execution.CallerWorkflow || session.Target == nil {
		return nil, execution.ErrInvalidRequest
	}
	if err := session.MCP.Validate(); err != nil {
		return nil, err
	}
	target := make(map[string]mcpTargetMember, len(session.MCP.Bindings))
	for _, b := range session.MCP.Bindings {
		var version string
		var revision int64
		err := tx.QueryRow(ctx, `SELECT plugin_version, revision FROM v2_tenant_mcps
			WHERE tenant_id=$1 AND plugin_id=$2 FOR SHARE`, scope.TenantID, b.PluginID).Scan(&version, &revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, &execution.MCPFailure{Code: "mcp_plugin_missing", PluginID: b.PluginID}
		}
		if err != nil {
			return nil, err
		}
		if version != b.PluginVersion || revision != b.Revision {
			return nil, execution.ErrConflict
		}
		target[b.PluginID] = mcpTargetMember{State: "configured", Version: version, Revision: revision}
	}
	return json.Marshal(target)
}

// advanceMCPTargets runs inside the configuration writer's transaction. Lock matching
// sessions in a common order even when different plugins affect overlapping sets.
func advanceMCPTargets(ctx context.Context, tx pgx.Tx, tenant, id string, member mcpTargetMember) error {
	raw, err := json.Marshal(member)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `WITH selected AS MATERIALIZED (
		SELECT id FROM v2_execution_sessions WHERE tenant_id=$1 AND ended_at IS NULL
		AND mcp_target ? $2 ORDER BY id FOR UPDATE
	) UPDATE v2_execution_sessions AS s SET mcp_generation=s.mcp_generation+1,
		mcp_target=jsonb_set(s.mcp_target,ARRAY[$2],$3::jsonb,false)
		FROM selected WHERE s.id=selected.id`, tenant, id, raw)
	return err
}

func advanceInstalledMCP(ctx context.Context, tx pgx.Tx, tenant string, item TenantMCP) error {
	member := mcpTargetMember{State: "configured", Version: item.PluginVersion, Revision: item.Revision}
	if !item.Complete {
		member = mcpTargetMember{State: "unavailable", Code: "mcp_setting_missing"}
	}
	return advanceMCPTargets(ctx, tx, tenant, item.ID, member)
}
