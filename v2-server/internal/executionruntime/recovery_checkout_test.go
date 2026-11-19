package executionruntime

import (
	"os"
	"strconv"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// checkoutGraph has two interactive nodes and a successor of the first, all Agents of one run.
const checkoutGraph = `{"nodes":[{"id":"start","data":{"kind":"start"}},
 {"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"起草","interactive":true}}},
 {"id":"b","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"另起草","interactive":true}}},
 {"id":"c","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"审阅"}}}],
 "edges":[{"source":"start","target":"a"},{"source":"start","target":"b"},{"source":"a","target":"c"}]}`

// runningRun creates a project and a running run of checkoutGraph.
func runningRun(t *testing.T, f *runtimeFixture) string {
	t.Helper()
	ctx := f.ctx
	if _, err := f.db.CreateProject(ctx, f.scope, store.Project{ID: "project", Name: "Demo", RepositoryURL: "https://example.com/demo.git", Commit: "fixed", MirrorPath: "/fake/unused"}); err != nil {
		t.Fatal(err)
	}
	w, _, err := f.db.CreateWorkflow(ctx, f.scope.TenantID, "接回目录", checkoutGraph)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.db.PublishWorkflow(ctx, f.scope.TenantID, w.ID, "v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.db.CreateWorkflowRun(ctx, f.scope.TenantID, w.ID, snapshot.ID, store.WorkflowRunInput{ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.StartWorkflowRun(ctx, f.scope.TenantID, run.ID); err != nil {
		t.Fatal(err)
	}
	return run.ID
}

// startNode runs the first turn of a node of the run to its result and binds the session to the
// node's row, which stays running; held nodes are then parked with a session summary.
func startNode(t *testing.T, f *runtimeFixture, runID, nodeID string, lifetime execution.Lifetime) (execution.Session, store.WorkflowNodeRun) {
	t.Helper()
	ctx := f.ctx
	node, err := f.db.CreateWorkflowNodeRun(ctx, f.scope.TenantID, runID, store.WorkflowNodeRunInput{NodeID: nodeID, NodeType: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := f.service.Start(ctx, f.scope, execution.StartRequest{Caller: execution.Caller{Kind: execution.CallerWorkflow, ID: runID, NodeID: nodeID, AttemptID: node.ID},
		Target: execution.Target{ProjectID: "project"}, ExecutorID: "opencode:fixture", Prompt: execution.TextPrompt("开始 " + nodeID), Lifetime: lifetime})
	if err != nil {
		t.Fatal(err)
	}
	awaitResult(t, f, ref)
	if _, err := f.db.BindWorkflowNodeSession(ctx, f.scope.TenantID, runID, node.ID, ref.SessionID); err != nil {
		t.Fatal(err)
	}
	if lifetime == execution.LifetimeHeld {
		if moved, err := f.db.MoveWorkflowEngineNode(ctx, runID, node.ID, store.WorkflowStatusRunning, store.WorkflowStatusPending,
			func([]byte) ([]byte, error) { return []byte(`{"interactive":{"turns":1}}`), nil }); err != nil || !moved {
			t.Fatalf("park %s = %v %v", nodeID, moved, err)
		}
	}
	session, err := f.db.ReadExecutionSession(ctx, f.scope, ref.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	return session, node
}

// restartService stops the service and starts the next process's over the same database.
func restartService(t *testing.T, f *runtimeFixture, agent *recoveryAgent) {
	t.Helper()
	if err := f.service.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	var err error
	if f.service, err = New(f.ctx, Config{Store: f.db, Models: f.models, Agent: agent}); err != nil {
		t.Fatal(err)
	}
}

// A run that survives a restart with no parked node (the sweep absorbed an interrupted Iteration
// or a pending LoopExit cleanup) keeps its directory: its next node's session shares the run's
// workspace and starts on the recorded checkout, not a fresh clone.
func TestARunningRunsNextNodeStartsOnItsCheckoutAfterARestart(t *testing.T) {
	f := runtimeDatabase(t, nil)
	agent := &recoveryAgent{}
	restartService(t, f, agent)
	runID := runningRun(t, f)
	first, _ := startNode(t, f, runID, "a", execution.LifetimeTurn)
	dir := t.TempDir()
	if err := f.db.RecordWorkspace(f.ctx, first.WorkspaceID, store.WorkspaceLocation{Path: dir, BaseCommit: "fixed", CloneExecution: "c-before"}); err != nil {
		t.Fatal(err)
	}

	next := &recoveryAgent{}
	restartService(t, f, next)
	next.mu.Lock()
	reattached := next.reattached[first.WorkspaceID]
	next.mu.Unlock()
	if reattached != [2]string{"c-before", dir} {
		t.Fatalf("reattached = %v, want the run's recorded checkout", next.reattached)
	}
	successor, _ := startNode(t, f, runID, "c", execution.LifetimeTurn)
	if successor.WorkspaceID != first.WorkspaceID {
		t.Fatalf("successor workspace = %s, want the run's %s", successor.WorkspaceID, first.WorkspaceID)
	}
}

// Two nodes parked on one checkout, then Node lost its ledger: the first one's follow-up found
// the checkout unknown, which was forgotten, and a successor cloned again. The second parked
// node's conversation ran in the forgotten clone, so its follow-up finds its checkout gone
// instead of continuing on the new clone without its edits, also after another restart.
func TestARecoveredSessionWhoseCheckoutWasReplacedEnds(t *testing.T) {
	for _, restartAgain := range []bool{false, true} {
		t.Run("restart again "+strconv.FormatBool(restartAgain), func(t *testing.T) {
			f := runtimeDatabase(t, nil)
			agent := &recoveryAgent{}
			restartService(t, f, agent)
			runID := runningRun(t, f)
			a, _ := startNode(t, f, runID, "a", execution.LifetimeHeld)
			b, rowB := startNode(t, f, runID, "b", execution.LifetimeHeld)
			if a.WorkspaceID != b.WorkspaceID {
				t.Fatalf("parked nodes in workspaces %s and %s, want one", a.WorkspaceID, b.WorkspaceID)
			}
			old := t.TempDir()
			if err := f.db.RecordWorkspace(f.ctx, a.WorkspaceID, store.WorkspaceLocation{Path: old, BaseCommit: "fixed", CloneExecution: "c-forgotten"}); err != nil {
				t.Fatal(err)
			}
			restartService(t, f, agent)

			// What the driver does once Node refuses A's checkout as unknown, and once the
			// successor of A clones again.
			if err := f.db.ForgetCloneExecution(f.ctx, a.WorkspaceID, "c-forgotten"); err != nil {
				t.Fatal(err)
			}
			fresh := t.TempDir()
			if err := f.db.RecordWorkspace(f.ctx, a.WorkspaceID, store.WorkspaceLocation{Path: fresh, BaseCommit: "fixed", CloneExecution: "c-fresh"}); err != nil {
				t.Fatal(err)
			}
			if restartAgain {
				restartService(t, f, agent)
			}
			agent.mu.Lock()
			runs := len(agent.runs)
			agent.mu.Unlock()

			ref, err := followUp(t, f, b.ID, rowB, 1, "再改一版")
			if err != nil {
				t.Fatal(err)
			}
			if r := awaitResult(t, f, ref); r.FailureCode != execution.FailureWorkspaceMissing || !r.AgentLost {
				t.Fatalf("B's follow-up after its checkout was replaced = %+v, want %s", r, execution.FailureWorkspaceMissing)
			}
			agent.mu.Lock()
			defer agent.mu.Unlock()
			if len(agent.runs) != runs {
				t.Fatal("B's new Agent started on the replacement clone")
			}
			if _, err := os.Stat(old); err != nil {
				t.Fatalf("the forgotten clone's directory must be left alone: %v", err)
			}
		})
	}
}

// A run whose sessions span two workspaces (an earlier build gave its later nodes a fresh one after
// a restart) and that has a parked node keeps the parked node's workspace for its next nodes: that
// is where the conversation the starter continues lives. Both checkouts are reattached.
func TestAParkedNodesWorkspaceWinsOverTheRunsLatest(t *testing.T) {
	f := runtimeDatabase(t, nil)
	agent := &recoveryAgent{}
	restartService(t, f, agent)
	runID := runningRun(t, f)
	parked, _ := startNode(t, f, runID, "a", execution.LifetimeHeld)
	// What a process that did not remember the run's workspace did: a later node in a new one.
	f.service.mu.Lock()
	f.service.workspaces = map[string]string{}
	f.service.mu.Unlock()
	later, _ := startNode(t, f, runID, "c", execution.LifetimeTurn)
	if later.WorkspaceID == parked.WorkspaceID {
		t.Fatal("the later node shares the parked node's workspace; the test needs two")
	}
	dirs := map[string]string{parked.WorkspaceID: t.TempDir(), later.WorkspaceID: t.TempDir()}
	for workspace, dir := range dirs {
		if err := f.db.RecordWorkspace(f.ctx, workspace, store.WorkspaceLocation{Path: dir, BaseCommit: "fixed", CloneExecution: "c-" + workspace}); err != nil {
			t.Fatal(err)
		}
	}

	next := &recoveryAgent{}
	restartService(t, f, next)
	next.mu.Lock()
	for workspace, dir := range dirs {
		if got := next.reattached[workspace]; got != [2]string{"c-" + workspace, dir} {
			t.Errorf("workspace %s reattached to %v, want its recorded checkout", workspace, got)
		}
	}
	next.mu.Unlock()
	successor, _ := startNode(t, f, runID, "b", execution.LifetimeTurn)
	if successor.WorkspaceID != parked.WorkspaceID {
		t.Fatalf("next node workspace = %s, want the parked node's %s (the run's latest is %s)", successor.WorkspaceID, parked.WorkspaceID, later.WorkspaceID)
	}
}
