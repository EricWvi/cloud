package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

// slotWaitOf reads the attempt's `session_slot_wait` marker, nil when it has none.
func slotWaitOf(t *testing.T, s *store.Store, runID, nodeRunID string) *store.WorkflowSessionSlotWait {
	t.Helper()
	nodes, err := s.WorkflowEngineNodeRuns(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.ID != nodeRunID {
			continue
		}
		var p struct {
			Wait *store.WorkflowSessionSlotWait `json:"session_slot_wait"`
		}
		if len(n.Payload) != 0 {
			if err := json.Unmarshal(n.Payload, &p); err != nil {
				t.Fatal(err)
			}
		}
		return p.Wait
	}
	t.Fatalf("node run %s not found", nodeRunID)
	return nil
}

// Binding a session ends the wait even when the bridge's own clear never landed, so an executing
// node cannot keep reading as queued.
func TestBindingASessionDropsALeftoverSlotWait(t *testing.T) {
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "slot-wait-owner")
	ctx := context.Background()
	run, _ := startedRun(t, s, user, tenant, "名额")
	node, err := s.CreateWorkflowNodeRun(ctx, tenant, run.ID, store.WorkflowNodeRunInput{NodeID: "agent-1", NodeType: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	wait := store.WorkflowSessionSlotWait{Since: 1_700_000_000_000, Limit: 8}
	if err := s.MarkWorkflowNodeAwaitingSessionSlot(ctx, run.ID, node.ID, wait); err != nil {
		t.Fatal(err)
	}
	if got := slotWaitOf(t, s, run.ID, node.ID); got == nil || *got != wait {
		t.Fatalf("marker = %+v", got)
	}
	if _, err := s.BindWorkflowNodeSession(ctx, tenant, run.ID, node.ID, "session-1"); err != nil {
		t.Fatal(err)
	}
	if got := slotWaitOf(t, s, run.ID, node.ID); got != nil {
		t.Fatalf("a bound attempt still carries the marker: %+v", got)
	}
}
