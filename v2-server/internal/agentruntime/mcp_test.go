//go:build linux

package agentruntime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func TestMCPSpecSerializesOnlyTheSelectedBindings(t *testing.T) {
	// This tests only construction. Cloud must not start MCP sessions until lifecycle
	// admission and reliable turn failures are supported by both ends.
	selection := ipcMCP()
	data, err := json.Marshal(mcpSpec(selection))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"plugin_id":"official/acme.tools","plugin_version":"1.0.0","settings":{"token":"MCP_SECRET_MARKER"}}]`
	if string(data) != want {
		t.Fatalf("unexpected MCP frame: %d bytes", len(data))
	}
	empty, err := json.Marshal(mcpSpec(&execution.MCPSelection{}))
	if err != nil || string(empty) != "[]" {
		t.Fatal("empty selection inherited previous bindings")
	}
}

func TestMCPPackageInstallationKeepsSettingsOutOfControlFrames(t *testing.T) {
	h := newIPCHarness(t, nil)
	ctx := ipcContext(t)
	session := h.session("plain-session", "workspace")
	done := startIPCRun(h, ctx, session, "first", discardIPCEvent)
	f := receiveIPC(t, h.created)
	start := nextIPCCommand(t, f, "start_agent_session")
	f.finishTurn(t, start, "first", "done")
	if result := receiveIPC(t, done); result.err != nil {
		t.Fatal(result.err)
	}
	selection := ipcMCP()
	if err := f.project.installMCP(ctx, selection.Packages[0]); err != nil {
		t.Fatal(err)
	}
	install := nextIPCCommand(t, f, "install_plugins")
	if !bytes.Contains(install.Payload, []byte("official/acme.tools")) || !bytes.Contains(install.Payload, []byte("1.0.0")) {
		t.Fatal("package identity did not reach install_plugins")
	}
	if bytes.Contains(install.Payload, []byte("MCP_SECRET_MARKER")) || bytes.Contains(install.Payload, []byte("settings")) {
		t.Fatal("package installation received session settings")
	}
	invalid := selection.Packages[0]
	invalid.Digest = "incorrect"
	if err := f.project.installMCP(ctx, invalid); err == nil {
		t.Fatal("invalid package digest accepted")
	}
}

func TestMCPRejectsBeforeStartUntilBothPeersSupportLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, missingCapability, code string
	}{
		{"missing agent session", "agent_session", "mcp_node_capability_missing"},
		{"missing session MCP", "session_mcp", "mcp_node_capability_missing"},
		{"frozen setup only", "session_mcp_lifecycle", "mcp_node_capability_missing"},
		{"lifecycle Node without Controller integration", "", "mcp_lifecycle_not_ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newIPCHarness(t, func(f *fakeNodeIPC, _ node.Message) bool {
				f.missingCapability.Store(&tc.missingCapability)
				return false
			})
			s := h.session("mcp", "workspace")
			s.MCP = ipcMCP()
			done := startIPCRun(h, ipcContext(t), s, "first", discardIPCEvent)
			f := receiveIPC(t, h.created)
			result := receiveIPC(t, done)
			if result.result.FailureCode != tc.code || result.result.MCPFailure == nil {
				t.Fatalf("wrong failure: %+v", result.result)
			}
			for len(f.sent) > 0 {
				m := <-f.sent
				if m.Type == "start_agent_session" || m.Type == "clone_repository" {
					t.Fatal("started preparing a session with unsupported MCP")
				}
				if m.Type == "install_plugins" {
					var payload struct {
						Spec struct{ Plugins []json.RawMessage }
					}
					if err := json.Unmarshal(m.Payload, &payload); err != nil || len(payload.Spec.Plugins) != 0 {
						t.Fatal("installed packages before the MCP capability gate")
					}
				}
				if bytes.Contains(m.Payload, []byte("official/acme.tools")) || bytes.Contains(m.Payload, []byte("MCP_SECRET_MARKER")) {
					t.Fatal("rejected MCP selection reached the Node")
				}
			}
		})
	}
}

func TestEmptyMCPDoesNotRequireLifecycleCapability(t *testing.T) {
	for _, present := range []bool{false, true} {
		name := "omitted"
		if present {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			h := newIPCHarness(t, func(f *fakeNodeIPC, _ node.Message) bool {
				missing := "session_mcp_lifecycle"
				f.missingCapability.Store(&missing)
				return false
			})
			s := h.session("empty-mcp", "workspace")
			if present {
				s.MCP = &execution.MCPSelection{}
			}
			done := startIPCRun(h, ipcContext(t), s, "first", discardIPCEvent)
			f := receiveIPC(t, h.created)
			start := nextIPCCommand(t, f, "start_agent_session")
			var payload struct{ Spec map[string]json.RawMessage }
			if err := json.Unmarshal(start.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"mcp", "mcp_lifecycle"} {
				if _, exists := payload.Spec[field]; exists {
					t.Fatalf("empty selection sent %s to a legacy Node", field)
				}
			}
			f.finishTurn(t, start, "first", "done")
			if result := receiveIPC(t, done); result.err != nil || result.result.Status != execution.StatusCompleted {
				t.Fatalf("empty selection failed: %+v", result)
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
