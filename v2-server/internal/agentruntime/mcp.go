//go:build linux

package agentruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/nodeservices"
)

func (p *projectRuntime) prepareMCP(ctx context.Context, selected *execution.MCPSelection) error {
	if selected == nil || len(selected.Bindings) == 0 {
		return nil
	}
	if err := selected.Validate(); err != nil {
		return err
	}
	capable, ok := p.ipc().(interface{ Supports(string) bool })
	if !ok || !capable.Supports("session_mcp") || !capable.Supports("agent_session") {
		return &execution.MCPFailure{Code: "mcp_node_capability_missing"}
	}
	p.skillsMu.Lock()
	defer p.skillsMu.Unlock()
	for _, binding := range selected.Bindings {
		var archive *execution.MCPPackage
		for i := range selected.Packages {
			candidate := &selected.Packages[i]
			if candidate.PluginID == binding.PluginID && candidate.PluginVersion == binding.PluginVersion {
				archive = candidate
				break
			}
		}
		if archive == nil || p.installMCP(ctx, *archive) != nil {
			return &execution.MCPFailure{Code: "mcp_plugin_missing", PluginID: binding.PluginID}
		}
	}
	return nil
}

// installMCP stages only package bytes. Settings never enter files, package URLs or install frames.
func (p *projectRuntime) installMCP(ctx context.Context, archive execution.MCPPackage) error {
	sum := sha256.Sum256(archive.Archive)
	if len(archive.Archive) == 0 || hex.EncodeToString(sum[:]) != archive.Digest || p.services == nil {
		return execution.ErrUnavailable
	}
	file, err := os.CreateTemp(p.root, "mcp-*.orax")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(archive.Archive)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	url, release, err := p.services.StageMarketPackage(nodeservices.Package{Name: "mcp-" + archive.Digest, Path: file.Name(), SHA256: archive.Digest})
	if err != nil {
		return err
	}
	defer release()
	download := map[string]string{"url": url, "sha256": archive.Digest}
	releaseSpec := map[string]any{"kind": "universal", "download": download}
	if archive.Target != "" {
		releaseSpec = map[string]any{"kind": "targets", "targets": []any{map[string]any{"target": archive.Target, "download": download}}}
	}
	item := map[string]any{"plugin_id": archive.PluginID, "version": archive.PluginVersion, "release": releaseSpec}
	message, err := p.control(ctx, p.driver.nextID("mcp"), "install_plugins", "plugins_result", map[string]any{"spec": map[string]any{"node_id": p.nodeID(), "plugins": []any{item}}})
	if err != nil {
		return err
	}
	var response struct {
		Kind   string
		Result struct {
			Items []struct {
				ID      string `json:"plugin_id"`
				Outcome struct{ Kind, Version string }
			}
		}
	}
	if json.Unmarshal(message.Payload, &response) != nil || response.Kind != "plugins_completed" || len(response.Result.Items) != 1 {
		return execution.ErrUnavailable
	}
	installed := response.Result.Items[0]
	if installed.ID != archive.PluginID || installed.Outcome.Kind != "installed" || installed.Outcome.Version != archive.PluginVersion {
		return execution.ErrUnavailable
	}
	return nil
}

func mcpSpec(selection *execution.MCPSelection) []any {
	bindings := make([]any, 0, len(selection.Bindings))
	for _, binding := range selection.Bindings {
		bindings = append(bindings, map[string]any{"plugin_id": binding.PluginID, "plugin_version": binding.PluginVersion, "settings": binding.Settings})
	}
	return bindings
}
