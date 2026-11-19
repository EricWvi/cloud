package server

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// notificationStore is the part of the database the in-app notification inbox needs.
type notificationStore interface {
	Notifications(ctx context.Context, tenant, user string, after *store.NotificationCursor, limit int) (store.NotificationPage, error)
	MarkNotificationsRead(ctx context.Context, tenant, user string, ids []string, all bool) (int, error)
}

var _ notificationStore = (*store.Store)(nil)

func (h *handler) notificationRoutes(rt *router) {
	rt.tenant("GET /api/v2/tenants/{tid}/notifications", h.listNotifications)
	rt.tenant("POST /api/v2/tenants/{tid}/notifications/read", h.readNotifications)
}

// listNotifications returns a page of the caller's notifications in the tenant, newest first, with
// the unread count and the cursor of the next page ("" on the last). The route's tenant check is
// what keeps a removed member out; their rows went with the membership.
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
	var after *store.NotificationCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, ok := decodeNotificationCursor(raw)
		if !ok {
			fault(w, http.StatusBadRequest, "invalid_cursor")
			return
		}
		after = &c
	}
	page, e := s.Notifications(r.Context(), a.TenantID, a.UserID, after, limit)
	if errors.Is(e, store.ErrNotFound) {
		fault(w, http.StatusBadRequest, "invalid_cursor")
		return
	}
	if e != nil {
		internal(w, "list notifications", e)
		return
	}
	next := ""
	if page.Next != nil {
		next = encodeNotificationCursor(*page.Next)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": page.Items, "unread": page.Unread, "nextCursor": next})
}

// A notification cursor is opaque to clients: the last item's creation time and id.
func encodeNotificationCursor(c store.NotificationCursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID))
}

func decodeNotificationCursor(raw string) (store.NotificationCursor, bool) {
	b, e := base64.RawURLEncoding.DecodeString(raw)
	if e != nil {
		return store.NotificationCursor{}, false
	}
	at, id, ok := strings.Cut(string(b), "|")
	if !ok {
		return store.NotificationCursor{}, false
	}
	t, e := time.Parse(time.RFC3339Nano, at)
	if e != nil {
		return store.NotificationCursor{}, false
	}
	return store.NotificationCursor{CreatedAt: t, ID: id}, true
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
