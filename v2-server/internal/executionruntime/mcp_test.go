package executionruntime

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

func TestRuntimeMCPGatePreservesIdlePredecessorAndCreatesNoRecords(t *testing.T) {
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
	req := execution.StartRequest{Caller: execution.Caller{Kind: execution.CallerWorkflow, ID: "run", NodeID: "a", AttemptID: "attempt"}, Target: execution.Target{ProjectID: "project"}, ExecutorID: "opencode:fixture", Prompt: execution.TextPrompt("run")}
	ref, err := f.service.Start(f.ctx, f.scope, req)
	if err != nil {
		t.Fatal(err)
	}
	call := nextAgentCall(t, f, a)
	call.unblock()
	observeTurn(t, f, ref, 0)
	req.Caller.NodeID, req.Caller.AttemptID = "b", "blocked"
	req.MCP = &execution.MCPSelection{Bindings: []execution.MCPBinding{{PluginID: "official/acme.tools", PluginVersion: "1.0.0", Settings: map[string]any{"token": "MCP_RUNTIME_SECRET"}}}}
	for range 2 {
		blocked, err := f.service.Start(f.ctx, f.scope, req)
		var failure *execution.MCPFailure
		if !errors.As(err, &failure) || failure.Code != "mcp_lifecycle_not_ready" || blocked != (execution.Ref{}) {
			t.Fatalf("gate admitted a nonempty selection: %+v %v", blocked, err)
		}
	}
	prior, err := f.service.ReadSession(f.ctx, f.scope, ref.SessionID)
	if err != nil || prior.EndedAt != nil {
		t.Fatal("gate ended the idle predecessor", err)
	}
	select {
	case <-a.ended:
		t.Fatal("gate stopped the predecessor's physical session")
	default:
	}
	select {
	case <-a.started:
		t.Fatal("gate started another Agent")
	default:
	}
	if _, err := f.db.ReadExecutionAttempt(f.ctx, f.scope, req.Caller); !errors.Is(err, execution.ErrNotFound) {
		t.Fatal("gate retained a blocked attempt", err)
	}
	conn, err := pgx.ConnectConfig(f.ctx, f.config.ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(f.ctx)
	var sessions, turns int
	if err := conn.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM v2_execution_sessions), (SELECT count(*) FROM v2_execution_turns)`).Scan(&sessions, &turns); err != nil || sessions != 1 || turns != 1 {
		t.Fatalf("gate created records: sessions=%d turns=%d err=%v", sessions, turns, err)
	}
}
