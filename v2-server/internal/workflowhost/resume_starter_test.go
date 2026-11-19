package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// starterExits are the two ways a member stops being able to act in the workspace: the owner
// removes them, or their account is disabled.
var starterExits = []struct {
	name  string
	leave func(t *testing.T, f *fixture, admin *pgx.Conn, user string)
}{
	{"removed", func(t *testing.T, f *fixture, _ *pgx.Conn, user string) {
		t.Helper()
		if err := f.s.RemoveMember(context.Background(), f.scope.TenantID, user); err != nil {
			t.Fatal(err)
		}
	}},
	{"disabled", func(t *testing.T, _ *fixture, admin *pgx.Conn, user string) {
		t.Helper()
		if _, err := admin.Exec(context.Background(), `UPDATE users SET status = 'disabled' WHERE id = $1`, user); err != nil {
			t.Fatal(err)
		}
	}},
}

func signInMember(t *testing.T, f *fixture, subject, name string) string {
	t.Helper()
	u, err := f.s.SessionUser(context.Background(), storetest.SignIn(t, f.s, subject, name, store.RoleMember))
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// noAutoRetryTwoAgents is twoAgents with automatic retries of its first Agent turned off, so a
// failed first Agent fails the run at once.
func noAutoRetryTwoAgents() string {
	return strings.Replace(twoAgents, `"roleId":"reviewer"`,
		`"roleId":"reviewer","retry":{"enabled":false,"maxRetries":0,"initialDelaySeconds":10}`, 1)
}

type failureDetail struct {
	Kind          string `json:"kind"`
	AutoRetryable bool   `json:"auto_retryable"`
	Resumable     bool   `json:"resumable"`
	Attempt       int    `json:"attempt"`
}

func nodeFailure(t *testing.T, row store.WorkflowNodeRun) failureDetail {
	t.Helper()
	var p struct {
		ErrorDetail failureDetail `json:"error_detail"`
	}
	if err := json.Unmarshal(row.Payload, &p); err != nil {
		t.Fatalf("node payload %s: %v", row.Payload, err)
	}
	return p.ErrorDetail
}

// TestResumeRefusedWhenTheStarterLeft is R2-2: the run records A as its starter, A fails it, then
// leaves the workspace. Resuming would execute every Agent under A and fail it, so the preview
// and the resume both refuse with starter_unavailable, the run stays failed with started_by A,
// and no Agent starts. Once A is back the same run resumes under A.
func TestResumeRefusedWhenTheStarterLeft(t *testing.T) {
	for _, exit := range starterExits {
		t.Run(exit.name, func(t *testing.T) {
			f, admin := newFixtureWithAdmin(t)
			ctx := context.Background()
			starter := signInMember(t, f, "resume-left-starter-"+exit.name, "发起人")
			asStarter := server.WorkflowRunScope{TenantID: f.scope.TenantID, UserID: starter}

			id := f.run(t, noAutoRetryTwoAgents(), str("发起人离开"))
			if _, err := f.host.StartRun(ctx, asStarter, id); err != nil {
				t.Fatal(err)
			}
			f.exec.finish(expectAgent(t, f, "a"), sessionFailed)
			if run, _ := f.await(t, id, terminal); run.Status != store.WorkflowStatusFailed || run.StartedBy != starter {
				t.Fatalf("run before leaving = %+v", run)
			}
			waitUntil(t, func() bool { return f.host.state(id) == nil })

			exit.leave(t, f, admin, starter)

			preview, err := f.host.PreviewRunResume(ctx, f.scope, id)
			if err != nil {
				t.Fatal(err)
			}
			if preview.Reason != workflow.ResumeRefusedStarterUnavailable {
				t.Fatalf("preview reason = %q, want %q", preview.Reason, workflow.ResumeRefusedStarterUnavailable)
			}
			_, err = f.host.ResumeRun(ctx, f.scope, id)
			if got := refusal(t, err); got != workflow.ResumeRefusedStarterUnavailable {
				t.Fatalf("resume refusal = %q, want %q", got, workflow.ResumeRefusedStarterUnavailable)
			}
			run, err := f.s.WorkflowRun(ctx, f.scope.TenantID, id)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != store.WorkflowStatusFailed || run.StartedBy != starter {
				t.Fatalf("run after the refused resume = %+v, want failed and still started by %s", run, starter)
			}
			select {
			case session := <-f.exec.startedCh:
				t.Fatalf("a refused resume started Agent session %s", session)
			default:
			}
			if st := f.host.state(id); st != nil {
				t.Fatalf("a refused resume left execution state %+v", st)
			}

			// The starter is back: the run resumes under them.
			switch exit.name {
			case "removed":
				if _, err := admin.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'member')`, f.scope.TenantID, starter); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if _, err := admin.Exec(ctx, `UPDATE users SET status = 'active' WHERE id = $1`, starter); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.host.ResumeRun(ctx, f.scope, id); err != nil {
				t.Fatalf("resume once the starter is back = %v", err)
			}
			session := expectAgent(t, f, "a")
			if got := f.exec.scope(session); got.UserID != starter {
				t.Fatalf("resumed Agent ran as %s, want the starter %s", got.UserID, starter)
			}
		})
	}
}

// TestLeftoverStateOfALeftOwnerIsReplaced is R2-1: a legacy run's resume by A is admitted, then
// its transaction and the cleanup after it fail, so A's execution state stays in the process.
// A then leaves the workspace and B resumes. The leftover state must not pin the run to A (every
// Agent refused, started_by A, every later resume picking A again): the run executes under, and
// records, B.
func TestLeftoverStateOfALeftOwnerIsReplaced(t *testing.T) {
	for _, exit := range starterExits {
		t.Run(exit.name, func(t *testing.T) {
			f, admin := newFixtureWithAdmin(t)
			ctx := context.Background()
			first := signInMember(t, f, "resume-stale-first-"+exit.name, "先续跑的人")
			second := signInMember(t, f, "resume-stale-second-"+exit.name, "后到的人")

			id := f.run(t, noAutoRetryTwoAgents(), str("遗留运行"))
			if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
				t.Fatal(err)
			}
			f.exec.finish(expectAgent(t, f, "a"), sessionFailed)
			f.await(t, id, terminal)
			waitUntil(t, func() bool { return f.host.state(id) == nil })
			if _, err := admin.Exec(ctx, `UPDATE v2_workflow_runs SET started_by = NULL WHERE id = $1`, id); err != nil {
				t.Fatal(err)
			}

			f.host.resuming = func(string) {
				if _, err := admin.Exec(ctx, `ALTER TABLE v2_workflow_runs RENAME COLUMN status TO broken_status`); err != nil {
					t.Fatalf("break v2_workflow_runs: %v", err)
				}
			}
			_, err := f.host.ResumeRun(ctx, server.WorkflowRunScope{TenantID: f.scope.TenantID, UserID: first}, id)
			f.host.resuming = nil
			var refused *server.WorkflowRunNotResumable
			if err == nil || errors.As(err, &refused) {
				t.Fatalf("resume against a broken v2_workflow_runs = %v, want a storage error", err)
			}
			if _, err := admin.Exec(ctx, `ALTER TABLE v2_workflow_runs RENAME COLUMN broken_status TO status`); err != nil {
				t.Fatal(err)
			}
			if leftover := f.host.state(id); leftover == nil || leftover.scope.UserID != first {
				t.Fatalf("leftover state = %+v, want one owned by %s", leftover, first)
			}

			exit.leave(t, f, admin, first)

			resumed, err := f.host.ResumeRun(ctx, server.WorkflowRunScope{TenantID: f.scope.TenantID, UserID: second}, id)
			if err != nil {
				t.Fatal(err)
			}
			if resumed.StartedBy != second {
				t.Fatalf("resumed starter = %q, want the resuming member %s, not the leftover owner %s who left", resumed.StartedBy, second, first)
			}
			session := expectAgent(t, f, "a")
			if got := f.exec.scope(session); got.UserID != second {
				t.Fatalf("resumed Agent ran as %s, want %s", got.UserID, second)
			}
			f.exec.finish(session, completed("初稿"))
			f.exec.finish(expectAgent(t, f, "b"), completed("已通过"))
			if done, _ := f.await(t, id, terminal); done.Status != store.WorkflowStatusSucceeded {
				t.Fatalf("run = %+v", done)
			}
		})
	}
}

// TestAgentRefusedForALeftStarterFailsWithoutRetry covers an Agent the execution bridge refuses
// because the identity the run executes under left the workspace while the run was going (the
// same refusal a recovery after a restart meets). The node fails as starter_unavailable on its
// first attempt, without the retries a session failure gets, and the run ends failed.
func TestAgentRefusedForALeftStarterFailsWithoutRetry(t *testing.T) {
	for _, exit := range starterExits {
		t.Run(exit.name, func(t *testing.T) {
			f, admin := newFixtureWithAdmin(t)
			ctx := context.Background()
			starter := signInMember(t, f, "agent-left-starter-"+exit.name, "发起人")
			asStarter := server.WorkflowRunScope{TenantID: f.scope.TenantID, UserID: starter}

			id := f.run(t, twoAgents, str("进行中离开"))
			if _, err := f.host.StartRun(ctx, asStarter, id); err != nil {
				t.Fatal(err)
			}
			session := expectAgent(t, f, "a")
			// Like the real bridge, refuse a start whose user no longer has access to the tenant.
			f.exec.beforeStart = func(execution.StartRequest) error {
				if _, err := f.s.TenantAccess(context.Background(), f.scope.TenantID, starter); err != nil {
					return execution.ErrNotFound
				}
				return nil
			}
			exit.leave(t, f, admin, starter)
			f.exec.finish(session, completed("初稿"))

			run, rows := f.await(t, id, terminal)
			if run.Status != store.WorkflowStatusFailed {
				t.Fatalf("run = %+v, want failed", run)
			}
			b := node(rows, "b")
			got := nodeFailure(t, b)
			if b.Status != store.WorkflowStatusFailed || got.Kind != string(workflow.FailureStarterUnavailable) || got.AutoRetryable || !got.Resumable || got.Attempt != 1 {
				t.Fatalf("node b = %s %+v, want failed starter_unavailable on attempt 1, not auto-retryable, resumable", b.Status, got)
			}
			select {
			case s := <-f.exec.startedCh:
				t.Fatalf("a refused Agent was retried as session %s", s)
			default:
			}
		})
	}
}
