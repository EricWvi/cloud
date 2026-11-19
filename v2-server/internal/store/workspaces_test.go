package store_test

import (
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

func TestChatWorkspaceFollowsTheConversationSession(t *testing.T) {
	f := executionDatabase(t)
	ctx := t.Context()
	c, err := f.s.CreateChatConversation(ctx, store.NewChatConversation{
		TenantID: f.scope.TenantID, UserID: f.scope.UserID, CreateKey: "k", ProjectID: "demo", ExecutorID: "fixture", Title: "t",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ChatWorkspace(ctx, f.scope.TenantID, f.scope.UserID, c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("before the first turn: %v, want ErrNotFound", err)
	}
	turn := executionTurn("s1", "t1")
	if err := f.s.CreateExecutionSession(ctx, f.scope, execution.Session{ID: "s1", WorkspaceID: "w1", Model: execution.Model{ID: "fixture"}}, turn); err != nil {
		t.Fatal(err)
	}
	if err := f.s.RecordChatTurn(ctx, c.ID, "s1", "k", "t1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ChatWorkspace(ctx, f.scope.TenantID, f.scope.UserID, c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("before the checkout is ready: %v, want ErrNotFound", err)
	}
	for _, at := range []store.WorkspaceLocation{{Path: "/old", BaseCommit: "a"}, {Path: "/data/w1", BaseCommit: "b"}} {
		if err := f.s.RecordWorkspace(ctx, "w1", at); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.s.ChatWorkspace(ctx, f.scope.TenantID, f.scope.UserID, c.ID)
	if err != nil || got != (store.WorkspaceLocation{Path: "/data/w1", BaseCommit: "b"}) {
		t.Fatalf("workspace = %+v, %v; want the latest record", got, err)
	}
	other := executionMember(t, f.s, "other")
	if _, err := f.s.ChatWorkspace(ctx, f.scope.TenantID, other.UserID, c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another person's conversation: %v, want ErrNotFound", err)
	}
}

func TestWorkflowRunWorkspaceUsesItsAgentNodes(t *testing.T) {
	f := executionDatabase(t)
	ctx := t.Context()
	run := pendingRun(t, f.s, f.scope.UserID, f.scope.TenantID, "flow")
	if _, err := f.s.WorkflowRunWorkspace(ctx, f.scope.TenantID, run.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("run without Agent nodes: %v, want ErrNotFound", err)
	}
	if err := f.s.CreateExecutionSession(ctx, f.scope, execution.Session{ID: "node-session", WorkspaceID: "run-w", Model: execution.Model{ID: "fixture"}}, executionTurn("node-session", "t1")); err != nil {
		t.Fatal(err)
	}
	if err := f.s.RecordWorkspace(ctx, "run-w", store.WorkspaceLocation{Path: "/data/run", BaseCommit: "c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.Exec(ctx, `
		INSERT INTO v2_workflow_node_runs (tenant_id, run_id, scope_id, node_id, node_type, session_id, status)
		VALUES ($1, $2, 'root', 'agent', 'agent', 'node-session', 'succeeded')`, f.scope.TenantID, run.ID); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.WorkflowRunWorkspace(ctx, f.scope.TenantID, run.ID)
	if err != nil || got != (store.WorkspaceLocation{Path: "/data/run", BaseCommit: "c"}) {
		t.Fatalf("run workspace = %+v, %v", got, err)
	}
}

// Forgetting a clone execution Node no longer knows clears only that one: a checkout recorded for
// the workspace since (a fresh clone after an earlier forget) is left for the next restart to
// reattach, and other workspaces are untouched.
func TestForgetCloneExecutionClearsOnlyTheForgottenClone(t *testing.T) {
	f := executionDatabase(t)
	ctx := t.Context()
	record := func(id, clone string) {
		t.Helper()
		if err := f.s.RecordWorkspace(ctx, id, store.WorkspaceLocation{Path: "/data/" + id, BaseCommit: "c", CloneExecution: clone}); err != nil {
			t.Fatal(err)
		}
	}
	clone := func(id string) string {
		t.Helper()
		at, err := f.s.ExecutionWorkspace(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return at.CloneExecution
	}
	record("run-w", "c-fresh")
	record("other-w", "c-old")
	if err := f.s.ForgetCloneExecution(ctx, "run-w", "c-old"); err != nil {
		t.Fatal(err)
	}
	if got := clone("run-w"); got != "c-fresh" {
		t.Fatalf("after forgetting another clone the workspace records %q, want the fresh clone kept", got)
	}
	if err := f.s.ForgetCloneExecution(ctx, "run-w", "c-fresh"); err != nil {
		t.Fatal(err)
	}
	if got := clone("run-w"); got != "" {
		t.Fatalf("after forgetting its clone the workspace records %q, want none", got)
	}
	if got := clone("other-w"); got != "c-old" {
		t.Fatalf("another workspace records %q, want its clone untouched", got)
	}
}

// RunningWorkflowSessions finds, for each running run that is not a task's, the session of its
// latest Agent node: the workspace its next node shares after a restart. Runs that ended or were
// deleted, runs without an Agent session, and task-owned runs (settled by their owners at startup)
// are left out; a run whose sessions span two workspaces names the latest one.
func TestRunningWorkflowSessionsNameEachRunsLatestWorkspace(t *testing.T) {
	f := executionDatabase(t)
	ctx := t.Context()
	running := func(name string) store.WorkflowRun {
		t.Helper()
		run := pendingRun(t, f.s, f.scope.UserID, f.scope.TenantID, name)
		if _, err := f.s.StartWorkflowRun(ctx, f.scope.TenantID, run.ID); err != nil {
			t.Fatal(err)
		}
		return run
	}
	// agentNode records an Agent node run of run created age ago, on a session in workspace.
	agentNode := func(run store.WorkflowRun, session, workspace string, age time.Duration) {
		t.Helper()
		turn := execution.Turn{Ref: execution.Ref{SessionID: session, TurnID: session + "-t"}, Prompt: execution.TextPrompt("开始"),
			Caller: execution.Caller{Kind: execution.CallerWorkflow, ID: run.ID, NodeID: "agent", AttemptID: session}}
		if err := f.s.CreateExecutionSession(ctx, f.scope, execution.Session{ID: session, WorkspaceID: workspace,
			Target: &execution.Target{ProjectID: run.ProjectID}, Model: execution.Model{ID: "fixture"}}, turn); err != nil {
			t.Fatal(err)
		}
		if _, err := f.admin.Exec(ctx, `
			INSERT INTO v2_workflow_node_runs (tenant_id, run_id, scope_id, node_id, node_type, session_id, status, created_at)
			VALUES ($1, $2, 'root', $3, 'agent', $3, 'succeeded', now() - $4::interval)`,
			f.scope.TenantID, run.ID, session, strconv.Itoa(int(age.Seconds()))+" seconds"); err != nil {
			t.Fatal(err)
		}
	}
	// taskOwned links run to a task, as its handling run or as its decomposition run.
	taskOwned := func(run store.WorkflowRun, decomposition bool) {
		t.Helper()
		var task string
		if err := f.admin.QueryRow(ctx, `INSERT INTO v2_tasks (tenant_id, title, project_id, created_by) VALUES ($1, '任务', $2, $3) RETURNING id::text`,
			f.scope.TenantID, run.ProjectID, f.scope.UserID).Scan(&task); err != nil {
			t.Fatal(err)
		}
		query := `INSERT INTO v2_task_workflow_runs (tenant_id, task_id, run_id, started_by, start_key) VALUES ($1, $2, $3, $4, 'k')`
		if decomposition {
			query = `INSERT INTO v2_task_decompositions (tenant_id, task_id, workflow_run_id, started_by, start_key, workflow_id, workflow_name)
				SELECT $1, $2, $3, $4, 'k', workflow_id, 'flow' FROM v2_workflow_runs WHERE id = $3`
		}
		if _, err := f.admin.Exec(ctx, query, f.scope.TenantID, task, run.ID, f.scope.UserID); err != nil {
			t.Fatal(err)
		}
	}

	resumed := running("resumed")
	agentNode(resumed, "resumed-later", "w-latest", time.Minute)
	agentNode(resumed, "resumed-earlier", "w-earlier", time.Hour)
	ended := running("ended")
	agentNode(ended, "ended-s", "w-ended", time.Minute)
	message := "done"
	if _, err := f.s.FinishWorkflowRun(ctx, f.scope.TenantID, ended.ID, store.WorkflowStatusSucceeded, nil, &message); err != nil {
		t.Fatal(err)
	}
	deleted := running("deleted")
	agentNode(deleted, "deleted-s", "w-deleted", time.Minute)
	if _, err := f.admin.Exec(ctx, `UPDATE v2_workflow_runs SET deleted_at = now() WHERE id = $1`, deleted.ID); err != nil {
		t.Fatal(err)
	}
	running("without-agents")
	handling := running("handling")
	agentNode(handling, "handling-s", "w-handling", time.Minute)
	taskOwned(handling, false)
	decomposing := running("decomposing")
	agentNode(decomposing, "decomposing-s", "w-decomposing", time.Minute)
	taskOwned(decomposing, true)

	got, err := f.s.RunningWorkflowSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range got {
		names = append(names, r.Caller.ID+"="+r.Session.WorkspaceID)
	}
	if want := []string{resumed.ID + "=w-latest"}; !slices.Equal(names, want) {
		t.Fatalf("running workflow sessions = %v, want only the running run's latest workspace %v", names, want)
	}
	if got[0].Scope != f.scope || got[0].Caller.Kind != execution.CallerWorkflow {
		t.Fatalf("session = %+v, want it under the run's scope with its workflow caller", got[0])
	}
}
