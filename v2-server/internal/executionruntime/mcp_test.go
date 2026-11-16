package executionruntime

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

func TestRuntimeMCPFreezesValuesWithoutPersistingThem(t *testing.T) {
	f := runtimeDatabase(t, nil)
	if err := f.service.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	a := &orchestrationAgent{started: make(chan *agentCall, 8), ended: make(chan string, 8)}
	var err error
	f.service, err = New(f.ctx, Config{Store: f.db, Models: f.models, Agent: a})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.CreateProject(f.ctx, f.scope, store.Project{ID: "project", Name: "Demo", RepositoryURL: "https://example.com/demo.git", Commit: "fixed", MirrorPath: "/fake/unused"}); err != nil {
		t.Fatal(err)
	}
	req := execution.StartRequest{Caller: execution.Caller{Kind: execution.CallerWorkflow, ID: "run", NodeID: "a", AttemptID: "attempt"}, Target: execution.Target{ProjectID: "project"}, ExecutorID: "opencode:fixture", Prompt: execution.TextPrompt("run"), MCP: &execution.MCPSelection{Bindings: []execution.MCPBinding{{PluginID: "official/acme.tools", PluginVersion: "1.0.0", Settings: map[string]any{"token": "MCP_RUNTIME_SECRET"}}}}}
	ref, err := f.service.Start(f.ctx, f.scope, req)
	if err != nil {
		t.Fatal(err)
	}
	call := nextAgentCall(t, f, a)
	req.MCP.Bindings[0].Settings["token"] = "changed"
	if call.session.MCP.Bindings[0].Settings["token"] != "MCP_RUNTIME_SECRET" {
		t.Fatal("caller mutation changed frozen credentials")
	}
	retained, err := f.service.ReadSession(f.ctx, f.scope, ref.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.MCP == nil || len(retained.MCP.Bindings) != 1 || retained.MCP.Bindings[0].Settings != nil {
		t.Fatal("retained selection contains runtime settings or lost identity")
	}
	conn, err := pgx.ConnectConfig(f.ctx, f.config.ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(f.ctx)
	var data string
	if err = conn.QueryRow(f.ctx, "SELECT data::text FROM v2_execution_sessions WHERE id=$1", ref.SessionID).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(data, "MCP_RUNTIME_SECRET") || strings.Contains(data, "changed") {
		t.Fatal("SQL stored settings")
	}
	if again, err := f.service.Start(f.ctx, f.scope, req); err != nil || again != ref {
		t.Fatal("same execution must keep frozen values", err)
	}
	if call.session.MCP.Bindings[0].Settings["token"] != "MCP_RUNTIME_SECRET" {
		t.Fatal("idempotent retry replaced settings")
	}
	req.MCP.Bindings[0].PluginVersion = "2.0.0"
	if _, err = f.service.Start(f.ctx, f.scope, req); !errors.Is(err, execution.ErrConflict) {
		t.Fatal("changed identity reused same execution", err)
	}
	call.unblock()
	observeTurn(t, f, ref, 0)
	page, err := f.service.ReadTurns(f.ctx, f.scope, ref.SessionID, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(page)
	if strings.Contains(string(encoded), "MCP_RUNTIME_SECRET") {
		t.Fatal("public turns exposed settings")
	}
}
