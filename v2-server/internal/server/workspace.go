package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workspacefs"
)

// The workspace routes are outside api/openapi.json; workspace.openapi.json describes them. Browsing
// (entries, file) reads the checkout behind a conversation, a workflow run or a task run, read-only,
// for the side panel; the change view and the patch read the source's turn deliveries (see
// workspace_delivery.go).
type workspaceStore interface {
	ChatConversation(ctx context.Context, tenant, user, id string) (store.ChatConversation, error)
	ChatWorkspace(ctx context.Context, tenant, user, conversationID string) (store.WorkspaceLocation, error)
	WorkflowRunWorkspace(ctx context.Context, tenant, runID string) (store.WorkspaceLocation, error)
	TaskRunWorkspace(ctx context.Context, tenant, taskID, runID string) (store.WorkspaceLocation, error)
	WorkspaceDeliveryView(ctx context.Context, scope execution.Scope, source store.DeliverySource) (store.DeliveryView, error)
	WorkspaceDeliveryByID(ctx context.Context, scope execution.Scope, source store.DeliverySource, id string) (store.DeliveryRecord, error)
}

// workspaceSource is the page source a request names, after its own resource checks passed. Every
// later read stays inside it: the delivery store functions re-check the same scope and source in
// SQL, so a source deleted after the check reads as missing rather than leaking.
type workspaceSource struct {
	store    workspaceStore
	scope    execution.Scope
	delivery store.DeliverySource
	// notFound is the code the source's own check answers for a missing source, reused when the
	// source disappears between that check and a delivery read.
	notFound string
	location func(context.Context) (store.WorkspaceLocation, error)
}

// resolveSource authorizes the source a request names. It writes the response itself and returns
// false when the request may not read it.
type resolveSource func(http.ResponseWriter, *http.Request, access) (workspaceSource, bool)

func (h *handler) workspaceRoutes(rt *router) {
	for _, r := range []struct {
		base string
		find resolveSource
	}{
		{chatBase + "/conversations/{cid}/workspace", h.chatWorkspace},
		{"/api/v2/tenants/{tid}/workflow-runs/{rid}/workspace", h.workflowRunWorkspace},
		{"/api/v2/tenants/{tid}/tasks/{taskid}/runs/{rid}/workspace", h.taskRunWorkspace},
	} {
		rt.tenant("GET "+r.base+"/entries", workspaceRead(r.find, func(w workspacefs.Workspace, p string) (any, error) { return w.List(p) }))
		rt.tenant("GET "+r.base+"/file", workspaceRead(r.find, func(w workspacefs.Workspace, p string) (any, error) { return w.Read(p) }))
		rt.tenant("GET "+r.base+"/changes", h.workspaceChanges(r.find))
		rt.tenant("GET "+r.base+"/diff", h.workspaceDiff(r.find))
		rt.tenant("GET "+r.base+"/deliveries/{deliveryId}/patch", h.workspacePatch(r.find))
	}
}

// workspaceRead browses the live checkout; it never reads a delivery.
func workspaceRead(find resolveSource, read func(workspacefs.Workspace, string) (any, error)) func(http.ResponseWriter, *http.Request, access) {
	return func(w http.ResponseWriter, r *http.Request, a access) {
		src, ok := find(w, r, a)
		if !ok {
			return
		}
		ws, ok := src.checkout(w, r)
		if !ok {
			return
		}
		out, e := read(ws, r.URL.Query().Get("path"))
		if e != nil {
			workspaceFault(w, e)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func (h *handler) chatWorkspace(w http.ResponseWriter, r *http.Request, a access) (src workspaceSource, found bool) {
	h.chat(func(w http.ResponseWriter, r *http.Request, c chatRequest) {
		s, ok := h.store.(workspaceStore)
		if !ok {
			fault(w, http.StatusServiceUnavailable, "workspace_unavailable")
			return
		}
		cid := r.PathValue("cid")
		if _, e := s.ChatConversation(r.Context(), c.scope.TenantID, c.scope.UserID, cid); e != nil {
			chatFault(w, "chat workspace conversation", e)
			return
		}
		scope := c.scope
		src = workspaceSource{
			store: s, scope: scope, notFound: "not_found",
			delivery: store.DeliverySource{Kind: execution.CallerChat, ID: cid},
			location: func(ctx context.Context) (store.WorkspaceLocation, error) {
				return s.ChatWorkspace(ctx, scope.TenantID, scope.UserID, cid)
			},
		}
		found = true
	})(w, r, a)
	return src, found
}

func (h *handler) workflowRunWorkspace(w http.ResponseWriter, r *http.Request, a access) (workspaceSource, bool) {
	runs, tenant, ok := h.workflowRunScope(w, a)
	if !ok {
		return workspaceSource{}, false
	}
	s, ok := h.store.(workspaceStore)
	if !ok {
		fault(w, http.StatusServiceUnavailable, "workspace_unavailable")
		return workspaceSource{}, false
	}
	runID := r.PathValue("rid")
	if !workflowUUID.MatchString(runID) {
		fault(w, http.StatusNotFound, "workflow_run_not_found")
		return workspaceSource{}, false
	}
	if _, e := runs.WorkflowRun(r.Context(), tenant, runID); e != nil {
		workflowRunFault(w, e, "workflow run workspace")
		return workspaceSource{}, false
	}
	return workspaceSource{
		store: s, scope: a.scope(), notFound: "workflow_run_not_found",
		delivery: store.DeliverySource{Kind: execution.CallerWorkflow, ID: runID},
		location: func(ctx context.Context) (store.WorkspaceLocation, error) {
			return s.WorkflowRunWorkspace(ctx, tenant, runID)
		},
	}, true
}

func (h *handler) taskRunWorkspace(w http.ResponseWriter, r *http.Request, a access) (workspaceSource, bool) {
	host, scope, id, ok := h.taskRunScope(w, r, a)
	if !ok {
		return workspaceSource{}, false
	}
	s, ok := h.store.(workspaceStore)
	if !ok {
		fault(w, http.StatusServiceUnavailable, "workspace_unavailable")
		return workspaceSource{}, false
	}
	runID := r.PathValue("rid")
	if _, e := host.Run(r.Context(), scope, id, runID); e != nil {
		taskRunFault(w, "task run workspace", e)
		return workspaceSource{}, false
	}
	return workspaceSource{
		store: s, scope: scope, notFound: "not_found",
		delivery: store.DeliverySource{Kind: execution.CallerTask, ID: runID, TaskID: id},
		location: func(ctx context.Context) (store.WorkspaceLocation, error) {
			return s.TaskRunWorkspace(ctx, scope.TenantID, id, runID)
		},
	}, true
}

// checkout finds the source's live working directory, for browsing and for legacy change views.
func (src workspaceSource) checkout(w http.ResponseWriter, r *http.Request) (workspacefs.Workspace, bool) {
	at, e := src.location(r.Context())
	return workspaceAt(w, at, e)
}

// workspaceAt turns a recorded location into a Workspace, or answers that there is none yet: the
// first turn has not cloned its checkout, or no Agent node of the run has started.
func workspaceAt(w http.ResponseWriter, at store.WorkspaceLocation, e error) (workspacefs.Workspace, bool) {
	if errors.Is(e, store.ErrNotFound) {
		fault(w, http.StatusNotFound, "workspace_not_ready")
		return workspacefs.Workspace{}, false
	}
	if e != nil {
		internal(w, "read workspace location", e)
		return workspacefs.Workspace{}, false
	}
	return workspacefs.Workspace{Root: at.Path, BaseCommit: at.BaseCommit}, true
}

func workspaceFault(w http.ResponseWriter, e error) {
	switch {
	case errors.Is(e, workspacefs.ErrGone):
		fault(w, http.StatusGone, "workspace_removed")
	case errors.Is(e, workspacefs.ErrInvalidPath):
		fault(w, http.StatusBadRequest, "invalid_path")
	case errors.Is(e, workspacefs.ErrNotFound):
		fault(w, http.StatusNotFound, "path_not_found")
	case errors.Is(e, workspacefs.ErrNotDirectory):
		fault(w, http.StatusBadRequest, "not_a_directory")
	case errors.Is(e, workspacefs.ErrNotFile):
		fault(w, http.StatusBadRequest, "not_a_file")
	case errors.Is(e, workspacefs.ErrBinary):
		fault(w, http.StatusUnprocessableEntity, "binary_file")
	case errors.Is(e, workspacefs.ErrTooLarge):
		fault(w, http.StatusRequestEntityTooLarge, "too_large")
	default:
		internal(w, "read workspace", e)
	}
}
