package workflowhost

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// pausedExit leaves the runtime's real break transaction committed without acknowledging
// cleanup: the durable crash boundary has cancelled children and frozen outputs, but a live
// Loop parent and scope. No Agent work may be dispatched on this side of the break.
type pausedExit struct{ t *testing.T }

func (p pausedExit) Dispatch(workflow.AgentDispatch)      { p.t.Fatal("break dispatched Agent work") }
func (pausedExit) CleanupLoopExit(string, string, string) {}

func pendingExitAtRestart(t *testing.T, f *fixture, graph string) (string, store.WorkflowScope, string) {
	t.Helper()
	ctx := context.Background()
	id := f.run(t, graph, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	session := f.exec.awaitStart(t)
	rounds, err := f.s.WorkflowEngineLoopRounds(ctx, id)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("rounds = %+v, %v", rounds, err)
	}
	awaitScopeSession(t, f, id, rounds[0].ID, "writer")
	f.host.Close()
	if _, err := f.s.StartWorkflowEngineNodes(ctx, id, []store.WorkflowEngineStart{{NodeID: "exit", NodeType: "loopExit", ScopeID: rounds[0].ID}}); err != nil {
		t.Fatal(err)
	}
	engine := workflow.NewEngine(repository{f.s}, pausedExit{t}, func(string) {}, f.host.now)
	defer engine.Close()
	if err := engine.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	rounds, err = f.s.WorkflowEngineLoopRounds(ctx, id)
	if err != nil || len(rounds) != 1 || !workflow.HasPendingLoopExit(rounds[0].State) {
		t.Fatalf("break was not persisted: %+v, %v", rounds, err)
	}
	return id, rounds[0], session
}

func TestRestartReplaysPendingLoopExitCleanup(t *testing.T) {
	for _, frozenError := range []bool{false, true} {
		name := "frozen output"
		graph := exitGraph()
		if frozenError {
			name = "frozen export error"
			graph = strings.Replace(graph, `"variableSelector":["loop","draft"]`, `"variableSelector":["writer","output"]`, 1)
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			id, round, session := pendingExitAtRestart(t, f, graph)
			// Exercise the same owner-before-workflow recovery sequence as main.
			bootProcess(t, f)
			run, rows := f.await(t, id, terminal)
			want := store.WorkflowStatusSucceeded
			if frozenError {
				want = store.WorkflowStatusFailed
				if run.Error == nil || !strings.Contains(*run.Error, "no value in this round: writer.output") || node(rows, "out").ID != "" {
					t.Fatalf("frozen error was replaced or downstream ran: %+v %+v", run, rows)
				}
			} else if run.Output == nil || *run.Output != `{"result":"seed"}` || !strings.Contains(string(node(rows, "loop").Payload), `"loop_exit"`) {
				t.Fatalf("frozen output or stop reason changed: %+v %+v", run, rows)
			}
			if run.Status != want || node(rows, "loop").Status != want {
				t.Fatalf("run = %s, loop = %s; want %s", run.Status, node(rows, "loop").Status, want)
			}
			children, err := f.s.WorkflowEngineScopeNodes(ctx, id, round.ID)
			if err != nil || node(children, "writer").Status != store.WorkflowStatusCancelled || node(children, "exit").Status != store.WorkflowStatusSucceeded {
				t.Fatalf("break children changed: %+v %v", children, err)
			}
			// Both a late turn and a stale cleanup acknowledgment are fenced after restart.
			f.host.complete(workflow.AgentDispatch{RunID: id, NodeRunID: f.exec.request(session).Caller.AttemptID}, workflow.Completion{Output: str("late replacement")})
			if err := f.host.engine.FinishLoopExit(ctx, id, *round.ParentLoopNodeRunID, round.ID, errors.New("late cleanup error")); err != nil {
				t.Fatal(err)
			}
			bootProcess(t, f)
			again, err := f.s.WorkflowRun(ctx, f.scope.TenantID, id)
			if err != nil || again.Status != run.Status || string(again.RuntimePayload) != string(run.RuntimePayload) {
				t.Fatalf("duplicate recovery or late callback changed run: %+v %v", again, err)
			}
			rounds, err := f.s.WorkflowEngineLoopRounds(ctx, id)
			if err != nil || len(rounds) != 1 || *rounds[0].Status != want || string(rounds[0].State) != string(round.State) || starts(f) != 1 {
				t.Fatalf("recovery replayed work or changed frozen state: %+v %v; starts = %d", rounds, err, starts(f))
			}
		})
	}
}

func TestPendingLoopExitDoesNotRecoverAnEndedRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id, round, _ := pendingExitAtRestart(t, f, exitGraph())
	message := "another branch failed"
	if _, err := f.s.FinishWorkflowRun(ctx, f.scope.TenantID, id, store.WorkflowStatusFailed, nil, &message); err != nil {
		t.Fatal(err)
	}
	bootProcess(t, f)
	run, rows := f.await(t, id, terminal)
	rounds, err := f.s.WorkflowEngineLoopRounds(ctx, id)
	if err != nil || run.Status != store.WorkflowStatusFailed || run.Error == nil || *run.Error != message || node(rows, "loop").Status != store.WorkflowStatusFailed || node(rows, "out").ID != "" || *rounds[0].Status != store.WorkflowStatusFailed || string(rounds[0].State) != string(round.State) || starts(f) != 1 {
		t.Fatalf("ended run was recovered: %+v %+v %+v %v", run, rows, rounds, err)
	}
}

func TestRestartDoesNotAbsorbAnInvalidOrInactiveLoopExit(t *testing.T) {
	for _, variant := range []string{"invalid state", "inactive round"} {
		t.Run(variant, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			id, round, _ := pendingExitAtRestart(t, f, exitGraph())
			status, state := store.WorkflowStatusRunning, round.State
			if variant == "invalid state" {
				state = []byte(`{"exit":{"nodeRunId":"exit"},"variablePool":null}`)
			} else {
				status = store.WorkflowStatusSucceeded
			}
			if _, err := f.s.SettleWorkflowLoopRound(ctx, f.scope.TenantID, id, round.ID, status, state); err != nil {
				t.Fatal(err)
			}
			bootProcess(t, f)
			run, rows := f.await(t, id, terminal)
			if run.Status != store.WorkflowStatusFailed || node(rows, "loop").Status != store.WorkflowStatusFailed || node(rows, "out").ID != "" || starts(f) != 1 {
				t.Fatalf("invalid break survived: %+v %+v", run, rows)
			}
		})
	}
}

func TestLoopExitRecoveryStillRejectsOuterWork(t *testing.T) {
	for _, kind := range []string{"executing", "retry wait"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			graph := strings.Replace(exitGraph(), `{"id":"out"`, `{"id":"outer","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"outer","retry":{"enabled":true,"maxRetries":2,"initialDelaySeconds":10}}}},{"id":"out"`, 1)
			graph = strings.Replace(graph, `"edges":[`, `"edges":[{"source":"loop","target":"outer"},`, 1)
			id, _, _ := pendingExitAtRestart(t, f, graph)
			active, err := f.s.StartWorkflowEngineNodes(ctx, id, []store.WorkflowEngineStart{{NodeID: "outer", NodeType: "agent"}})
			if err != nil || len(active) != 1 {
				t.Fatalf("outer attempt = %+v %v", active, err)
			}
			if kind == "retry wait" {
				// Arm an actual retry wait through the engine before restarting.
				engine := workflow.NewEngine(repository{f.s}, pausedExit{t}, func(string) {}, f.host.now)
				if err := engine.FailNode(ctx, id, active[0].ID, workflow.Failure{Kind: workflow.FailureSession, Message: "lost turn"}); err != nil {
					engine.Close()
					t.Fatal(err)
				}
				engine.Close()
				rows, err := f.s.WorkflowEngineNodeRuns(ctx, id)
				outer := node(rows, "outer")
				if err != nil || outer.Status != store.WorkflowStatusRunning || outer.StartedAt != nil || !strings.Contains(string(outer.Payload), "retry_wait") {
					t.Fatalf("outer retry wait = %+v %v", rows, err)
				}
			}
			bootProcess(t, f)
			run, rows := f.await(t, id, terminal)
			if run.Status != store.WorkflowStatusFailed || node(rows, "out").ID != "" || node(rows, "loop").Status != store.WorkflowStatusFailed || strings.Contains(string(node(rows, "outer").Payload), "retry_wait") || starts(f) != 1 {
				t.Fatalf("outer work survived the sweep: %+v %+v", run, rows)
			}
		})
	}
}
