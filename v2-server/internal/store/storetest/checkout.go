package storetest

import (
	"crypto/rand"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// TurnOutcome is how one turn of a fixture conversation ends, as far as delivery is concerned.
type TurnOutcome string

// Turn outcomes for ChatWithCheckout.
const (
	// TurnDelivered, TurnEmpty and TurnFailed end the turn with a delivery of that result.
	TurnDelivered TurnOutcome = "delivered"
	TurnEmpty     TurnOutcome = "empty"
	TurnFailed    TurnOutcome = "failed"
	// TurnDelivering leaves the turn admitted with its delivery obligation and not ended.
	TurnDelivering TurnOutcome = "delivering"
	// TurnUnenrolled ends the turn without a delivery obligation. As the first turn it makes the
	// session one from before turn delivery.
	TurnUnenrolled TurnOutcome = "unenrolled"
)

// BaseCommit is the commit fixture checkouts start from.
var BaseCommit = strings.Repeat("a", 40)

// ChatCheckout is a fixture conversation with one session on a recorded checkout.
type ChatCheckout struct {
	Scope                                                   execution.Scope
	ProjectID, ConversationID, SessionID, WorkspaceID, Path string
	// Turns are the session's turn references, in order.
	Turns      []execution.Ref
	Repository Repository
}

// UUID returns a random version 4 UUID.
func UUID(t testing.TB) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ChatWithCheckout creates a project and a conversation of scope's user whose session runs one
// turn per outcome on a checkout recorded at path. The session stays live; End ends it. Its
// deliveries are records only: the mirror and commits are not real.
func ChatWithCheckout(t *testing.T, s *store.Store, scope execution.Scope, path string, outcomes ...TurnOutcome) ChatCheckout {
	t.Helper()
	return ChatWithRepository(t, s, scope, Repository{Mirror: "/mirror", Base: BaseCommit}, path, outcomes...)
}

// Repository is where a fixture conversation's deliveries live. Deliver, when set, makes the
// delivered commit for turn i (for instance with workspacefs.DeliveryFS); otherwise it is fake.
type Repository struct {
	Mirror, Base string
	Deliver      func(i int) string
}

// ChatWithRepository is ChatWithCheckout on a given mirror and base.
func ChatWithRepository(t *testing.T, s *store.Store, scope execution.Scope, repo Repository, path string, outcomes ...TurnOutcome) ChatCheckout {
	t.Helper()
	ctx := t.Context()
	project, err := s.CreateProject(ctx, scope, store.Project{
		Name: "reclaim-" + UUID(t)[:8], ID: UUID(t), RepositoryURL: "https://example.test/git", MirrorPath: repo.Mirror, Commit: repo.Base,
	})
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := s.CreateChatConversation(ctx, store.NewChatConversation{
		TenantID: scope.TenantID, UserID: scope.UserID, CreateKey: UUID(t), ProjectID: project.ID, ExecutorID: "fixture", Title: "reclaim",
	})
	if err != nil {
		t.Fatal(err)
	}
	c := ChatCheckout{Repository: repo, Scope: scope, ProjectID: project.ID, ConversationID: conversation.ID, SessionID: "session-" + UUID(t), WorkspaceID: "workspace-" + UUID(t), Path: path}
	session := execution.Session{ID: c.SessionID, WorkspaceID: c.WorkspaceID, Target: &execution.Target{ProjectID: project.ID},
		Model: execution.Model{ID: "fixture", Name: "Fixture", BindingVersion: "v1"}}
	for i, outcome := range outcomes {
		attempt := UUID(t)
		turn := execution.Turn{Ref: execution.Ref{SessionID: c.SessionID, TurnID: "turn-" + attempt}, Prompt: execution.TextPrompt("hi"),
			Caller: execution.Caller{Kind: execution.CallerChat, ID: conversation.ID, AttemptID: attempt}, StartedAt: time.Now().UTC()}
		deliver := outcome != TurnUnenrolled
		if i == 0 {
			_, err = s.AdmitExecutionSession(ctx, scope, session, turn, deliver)
		} else {
			_, err = s.AdmitExecutionTurn(ctx, scope, c.SessionID, turn, deliver)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := s.RecordChatTurn(ctx, conversation.ID, c.SessionID, attempt, turn.Ref.TurnID, time.Now()); err != nil {
			t.Fatal(err)
		}
		c.Turns = append(c.Turns, turn.Ref)
		if i == 0 {
			if err := s.RecordWorkspace(ctx, c.WorkspaceID, store.WorkspaceLocation{Path: path, BaseCommit: repo.Base, CloneExecution: "clone-" + c.WorkspaceID}); err != nil {
				t.Fatal(err)
			}
		}
		result := execution.Result{Status: execution.StatusCompleted, Answer: "ok", AnswerComplete: true, StopReason: "stop"}
		switch outcome {
		case TurnDelivering:
			if i != len(outcomes)-1 {
				t.Fatal("only the last turn can still be delivering")
			}
		case TurnUnenrolled:
			if _, err := s.FinishExecutionTurn(ctx, scope, turn.Ref, result); err != nil {
				t.Fatal(err)
			}
		default:
			c.Deliver(t, s, i, outcome)
		}
	}
	return c
}

// Deliver settles the delivery of turn i, which must still be delivering, with outcome.
func (c ChatCheckout) Deliver(t *testing.T, s *store.Store, i int, outcome TurnOutcome) {
	t.Helper()
	ctx := t.Context()
	ref := c.Turns[i]
	if _, err := s.StageDeliveryTerminal(ctx, c.Scope, ref, execution.Result{Status: execution.StatusCompleted, Answer: "ok", AnswerComplete: true, StopReason: "stop"}); err != nil {
		t.Fatal(err)
	}
	work, err := s.DeliveryWork(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	record := store.DeliveryRecord{ID: UUID(t), BaseCommit: c.Repository.Base, Status: store.DeliveryStatus(outcome)}
	switch outcome {
	case TurnDelivered:
		record.DeliveryCommit = strings.Repeat("b", 40)
		if c.Repository.Deliver != nil {
			record.DeliveryCommit = c.Repository.Deliver(i)
		}
	case TurnFailed:
		record.ReasonCode, record.Reason = "delivery_unavailable", "无法保存本轮交付"
	case TurnEmpty:
	default:
		t.Fatalf("cannot deliver %q", outcome)
	}
	if _, err := s.CommitTurnDelivery(ctx, work, record); err != nil {
		t.Fatal(err)
	}
}

// End ends the conversation's session, as deleting a live conversation does first.
func (c ChatCheckout) End(t *testing.T, s *store.Store) {
	t.Helper()
	if err := s.EndExecutionSession(t.Context(), c.Scope, c.SessionID); err != nil {
		t.Fatal(err)
	}
}
