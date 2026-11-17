package server

import (
	"context"
	"errors"
	"net/http"
	"os"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

type mcpStore interface {
	TenantMCPs(context.Context, store.Access) ([]store.TenantMCP, error)
	MCPPackageSource(context.Context, string, string, string) (store.PluginPackageSource, error)
	InstallTenantMCP(context.Context, store.Access, string, store.PluginPackageSource, []byte, int64) (store.TenantMCP, error)
	SetTenantMCPSettings(context.Context, store.Access, string, string, int64, map[string]any, []string) (store.TenantMCP, error)
	RemoveTenantMCP(context.Context, store.Access, string, string, int64) error
}

func (h *handler) listMCP(w http.ResponseWriter, r *http.Request, a access) {
	s, ok := h.store.(mcpStore)
	if !ok {
		fault(w, 503, "mcp_unavailable")
		return
	}
	items, err := s.TenantMCPs(r.Context(), a.Access)
	if err != nil {
		mcpError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"items": items})
}

func (h *handler) installMCP(w http.ResponseWriter, r *http.Request, a access) {
	s, ok := h.store.(mcpStore)
	if !ok {
		fault(w, 503, "mcp_unavailable")
		return
	}
	var in struct {
		Identifier    string `json:"identifier"`
		PluginVersion string `json:"pluginVersion"`
		Revision      int64  `json:"revision"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if !squadUUID.MatchString(r.PathValue("sid")) || in.Revision < 0 {
		fault(w, 400, "invalid_request")
		return
	}
	source, err := s.MCPPackageSource(r.Context(), a.TenantID, r.PathValue("sid"), in.Identifier)
	if err != nil {
		mcpError(w, err)
		return
	}
	if source.Entry.Kind != "mcp" {
		mcpError(w, store.ErrPluginKindNotInstallable)
		return
	}
	if in.PluginVersion != "" && in.PluginVersion != source.Entry.Version {
		mcpError(w, store.ErrPluginVersionUnavailable)
		return
	}
	download := h.opts.DownloadPluginPackage
	if download == nil {
		download = pluginmarket.Download
	}
	file, err := download(r.Context(), source.Locator, source.Digest, source.Retrieval, source.Credentials)
	if err != nil {
		pluginPackageError(w, err)
		return
	}
	defer os.Remove(file)
	archive, err := os.ReadFile(file)
	if err != nil {
		pluginPackageError(w, err)
		return
	}
	item, err := s.InstallTenantMCP(r.Context(), a.Access, r.PathValue("sid"), source, archive, in.Revision)
	if err != nil {
		mcpError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"resource": item})
}

func (h *handler) setMCPSettings(w http.ResponseWriter, r *http.Request, a access) {
	s, ok := h.store.(mcpStore)
	if !ok {
		fault(w, 503, "mcp_unavailable")
		return
	}
	var in struct {
		Identifier string         `json:"identifier"`
		Revision   *int64         `json:"revision"`
		Values     map[string]any `json:"values"`
		Clear      []string       `json:"clear"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if !squadUUID.MatchString(r.PathValue("sid")) || in.Revision == nil || *in.Revision < 1 {
		fault(w, 400, "invalid_request")
		return
	}
	item, err := s.SetTenantMCPSettings(r.Context(), a.Access, r.PathValue("sid"), in.Identifier, *in.Revision, in.Values, in.Clear)
	if err != nil {
		mcpError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"resource": item})
}

func (h *handler) removeMCP(w http.ResponseWriter, r *http.Request, a access) {
	s, ok := h.store.(mcpStore)
	if !ok {
		fault(w, 503, "mcp_unavailable")
		return
	}
	var in struct {
		Identifier string `json:"identifier"`
		Revision   *int64 `json:"revision"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if !squadUUID.MatchString(r.PathValue("sid")) || in.Revision == nil || *in.Revision < 1 {
		fault(w, 400, "invalid_request")
		return
	}
	if err := s.RemoveTenantMCP(r.Context(), a.Access, r.PathValue("sid"), in.Identifier, *in.Revision); err != nil {
		mcpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func mcpError(w http.ResponseWriter, err error) {
	var failure *execution.MCPFailure
	switch {
	case errors.As(err, &failure):
		fault(w, 400, failure.Code)
	case errors.Is(err, pluginmarket.ErrCredentials):
		fault(w, 503, "mcp_credentials_unavailable")
	case errors.Is(err, pluginmarket.ErrPackage):
		fault(w, 400, "plugin_package_invalid")
	default:
		pluginError(w, "MCP configuration", err)
	}
}
