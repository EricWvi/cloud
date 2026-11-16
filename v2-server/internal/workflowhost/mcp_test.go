package workflowhost

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

func TestMCPSetupFailureRetainsIdentifiersAndCannotAutoRetry(t *testing.T) {
	f := newFixture(t)
	runID := f.run(t, twoAgents, str("check"))
	if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	ref := f.exec.awaitStart(t)
	f.exec.finish(ref, execution.Result{Status: execution.StatusFailed, FailureCode: "mcp_setting_missing", MCPFailure: &execution.MCPFailure{Code: "mcp_setting_missing", PluginID: "official/acme.tools", SettingID: "token", Transport: "http"}})
	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusFailed {
		t.Fatal(run.Status)
	}
	var payload struct {
		Detail map[string]any `json:"error_detail"`
	}
	if err := json.Unmarshal(node(nodes, "a").Payload, &payload); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"kind": "missing_agent_config", "code": "mcp_setting_missing", "plugin_id": "official/acme.tools", "setting_id": "token", "transport": "http", "auto_retryable": false, "resumable": true} {
		if payload.Detail[key] != want {
			t.Fatalf("%s = %v, want %v", key, payload.Detail[key], want)
		}
	}
	f.exec.mu.Lock()
	defer f.exec.mu.Unlock()
	if len(f.exec.started) != 1 {
		t.Fatal("MCP setup failure retried")
	}
}
