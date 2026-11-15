package store_test

import (
	"context"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

// waitingRetry creates a run holding one waiting retry and one started attempt.
func waitingRetry(t *testing.T, s *store.Store, subject string) (tenant, runID, waitID, liveID string) {
	t.Helper()
	ctx := context.Background()
	user, tenant := storetest.Login(t, s, subject)
	run, _ := startedRun(t, s, user, tenant, "重试等待")
	node, err := s.CreateWorkflowNodeRun(ctx, tenant, run.ID, store.WorkflowNodeRunInput{NodeID: "agent-1", NodeType: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	live, err := s.CreateWorkflowNodeRun(ctx, tenant, run.ID, store.WorkflowNodeRunInput{NodeID: "agent-2", NodeType: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := s.RetryWorkflowNodeRun(ctx, tenant, run.ID, node.ID, store.WorkflowNodeRetry{
		Failure:     store.WorkflowNodeFailure{Error: "第一次失败"},
		NextPayload: []byte(`{"retry_wait":{"attempt":2},"retry_chain":["x"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return tenant, run.ID, waiting.ID, live.ID
}

func nodeByID(t *testing.T, s *store.Store, tenant, runID, id string) store.WorkflowNodeRun {
	t.Helper()
	nodes, err := s.WorkflowRunNodes(context.Background(), tenant, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("node run %s missing", id)
	return store.WorkflowNodeRun{}
}

func assertAbandoned(t *testing.T, n store.WorkflowNodeRun) {
	t.Helper()
	if n.Status != store.WorkflowStatusCancelled || n.FinishedAt == nil || n.Error == nil || *n.Error != store.RetryAbandoned {
		t.Fatalf("abandoned wait = %+v (error %v)", n, n.Error)
	}
	if string(n.Payload) != `{"retry_chain": ["x"]}` {
		t.Fatalf("abandoned wait payload = %s, want the wait marker dropped and the chain kept", n.Payload)
	}
}

// A failed run settles its waiting retries in the same transaction; started attempts are not
// waits and keep their own settlement.
func TestFailedRunAbandonsRetryWaits(t *testing.T) {
	s := storetest.Open(t)
	tenant, runID, waitID, liveID := waitingRetry(t, s, "abandon-on-fail")
	msg := "另一分支失败"
	if _, err := s.FinishWorkflowRun(context.Background(), tenant, runID, store.WorkflowStatusFailed, nil, &msg); err != nil {
		t.Fatal(err)
	}
	assertAbandoned(t, nodeByID(t, s, tenant, runID, waitID))
	if live := nodeByID(t, s, tenant, runID, liveID); live.Status != store.WorkflowStatusRunning {
		t.Fatalf("started attempt was settled as a wait: %+v", live)
	}
}

// The wake-time safety net abandons a wait only once its run left running.
func TestAbandonWorkflowRetryWaitRequiresAnEndedRun(t *testing.T) {
	s := storetest.Open(t)
	ctx := context.Background()
	tenant, runID, waitID, liveID := waitingRetry(t, s, "abandon-on-wake")
	if ok, err := s.AbandonWorkflowRetryWait(ctx, runID, waitID); err != nil || ok {
		t.Fatalf("running run abandoned its wait: %v %v", ok, err)
	}
	if n := nodeByID(t, s, tenant, runID, waitID); n.Status != store.WorkflowStatusRunning {
		t.Fatalf("wait under a running run = %+v", n)
	}
	// A succeeded run does not settle waits itself, so only the safety net can.
	if _, err := s.FinishWorkflowRun(ctx, tenant, runID, store.WorkflowStatusSucceeded, nil, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AbandonWorkflowRetryWait(ctx, runID, liveID); err != nil || ok {
		t.Fatalf("started attempt abandoned as a wait: %v %v", ok, err)
	}
	if ok, err := s.AbandonWorkflowRetryWait(ctx, runID, waitID); err != nil || !ok {
		t.Fatalf("abandon = %v %v", ok, err)
	}
	assertAbandoned(t, nodeByID(t, s, tenant, runID, waitID))
	if ok, err := s.AbandonWorkflowRetryWait(ctx, runID, waitID); err != nil || ok {
		t.Fatalf("second abandon = %v %v", ok, err)
	}
}
