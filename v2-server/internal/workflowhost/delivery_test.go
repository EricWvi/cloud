package workflowhost

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/executionruntime"
	"github.com/wanglongan587/cloud/v2-server/internal/projectrepos"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workspacefs"
)

type workflowDeliveryAgent struct {
	db *store.Store
	at store.WorkspaceLocation
}

func (a workflowDeliveryAgent) Run(ctx context.Context, _ execution.Scope, session execution.Session, turn execution.Turn, emit func(context.Context, execution.Event) error) (execution.Result, error) {
	if err := a.db.RecordWorkspace(ctx, session.WorkspaceID, a.at); err != nil {
		return execution.Result{}, err
	}
	name := turn.Caller.NodeID
	if strings.Contains(turn.Caller.AttemptID, "/turn-") {
		name += "-follow-up"
	}
	if err := os.WriteFile(filepath.Join(a.at.Path, name), []byte(name), 0600); err != nil {
		return execution.Result{}, err
	}
	result := completed("answer " + name)
	return result, emit(ctx, execution.Event{Kind: execution.EventTurnEnded, Result: &result})
}
func (workflowDeliveryAgent) End(context.Context, string) error { return nil }
func (workflowDeliveryAgent) Rebuild(string)                    {}
func (workflowDeliveryAgent) Reattach(string, string, string)   {}
func (workflowDeliveryAgent) Close(context.Context) error       { return nil }

type failFirstWorkflowDelivery struct{ *workspacefs.DeliveryFS }

func (f failFirstWorkflowDelivery) Deliver(ctx context.Context, req workspacefs.DeliveryRequest) (workspacefs.Snapshot, error) {
	if strings.Contains(req.Message, " node a ") {
		return workspacefs.Snapshot{}, workspacefs.ErrDeliveryTooLarge
	}
	return f.DeliveryFS.Deliver(ctx, req)
}

func deliveryWorkflow(t *testing.T, failFirst bool) (*fixture, execution.Scope) {
	t.Helper()
	f := newFixture(t)
	f.host.Close()
	ctx := t.Context()
	scope := executionScope(f.scope)
	projects, err := projectrepos.New(f.s, projectrepos.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	project, err := projects.SeedDemo(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	f.project = project.ID
	checkout := filepath.Join(t.TempDir(), "checkout")
	if raw, err := exec.Command("git", "clone", project.MirrorPath, checkout).CombinedOutput(); err != nil {
		t.Fatalf("clone: %s %v", raw, err)
	}
	dir, err := workspacefs.NewDeliveryFS(t.TempDir(), workspacefs.DefaultDeliveryObjectBytes)
	if err != nil {
		t.Fatal(err)
	}
	var delivery executionruntime.DeliveryFilesystem = dir
	if failFirst {
		delivery = failFirstWorkflowDelivery{dir}
	}
	models := executionruntime.WithFixtureModels(nil)
	at := store.WorkspaceLocation{Path: checkout, BaseCommit: project.Commit}
	runtime, err := executionruntime.New(ctx, executionruntime.Config{Store: f.s, Models: models, Agent: workflowDeliveryAgent{f.s, at}, Delivery: delivery})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	f.host, err = NewHost(Config{Store: f.s, Execution: runtime, Catalog: runtime})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.host.Close)
	return f, scope
}

func TestDeliveryFailurePreservesNodeAndRunOutcome(t *testing.T) {
	f, scope := deliveryWorkflow(t, true)
	runID := f.run(t, strings.ReplaceAll(twoAgents, "Other Model", "fixture"), str("original"))
	if _, err := f.host.StartRun(t.Context(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusSucceeded || run.Output == nil || *run.Output != `{"review":"answer b"}` ||
		node(nodes, "a").Status != store.WorkflowStatusSucceeded || node(nodes, "b").Status != store.WorkflowStatusSucceeded {
		t.Fatalf("delivery error affected workflow: %+v, %+v", run, nodes)
	}
	src := store.DeliverySource{Kind: execution.CallerWorkflow, ID: runID}
	view, err := f.s.WorkspaceDeliveryView(t.Context(), scope, src)
	if err != nil || view.Failed == nil || view.Failed.NodeRunID != node(nodes, "a").ID || view.Failed.Reason != "改动过大" ||
		view.Good == nil || view.Good.NodeRunID != node(nodes, "b").ID {
		t.Fatalf("run delivery outcomes: %+v %v", view, err)
	}
	commit, err := exec.Command("git", "-C", view.Good.MirrorPath, "ls-tree", "-r", "--name-only", view.Good.DeliveryCommit).Output()
	if err != nil || !strings.Contains(string(commit), "\na\nb\n") {
		t.Fatalf("second snapshot was not cumulative: %s, %v", commit, err)
	}
}

func TestSharedWorkspaceAttemptsAndInteractiveTurnsDeliver(t *testing.T) {
	f, scope := deliveryWorkflow(t, false)
	runID := f.run(t, interactiveChain, str("initial"))
	if _, err := f.host.StartRun(t.Context(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	src := store.DeliverySource{Kind: execution.CallerWorkflow, ID: runID}
	_, nodes := f.await(t, runID, awaitingInput("a"))
	first, err := f.s.WorkspaceDeliveryView(t.Context(), scope, src)
	if err != nil || first.InProgress || first.Good == nil || first.Good.TurnPosition != 1 || first.Good.NodeRunID != node(nodes, "a").ID {
		t.Fatalf("initial delivery: %+v, %v", first, err)
	}
	if _, err := f.host.SendNodeTurn(t.Context(), f.scope, runID, "a", "second"); err != nil {
		t.Fatal(err)
	}
	f.await(t, runID, func(run store.WorkflowRun, nodes []store.WorkflowNodeRun) bool {
		payload := interactivePayload(t, node(nodes, "a"))
		return awaitingInput("a")(run, nodes) && payload["turns"] == float64(2)
	})
	second, err := f.s.WorkspaceDeliveryView(t.Context(), scope, src)
	if err != nil || second.InProgress || second.Good == nil || second.Good.TurnPosition != 2 || second.Good.NodeRunID != first.Good.NodeRunID || second.Good.ID == first.Good.ID {
		t.Fatalf("follow-up delivery: %+v, %v", second, err)
	}
	if _, err := f.host.CompleteNode(t.Context(), f.scope, runID, "a", false, ""); err != nil {
		t.Fatal(err)
	}
	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("workflow status = %s", run.Status)
	}
	last, err := f.s.WorkspaceDeliveryView(t.Context(), scope, src)
	if err != nil || last.Good == nil || last.Good.NodeRunID != node(nodes, "b").ID {
		t.Fatalf("successor delivery: %+v, %v", last, err)
	}
	refs, err := exec.Command("git", "-C", last.Good.MirrorPath, "for-each-ref", "--format=%(refname)", "refs/ora/deliveries").Output()
	if err != nil || len(strings.Fields(string(refs))) != 3 {
		t.Fatalf("one ref per accepted turn: %s, %v", refs, err)
	}
}

// A node attempt that fails before any prompt is admitted runs no Agent turn, so it has nothing to
// deliver: no work row, no record and no ref, while the node fails exactly as without delivery.
func TestPreAdmissionFailureProducesNoDelivery(t *testing.T) {
	for name, c := range map[string]struct{ agent, kind string }{
		"unassigned template": {`{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"{{#start.topic#}}"}`, "prompt_template"},
		"unknown agent":       {`{"executor":{"agentCli":"official/ora-space.codex","modelId":"fixture"},"prompt":"hi"}`, "missing_agent_ref"},
	} {
		t.Run(name, func(t *testing.T) {
			f, scope := deliveryWorkflow(t, false)
			graph := `{"nodes":[{"id":"start","data":{"kind":"start","inputVariables":[{"name":"topic","valueType":"string"}]}},{"id":"a","data":{"kind":"agent","agentConfig":` + c.agent + `}}],"edges":[{"source":"start","target":"a"}]}`
			runID := f.run(t, graph, nil)
			if _, err := f.host.StartRun(t.Context(), f.scope, runID); err != nil {
				t.Fatal(err)
			}
			run, nodes := f.await(t, runID, terminal)
			view, err := f.s.WorkspaceDeliveryView(t.Context(), scope, store.DeliverySource{Kind: execution.CallerWorkflow, ID: runID})
			if err != nil || view.Latest != nil || view.HasWork || view.InProgress {
				t.Fatalf("pre-admission failure left delivery state: %+v, %v", view, err)
			}
			project, err := f.s.GetProject(t.Context(), scope, f.project)
			if err != nil {
				t.Fatal(err)
			}
			refs, err := exec.Command("git", "--git-dir="+project.MirrorPath, "for-each-ref", "refs/ora/deliveries").Output()
			if err != nil || len(refs) != 0 {
				t.Fatalf("pre-admission failure wrote delivery refs: %q, %v", refs, err)
			}
			if run.Status != store.WorkflowStatusFailed || errorKind(t, node(nodes, "a")) != c.kind {
				t.Fatalf("run %s, node %+v", run.Status, node(nodes, "a"))
			}
		})
	}
}
