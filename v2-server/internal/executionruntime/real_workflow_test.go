//go:build linux

package executionruntime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/agentruntime"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/nodeservices"
	"github.com/wanglongan587/cloud/v2-server/internal/projectrepos"
)

// TestRealWorkflowHeldSessionEndsBeforeTheRunsNextSession is the real-Node evidence for how a
// workflow run shares its directory. A held session (a parked interactive node) takes further
// turns. The pinned Node refuses a second Agent session on a checkout that still has one and then
// closes its whole control session (node-db session.rs:117-120 at aa21210e), so the run's next
// Start ends the held session first: the sibling runs on a healthy Node and the held session
// refuses further turns.
func TestRealWorkflowHeldSessionEndsBeforeTheRunsNextSession(t *testing.T) {
	assets := os.Getenv("V2_NODE_ASSETS_DIR")
	if assets == "" {
		t.Skip("set V2_NODE_ASSETS_DIR for real Service/Node/Agent/PostgreSQL evidence")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, "vw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	f := runtimeDatabase(t, nil)
	if err := f.service.Close(ctx); err != nil {
		t.Fatal(err)
	}
	f.ctx = ctx
	repositories, err := projectrepos.New(f.db, projectrepos.Options{Root: filepath.Join(root, "r")})
	if err != nil {
		t.Fatal(err)
	}
	project, err := repositories.SeedDemo(ctx, f.scope)
	if err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.CommandContext(ctx, "git", "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	driver, err := agentruntime.New(agentruntime.Options{
		Root: filepath.Join(root, "a"), AssetsDirectory: assets,
		GitBackend: filepath.Join(strings.TrimSpace(string(gitPath)), "git-http-backend"),
		Bindings:   map[string]nodeservices.Binding{"fixture": f.models["fixture"].Binding},
		ResolveProject: func(ctx context.Context, scope execution.Scope, id string) (agentruntime.Project, error) {
			project, err := f.db.GetProject(ctx, scope, id)
			return agentruntime.Project{MirrorPath: project.MirrorPath, Commit: project.Commit, SnapshotRef: project.SnapshotRef}, err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.service, err = New(ctx, Config{Store: f.db, Models: f.models, Agent: driver})
	if err != nil {
		driver.Close(ctx)
		t.Fatal(err)
	}
	// The runtime fixture's cleanup closes the service; the driver's Nodes close after it.
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := f.service.Close(cleanup); err != nil {
			t.Error(err)
		}
	})
	caller := func(node string) execution.Caller {
		return execution.Caller{Kind: execution.CallerWorkflow, ID: "run", NodeID: node, AttemptID: node + "-1"}
	}
	start := func(node string, lifetime execution.Lifetime) execution.Ref {
		t.Helper()
		ref, err := f.service.Start(ctx, f.scope, execution.StartRequest{Caller: caller(node), Target: execution.Target{ProjectID: project.ID}, ExecutorID: "opencode:fixture",
			Prompt: execution.TextPrompt("Read the repository README and write the requested fixture marker using the shell tool."), Lifetime: lifetime})
		if err != nil {
			t.Fatalf("start %s: %v", node, err)
		}
		return ref
	}
	live := func(ref execution.Ref) bool {
		t.Helper()
		session, err := f.service.ReadSession(ctx, f.scope, ref.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		return session.EndedAt == nil
	}

	draft := start("draft", execution.LifetimeHeld)
	result, cursor := observeTurn(t, f, draft, 0)
	requireRealFixtureResult(t, result)
	follow := caller("draft")
	follow.AttemptID = "draft-1-turn-2"
	turn, err := f.service.Append(ctx, f.scope, execution.AppendRequest{SessionID: draft.SessionID, Caller: follow, Prompt: execution.TextPrompt("Write the fixture marker once more.")})
	if err != nil {
		t.Fatalf("follow-up to the held session: %v", err)
	}
	followResult, _ := observeTurn(t, f, turn, cursor)
	requireRealFixtureResult(t, followResult)
	if page, err := f.service.ReadTurns(ctx, f.scope, draft.SessionID, "", 10); err != nil || len(page.Items) != 2 || !live(draft) {
		t.Fatalf("held session turns = %d err=%v live=%v, want 2 turns and still live", len(page.Items), err, live(draft))
	}
	side := start("side", execution.LifetimeTurn)
	sideResult, _ := observeTurn(t, f, side, 0)
	requireRealFixtureResult(t, sideResult) // the Node survived: no second session met the first
	if live(draft) {
		t.Fatal("the held session stayed live beside the run's next session")
	}
	late := caller("draft")
	late.AttemptID = "draft-1-turn-3"
	if _, err := f.service.Append(ctx, f.scope, execution.AppendRequest{SessionID: draft.SessionID, Caller: late, Prompt: execution.TextPrompt("still there?")}); !errors.Is(err, execution.ErrSessionEnded) {
		t.Fatalf("follow-up after the sibling started = %v, want ErrSessionEnded", err)
	}
	if err := f.service.End(ctx, f.scope, side.SessionID); err != nil {
		t.Fatal(err)
	}
	t.Logf("real held-session evidence: %s took 2 turns, ended when %s started; %s completed", draft.SessionID, side.SessionID, side.SessionID)
}
