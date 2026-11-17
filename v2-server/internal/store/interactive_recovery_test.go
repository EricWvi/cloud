package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

// workflowAgentSession records an Agent session whose first turn the workflow run's node attempt
// owns, as the execution service does for a workflow node, and ends that turn normally.
func workflowAgentSession(t *testing.T, s *store.Store, scope execution.Scope, projectID, runID, nodeRunID, sid string) {
	t.Helper()
	ctx := context.Background()
	session := execution.Session{ID: sid, Target: &execution.Target{ProjectID: projectID}, Executor: &execution.Executor{ID: "opencode:fixture"},
		Model: execution.Model{ID: "fixture", Name: "Fixture", BindingVersion: "v1"}, WorkspaceID: "workspace-" + runID}
	turn := execution.Turn{Ref: execution.Ref{SessionID: sid, TurnID: sid + "-first"}, Caller: execution.Caller{Kind: execution.CallerWorkflow, ID: runID, NodeID: "agent-1", AttemptID: nodeRunID}, Prompt: execution.TextPrompt("起草")}
	if err := s.CreateExecutionSession(ctx, scope, session, turn); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishExecutionTurn(ctx, scope, turn.Ref, execution.Result{Status: execution.StatusCompleted, Answer: "初稿", AnswerComplete: true, StopReason: "end_turn"}); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryKeepsTheSessionOfAParkedInteractiveNode(t *testing.T) {
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "interactive-recovery")
	scope := execution.Scope{TenantID: tenant, UserID: user}
	ctx := context.Background()
	project := workflowProject(t, s, user, tenant, "project-recovery")
	w, published := publishedWorkflow(t, s, tenant, "交互恢复", "v1")
	run, err := s.CreateWorkflowRun(ctx, tenant, w.ID, published.ID, store.WorkflowRunInput{ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartWorkflowRun(ctx, tenant, run.ID); err != nil {
		t.Fatal(err)
	}
	attempt := func(nodeID, sid string, park bool) store.WorkflowNodeRun {
		t.Helper()
		node, err := s.CreateWorkflowNodeRun(ctx, tenant, run.ID, store.WorkflowNodeRunInput{NodeID: nodeID, NodeType: "agent"})
		if err != nil {
			t.Fatal(err)
		}
		workflowAgentSession(t, s, scope, project.ID, run.ID, node.ID, sid)
		if _, err := s.BindWorkflowNodeSession(ctx, tenant, run.ID, node.ID, sid); err != nil {
			t.Fatal(err)
		}
		if park {
			if moved, err := s.MoveWorkflowEngineNode(ctx, run.ID, node.ID, store.WorkflowStatusRunning, store.WorkflowStatusPending,
				func([]byte) ([]byte, error) { return []byte(`{"interactive":{"turns":1}}`), nil }); err != nil || !moved {
				t.Fatalf("park = %v %v", moved, err)
			}
		}
		return node
	}
	parked := attempt("agent-1", "parked", true)
	attempt("agent-2", "executing", false) // its node is running: no interactive wait to keep

	kept, err := s.RecoverExecutions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].Session.ID != "parked" || kept[0].Scope != scope || kept[0].Caller.AttemptID != parked.ID || kept[0].Caller.Kind != execution.CallerWorkflow {
		t.Fatalf("kept = %+v, want only the parked node's session with its owner and caller", kept)
	}
	if session, err := s.ReadExecutionSession(ctx, scope, "parked"); err != nil || session.EndedAt != nil {
		t.Fatalf("parked session = %+v %v, want it open", session, err)
	}
	if session, err := s.ReadExecutionSession(ctx, scope, "executing"); err != nil || session.EndedAt == nil {
		t.Fatalf("executing session = %+v %v, want it ended as before", session, err)
	}
	// The kept session accepts the next turn: it is the same conversation.
	next := execution.Turn{Ref: execution.Ref{SessionID: "parked", TurnID: "parked-next"}, Caller: execution.Caller{Kind: execution.CallerWorkflow, ID: run.ID, NodeID: "agent-1", AttemptID: parked.ID + "/turn-1"}, Prompt: execution.TextPrompt("再改")}
	if err := s.AppendExecutionTurn(ctx, scope, "parked", next); err != nil {
		t.Fatalf("append to the kept session: %v", err)
	}

	// While the node still waits nothing is left to end; once its run stops, its session is.
	if open, err := s.UnparkedWorkflowSessions(ctx, ""); err != nil || len(open) != 0 {
		t.Fatalf("unparked = %+v %v, want none while the node waits", open, err)
	}
	if _, err := s.FinishWorkflowRun(ctx, tenant, run.ID, store.WorkflowStatusFailed, nil, nil); err != nil {
		t.Fatal(err)
	}
	if other, err := s.UnparkedWorkflowSessions(ctx, "00000000-0000-0000-0000-000000000000"); err != nil || len(other) != 0 {
		t.Fatalf("unparked of another run = %+v %v, want none", other, err)
	}
	open, err := s.UnparkedWorkflowSessions(ctx, run.ID)
	if err != nil || len(open) != 1 || open[0] != (store.OpenWorkflowSession{TenantID: tenant, UserID: user, SessionID: "parked"}) {
		t.Fatalf("unparked = %+v %v, want the failed run's session", open, err)
	}
}

func TestWorkspaceKeepsItsCloneExecution(t *testing.T) {
	s := storetest.Open(t)
	ctx := context.Background()
	if _, err := s.ExecutionWorkspace(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown workspace = %v", err)
	}
	at := store.WorkspaceLocation{Path: "/data/w/r/one", BaseCommit: "abc", CloneExecution: "c-1-workspace"}
	if err := s.RecordWorkspace(ctx, "workspace", at); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ExecutionWorkspace(ctx, "workspace"); err != nil || got != at {
		t.Fatalf("workspace = %+v %v, want %+v", got, err, at)
	}
}
