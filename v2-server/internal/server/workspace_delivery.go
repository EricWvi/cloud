package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workspacefs"
)

// DefaultMaxPatchBytes bounds a downloadable delivery patch when Options.MaxPatchBytes is unset.
const DefaultMaxPatchBytes = 16 << 20

// MaxPatchBytesCeiling is the largest patch cap a deployment may configure; a larger
// Options.MaxPatchBytes is lowered to it.
const MaxPatchBytesCeiling = 256 << 20

// patchWriteTimeout bounds how long one client may take to read a built patch, so memory held for
// slow or stalled readers is released.
const patchWriteTimeout = 2 * time.Minute

// maxConcurrentPatches is how many patches the process builds at once. Further downloads wait for
// a slot for as long as their request lives, so memory stays within this many caps.
const maxConcurrentPatches = 4

// DeliveryReader reads retained turn deliveries from a project mirror. *workspacefs.DeliveryFS
// implements it; each method verifies the snapshot's ref and parent before reading.
type DeliveryReader interface {
	DeliveryChanges(ctx context.Context, snap workspacefs.DeliverySnapshot) (workspacefs.Changes, error)
	DeliveryDiff(ctx context.Context, snap workspacefs.DeliverySnapshot, path string) (workspacefs.Diff, error)
	DeliveryPatch(ctx context.Context, snap workspacefs.DeliverySnapshot, maxBytes int) ([]byte, error)
}

// Change view modes: a source with any enrolled turn or recorded delivery reads deliveries only;
// one from before deliveries existed keeps reading its working directory.
const (
	modeDelivery         = "delivery"
	modeWorkingDirectory = "working_directory"
)

// workspaceDelivery is one delivery as the panel sees it. MirrorPath and project identity never
// leave the server.
type workspaceDelivery struct {
	ID             string    `json:"id"`
	Status         string    `json:"status"`
	TurnID         string    `json:"turnId"`
	TurnPosition   int64     `json:"turnPosition"`
	NodeID         string    `json:"nodeId,omitempty"`
	NodeRunID      string    `json:"nodeRunId,omitempty"`
	BaseCommit     string    `json:"baseCommit,omitempty"`
	DeliveryCommit string    `json:"deliveryCommit,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	ReasonCode     string    `json:"reasonCode,omitempty"`
	Reason         string    `json:"reason,omitempty"`
}

func deliveryJSON(r *store.DeliveryRecord) *workspaceDelivery {
	if r == nil {
		return nil
	}
	return &workspaceDelivery{
		ID: r.ID, Status: string(r.Status), TurnID: r.TurnID, TurnPosition: r.TurnPosition,
		NodeID: r.NodeID, NodeRunID: r.NodeRunID, BaseCommit: r.BaseCommit, DeliveryCommit: r.DeliveryCommit,
		CreatedAt: r.CreatedAt.UTC(), ReasonCode: r.ReasonCode, Reason: r.Reason,
	}
}

// deliveryState tells the panel what its change view shows: viewDelivery is the good delivery
// rendered, latestDelivery the newest outcome (failed included) and latestFailedDelivery the
// newest failure, which a later success would otherwise hide.
type deliveryState struct {
	Mode           string             `json:"mode"`
	InProgress     bool               `json:"inProgress"`
	ViewDelivery   *workspaceDelivery `json:"viewDelivery,omitempty"`
	LatestDelivery *workspaceDelivery `json:"latestDelivery,omitempty"`
	LatestFailed   *workspaceDelivery `json:"latestFailedDelivery,omitempty"`
}

type workspaceChanges struct {
	workspacefs.Changes
	DeliveryState deliveryState `json:"deliveryState"`
}

type workspaceDiff struct {
	workspacefs.Diff
	DeliveryState deliveryState `json:"deliveryState"`
}

func snapshotOf(r *store.DeliveryRecord) workspacefs.DeliverySnapshot {
	if r == nil {
		return workspacefs.DeliverySnapshot{}
	}
	return workspacefs.DeliverySnapshot{MirrorPath: r.MirrorPath, ProjectID: r.ProjectID, DeliveryID: r.ID,
		BaseCommit: r.BaseCommit, Commit: r.DeliveryCommit}
}

// legacy reports whether a source predates deliveries: no turn was ever enrolled and none was
// delivered. Once one is, the working directory is never read for changes again, even while the
// first delivery is pending or after it failed.
func legacy(v store.DeliveryView) bool { return v.Latest == nil && !v.HasWork }

// deliveryView reads the source's delivery state from one database snapshot. It writes the
// response itself and returns false on failure.
func deliveryView(w http.ResponseWriter, r *http.Request, src workspaceSource) (store.DeliveryView, bool) {
	v, e := src.store.WorkspaceDeliveryView(r.Context(), src.scope, src.delivery)
	switch {
	case errors.Is(e, store.ErrNotFound):
		fault(w, http.StatusNotFound, src.notFound)
		return v, false
	case e != nil:
		internal(w, "read workspace deliveries", e)
		return v, false
	}
	return v, true
}

// pinnedDelivery resolves a delivery ID the caller named, only within the authorized source. It
// writes the response itself and returns false when there is none to read.
func pinnedDelivery(w http.ResponseWriter, r *http.Request, src workspaceSource, id string) (store.DeliveryRecord, bool) {
	if !taskUUID.MatchString(id) {
		fault(w, http.StatusBadRequest, "invalid_delivery_id")
		return store.DeliveryRecord{}, false
	}
	rec, e := src.store.WorkspaceDeliveryByID(r.Context(), src.scope, src.delivery, id)
	switch {
	case errors.Is(e, store.ErrNotFound):
		fault(w, http.StatusNotFound, "delivery_not_found")
		return rec, false
	case e != nil:
		internal(w, "read workspace delivery", e)
		return rec, false
	case rec.Status == store.DeliveryFailed:
		faultWithParams(w, http.StatusConflict, "delivery_failed", map[string]any{"reasonCode": rec.ReasonCode, "reason": rec.Reason})
		return rec, false
	}
	return rec, true
}

// deliveryFault maps a mirror read failure. Path and size errors keep the live view's codes; any
// other failure is an unreadable mirror or ref, reported without its Git diagnostics.
func deliveryFault(w http.ResponseWriter, op string, e error) {
	switch {
	case errors.Is(e, workspacefs.ErrInvalidPath), errors.Is(e, workspacefs.ErrNotFile), errors.Is(e, workspacefs.ErrTooLarge):
		workspaceFault(w, e)
	default:
		slog.Error(op, "error", e)
		fault(w, http.StatusServiceUnavailable, "workspace_unavailable")
	}
}

// noStore marks a response private: it depends on who asks and on deliveries that keep arriving.
func noStore(w http.ResponseWriter) { w.Header().Set("Cache-Control", "private, no-store") }

func (h *handler) workspaceChanges(find resolveSource) func(http.ResponseWriter, *http.Request, access) {
	return func(w http.ResponseWriter, r *http.Request, a access) {
		noStore(w)
		src, ok := find(w, r, a)
		if !ok {
			return
		}
		v, ok := deliveryView(w, r, src)
		if !ok {
			return
		}
		if legacy(v) {
			ws, ok := src.checkout(w, r)
			if !ok {
				return
			}
			out, e := ws.Changes(r.Context())
			if e != nil {
				workspaceFault(w, e)
				return
			}
			writeJSON(w, http.StatusOK, workspaceChanges{out, deliveryState{Mode: modeWorkingDirectory}})
			return
		}
		state := deliveryState{Mode: modeDelivery, InProgress: v.InProgress,
			ViewDelivery: deliveryJSON(v.Good), LatestDelivery: deliveryJSON(v.Latest), LatestFailed: deliveryJSON(v.Failed)}
		if v.Good == nil {
			base, ok := recordedBase(w, r, src, v)
			if !ok {
				return
			}
			writeJSON(w, http.StatusOK, workspaceChanges{workspacefs.Changes{BaseCommit: base, Files: []workspacefs.Change{}}, state})
			return
		}
		if h.opts.Deliveries == nil {
			fault(w, http.StatusServiceUnavailable, "workspace_unavailable")
			return
		}
		out, e := h.opts.Deliveries.DeliveryChanges(r.Context(), snapshotOf(v.Good))
		if e != nil {
			deliveryFault(w, "read delivery changes", e)
			return
		}
		writeJSON(w, http.StatusOK, workspaceChanges{out, state})
	}
}

// recordedBase is the start commit to show before any good delivery: the latest outcome's if it
// recorded one, else the checkout's, else none.
func recordedBase(w http.ResponseWriter, r *http.Request, src workspaceSource, v store.DeliveryView) (string, bool) {
	if v.Latest != nil && v.Latest.BaseCommit != "" {
		return v.Latest.BaseCommit, true
	}
	at, e := src.location(r.Context())
	if errors.Is(e, store.ErrNotFound) {
		return "", true
	}
	if e != nil {
		internal(w, "read workspace location", e)
		return "", false
	}
	return at.BaseCommit, true
}

func (h *handler) workspaceDiff(find resolveSource) func(http.ResponseWriter, *http.Request, access) {
	return func(w http.ResponseWriter, r *http.Request, a access) {
		noStore(w)
		src, ok := find(w, r, a)
		if !ok {
			return
		}
		q := r.URL.Query()
		path, pinned := q.Get("path"), q.Get("deliveryId")
		v, ok := deliveryView(w, r, src)
		if !ok {
			return
		}
		if pinned == "" && legacy(v) {
			ws, ok := src.checkout(w, r)
			if !ok {
				return
			}
			out, e := ws.Diff(r.Context(), path)
			if e != nil {
				workspaceFault(w, e)
				return
			}
			writeJSON(w, http.StatusOK, workspaceDiff{out, deliveryState{Mode: modeWorkingDirectory}})
			return
		}
		view := v.Good
		if pinned != "" {
			rec, ok := pinnedDelivery(w, r, src, pinned)
			if !ok {
				return
			}
			view = &rec
		}
		if h.opts.Deliveries == nil {
			fault(w, http.StatusServiceUnavailable, "workspace_unavailable")
			return
		}
		// With no good delivery the snapshot is empty, which reads no Git and yields an empty patch
		// after the usual path checks.
		out, e := h.opts.Deliveries.DeliveryDiff(r.Context(), snapshotOf(view), path)
		if e != nil {
			deliveryFault(w, "read delivery diff", e)
			return
		}
		writeJSON(w, http.StatusOK, workspaceDiff{out, deliveryState{Mode: modeDelivery, InProgress: v.InProgress,
			ViewDelivery: deliveryJSON(view), LatestDelivery: deliveryJSON(v.Latest), LatestFailed: deliveryJSON(v.Failed)}})
	}
}

// workspacePatch downloads one delivery's whole patch against its base. The patch is staged in
// full, within the cap, before any header is written, so an error never follows partial bytes.
func (h *handler) workspacePatch(find resolveSource) func(http.ResponseWriter, *http.Request, access) {
	return func(w http.ResponseWriter, r *http.Request, a access) {
		noStore(w)
		src, ok := find(w, r, a)
		if !ok {
			return
		}
		rec, ok := pinnedDelivery(w, r, src, r.PathValue("deliveryId"))
		if !ok {
			return
		}
		if rec.Status == store.DeliveryEmpty {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if h.opts.Deliveries == nil {
			fault(w, http.StatusServiceUnavailable, "workspace_unavailable")
			return
		}
		limit := h.opts.MaxPatchBytes
		if limit <= 0 {
			limit = DefaultMaxPatchBytes
		}
		limit = min(limit, MaxPatchBytesCeiling)
		select {
		case h.patchSlots <- struct{}{}:
		case <-r.Context().Done():
			// The caller left while waiting; nobody reads an answer.
			return
		}
		// The slot bounds concurrent Git work and buffers, not the client's reading speed: it is
		// released as soon as the patch is built, and the write below has its own deadline, so a
		// few clients that stop reading cannot hold every slot. The source may have been deleted
		// while this request waited, so the delivery is looked up again under the slot.
		rec, ok = pinnedDelivery(w, r, src, r.PathValue("deliveryId"))
		if !ok {
			<-h.patchSlots
			return
		}
		patch, e := h.opts.Deliveries.DeliveryPatch(r.Context(), snapshotOf(&rec), limit)
		<-h.patchSlots
		switch {
		case errors.Is(e, workspacefs.ErrTooLarge):
			faultWithParams(w, http.StatusRequestEntityTooLarge, "patch_too_large", map[string]any{"message": "改动过大，无法在线下载", "maxBytes": limit})
			return
		case e != nil:
			slog.Error("read delivery patch", "delivery", rec.ID, "error", e)
			fault(w, http.StatusServiceUnavailable, "workspace_unavailable")
			return
		case len(patch) == 0:
			w.WriteHeader(http.StatusNoContent)
			return
		}
		header := w.Header()
		header.Set("Content-Type", "application/octet-stream")
		header.Set("Content-Disposition", `attachment; filename="delivery-`+rec.ID+`.patch"`)
		header.Set("Content-Length", strconv.Itoa(len(patch)))
		header.Set("X-Content-Type-Options", "nosniff")
		if e := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(patchWriteTimeout)); e != nil {
			slog.Warn("set delivery patch write deadline", "delivery", rec.ID, "error", e)
		}
		w.WriteHeader(http.StatusOK)
		if _, e := w.Write(patch); e != nil {
			slog.Error("write delivery patch", "delivery", rec.ID, "error", e)
		}
	}
}
