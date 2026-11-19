package store_test

import (
	"context"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

// failedAndReplaced creates one started attempt of (nodeID, iteration) and retries it: the attempt
// becomes a soft-deleted failure and the retry waits to start (started_at NULL), the history
// desktop reads when it injects the previous failure. It returns both rows.
func failedAndReplaced(t *testing.T, s *store.Store, tenant, runID, nodeID string, iteration *int) (failed, next store.WorkflowNodeRun) {
	t.Helper()
	ctx := context.Background()
	failed, err := s.CreateWorkflowNodeRun(ctx, tenant, runID, store.WorkflowNodeRunInput{
		NodeID: nodeID, NodeType: "agent", Iteration: iteration,
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed.StartedAt == nil {
		t.Fatalf("a dispatched attempt starts: %+v", failed)
	}
	next, err = s.RetryWorkflowNodeRun(ctx, tenant, runID, failed.ID, store.WorkflowNodeRetry{
		Failure:     store.WorkflowNodeFailure{Error: "失败原因"},
		NextPayload: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.StartedAt != nil {
		t.Fatalf("a retry waits for its wake: %+v", next)
	}
	// Check the replacement really left a soft-deleted failure behind, so a green finder below is
	// not green because there was nothing to find.
	attempts, err := s.WorkflowRunFailedAttempts(ctx, tenant, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range attempts {
		if attempt.ID == failed.ID {
			if attempt.Status != store.WorkflowStatusFailed || attempt.StartedAt == nil {
				t.Fatalf("failed attempt = %+v", attempt)
			}
			return attempt, next
		}
	}
	t.Fatalf("attempt %s was not recorded as failed", failed.ID)
	return store.WorkflowNodeRun{}, store.WorkflowNodeRun{}
}

func findLastFailed(t *testing.T, s *store.Store, tenant, runID, nodeID string, iteration *int) *store.WorkflowNodeRun {
	t.Helper()
	found, err := s.FindLastFailedWorkflowNodeAttempt(context.Background(), tenant, runID, nodeID, iteration)
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// A retry that succeeded recovered the failure: the retry's success still counts after a resume or
// restart soft-deleted it, so a later attempt of the same node must not inherit the old failure.
// Without the succeeded-attempt guard the soft-deleted failure below is returned instead.
func TestLastFailedNodeAttemptIgnoresAFailureARetryRecovered(t *testing.T) {
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "last-failed-recovered")
	ctx := context.Background()
	run, _ := startedRun(t, s, user, tenant, "重试成功")
	failed, next := failedAndReplaced(t, s, tenant, run.ID, "agent-recovered", nil)

	if _, err := s.WakeWorkflowNodeRun(ctx, tenant, run.ID, next.ID, nil); err != nil {
		t.Fatal(err)
	}
	answer := "修好了"
	if _, err := s.CompleteWorkflowNodeRun(ctx, tenant, run.ID, next.ID, store.WorkflowNodeResult{Output: &answer}); err != nil {
		t.Fatal(err)
	}
	// Resuming soft-deletes the recovering success together with the failure; it still counts.
	if err := s.ResumeWorkflowEngineRun(ctx, run.ID, user, func(store.WorkflowResumeState) ([]string, error) {
		return []string{"agent-recovered"}, nil
	}, func(payload []byte, _ []string) ([]byte, error) {
		return payload, nil
	}); err != nil {
		t.Fatal(err)
	}

	if found := findLastFailed(t, s, tenant, run.ID, "agent-recovered", nil); found != nil {
		t.Fatalf("recovered failure %s still offered as the previous failure: %+v", failed.ID, found)
	}
}

// Item 1 of an Iteration must not inherit item 0's failure: the finder matches the iteration index
// exactly, including "no iteration at all" for a node outside any Iteration. Loop rounds carry no
// index and are deliberately not told apart, as in desktop.
func TestLastFailedNodeAttemptIsScopedToItsIteration(t *testing.T) {
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "last-failed-iteration")
	run, _ := startedRun(t, s, user, tenant, "轮次")
	round := 0
	failed, _ := failedAndReplaced(t, s, tenant, run.ID, "agent-round", &round)
	other := 1

	if found := findLastFailed(t, s, tenant, run.ID, "agent-round", &other); found != nil {
		t.Fatalf("round 1 inherited round 0's failure: %+v", found)
	}
	if found := findLastFailed(t, s, tenant, run.ID, "agent-round", nil); found != nil {
		t.Fatalf("a node outside the Loop inherited round 0's failure: %+v", found)
	}
	found := findLastFailed(t, s, tenant, run.ID, "agent-round", &round)
	if found == nil || found.ID != failed.ID {
		t.Fatalf("round 0 failure = %+v, want %s", found, failed.ID)
	}
}

// A retry a restart settled before it ran must not hide the attempt that really failed: it carries
// started_at NULL, so the finder only offers attempts that started.
func TestLastFailedNodeAttemptIgnoresAFailureThatNeverStarted(t *testing.T) {
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "last-failed-unstarted")
	ctx := context.Background()
	run, _ := startedRun(t, s, user, tenant, "重启")

	// The first attempt started and failed; its retry only waited.
	started, waiting := failedAndReplaced(t, s, tenant, run.ID, "agent-restart", nil)
	if _, err := s.FailWorkflowNodeRun(ctx, tenant, run.ID, waiting.ID, store.WorkflowNodeFailure{Error: "重启前没跑起来"}, nil); err != nil {
		t.Fatal(err)
	}
	// Resuming clears the run's live attempts, so the failed wait is soft-deleted like history.
	if err := s.ResumeWorkflowEngineRun(ctx, run.ID, user, func(store.WorkflowResumeState) ([]string, error) {
		return []string{"agent-restart"}, nil
	}, func(payload []byte, _ []string) ([]byte, error) {
		return payload, nil
	}); err != nil {
		t.Fatal(err)
	}

	found := findLastFailed(t, s, tenant, run.ID, "agent-restart", nil)
	if found == nil || found.ID != started.ID {
		t.Fatalf("previous failure = %+v, want the started attempt %s", found, started.ID)
	}
}
