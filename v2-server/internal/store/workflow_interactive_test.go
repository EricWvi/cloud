package store_test

import (
	"context"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

func TestInteractiveNodeMovesOnlyUnderARunningRun(t *testing.T) {
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "interactive-owner")
	ctx := context.Background()
	project := workflowProject(t, s, user, tenant, "project-interactive")
	w, published := publishedWorkflow(t, s, tenant, "交互工作流", "v1")
	run, err := s.CreateWorkflowRun(ctx, tenant, w.ID, published.ID, store.WorkflowRunInput{ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartWorkflowRun(ctx, tenant, run.ID); err != nil {
		t.Fatal(err)
	}
	node, err := s.CreateWorkflowNodeRun(ctx, tenant, run.ID, store.WorkflowNodeRunInput{NodeID: "agent-1", NodeType: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	keep := func(p []byte) ([]byte, error) { return []byte(`{"interactive":{"turns":1}}`), nil }
	read := func() store.WorkflowNodeRun {
		t.Helper()
		nodes, err := s.WorkflowRunNodes(ctx, tenant, run.ID)
		if err != nil || len(nodes) != 1 {
			t.Fatalf("nodes = %+v %v", nodes, err)
		}
		return nodes[0]
	}

	if moved, err := s.MoveWorkflowEngineNode(ctx, run.ID, node.ID, store.WorkflowStatusRunning, store.WorkflowStatusPending, keep); err != nil || !moved {
		t.Fatalf("park = %v %v", moved, err)
	}
	if n := read(); n.Status != store.WorkflowStatusPending || string(n.Payload) != `{"interactive": {"turns": 1}}` {
		t.Fatalf("parked row = %s %s, want the summary written with the flip", n.Status, n.Payload)
	}
	// A second park finds the node already pending: nothing moves.
	if moved, err := s.MoveWorkflowEngineNode(ctx, run.ID, node.ID, store.WorkflowStatusRunning, store.WorkflowStatusPending, keep); err != nil || moved {
		t.Fatalf("double park = %v %v", moved, err)
	}
	if _, err := s.MoveWorkflowEngineNode(ctx, run.ID, node.ID, store.WorkflowStatusPending, store.WorkflowStatusSucceeded, keep); err == nil {
		t.Fatal("a move outside running ↔ pending was accepted")
	}
	// Orphans close only once the run stopped running.
	if closed, err := s.CloseOrphanedWorkflowNode(ctx, run.ID, node.ID); err != nil || closed {
		t.Fatalf("close under a running run = %v %v", closed, err)
	}

	if _, err := s.FinishWorkflowRun(ctx, tenant, run.ID, store.WorkflowStatusFailed, nil, nil); err != nil {
		t.Fatal(err)
	}
	if moved, err := s.MoveWorkflowEngineNode(ctx, run.ID, node.ID, store.WorkflowStatusPending, store.WorkflowStatusRunning, keep); err != nil || moved {
		t.Fatalf("turn under a failed run = %v %v", moved, err)
	}
	if n := read(); n.Status != store.WorkflowStatusPending {
		t.Fatalf("row under a failed run moved to %s", n.Status)
	}
	if closed, err := s.CloseOrphanedWorkflowNode(ctx, run.ID, node.ID); err != nil || !closed {
		t.Fatalf("close orphan = %v %v", closed, err)
	}
	if n := read(); n.Status != store.WorkflowStatusCancelled || n.FinishedAt == nil {
		t.Fatalf("orphan = %+v", n)
	}
}
