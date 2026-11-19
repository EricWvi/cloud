package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// notificationStore is the part of the database the in-app notification inbox needs.
type notificationStore interface {
	Notifications(ctx context.Context, tenant, user string, limit int) ([]store.Notification, int, error)
	MarkNotificationsRead(ctx context.Context, tenant, user string, ids []string, all bool) (int, error)
}

var _ notificationStore = (*store.Store)(nil)

func (h *handler) notificationRoutes(rt *router) {
	rt.tenant("GET /api/v2/tenants/{tid}/notifications", h.listNotifications)
	rt.tenant("POST /api/v2/tenants/{tid}/notifications/read", h.readNotifications)
}

// listNotifications returns the caller's newest notifications in the tenant with the unread count.
// The route's tenant check is what keeps a removed member out; their rows went with the membership.
func (h *handler) listNotifications(w http.ResponseWriter, r *http.Request, a access) {
	s, ok := h.store.(notificationStore)
	if !ok {
		fault(w, http.StatusServiceUnavailable, "notification_unavailable")
		return
	}
	limit := store.MaxNotifications
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 1 || n > store.MaxNotifications {
			fault(w, http.StatusBadRequest, "invalid_limit")
			return
		}
		limit = n
	}
	items, unread, e := s.Notifications(r.Context(), a.TenantID, a.UserID, limit)
	if e != nil {
		internal(w, "list notifications", e)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "unread": unread})
}

type notificationReadRequest struct {
	IDs []string `json:"ids"`
	All bool     `json:"all"`
}

// readNotifications marks the listed notifications, or all of them, read for the caller only.
func (h *handler) readNotifications(w http.ResponseWriter, r *http.Request, a access) {
	s, ok := h.store.(notificationStore)
	if !ok {
		fault(w, http.StatusServiceUnavailable, "notification_unavailable")
		return
	}
	var req notificationReadRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if !req.All && len(req.IDs) == 0 || len(req.IDs) > store.MaxNotifications {
		fault(w, http.StatusBadRequest, "invalid_notification_ids")
		return
	}
	unread, e := s.MarkNotificationsRead(r.Context(), a.TenantID, a.UserID, req.IDs, req.All)
	if errors.Is(e, store.ErrNotFound) {
		fault(w, http.StatusBadRequest, "invalid_notification_ids")
		return
	}
	if e != nil {
		internal(w, "mark notifications read", e)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"unread": unread})
}
