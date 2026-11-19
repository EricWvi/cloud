package workflowhost

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

func TestMCPResolutionInfrastructureErrorsRemainRetryable(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, store.ErrForbidden, execution.ErrNotFound, errors.New("database commit failed: private detail"), fmt.Errorf("begin: %w", context.Canceled)} {
		failure := mcpFailure(err)
		detail := failure.Detail(1, false, time.Unix(0, 0))
		if failure.Kind != workflow.FailureSession || failure.Code != "" || detail["auto_retryable"] != true || failure.Message != "execution could not resolve MCP configuration" {
			t.Fatalf("infrastructure error misclassified or exposed: %+v", failure)
		}
	}
	for _, code := range []string{"mcp_plugin_missing", "mcp_setting_missing"} {
		failure := mcpFailure(fmt.Errorf("resolve: %w", &execution.MCPFailure{Code: code, PluginID: "official/acme.tools"}))
		if failure.Code != code || failure.Detail(1, false, time.Unix(0, 0))["auto_retryable"] != false {
			t.Fatalf("typed setup error lost classification: %+v", failure)
		}
	}
}

func TestMCPLifecycleGateFailuresAreExplicitAndNotRetryable(t *testing.T) {
	for _, code := range []string{"mcp_node_capability_missing", "mcp_lifecycle_not_ready"} {
		t.Run(code, func(t *testing.T) {
			failure := mcpFailure(&execution.MCPFailure{Code: code})
			detail := failure.Detail(1, false, time.Unix(0, 0))
			if detail["code"] != code || detail["auto_retryable"] != false || detail["resumable"] != true {
				t.Fatalf("capability gate lost its failure contract: %+v", detail)
			}
		})
	}
}

func TestInteractiveMCPResolutionLogsCredentialFailureWithoutRetry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	access := storetest.Access(t, f.s, f.scope.TenantID, f.scope.UserID)
	spaces, err := f.s.TenantSpaces(ctx, access)
	if err != nil || len(spaces) == 0 {
		t.Fatal("list spaces", err)
	}
	archive, digest := workflowMCPArchive(t)
	const id = "official/fixture.workflow-mcp"
	entry := store.PluginCatalogEntry{ID: id, Kind: "mcp", Version: "1.0.0", Title: "Workflow MCP"}
	installed, err := f.s.InstallTenantMCP(ctx, access, spaces[0].ID, store.PluginPackageSource{Entry: entry, Digest: digest}, archive, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("V2_MARKET_CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if _, err := f.s.SetTenantMCPSettings(ctx, access, spaces[0].ID, id, installed.Revision, map[string]any{"token": mcpFixtureSecret}, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("V2_MARKET_CREDENTIAL_KEY", "")
	var logs bytes.Buffer
	f.host.log = slog.New(slog.NewJSONHandler(&logs, nil))
	graph := strings.Replace(interactiveChain, `"interactive":true`, `"interactive":true,"retry":{"enabled":true,"maxRetries":2,"initialDelaySeconds":0},"mcps":[{"mcpId":"official/fixture.workflow-mcp","enabled":true}]`, 1)
	runID := f.run(t, graph, str("check"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	_, nodes := f.await(t, runID, terminal)
	f.host.workers.Wait()
	n := node(nodes, "a")
	assertMCPDetail(t, n, map[string]any{"kind": "interactive_session", "auto_retryable": false, "attempt": float64(1), "message": "execution could not resolve MCP configuration"})
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil {
		t.Fatal("expected one structured diagnostic", err)
	}
	if record["run"] != runID || record["node_run"] != n.ID || record["error"] != pluginmarket.ErrCredentials.Error() || strings.Contains(logs.String(), mcpFixtureSecret) {
		t.Fatal("log must identify the attempt and the deployment error without settings")
	}
}

func TestMCPStartFailureCannotAutoRetry(t *testing.T) {
	f := newFixture(t)
	f.exec.beforeStart = func(execution.StartRequest) error {
		return &execution.MCPFailure{Code: "mcp_lifecycle_not_ready", PluginID: "official/acme.tools"}
	}
	graph := strings.Replace(twoAgents, `"roleId":"reviewer"`, `"roleId":"reviewer","retry":{"enabled":true,"maxRetries":2,"initialDelaySeconds":0}`, 1)
	runID := f.run(t, graph, str("check"))
	if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	_, nodes := f.await(t, runID, terminal)
	assertMCPDetail(t, node(nodes, "a"), map[string]any{"kind": "missing_agent_config", "code": "mcp_lifecycle_not_ready", "plugin_id": "official/acme.tools", "auto_retryable": false, "attempt": float64(1)})
}

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
	assertMCPDetail(t, node(nodes, "a"), map[string]any{"kind": "missing_agent_config", "code": "mcp_setting_missing", "plugin_id": "official/acme.tools", "setting_id": "token", "transport": "http", "auto_retryable": false, "resumable": true})
	f.exec.mu.Lock()
	defer f.exec.mu.Unlock()
	if len(f.exec.started) != 1 {
		t.Fatal("MCP setup failure retried")
	}
}

func assertMCPDetail(t *testing.T, n store.WorkflowNodeRun, expected map[string]any) {
	t.Helper()
	var payload struct {
		Detail map[string]any `json:"error_detail"`
	}
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	for key, want := range expected {
		if payload.Detail[key] != want {
			t.Fatalf("%s = %v, want %v", key, payload.Detail[key], want)
		}
	}
}
