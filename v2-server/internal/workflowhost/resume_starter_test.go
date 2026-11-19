package workflowhost

import (
	"context"
	"encoding/json"
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
