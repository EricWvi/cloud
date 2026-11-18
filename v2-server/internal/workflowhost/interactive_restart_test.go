package workflowhost

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// interactiveBeside parks A while C, an automatic sibling, may still be executing.
const interactiveBeside = `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"起草","interactive":true}}},
 {"id":"c","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"另写一份"}}}
],"edges":[{"source":"start","target":"a"},{"source":"start","target":"c"}]}`

// recordedTurns makes the fake execution service answer reads of session as the store would
// after a restart: the session is open and its turns are the given results, the first one owned
// by the node run and each later one by its numbered follow-up.
func (f *fakeExecution) recordedTurns(session, nodeRun string, results ...execution.Result) {
	f.ReadSessionFunc = func(_ context.Context, _ execution.Scope, id string) (execution.Session, error) {
		if id != session {
			return execution.Session{}, execution.ErrNotFound
		}
		return execution.Session{ID: id}, nil
	}
	f.ReadTurnsFunc = func(_ context.Context, _ execution.Scope, id, _ string, _ int) (execution.TurnPage, error) {
		page := execution.TurnPage{}
		for i, r := range results {
			attempt := nodeRun
			if i > 0 {
				attempt = nodeRun + "/turn-" + strconv.Itoa(i)
			}
			result := r
			page.Items = append(page.Items, execution.Turn{Caller: execution.Caller{Kind: execution.CallerWorkflow, AttemptID: attempt}, Result: &result})
		}
		return page, nil
	}
}

// restart stops the host as the process would and starts the next one over the same store.
func (f *fixture) restart(t *testing.T) {
	t.Helper()
	f.host.Close()
	f.host = f.newHost(t)
	if err := f.host.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestParkedNodeSurvivesARestartAndContinues(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	_, nodes := f.await(t, runID, parked("a"))
	rowA := node(nodes, "a")
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再改一版"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.exec.finish(a, completed("初稿二"))
	f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusPending && interactivePayload(t, node(n, "a"))["turns"] == float64(2)
	})
	f.exec.recordedTurns(a, rowA.ID, completed("初稿一"), completed("初稿二"))

	f.restart(t)
	run, nodes := f.await(t, runID, awaitingInput("a"))
	if run.Status != store.WorkflowRunAwaitingInput || node(nodes, "a").ID != rowA.ID || node(nodes, "a").FinishedAt != nil {
		t.Fatalf("after the restart run %s node %+v, want A still awaiting input as the same attempt", run.Status, node(nodes, "a"))
	}
	if f.exec.isClosed(a) {
		t.Fatal("the restart ended the session of a valid interactive wait")
	}

	// The conversation continues in the same session; the follow-up numbering continues too.
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "最后改一次"); err != nil {
		t.Fatalf("follow-up after the restart: %v", err)
	}
	if id := f.exec.awaitAppend(t); id != a {
		t.Fatalf("follow-up went to session %s, want %s", id, a)
	}
	f.exec.mu.Lock()
	last := f.exec.appended[len(f.exec.appended)-1]
	f.exec.mu.Unlock()
	if last.Caller.AttemptID != rowA.ID+"/turn-2" {
		t.Fatalf("follow-up attempt = %q, want the numbering to continue after the restart", last.Caller.AttemptID)
	}
	f.exec.finish(a, completed("终稿"))
	_, nodes = f.await(t, runID, parked("a"))
	if p := interactivePayload(t, node(nodes, "a")); p["turns"] != float64(3) || p["answer"] != "终稿" {
		t.Fatalf("summary after the restart = %v", p)
	}

	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", false, "human"); err != nil {
		t.Fatalf("complete after the restart: %v", err)
	}
	b := f.exec.awaitStart(t)
	if got := promptText(t, f.exec.request(b).Prompt); !strings.Contains(got, "审阅：终稿") {
		t.Fatalf("B's prompt = %q, want the answer accepted after the restart", got)
	}
	if len(f.exec.started) != 2 {
		t.Fatalf("started %d sessions, want A's first turn and B only: the restart replayed nothing", len(f.exec.started))
	}
}

func TestRestartFailsANodeWhoseTurnWasExecuting(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再改一版"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	_, nodes := f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusRunning
	})
	f.exec.recordedTurns(a, node(nodes, "a").ID, completed("初稿一"))

	f.restart(t) // the follow-up was executing: it is not replayed
	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusFailed || errorKind(t, node(nodes, "a")) != "interrupted_by_restart" {
		t.Fatalf("run %s node %+v, want the interrupted turn to fail the node and run", run.Status, node(nodes, "a"))
	}
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "继续"); !errors.Is(err, server.ErrWorkflowNodeNotAwaitingInput) {
		t.Fatalf("follow-up to a failed node = %v", err)
	}
}

func TestRestartFailsAParkedNodeBesideAnExecutingAgent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, interactiveBeside, str("支付"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	sessions := f.exec.awaitStarts(t, 2)
	f.exec.finish(sessions["a"], completed("初稿"))
	_, nodes := f.await(t, runID, parked("a"))
	f.exec.recordedTurns(sessions["a"], node(nodes, "a").ID, completed("初稿"))

	f.restart(t) // C was executing: the run fails, A with it, as desktop's sweep does
	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusFailed || errorKind(t, node(nodes, "a")) != "interrupted_by_restart" || errorKind(t, node(nodes, "c")) != "interrupted_by_restart" {
		t.Fatalf("run %s nodes %+v, want both attempts interrupted", run.Status, nodes)
	}
}

func TestMissingWorkspaceFailsTheRecoveredNode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	_, nodes := f.await(t, runID, parked("a"))
	f.exec.recordedTurns(a, node(nodes, "a").ID, completed("初稿一"))
	f.restart(t)

	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再改一版"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.exec.finish(a, execution.Result{Status: execution.StatusFailed, FailureCode: execution.FailureWorkspaceMissing, AgentLost: true, NotStarted: true})
	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusFailed || errorKind(t, node(nodes, "a")) != "interactive_session" {
		t.Fatalf("run %s node %+v, want the node to fail once its directory is gone", run.Status, node(nodes, "a"))
	}
}

func TestUnstartedRecoveryKeepsTheNodeWaiting(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	_, nodes := f.await(t, runID, parked("a"))
	f.exec.recordedTurns(a, node(nodes, "a").ID, completed("初稿一"))
	f.restart(t)

	// The new Agent could not be started (Node was busy): the turn failed, nothing ran, and the
	// starter can try again in the same session.
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再改一版"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.exec.finish(a, execution.Result{Status: execution.StatusFailed, FailureCode: "agent_prepare_failed", NotStarted: true})
	_, nodes = f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusPending && interactivePayload(t, node(n, "a"))["turns"] == float64(2)
	})
	if p := interactivePayload(t, node(nodes, "a")); p["session_ended"] != false || p["answer"] != "初稿一" {
		t.Fatalf("summary = %v, want the session open with the earlier answer", p)
	}
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再试一次"); err != nil {
		t.Fatalf("retry after an unstarted recovery: %v", err)
	}
	if id := f.exec.awaitAppend(t); id != a {
		t.Fatalf("retry went to session %s, want %s", id, a)
	}
}

func TestParkedNodeThatCannotContinueFailsOnRestart(t *testing.T) {
	for name, breakIt := range map[string]func(f *fixture, session string){
		"executor gone": func(f *fixture, _ string) {
			f.exec.ListExecutorsFunc = func(context.Context, execution.Scope) ([]execution.Executor, error) { return nil, nil }
		},
		"session gone": func(f *fixture, _ string) {
			f.exec.ReadSessionFunc = func(context.Context, execution.Scope, string) (execution.Session, error) {
				return execution.Session{}, execution.ErrNotFound
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			runID, a := f.startParked(t, interactiveChain, "初稿一")
			_, nodes := f.await(t, runID, parked("a"))
			f.exec.recordedTurns(a, node(nodes, "a").ID, completed("初稿一"))
			breakIt(f, a)

			f.restart(t)
			run, nodes := f.await(t, runID, terminal)
			if run.Status != store.WorkflowStatusFailed || node(nodes, "a").Status != store.WorkflowStatusFailed {
				t.Fatalf("run %s node %+v, want a node that cannot continue to fail with its run, not to wait forever", run.Status, node(nodes, "a"))
			}
			if !f.exec.isClosed(a) {
				t.Fatal("the session of a node that failed on restart was left open")
			}
		})
	}
}
