package store_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

func TestDeliveryStoreStagesAndScopesOutcomes(t *testing.T) {
	f := executionDatabase(t)
	ctx := t.Context()
	base := strings.Repeat("a", 40)
	project, err := f.s.CreateProject(ctx, f.scope, store.Project{
		Name: "delivery", ID: "95292240-3c19-4665-b1b0-70dfdff3fc00", RepositoryURL: "https://example.test/git", MirrorPath: "/mirror", Commit: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.s.CreateChatConversation(ctx, store.NewChatConversation{
		TenantID: f.scope.TenantID, UserID: f.scope.UserID, CreateKey: "delivery", ProjectID: project.ID, ExecutorID: "fixture", Title: "delivery",
	})
	if err != nil {
		t.Fatal(err)
	}
	source := store.DeliverySource{Kind: execution.CallerChat, ID: c.ID}
	turn := executionTurn("delivery-session", "delivery-turn")
	turn.Caller = execution.Caller{Kind: source.Kind, ID: source.ID, AttemptID: "first"}
	session := execution.Session{ID: turn.Ref.SessionID, WorkspaceID: "delivery-workspace", Target: &execution.Target{ProjectID: project.ID}}
	if err := f.s.CreateDeliveryExecutionSession(ctx, f.scope, session, turn); err != nil {
		t.Fatal(err)
	}
	result := execution.Result{Status: execution.StatusFailed, FailureCode: "first"}
	if _, err := f.s.StageDeliveryTerminal(ctx, f.scope, turn.Ref, result); err != nil {
		t.Fatal(err)
	}
	winner, err := f.s.StageDeliveryTerminal(ctx, f.scope, turn.Ref, execution.Result{Status: execution.StatusCancelled})
	if err != nil || winner.FailureCode != "first" {
		t.Fatalf("staged result = %+v, %v", winner, err)
	}
	page, err := f.s.ReadExecutionTurns(ctx, f.scope, session.ID, "", 1)
	if err != nil || len(page.Items) != 1 || page.Items[0].Result != nil {
		t.Fatalf("premature publication: %+v, %v", page, err)
	}
	view, err := f.s.WorkspaceDeliveryView(ctx, f.scope, source)
	if err != nil || !view.InProgress || view.Latest != nil {
		t.Fatalf("pending view: %+v, %v", view, err)
	}
	work, err := f.s.DeliveryWork(ctx, turn.Ref)
	if err != nil {
		t.Fatal(err)
	}
	record := store.DeliveryRecord{ID: "f388d366-7313-4f70-8355-9423589a3446", Status: store.DeliveryEmpty, BaseCommit: base}
	for range 2 {
		got, err := f.s.CommitTurnDelivery(ctx, work, record)
		if err != nil || got.ID != record.ID {
			t.Fatalf("commit delivery: %+v, %v", got, err)
		}
	}
	view, err = f.s.WorkspaceDeliveryView(ctx, f.scope, source)
	if err != nil || view.InProgress || view.Latest == nil || view.Good == nil || view.Good.ID != record.ID || view.Failed != nil {
		t.Fatalf("settled view: %+v, %v", view, err)
	}
	got, err := f.s.WorkspaceDeliveryByID(ctx, f.scope, source, record.ID)
	if err != nil || got.MirrorPath != project.MirrorPath {
		t.Fatalf("lookup: %+v, %v", got, err)
	}
	other := executionMember(t, f.s, "delivery-non-owner")
	if _, err := f.s.WorkspaceDeliveryByID(ctx, other, source, record.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("non-owner delivery lookup: %v", err)
	}
	if err := f.s.DeleteChatConversation(ctx, f.scope.TenantID, f.scope.UserID, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.WorkspaceDeliveryView(ctx, f.scope, source); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted source view: %v", err)
	}
}
