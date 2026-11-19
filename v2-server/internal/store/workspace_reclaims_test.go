package store_test

import (
	"errors"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	st "github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

func TestChatCheckoutReadsTheSessionsDeliveries(t *testing.T) {
	f := executionDatabase(t)
	cases := []struct {
		name                  string
		outcomes              []st.TurnOutcome
		busy, delivered, good bool
		last                  store.DeliveryStatus
	}{
		{"delivered", []st.TurnOutcome{st.TurnDelivered}, false, true, true, store.DeliveryDelivered},
		{"empty after delivered", []st.TurnOutcome{st.TurnDelivered, st.TurnEmpty}, false, true, true, store.DeliveryEmpty},
		{"failed last", []st.TurnOutcome{st.TurnDelivered, st.TurnFailed}, false, true, true, store.DeliveryFailed},
		{"delivering", []st.TurnOutcome{st.TurnDelivered, st.TurnDelivering}, true, true, true, ""},
		{"legacy", []st.TurnOutcome{st.TurnUnenrolled}, false, false, false, ""},
		// The newest delivery belongs to an earlier turn: the last turn has none.
		{"last turn unenrolled", []st.TurnOutcome{st.TurnDelivered, st.TurnUnenrolled}, false, true, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := st.ChatWithCheckout(t, f.s, f.scope, "/checkouts/"+st.UUID(t), tc.outcomes...)
			got, err := f.s.ChatCheckout(t.Context(), f.scope.TenantID, f.scope.UserID, c.ConversationID)
			if err != nil || !got.Registered || got.Path != c.Path || got.WorkspaceID != c.WorkspaceID || got.BaseCommit != st.BaseCommit || got.MirrorPath != "/mirror" {
				t.Fatalf("checkout = %+v, %v", got, err)
			}
			d := got.Delivery
			if d.Busy != tc.busy || d.Delivered != tc.delivered || d.LastStatus != tc.last || (d.GoodDeliveryID != "") != tc.good ||
				d.LastTurnID != c.Turns[len(c.Turns)-1].TurnID {
				t.Fatalf("delivery = %+v", d)
			}
			if good := d.LastStatus == store.DeliveryDelivered || d.LastStatus == store.DeliveryEmpty; good != (d.LastDeliveryID != "") {
				t.Fatalf("last delivery id = %q for status %q", d.LastDeliveryID, d.LastStatus)
			}
		})
	}
}

func TestChatCheckoutWithoutCheckoutAndForOthers(t *testing.T) {
	f := executionDatabase(t)
	ctx := t.Context()
	bare, err := f.s.CreateChatConversation(ctx, store.NewChatConversation{
		TenantID: f.scope.TenantID, UserID: f.scope.UserID, CreateKey: "bare", ProjectID: "p", ExecutorID: "fixture", Title: "bare",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.ChatCheckout(ctx, f.scope.TenantID, f.scope.UserID, bare.ID); err != nil || got.Registered {
		t.Fatalf("no session: %+v, %v", got, err)
	}
	// Deleting it records nothing: there is no checkout.
	if ws, err := f.s.DeleteChatConversationReclaiming(ctx, f.scope.TenantID, f.scope.UserID, bare.ID, store.ReclaimRecoverable); err != nil || ws != "" {
		t.Fatalf("delete without checkout = %q, %v", ws, err)
	}
	c := st.ChatWithCheckout(t, f.s, f.scope, "/checkouts/mine", st.TurnDelivered)
	other := executionMember(t, f.s, "reclaim-other")
	if _, err := f.s.ChatCheckout(ctx, other.TenantID, other.UserID, c.ConversationID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other user's conversation: %v", err)
	}
	if _, err := f.s.DeleteChatConversationReclaiming(ctx, other.TenantID, other.UserID, c.ConversationID, store.ReclaimDiscard); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other user deleted the conversation: %v", err)
	}
	// Removing a conversation whose first turn was refused is not a person's deletion.
	if err := f.s.DeleteChatConversation(ctx, f.scope.TenantID, f.scope.UserID, c.ConversationID); err != nil {
		t.Fatal(err)
	}
	if list, err := f.s.WorkspaceReclaims(ctx); err != nil || len(list) != 0 {
		t.Fatalf("reclaims recorded outside a person's deletion: %+v, %v", list, err)
	}
}

func TestDeleteChatConversationRecordsReclaimInTheSameTransaction(t *testing.T) {
	f := executionDatabase(t)
	ctx := t.Context()
	c := st.ChatWithCheckout(t, f.s, f.scope, "/checkouts/deleted", st.TurnDelivered, st.TurnFailed)
	c.End(t, f.s)
	if _, err := f.s.DeleteChatConversationReclaiming(ctx, f.scope.TenantID, f.scope.UserID, c.ConversationID, "keep"); !errors.Is(err, store.ErrInvalidReclaimMode) {
		t.Fatalf("invalid mode: %v", err)
	}
	workspace, err := f.s.DeleteChatConversationReclaiming(ctx, f.scope.TenantID, f.scope.UserID, c.ConversationID, store.ReclaimDiscard)
	if err != nil || workspace != c.WorkspaceID {
		t.Fatalf("delete = %q, %v", workspace, err)
	}
	if _, err := f.s.ChatConversation(ctx, f.scope.TenantID, f.scope.UserID, c.ConversationID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("conversation still there: %v", err)
	}
	r, err := f.s.WorkspaceReclaim(ctx, c.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if r.ProjectID != c.ProjectID || r.SessionID != c.SessionID || r.ConversationID != c.ConversationID || r.Path != c.Path ||
		r.Mode != store.ReclaimDiscard || r.Status != store.ReclaimPending || r.TenantID != f.scope.TenantID {
		t.Fatalf("reclaim = %+v", r)
	}
	// The judgement reads the session alone, now that the conversation is gone.
	checkout, err := f.s.ReclaimCheckout(ctx, r)
	if err != nil || checkout.BaseCommit != st.BaseCommit || checkout.MirrorPath != "/mirror" || checkout.Delivery.LastStatus != store.DeliveryFailed {
		t.Fatalf("reclaim checkout = %+v, %v", checkout, err)
	}
	if id, err := f.s.CheckReclaimIdentity(ctx, r); err != nil || !id.Recorded || id.Live || id.Shared {
		t.Fatalf("identity = %+v, %v", id, err)
	}
	if err := f.s.NoteWorkspaceReclaimAttempt(ctx, c.WorkspaceID, "disk"); err != nil {
		t.Fatal(err)
	}
	basis := store.ReclaimBasis{DeliveryID: st.UUID(t), DeliveryCommit: "b", BaseCommit: st.BaseCommit}
	if moved, err := f.s.MarkWorkspaceDeleting(ctx, c.WorkspaceID, basis); err != nil || !moved {
		t.Fatalf("mark deleting = %v, %v", moved, err)
	}
	if moved, err := f.s.MarkWorkspaceDeleting(ctx, c.WorkspaceID, basis); err != nil || moved {
		t.Fatalf("a deleting reclaim moved again: %v, %v", moved, err)
	}
	basis.Bytes, basis.Files = 10, 2
	if err := f.s.FinishWorkspaceReclaimed(ctx, c.WorkspaceID, basis); err != nil {
		t.Fatal(err)
	}
	// A finished reclaim is final.
	if err := f.s.FinishWorkspaceKept(ctx, c.WorkspaceID, "late"); err != nil {
		t.Fatal(err)
	}
	r, err = f.s.WorkspaceReclaim(ctx, c.WorkspaceID)
	if err != nil || r.Status != store.ReclaimReclaimed || r.Attempts != 1 || r.LastError != "disk" || r.RemovedBytes != 10 ||
		r.RemovedFiles != 2 || r.DeliveryID != basis.DeliveryID || r.DeliveryCommit != "b" || r.BaseCommit != st.BaseCommit || r.FinishedAt == nil {
		t.Fatalf("finished reclaim = %+v, %v", r, err)
	}
	if open, err := f.s.WorkspaceReclaims(ctx, store.ReclaimPending, store.ReclaimDeleting); err != nil || len(open) != 0 {
		t.Fatalf("open after finish = %+v, %v", open, err)
	}
	// The workspace record stays: deliveries and recovery look it up by ID.
	if _, err := f.s.ExecutionWorkspace(ctx, c.WorkspaceID); err != nil {
		t.Fatalf("workspace record: %v", err)
	}
}

func TestCheckReclaimIdentitySeesLiveSharedAndChangedCheckouts(t *testing.T) {
	f := executionDatabase(t)
	ctx := t.Context()
	c := st.ChatWithCheckout(t, f.s, f.scope, "/checkouts/live", st.TurnDelivered)
	if _, err := f.s.DeleteChatConversationReclaiming(ctx, f.scope.TenantID, f.scope.UserID, c.ConversationID, store.ReclaimRecoverable); err != nil {
		t.Fatal(err)
	}
	r, err := f.s.WorkspaceReclaim(ctx, c.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := f.s.CheckReclaimIdentity(ctx, r); err != nil || !id.Live || id.Shared || !id.Recorded {
		t.Fatalf("live session not seen: %+v, %v", id, err)
	}
	turn := execution.Turn{Ref: execution.Ref{SessionID: "shared-" + st.UUID(t), TurnID: "t-" + st.UUID(t)}, Prompt: execution.TextPrompt("x"),
		Caller: execution.Caller{Kind: execution.CallerChat, ID: "other", AttemptID: st.UUID(t)}}
	err = f.s.CreateExecutionSession(ctx, f.scope, execution.Session{ID: turn.Ref.SessionID, WorkspaceID: c.WorkspaceID,
		Target: &execution.Target{ProjectID: c.ProjectID}, Model: execution.Model{ID: "fixture", Name: "Fixture", BindingVersion: "v1"}}, turn)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := f.s.CheckReclaimIdentity(ctx, r); err != nil || !id.Shared {
		t.Fatalf("shared checkout not seen: %+v, %v", id, err)
	}
	// Another workspace recording the same path, or a different clone execution, breaks identity.
	if err := f.s.RecordWorkspace(ctx, "other-"+st.UUID(t), store.WorkspaceLocation{Path: c.Path, BaseCommit: st.BaseCommit}); err != nil {
		t.Fatal(err)
	}
	if id, err := f.s.CheckReclaimIdentity(ctx, r); err != nil || id.Recorded {
		t.Fatalf("path recorded twice still counts as this checkout's: %+v, %v", id, err)
	}
	r.CloneExecution = "another-clone"
	if id, err := f.s.CheckReclaimIdentity(ctx, r); err != nil || id.Recorded {
		t.Fatalf("different clone execution still counts: %+v, %v", id, err)
	}
}
