//go:build linux

package executionruntime

import (
	"context"
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

// TestRealWorkflowSessionsShareTheRunDirectory is the real-Node evidence for how a workflow run
// shares its directory. A held session (a parked interactive node) takes further turns. The Node
// admits several live Agent sessions on one checkout (its shared_checkout_sessions capability,
// which the Agent driver requires at the handshake), so the run's next Start leaves the held
// session live: the sibling runs beside it on the same checkout, the held session takes another
// turn after the sibling's, and each ends on its own.
func TestRealWorkflowSessionsShareTheRunDirectory(t *testing.T) {
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
	followResult, followCursor := observeTurn(t, f, turn, cursor)
	requireRealFixtureResult(t, followResult)
	if page, err := f.service.ReadTurns(ctx, f.scope, draft.SessionID, "", 10); err != nil || len(page.Items) != 2 || !live(draft) {
		t.Fatalf("held session turns = %d err=%v live=%v, want 2 turns and still live", len(page.Items), err, live(draft))
	}
	side := start("side", execution.LifetimeTurn)
	if !live(draft) || !live(side) {
		t.Fatalf("live while the sibling's turn runs: draft = %v, side = %v; want two live sessions on one checkout", live(draft), live(side))
	}
	draftSession, err := f.service.ReadSession(ctx, f.scope, draft.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sideSession, err := f.service.ReadSession(ctx, f.scope, side.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if draftSession.WorkspaceID == "" || draftSession.WorkspaceID != sideSession.WorkspaceID {
		t.Fatalf("workspaces %q and %q, want the run's one directory", draftSession.WorkspaceID, sideSession.WorkspaceID)
	}
	sideResult, _ := observeTurn(t, f, side, 0)
	requireRealFixtureResult(t, sideResult)
	if !live(draft) {
		t.Fatal("the run's next session ended the held one")
	}
	// The starter's next message reaches the held session after its sibling ran beside it.
	late := caller("draft")
	late.AttemptID = "draft-1-turn-3"
	third, err := f.service.Append(ctx, f.scope, execution.AppendRequest{SessionID: draft.SessionID, Caller: late, Prompt: execution.TextPrompt("Write the fixture marker a third time.")})
	if err != nil {
		t.Fatalf("follow-up after the sibling ran: %v", err)
	}
	thirdResult, _ := observeTurn(t, f, third, followCursor)
	requireRealFixtureResult(t, thirdResult)
	if page, err := f.service.ReadTurns(ctx, f.scope, draft.SessionID, "", 10); err != nil || len(page.Items) != 3 {
		t.Fatalf("held session turns = %d err=%v, want 3 in one session", len(page.Items), err)
	}
	// Ending the sibling leaves the held session live; then it ends on its own.
	if err := f.service.End(ctx, f.scope, side.SessionID); err != nil {
		t.Fatal(err)
	}
	if !live(draft) || live(side) {
		t.Fatalf("after ending the sibling: draft live = %v, side live = %v", live(draft), live(side))
	}
	if err := f.service.End(ctx, f.scope, draft.SessionID); err != nil {
		t.Fatal(err)
	}
	t.Logf("real shared-checkout evidence: %s took 3 turns, the last after %s ran beside it in workspace %s", draft.SessionID, side.SessionID, draftSession.WorkspaceID)
}
