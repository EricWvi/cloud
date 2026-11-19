package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// members returns the workspace roster in one page; v2 workspaces are small enough to skip
// pagination, and the squad picker filters the list client-side.
func (h *handler) members(w http.ResponseWriter, r *http.Request, a access) {
	tenant := a.TenantID
	items, e := h.store.TenantMembers(r.Context(), tenant)
	if e != nil {
		internal(w, "tenant members", e)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": ""})
}

// invite adds one member to the workspace roster. It is an owner route; the address alone creates
// the account, so the invitee signs in with that address and nothing else. This route is
// outside Cloud's contract (see README), the v2 stand-in until a roster write reaches the contract.
//
// It exists only where email login does: without it an address cannot sign in, and members join by
// invitation link instead (A6 D4).
func (h *handler) invite(w http.ResponseWriter, r *http.Request, a access) {
	if !slices.Contains(h.loginProviders(), LoginProviderEmail) {
		fault(w, http.StatusNotFound, "not_found")
		return
	}
	tenant := a.TenantID
	var in struct {
		Email       string `json:"email"`
		DisplayName string `json:"displayName"`
		SquadID     string `json:"squadId"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if e := dec.Decode(&in); e != nil {
		fault(w, http.StatusBadRequest, "invalid_json")
		return
	}
	email, e := store.NormalizeEmail(in.Email)
	if e != nil {
		fault(w, http.StatusBadRequest, "invalid_email")
		return
	}
	display := strings.TrimSpace(in.DisplayName)
	if len([]rune(display)) > 64 {
		fault(w, http.StatusBadRequest, "invalid_display_name")
		return
	}
	if in.SquadID != "" && !squadUUID.MatchString(in.SquadID) {
		fault(w, http.StatusNotFound, "not_found")
		return
	}
	m, e := h.store.InviteMember(r.Context(), tenant, store.Invite{Email: email, DisplayName: display, SquadID: in.SquadID})
	switch {
	case errors.Is(e, store.ErrSquadNotFound):
		fault(w, http.StatusNotFound, "not_found")
	case errors.Is(e, store.ErrAlreadyMember):
		fault(w, http.StatusConflict, "member_exists")
	case errors.Is(e, store.ErrUserUnavailable):
		fault(w, http.StatusConflict, "user_unavailable")
	case e != nil:
		internal(w, "invite member", e)
	default:
		writeJSON(w, http.StatusCreated, m)
	}
}

// removeMember ends one person's membership; their account stays and can still sign in, but the
// next request no longer sees this workspace. The last owner cannot be removed.
func (h *handler) removeMember(w http.ResponseWriter, r *http.Request, a access) {
	uid := r.PathValue("uid")
	if !squadUUID.MatchString(uid) {
		fault(w, http.StatusNotFound, "member_not_found")
		return
	}
	switch e := h.store.RemoveMember(r.Context(), a.TenantID, uid); {
	case errors.Is(e, store.ErrNotFound):
		fault(w, http.StatusNotFound, "member_not_found")
	case errors.Is(e, store.ErrLastOwner):
		fault(w, http.StatusConflict, "last_owner")
	case e != nil:
		internal(w, "remove member", e)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// inviteURL is where a link's token is opened: absolute under V2_PUBLIC_URL when it is set,
// otherwise site-relative for the page to resolve against its own address.
func (h *handler) inviteURL(token string) string {
	path := "/invite/" + token
	if p := h.opts.PublicURL; p != nil {
		return p.Scheme + "://" + p.Host + path
	}
	return path
}

// createInviteLink makes a one-time member link, optionally into one squad. The response is the
// only place the link's token ever appears.
func (h *handler) createInviteLink(w http.ResponseWriter, r *http.Request, a access) {
	var in struct {
		SquadID string `json:"squadId"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if e := dec.Decode(&in); e != nil {
		fault(w, http.StatusBadRequest, "invalid_json")
		return
	}
	if in.SquadID != "" && !squadUUID.MatchString(in.SquadID) {
		fault(w, http.StatusNotFound, "squad_not_found")
		return
	}
	link, token, e := h.store.CreateInviteLink(r.Context(), a.TenantID, a.UserID, in.SquadID)
	switch {
	case errors.Is(e, store.ErrSquadNotFound):
		fault(w, http.StatusNotFound, "squad_not_found")
	case e != nil:
		internal(w, "create invite link", e)
	default:
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusCreated, map[string]any{"link": link, "url": h.inviteURL(token)})
	}
}

// inviteLinks lists the workspace's usable links and who used the others; never a token.
func (h *handler) inviteLinks(w http.ResponseWriter, r *http.Request, a access) {
	items, e := h.store.ListInviteLinks(r.Context(), a.TenantID)
	if e != nil {
		internal(w, "list invite links", e)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *handler) revokeInviteLink(w http.ResponseWriter, r *http.Request, a access) {
	id := r.PathValue("id")
	if !squadUUID.MatchString(id) {
		fault(w, http.StatusNotFound, "invite_not_found")
		return
	}
	switch e := h.store.RevokeInviteLink(r.Context(), a.TenantID, id); {
	case errors.Is(e, store.ErrNotFound):
		fault(w, http.StatusNotFound, "invite_not_found")
	case errors.Is(e, store.ErrInviteUsed):
		fault(w, http.StatusConflict, "invite_already_used")
	case e != nil:
		internal(w, "revoke invite link", e)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// inviteToken reads the token from the body, never the path, so it stays out of access logs.
func inviteToken(w http.ResponseWriter, r *http.Request) (string, bool) {
	var in struct {
		Token string `json:"token"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if e := dec.Decode(&in); e != nil {
		fault(w, http.StatusBadRequest, "invalid_json")
		return "", false
	}
	if in.Token == "" || len(in.Token) > 128 {
		fault(w, http.StatusNotFound, "invite_not_found")
		return "", false
	}
	return in.Token, true
}

// previewInvite shows where a link leads and whether it can still be used; it changes nothing.
func (h *handler) previewInvite(w http.ResponseWriter, r *http.Request, u store.User) {
	token, ok := inviteToken(w, r)
	if !ok {
		return
	}
	p, e := h.store.PreviewInvite(r.Context(), token, u.ID)
	switch {
	case errors.Is(e, store.ErrNotFound):
		fault(w, http.StatusNotFound, "invite_not_found")
	case e != nil:
		internal(w, "preview invite", e)
	default:
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, p)
	}
}

// acceptInvite joins the signed-in user to the link's workspace. A member already in it gets the
// workspace back without using the link up.
func (h *handler) acceptInvite(w http.ResponseWriter, r *http.Request, u store.User) {
	token, ok := inviteToken(w, r)
	if !ok {
		return
	}
	tenant, e := h.store.AcceptInvite(r.Context(), token, u.ID)
	var unavailable store.InviteUnavailableError
	switch {
	case errors.Is(e, store.ErrNotFound):
		fault(w, http.StatusNotFound, "invite_not_found")
	case errors.As(e, &unavailable):
		faultWithParams(w, http.StatusGone, "invite_unavailable", map[string]any{"reason": unavailable.Reason})
	case e != nil:
		internal(w, "accept invite", e)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"tenantId": tenant})
	}
}
