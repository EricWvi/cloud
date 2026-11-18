package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

func legacyFailedRun(t *testing.T, s *store.Store, user, tenant, suffix string) (store.WorkflowRun, store.WorkflowNodeRun, store.WorkflowNodeRun) {
	t.Helper()
	ctx := context.Background()
	project := workflowProject(t, s, user, tenant, "project-"+suffix)
	w, _ := publishedWorkflow(t, s, tenant, "遗留运行-"+suffix, "v1")
	run, err := s.CreateWorkflowRun(ctx, tenant, w.ID, "", store.WorkflowRunInput{ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartWorkflowRun(ctx, tenant, run.ID); err != nil {
		t.Fatal(err)
	}
	nodeA, err := s.CreateWorkflowNodeRun(ctx, tenant, run.ID, store.WorkflowNodeRunInput{NodeID: "a", NodeType: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	answer := "初稿"
	if _, err := s.CompleteWorkflowNodeRun(ctx, tenant, run.ID, nodeA.ID, store.WorkflowNodeResult{Output: &answer}); err != nil {
		t.Fatal(err)
	}
	nodeB, err := s.CreateWorkflowNodeRun(ctx, tenant, run.ID, store.WorkflowNodeRunInput{NodeID: "b", NodeType: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FailWorkflowNodeRun(ctx, tenant, run.ID, nodeB.ID, store.WorkflowNodeFailure{Error: "失败"}, nil); err != nil {
		t.Fatal(err)
	}
	msg := "失败"
	ended, err := s.FinishWorkflowRun(ctx, tenant, run.ID, store.WorkflowStatusFailed, nil, &msg)
	if err != nil {
		t.Fatal(err)
	}
	return ended, nodeA, nodeB
}

func TestResumeWorkflowEngineRunBackfillsStarterAtomically(t *testing.T) {
	s := storetest.Open(t)
	starter, tenant := storetest.Login(t, s, "resume-starter-atomic")
	memberToken := storetest.SignIn(t, s, "resume-member-atomic", "续跑成员", store.RoleMember)
	member, err := s.SessionUser(context.Background(), memberToken)
	if err != nil {
		t.Fatal(err)
	}
	thirdToken := storetest.SignIn(t, s, "resume-third-atomic", "第三位成员", store.RoleMember)
	third, err := s.SessionUser(context.Background(), thirdToken)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	run, _, _ := legacyFailedRun(t, s, starter, tenant, "atomic")
	if got, err := s.WorkflowStarter(ctx, run.ID); err != nil || got != "" {
		t.Fatalf("initial WorkflowStarter = %q %v, want empty", got, err)
	}

	assertStarterEmpty := func(label string) {
		t.Helper()
		if got, err := s.WorkflowStarter(ctx, run.ID); err != nil || got != "" {
			t.Fatalf("%s: WorkflowStarter = %q %v, want empty", label, got, err)
		}
		gotRun, err := s.WorkflowRun(ctx, tenant, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if gotRun.StartedBy != "" || gotRun.StartedByName != "" {
			t.Fatalf("%s: WorkflowRun starter = (%q, %q), want empty", label, gotRun.StartedBy, gotRun.StartedByName)
		}
		list, err := s.WorkflowRuns(ctx, tenant, run.WorkflowID, "")
		if err != nil || len(list) != 1 || list[0].StartedBy != "" || list[0].StartedByName != "" {
			t.Fatalf("%s: WorkflowRuns = %+v %v", label, list, err)
		}
	}

	refusal := errors.New("plan refused")
	if err := s.ResumeWorkflowEngineRun(ctx, run.ID, member.ID, func(store.WorkflowResumeState) ([]string, error) {
		return nil, refusal
	}, func(payload []byte, _ []string) ([]byte, error) {
		return payload, nil
	}); !errors.Is(err, refusal) {
		t.Fatalf("plan error = %v, want %v", err, refusal)
	}
	assertStarterEmpty("after plan error")

	if err := s.ResumeWorkflowEngineRun(ctx, run.ID, member.ID, func(store.WorkflowResumeState) ([]string, error) {
		return nil, nil
	}, func(payload []byte, _ []string) ([]byte, error) {
		return payload, nil
	}); !errors.Is(err, store.ErrWorkflowRunState) {
		t.Fatalf("empty plan error = %v, want ErrWorkflowRunState", err)
	}
	assertStarterEmpty("after empty clear ids")

	keepPayload := func(payload []byte, _ []string) ([]byte, error) { return payload, nil }
	if err := s.ResumeWorkflowEngineRun(ctx, run.ID, member.ID, func(store.WorkflowResumeState) ([]string, error) {
		return []string{"b"}, nil
	}, keepPayload); err != nil {
		t.Fatalf("commit resume = %v", err)
	}

	if got, err := s.WorkflowStarter(ctx, run.ID); err != nil || got != member.ID {
		t.Fatalf("committed WorkflowStarter = %q %v, want %q", got, err, member.ID)
	}
	resumed, err := s.WorkflowRun(ctx, tenant, run.ID)
	if err != nil || resumed.StartedBy != member.ID || resumed.StartedByName != "续跑成员" {
		t.Fatalf("committed WorkflowRun = %+v %v", resumed, err)
	}
	list, err := s.WorkflowRuns(ctx, tenant, run.WorkflowID, "")
	if err != nil || len(list) != 1 || list[0].StartedBy != member.ID || list[0].StartedByName != "续跑成员" {
		t.Fatalf("committed WorkflowRuns = %+v %v", list, err)
	}

	// Fail again and resume as third user: an existing started_by is preserved.
	if _, err := s.FinishWorkflowRun(ctx, tenant, run.ID, store.WorkflowStatusFailed, nil, nil); err != nil {
		t.Fatal(err)
	}
	nodeB2, err := s.CreateWorkflowNodeRun(ctx, tenant, run.ID, store.WorkflowNodeRunInput{NodeID: "b", NodeType: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FailWorkflowNodeRun(ctx, tenant, run.ID, nodeB2.ID, store.WorkflowNodeFailure{Error: "再次失败"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeWorkflowEngineRun(ctx, run.ID, third.ID, func(store.WorkflowResumeState) ([]string, error) {
		return []string{"b"}, nil
	}, keepPayload); err != nil {
		t.Fatalf("second resume = %v", err)
	}
	if got, err := s.WorkflowStarter(ctx, run.ID); err != nil || got != member.ID {
		t.Fatalf("overwrote starter = %q %v, want %q", got, err, member.ID)
	}
}

func TestWorkflowResumeStarterRecoversLegacySessionOwner(t *testing.T) {
	f := executionDatabase(t)
	ctx := context.Background()
	ownerScope := f.scope
	memberToken := storetest.SignIn(t, f.s, "resume-member-legacy", "成员乙", store.RoleMember)
	member, err := f.s.SessionUser(ctx, memberToken)
	if err != nil {
		t.Fatal(err)
	}

	// Legacy run without any bound session yields empty starter for caller fallback.
	noSessionRun, _, _ := legacyFailedRun(t, f.s, ownerScope.UserID, ownerScope.TenantID, "no-session")
	if got, err := f.s.WorkflowResumeStarter(ctx, noSessionRun.ID); err != nil || got != "" {
		t.Fatalf("no-session WorkflowResumeStarter = %q %v, want empty", got, err)
	}

	// Legacy run whose live attempt bound a session owned by owner recovers owner.
	projectLive := workflowProject(t, f.s, ownerScope.UserID, ownerScope.TenantID, "project-live-session")
	wfLive, _ := publishedWorkflow(t, f.s, ownerScope.TenantID, "遗留活会话", "v1")
	liveSessionRun, err := f.s.CreateWorkflowRun(ctx, ownerScope.TenantID, wfLive.ID, "", store.WorkflowRunInput{ProjectID: projectLive.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.StartWorkflowRun(ctx, ownerScope.TenantID, liveSessionRun.ID); err != nil {
		t.Fatal(err)
	}
	nodeA, err := f.s.CreateWorkflowNodeRun(ctx, ownerScope.TenantID, liveSessionRun.ID, store.WorkflowNodeRunInput{NodeID: "a", NodeType: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	turnA := execution.Turn{
		Ref:    execution.Ref{SessionID: "legacy-live-session", TurnID: "turn-1"},
		Caller: execution.Caller{Kind: execution.CallerWorkflow, ID: liveSessionRun.ID, NodeID: "a", AttemptID: nodeA.ID},
		Prompt: execution.TextPrompt("遗留会话"),
	}
	if err := f.s.CreateExecutionSession(ctx, ownerScope, execution.Session{ID: turnA.Ref.SessionID, Model: execution.Model{ID: "fixture", Name: "Fixture", BindingVersion: "v1"}}, turnA); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.BindWorkflowNodeSession(ctx, ownerScope.TenantID, liveSessionRun.ID, nodeA.ID, turnA.Ref.SessionID); err != nil {
		t.Fatal(err)
	}
	okOut := "初稿"
	if _, err := f.s.CompleteWorkflowNodeRun(ctx, ownerScope.TenantID, liveSessionRun.ID, nodeA.ID, store.WorkflowNodeResult{Output: &okOut}); err != nil {
		t.Fatal(err)
	}
	nodeB, err := f.s.CreateWorkflowNodeRun(ctx, ownerScope.TenantID, liveSessionRun.ID, store.WorkflowNodeRunInput{NodeID: "b", NodeType: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.FailWorkflowNodeRun(ctx, ownerScope.TenantID, liveSessionRun.ID, nodeB.ID, store.WorkflowNodeFailure{Error: "失败"}, nil); err != nil {
		t.Fatal(err)
	}
	liveErr := "失败"
	if _, err := f.s.FinishWorkflowRun(ctx, ownerScope.TenantID, liveSessionRun.ID, store.WorkflowStatusFailed, nil, &liveErr); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.WorkflowResumeStarter(ctx, liveSessionRun.ID); err != nil || got != ownerScope.UserID {
		t.Fatalf("live-session WorkflowResumeStarter = %q %v, want %q", got, err, ownerScope.UserID)
	}

	// A soft-deleted failed attempt still binds the legacy run to its original session owner.
	projectDel := workflowProject(t, f.s, ownerScope.UserID, ownerScope.TenantID, "project-soft-deleted-session")
	wfDel, _ := publishedWorkflow(t, f.s, ownerScope.TenantID, "遗留软删会话", "v1")
	softDeletedRun, err := f.s.CreateWorkflowRun(ctx, ownerScope.TenantID, wfDel.ID, "", store.WorkflowRunInput{ProjectID: projectDel.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.StartWorkflowRun(ctx, ownerScope.TenantID, softDeletedRun.ID); err != nil {
		t.Fatal(err)
	}
	nodeBDel, err := f.s.CreateWorkflowNodeRun(ctx, ownerScope.TenantID, softDeletedRun.ID, store.WorkflowNodeRunInput{NodeID: "b", NodeType: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	turnB := execution.Turn{
		Ref:    execution.Ref{SessionID: "legacy-deleted-session", TurnID: "turn-2"},
		Caller: execution.Caller{Kind: execution.CallerWorkflow, ID: softDeletedRun.ID, NodeID: "b", AttemptID: nodeBDel.ID},
		Prompt: execution.TextPrompt("重试前遗留会话"),
	}
	if err := f.s.CreateExecutionSession(ctx, ownerScope, execution.Session{ID: turnB.Ref.SessionID, Model: execution.Model{ID: "fixture", Name: "Fixture", BindingVersion: "v1"}}, turnB); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.BindWorkflowNodeSession(ctx, ownerScope.TenantID, softDeletedRun.ID, nodeBDel.ID, turnB.Ref.SessionID); err != nil {
		t.Fatal(err)
	}
	retryB, err := f.s.RetryWorkflowNodeRun(ctx, ownerScope.TenantID, softDeletedRun.ID, nodeBDel.ID, store.WorkflowNodeRetry{
		Failure:     store.WorkflowNodeFailure{Error: "一次重试"},
		NextPayload: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.FailWorkflowNodeRun(ctx, ownerScope.TenantID, softDeletedRun.ID, retryB.ID, store.WorkflowNodeFailure{Error: "终败"}, nil); err != nil {
		t.Fatal(err)
	}
	failMsg := "终败"
	if _, err := f.s.FinishWorkflowRun(ctx, ownerScope.TenantID, softDeletedRun.ID, store.WorkflowStatusFailed, nil, &failMsg); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.WorkflowResumeStarter(ctx, softDeletedRun.ID); err != nil || got != ownerScope.UserID {
		t.Fatalf("soft-deleted-session WorkflowResumeStarter = %q %v, want %q", got, err, ownerScope.UserID)
	}

	// Explicit started_by wins over session recovery.
	if _, err := f.admin.Exec(ctx, `UPDATE v2_workflow_runs SET started_by = $2 WHERE id = $1`, liveSessionRun.ID, member.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.WorkflowResumeStarter(ctx, liveSessionRun.ID); err != nil || got != member.ID {
		t.Fatalf("explicit starter WorkflowResumeStarter = %q %v, want %q", got, err, member.ID)
	}

	// Inactive or removed members are not recovered as the execution starter.
	if _, err := f.admin.Exec(ctx, `DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`, ownerScope.TenantID, ownerScope.UserID); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.WorkflowResumeStarter(ctx, softDeletedRun.ID); err != nil || got != "" {
		t.Fatalf("removed-member WorkflowResumeStarter = %q %v, want empty", got, err)
	}
}
