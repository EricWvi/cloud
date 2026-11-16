package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/wanglongan587/cloud/v2-server/internal/roledoc"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// Role routes are tenant-wide: every member reads and writes the same catalog, as with workflows
// and squads, so a workflow one member edits stays runnable by the rest of the workspace.

func (h *handler) listRoles(w http.ResponseWriter, r *http.Request, a access) {
	items, e := h.store.ListRoles(r.Context(), a.TenantID)
	if e != nil {
		roleError(w, "list roles", e)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *handler) getRole(w http.ResponseWriter, r *http.Request, a access) {
	id := r.PathValue("rid")
	if !squadUUID.MatchString(id) {
		fault(w, http.StatusNotFound, "role_not_found")
		return
	}
	item, e := h.store.GetRole(r.Context(), a.TenantID, id)
	if e != nil {
		roleError(w, "get role", e)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (h *handler) createRole(w http.ResponseWriter, r *http.Request, a access) {
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Content     string `json:"content"`
	}
	if !decodeRoleJSON(w, r, &in) {
		return
	}
	item, e := h.store.CreateRole(r.Context(), a.TenantID, a.UserID, store.RoleInput{
		Name: in.Name, Description: in.Description, Content: in.Content,
	})
	if e != nil {
		roleError(w, "create role", e)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (h *handler) updateRole(w http.ResponseWriter, r *http.Request, a access) {
	id := r.PathValue("rid")
	if !squadUUID.MatchString(id) {
		fault(w, http.StatusNotFound, "role_not_found")
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Content     string `json:"content"`
	}
	if !decodeRoleJSON(w, r, &in) {
		return
	}
	item, e := h.store.UpdateRole(r.Context(), a.TenantID, id, store.RoleInput{
		Name: in.Name, Description: in.Description, Content: in.Content,
	})
	if e != nil {
		roleError(w, "update role", e)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (h *handler) deleteRole(w http.ResponseWriter, r *http.Request, a access) {
	id := r.PathValue("rid")
	if !squadUUID.MatchString(id) {
		fault(w, http.StatusNotFound, "role_not_found")
		return
	}
	if e := h.store.DeleteRole(r.Context(), a.TenantID, id); e != nil {
		roleError(w, "delete role", e)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) importRole(w http.ResponseWriter, r *http.Request, a access) {
	var in struct {
		FileName  string `json:"fileName"`
		Content   string `json:"content"`
		Overwrite bool   `json:"overwrite"`
	}
	if !decodeRoleJSON(w, r, &in) {
		return
	}
	if ext := strings.ToLower(filepath.Ext(in.FileName)); ext != ".md" && ext != ".markdown" {
		fault(w, http.StatusBadRequest, "role_file_type")
		return
	}
	item, e := h.store.ImportRole(r.Context(), a.TenantID, a.UserID, []byte(in.Content), in.Overwrite)
	if e != nil {
		roleError(w, "import role", e)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

// decodeRoleJSON keeps the existing strict JSON behavior but admits a full 1 MiB role file.
func decodeRoleJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(&struct{}{}) != io.EOF {
		fault(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	return true
}

func roleError(w http.ResponseWriter, operation string, e error) {
	if code := roledoc.Code(e); code != "" {
		fault(w, http.StatusBadRequest, code)
		return
	}
	switch {
	case errors.Is(e, store.ErrRoleNotFound):
		fault(w, http.StatusNotFound, "role_not_found")
	case errors.Is(e, store.ErrRoleNameConflict):
		fault(w, http.StatusConflict, "role_name_conflict")
	default:
		internal(w, operation, e)
	}
}
