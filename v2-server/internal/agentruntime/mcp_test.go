//go:build linux

package agentruntime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/node"
)

func ipcMCP() *execution.MCPSelection {
	archive := []byte("MCP package fixture")
	sum := sha256.Sum256(archive)
	return &execution.MCPSelection{
		Bindings: []execution.MCPBinding{{PluginID: "official/acme.tools", PluginVersion: "1.0.0", Settings: map[string]any{"token": "MCP_SECRET_MARKER"}}},
		Packages: []execution.MCPPackage{{PluginID: "official/acme.tools", PluginVersion: "1.0.0", Digest: hex.EncodeToString(sum[:]), Archive: archive}},
	}
}

func TestMCPFrameDeliveryAndNextSessionIsolation(t *testing.T) {
	h := newIPCHarness(t, nil)
	ctx := ipcContext(t)
	session := h.session("mcp-session", "workspace")
	session.MCP = ipcMCP()
	done := startIPCRun(h, ctx, session, "first", discardIPCEvent)
	f := receiveIPC(t, h.created)
	start := nextIPCCommand(t, f, "start_agent_session")
	var payload struct {
		Spec struct {
			MCP []struct {
				PluginID      string `json:"plugin_id"`
				PluginVersion string `json:"plugin_version"`
				Settings      map[string]any
			}
		}
	}
	if json.Unmarshal(start.Payload, &payload) != nil || len(payload.Spec.MCP) != 1 || payload.Spec.MCP[0].Settings["token"] != "MCP_SECRET_MARKER" || payload.Spec.MCP[0].PluginID != "official/acme.tools" {
		t.Fatalf("missing explicit wire selection: %d bytes", len(start.Payload))
	}
	f.finishTurn(t, start, "first", "done")
	if result := receiveIPC(t, done); result.err != nil {
		t.Fatal(result.err)
	}
	ended := make(chan error, 1)
	go func() { ended <- h.driver.End(ctx, session.ID) }()
	f.terminal(nextIPCCommand(t, f, "end_session"))
	if err := receiveIPC(t, ended); err != nil {
		t.Fatal(err)
	}
	next := h.session("empty-session", "workspace")
	next.MCP = &execution.MCPSelection{}
	done = startIPCRun(h, ctx, next, "second", discardIPCEvent)
	start = nextIPCCommand(t, f, "start_agent_session")
	var empty map[string]json.RawMessage
	var envelope struct{ Spec json.RawMessage }
	_ = json.Unmarshal(start.Payload, &envelope)
	_ = json.Unmarshal(envelope.Spec, &empty)
	if _, exists := empty["mcp"]; exists {
		t.Fatal("next session inherited MCP")
	}
	f.finishTurn(t, start, "second", "done")
	receiveIPC(t, done)
	data, err := os.ReadFile(filepath.Join(f.project.root, "node-events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("MCP_SECRET_MARKER")) {
		t.Fatal("settings leaked to journal")
	}
}

func TestMCPRejectsUnsupportedNodeAndMissingPackageBeforeStart(t *testing.T) {
	for _, code := range []string{"mcp_node_capability_missing", "mcp_plugin_missing"} {
		t.Run(code, func(t *testing.T) {
			h := newIPCHarness(t, func(f *fakeNodeIPC, _ node.Message) bool {
				f.mcpUnsupported.Store(code == "mcp_node_capability_missing")
				return false
			})
			s := h.session("mcp", "workspace")
			s.MCP = ipcMCP()
			if code == "mcp_plugin_missing" {
				s.MCP.Packages = nil
			}
			done := startIPCRun(h, ipcContext(t), s, "first", discardIPCEvent)
			f := receiveIPC(t, h.created)
			result := receiveIPC(t, done)
			if result.result.FailureCode != code || result.result.MCPFailure == nil {
				t.Fatalf("wrong failure: %+v", result.result)
			}
			for len(f.sent) > 0 {
				if m := <-f.sent; m.Type == "start_agent_session" {
					t.Fatal("started with unsupported MCP")
				}
			}
		})
	}
}

func TestMCPTerminalPreservesSafeFailureDetails(t *testing.T) {
	for _, code := range []string{"mcp_plugin_missing", "mcp_setting_missing", "mcp_http_capability_missing", "mcp_load_session_missing"} {
		t.Run(code, func(t *testing.T) {
			h := newIPCHarness(t, nil)
			s := h.session("session", "workspace")
			done := startIPCRun(h, ipcContext(t), s, "first", discardIPCEvent)
			f := receiveIPC(t, h.created)
			start := nextIPCCommand(t, f, "start_agent_session")
			sequence := uint64(1)
			payload, _ := json.Marshal(map[string]any{"kind": "agent_session_ended", "result": map[string]any{"reason": "agent_failed", "node": f.id, "detail": code, "plugin_id": "official/acme.tools", "setting_id": "token", "transport": "http"}})
			f.in <- node.Message{Type: "agent_session_ended", OperationID: start.OperationID, ExecutionID: start.ExecutionID, Sequence: &sequence, Payload: payload}
			result := receiveIPC(t, done)
			if result.result.FailureCode != code || result.result.MCPFailure == nil || result.result.MCPFailure.PluginID != "official/acme.tools" || result.result.MCPFailure.SettingID != "token" || result.result.MCPFailure.Transport != "http" {
				t.Fatalf("failure details lost: %+v", result.result)
			}
		})
	}
}
