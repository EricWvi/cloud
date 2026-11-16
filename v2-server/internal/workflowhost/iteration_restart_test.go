package workflowhost

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// restartHost closes the current host and boots a fresh one, as a process restart does, then
// applies the boot sweep. Closing first stops the run's workers without reporting their turns,
// which is what a kill does to an executing Agent.
func restartHost(t *testing.T, f *fixture) {
	t.Helper()
	f.host.Close()
	f.host = f.newHost(t)
	if err := f.host.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// settleBetweenRounds commits a round-0 member's outcome through the store, reproducing the
// crash window between a member's terminal commit and the container advance that settles the
// ledger and starts the next round: the owner stays running, the member row is terminal, the
// ledger has no round-0 entry and no round-1 row exists. The still-open session is then left to
// the closed host, whose worker gives up without reporting (a late duplicate).
func settleBetweenRounds(t *testing.T, f *fixture, runID string, row store.WorkflowNodeRun, output *string) {
	t.Helper()
	ctx := context.Background()
	if output == nil {
		message := "round zero broke"
		if _, err := f.s.FailWorkflowEngineNode(ctx, runID, row.ID, message, nil, func(int, []byte, []byte) ([]byte, error) {
			return []byte(`{"error_detail":{"kind":"session"}}`), nil
		}); err != nil {
			t.Fatal(err)
		}
	} else if _, err := f.s.CompleteWorkflowEngineNode(ctx, runID, row.ID, output,
		func(node []byte) ([]byte, error) { return node, nil },
		func(raw []byte, node store.WorkflowNodeRun) ([]byte, error) {
			p, ok, err := workflow.DecodeRunPayload(raw)
			if err != nil || !ok {
				return raw, err
			}
			if err := p.Complete(node.NodeID, node.NodeType, node.Iteration, output, nil); err != nil {
				return nil, err
			}
			return p.Encode()
		}); err != nil {
		t.Fatal(err)
	}
}

// iterationSessionsByRound counts the worker sessions started per Iteration round from the
// persisted attempts, proving completed rounds are not replayed after a restart.
func iterationSessionsByRound(t *testing.T, f *fixture, runID string) map[int][]string {
	t.Helper()
	rows, err := f.s.WorkflowEngineNodeRuns(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	byRound := map[int][]string{}
	for _, row := range rows {
		if row.NodeID != "worker" || row.Iteration == nil || row.SessionID == nil {
			continue
		}
		byRound[*row.Iteration] = append(byRound[*row.Iteration], *row.SessionID)
	}
	return byRound
}

// Restarting while an Iteration member's Agent turn executes (retry disabled) settles the
// interrupted round as failed and lets the error strategy decide: continue runs the remaining
// rounds; fail stops the run through the Iteration. Completed rounds are never replayed.
func TestRestartDuringExecutingIterationMember(t *testing.T) {
	for _, strategy := range []string{"continue", "fail"} {
		t.Run(strategy, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			id := f.run(t, iterationGraph(strategy, []string{"one", "two"}, 50), nil)
			if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
				t.Fatal(err)
			}
			first := f.exec.awaitStart(t)
			if !strings.Contains(promptText(t, f.exec.request(first).Prompt), "item 0: one") {
				t.Fatalf("first round prompt: %s", promptText(t, f.exec.request(first).Prompt))
			}
			// Wait for the session binding so the interrupted round keeps its real session.
			f.await(t, id, func(_ store.WorkflowRun, rows []store.WorkflowNodeRun) bool {
				return iterationRow(rows, "worker", 0).SessionID != nil
			})
			restartHost(t, f)
			var run store.WorkflowRun
			var rows []store.WorkflowNodeRun
			if strategy == "continue" {
				second := f.exec.awaitStart(t)
				if text := promptText(t, f.exec.request(second).Prompt); !strings.Contains(text, "item 1: two") {
					t.Fatalf("restarted round prompt: %s", text)
				}
				// The resumed round runs as the run's frozen starter, not as nobody.
				if got, want := f.exec.scope(second), (execution.Scope{TenantID: f.scope.TenantID, UserID: f.scope.UserID}); got != want {
					t.Fatalf("resumed round started under %+v, want the frozen starter %+v", got, want)
				}
				f.exec.finish(second, completed("two"))
				run, rows = f.await(t, id, terminal)
				if run.Status != store.WorkflowStatusSucceeded || run.Output == nil || *run.Output != `{"failed":1,"items":["two"]}` {
					t.Fatalf("continue run = %+v", run)
				}
			} else {
				run, rows = f.await(t, id, terminal)
				if run.Status != store.WorkflowStatusFailed || node(rows, "iter").Status != store.WorkflowStatusFailed {
					t.Fatalf("fail run = %+v iter = %+v", run, node(rows, "iter"))
				}
				if iterationRow(rows, "worker", 1).ID != "" {
					t.Fatal("fail strategy started a later round after the restart")
				}
			}
			interrupted := iterationRow(rows, "worker", 0)
			if interrupted.Status != store.WorkflowStatusFailed || errorKind(t, interrupted) != "interrupted_by_restart" {
				t.Fatalf("interrupted member = %+v", interrupted)
			}
			sessions := iterationSessionsByRound(t, f, id)
			if len(sessions[0]) != 1 || sessions[0][0] != first {
				t.Fatalf("round 0 was replayed or rebound: %v", sessions)
			}
			wantRound1 := 0
			if strategy == "continue" {
				wantRound1 = 1
			}
			if len(sessions[1]) != wantRound1 {
				t.Fatalf("round 1 sessions = %v, want %d", sessions[1], wantRound1)
			}
		})
	}
}

// A restart in the crash window between a member's terminal commit and the container
// advance leaves the owner running with no active member. The sweep absorbs the run and the
// resume settles round 0 from the persisted rows: a failed member applies the error strategy,
// a succeeded one lets the next round run.
func TestRestartBetweenIterationRounds(t *testing.T) {
	for _, tc := range []struct {
		strategy, round0 string
		wantStatus       string
		wantOutput       *string
	}{
		{"continue", "failed", store.WorkflowStatusSucceeded, str(`{"failed":1,"items":["two"]}`)},
		{"fail", "failed", store.WorkflowStatusFailed, nil},
		{"continue", "succeeded", store.WorkflowStatusSucceeded, str(`{"failed":0,"items":["one","two"]}`)},
	} {
		name := tc.strategy + "-" + tc.round0
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			id := f.run(t, iterationGraph(tc.strategy, []string{"one", "two"}, 50), nil)
			if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
				t.Fatal(err)
			}
			first := f.exec.awaitStart(t)
			// Wait for the session binding so the round-0 attempt keeps its real session and the
			// store-level settle below is not racing the worker's own bind.
			f.await(t, id, func(_ store.WorkflowRun, rows []store.WorkflowNodeRun) bool {
				return iterationRow(rows, "worker", 0).SessionID != nil
			})
			rows, err := f.s.WorkflowEngineNodeRuns(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			var round0Output *string
			if tc.round0 == "succeeded" {
				round0Output = str("one")
			}
			settleBetweenRounds(t, f, id, iterationRow(rows, "worker", 0), round0Output)
			restartHost(t, f)
			if tc.strategy == "continue" || tc.round0 == "succeeded" {
				next := f.exec.awaitStart(t)
				if text := promptText(t, f.exec.request(next).Prompt); !strings.Contains(text, "item 1: two") {
					t.Fatalf("resumed round prompt: %s", text)
				}
				f.exec.finish(next, completed("two"))
			}
			run, rows := f.await(t, id, terminal)
			if run.Status != tc.wantStatus || (tc.wantOutput != nil && (run.Output == nil || *run.Output != *tc.wantOutput)) {
				t.Fatalf("run = %+v, want %s %v", run, tc.wantStatus, tc.wantOutput)
			}
			round0 := iterationRow(rows, "worker", 0)
			wantRound0 := map[string]string{"failed": store.WorkflowStatusFailed, "succeeded": store.WorkflowStatusSucceeded}[tc.round0]
			if round0.Status != wantRound0 {
				t.Fatalf("round 0 row = %s, want %s", round0.Status, wantRound0)
			}
			sessions := iterationSessionsByRound(t, f, id)
			if len(sessions[0]) != 1 || sessions[0][0] != first {
				t.Fatalf("round 0 was replayed or rebound: %v", sessions)
			}
		})
	}
}

// A root Agent executing beside the Iteration makes the sweep fail the whole run;
// the Iteration must not absorb outer work.
func TestRestartWithARootAgentBesideTheIterationFailsTheRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	graph := strings.Replace(iterationGraph("continue", []string{"one"}, 50), `{"id":"iter"`, `{"id":"outer","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"outer"}}},
	 {"id":"iter"`, 1)
	graph = strings.Replace(graph, `"edges":[`, `"edges":[{"source":"start","target":"outer"},`, 1)
	id := f.run(t, graph, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	// The outer Agent and the worker execute concurrently; both are running when the process dies.
	f.exec.awaitStarts(t, 2)
	f.await(t, id, func(_ store.WorkflowRun, rows []store.WorkflowNodeRun) bool {
		return node(rows, "outer").SessionID != nil && node(rows, "worker").SessionID != nil
	})
	restartHost(t, f)
	run, rows := f.await(t, id, terminal)
	if run.Status != store.WorkflowStatusFailed || node(rows, "iter").Status != store.WorkflowStatusFailed {
		t.Fatalf("run = %+v iter = %+v; the Iteration absorbed an outer Agent", run, node(rows, "iter"))
	}
	for _, rowID := range []string{"outer", "worker"} {
		if node(rows, rowID).Status != store.WorkflowStatusFailed && node(rows, rowID).Status != store.WorkflowStatusCancelled {
			t.Fatalf("%s = %+v", rowID, node(rows, rowID))
		}
	}
	if starts(f) != 2 {
		t.Fatalf("restart replayed sessions: %d started", starts(f))
	}
}

// A dispatch failure after a wake inside an Iteration must fail the attempt AND run
// the scheduling pass. The only Iteration dispatch-failure source is an undecodable run
// payload, which the pass decodes identically, so the pass's observable contribution here is
// that it runs and surfaces the corruption (the mutant that skips it returns nil and the
// corruption is silently swallowed). Repairing the payload and resuming then settles the round.
func TestWakeDispatchFailureInsideIterationRunsTheSchedulingPass(t *testing.T) {
	for _, strategy := range []string{"continue", "fail"} {
		t.Run(strategy, func(t *testing.T) {
			f := newFixture(t)
			clock := retryClock(t, f)
			ctx := context.Background()
			id := f.run(t, retryContainer(iterationGraph(strategy, []string{"one", "two"}, 50)), nil)
			if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
				t.Fatal(err)
			}
			f.exec.finish(f.exec.awaitStart(t), execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
			zero := 0
			wait, payload := containerWait(t, f, id, "", "worker", &zero)
			rows, err := f.s.WorkflowEngineNodeRuns(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			// Capture the live payload, then corrupt the run payload after the wait is armed:
			// the wake commits the start, then the dispatch cannot decode the payload.
			healthy, err := f.s.WorkflowEngineContext(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			good := healthy.Run.RuntimePayload
			if _, _, err := f.s.AdvanceWorkflowEngineContainer(ctx, id, node(rows, "iter").ID, store.WorkflowEngineContainerAdvance{
				Fold: func([]byte) ([]byte, error) { return []byte(`{"locale":5}`), nil },
			}); err != nil {
				t.Fatal(err)
			}
			wakeContainerRetryExpectError(t, f, clock, id, payload, wait)
			afterWake, rows := f.await(t, id, func(_ store.WorkflowRun, rows []store.WorkflowNodeRun) bool {
				return iterationRow(rows, "worker", 0).Status == store.WorkflowStatusFailed
			})
			final := iterationRow(rows, "worker", 0)
			if errorKind(t, final) != "invalid_run_payload" || afterWake.Status != store.WorkflowStatusRunning {
				t.Fatalf("dispatch-failed attempt = %+v kind = %s run = %s", final, errorKind(t, final), afterWake.Status)
			}
			// Repair the payload and resume: the round settles by the strategy.
			if _, _, err := f.s.AdvanceWorkflowEngineContainer(ctx, id, node(rows, "iter").ID, store.WorkflowEngineContainerAdvance{
				Fold: func([]byte) ([]byte, error) { return good, nil },
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.host.engine.Resume(ctx, id); err != nil {
				t.Fatal(err)
			}
			if strategy == "continue" {
				next := f.exec.awaitStart(t)
				if text := promptText(t, f.exec.request(next).Prompt); !strings.Contains(text, "item 1: two") {
					t.Fatalf("resumed round prompt: %s", text)
				}
				f.exec.finish(next, completed("two"))
			}
			run, rows := f.await(t, id, terminal)
			if strategy == "continue" {
				if run.Status != store.WorkflowStatusSucceeded || run.Output == nil || *run.Output != `{"failed":1,"items":["two"]}` {
					t.Fatalf("continue run = %+v", run)
				}
			} else if run.Status != store.WorkflowStatusFailed || node(rows, "iter").Status != store.WorkflowStatusFailed {
				t.Fatalf("fail run = %+v iter = %+v", run, node(rows, "iter"))
			}
		})
	}
}

// wakeContainerRetryExpectError wakes like wakeContainerRetry but asserts the engine reported
// the scheduling pass's failure instead of silently returning.
func wakeContainerRetryExpectError(t *testing.T, f *fixture, clock interface{ Store(int64) }, runID string, payload waitPayload, row store.WorkflowNodeRun) {
	t.Helper()
	clock.Store(payload.Wait.Due)
	if err := f.host.engine.WakeRetry(context.Background(), runID, row.ID); err == nil {
		t.Fatal("the wake reported no error: the scheduling pass after the dispatch failure did not run")
	}
}

// awaitWorkerDrained waits until every Agent attempt of a still-running run has finished handling
// its turn, including the engine callback and the scheduling pass that callback runs: an attempt's
// goroutine stays counted in dispatched until both returned.
func awaitWorkerDrained(t *testing.T, f *fixture, runID string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; {
		st := f.host.state(runID)
		if st == nil {
			t.Fatal("the run's host state is gone: the run is no longer running")
		}
		st.mu.Lock()
		drained := st.dispatched == 0 && len(st.attempts) == 0
		st.mu.Unlock()
		if drained {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the run's attempts never finished their turns")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A dispatch failure after a wake must fail the attempt and run a scheduling pass, so
// the container settles instead of the run hanging on a failed row nothing drives. A Loop member
// is used because a corrupt round state breaks the dispatch only, while `schedule` still runs and
// fails the Loop through the same decode.
func TestWakeDispatchFailureSettlesTheContainer(t *testing.T) {
	f := newFixture(t)
	clock := retryClock(t, f)
	ctx := context.Background()
	id := f.run(t, retryContainer(loopGraph), nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(f.exec.awaitStart(t), execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	rounds, err := f.s.WorkflowEngineLoopRounds(ctx, id)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("rounds = %+v %v", rounds, err)
	}
	wait, payload := containerWait(t, f, id, rounds[0].ID, "writer", nil)
	// The wait is visible as soon as the failure callback commits it, but the callback then still
	// runs a scheduling pass; corrupting the state before that pass ends would let it fail the Loop
	// and cancel the wait before the wake this test is about.
	awaitWorkerDrained(t, f, id)
	// Corrupt the round state after the wait is armed (valid JSON, but the Loop pool is gone):
	// the wake commits the start, the dispatch cannot decode the scope state and must fail the
	// attempt, and the scheduling pass then fails the Loop through the same decode.
	if _, err := f.s.SettleWorkflowLoopRound(ctx, f.scope.TenantID, id, rounds[0].ID, store.WorkflowStatusRunning, []byte(`{"conditionDecisions":{}}`)); err != nil {
		t.Fatal(err)
	}
	wakeContainerRetry(t, f, clock, id, payload, wait)
	run, rows := f.await(t, id, terminal)
	if run.Status != store.WorkflowStatusFailed || node(rows, "loop").Status != store.WorkflowStatusFailed {
		t.Fatalf("run = %+v loop = %+v; the scheduling pass did not settle the container", run, node(rows, "loop"))
	}
	children, err := f.s.WorkflowEngineScopeNodes(ctx, id, rounds[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	writer := node(children, "writer")
	if writer.Status != store.WorkflowStatusFailed || errorKind(t, writer) != "invalid_run_payload" {
		t.Fatalf("dispatch-failed attempt = %+v kind = %s", writer, errorKind(t, writer))
	}
}

// LoopExit against an executing retried turn must cancel that turn; the dispatch must
// carry the Loop scope so the drain finds it.
func TestLoopExitCancelsAnExecutingRetriedTurn(t *testing.T) {
	f := newFixture(t)
	clock := retryClock(t, f)
	ctx := context.Background()
	graph := strings.Replace(retryContainer(exitGraph()), `{"source":"entry","target":"writer"}`, `{"source":"entry","target":"writer"},{"source":"writer","target":"exit"}`, 1)
	id := f.run(t, graph, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(f.exec.awaitStart(t), execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	rounds, err := f.s.WorkflowEngineLoopRounds(ctx, id)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("rounds = %+v %v", rounds, err)
	}
	wait, payload := containerWait(t, f, id, rounds[0].ID, "writer", nil)
	wakeContainerRetry(t, f, clock, id, payload, wait)
	retried := f.exec.awaitStart(t)
	f.await(t, id, func(_ store.WorkflowRun, _ []store.WorkflowNodeRun) bool {
		children, err := f.s.WorkflowEngineScopeNodes(ctx, id, rounds[0].ID)
		return err == nil && node(children, "writer").SessionID != nil
	})
	// The committed break takes precedence while the retried turn executes.
	if _, err := f.s.StartWorkflowEngineNodes(ctx, id, []store.WorkflowEngineStart{{NodeID: "exit", NodeType: "loopExit", ScopeID: rounds[0].ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.host.engine.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	run, _ := f.await(t, id, terminal)
	if run.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("run = %s %v", run.Status, run.Error)
	}
	cancelled := false
	f.exec.mu.Lock()
	for _, ref := range f.exec.cancels {
		if ref.SessionID == retried {
			cancelled = true
		}
	}
	f.exec.mu.Unlock()
	if !cancelled {
		t.Fatalf("LoopExit did not cancel the executing retried turn %s", retried)
	}
	children, err := f.s.WorkflowEngineScopeNodes(ctx, id, rounds[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if writer := node(children, "writer"); writer.Status != store.WorkflowStatusCancelled {
		t.Fatalf("retried writer row = %+v", writer)
	}
}

// A Loop member exhausting its retries fails the Loop and the run through the child row.
func TestLoopMemberExhaustingRetriesFailsTheLoop(t *testing.T) {
	f := newFixture(t)
	clock := retryClock(t, f)
	ctx := context.Background()
	id := f.run(t, retryContainer(loopGraph), nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	sessions := []string{}
	for attempt := 0; attempt < 2; attempt++ {
		session := f.exec.awaitStart(t)
		sessions = append(sessions, session)
		f.exec.finish(session, execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
		if attempt == 0 {
			rounds, err := f.s.WorkflowEngineLoopRounds(ctx, id)
			if err != nil || len(rounds) != 1 {
				t.Fatalf("rounds = %+v %v", rounds, err)
			}
			wait, payload := containerWait(t, f, id, rounds[0].ID, "writer", nil)
			wakeContainerRetry(t, f, clock, id, payload, wait)
		}
	}
	run, rows := f.await(t, id, terminal)
	if run.Status != store.WorkflowStatusFailed || node(rows, "loop").Status != store.WorkflowStatusFailed {
		t.Fatalf("run = %+v loop = %+v", run, node(rows, "loop"))
	}
	rounds, err := f.s.WorkflowEngineLoopRounds(ctx, id)
	if err != nil || len(rounds) != 1 || *rounds[0].Status != store.WorkflowStatusFailed {
		t.Fatalf("rounds = %+v %v", rounds, err)
	}
	children, err := f.s.WorkflowEngineScopeNodes(ctx, id, rounds[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	writer := node(children, "writer")
	if writer.Status != store.WorkflowStatusFailed {
		t.Fatalf("exhausted writer = %+v", writer)
	}
	var detail waitPayload
	if err := json.Unmarshal(writer.Payload, &detail); err != nil || detail.Detail.Attempt != 2 {
		t.Fatalf("exhausted attempt detail = %s (%v)", writer.Payload, err)
	}
	history, err := f.s.WorkflowRunFailedAttempts(ctx, f.scope.TenantID, id)
	if err != nil || len(history) != 1 || history[0].SessionID == nil || *history[0].SessionID != sessions[0] {
		t.Fatalf("history = %+v %v", history, err)
	}
	if rounds[0].Status == nil || strings.Contains(string(writer.Payload), "retry_wait") {
		t.Fatal("the exhausted member kept waiting")
	}
}

// Cancel and run-failure abandonment for an Iteration wait (root-scope rows, pinned here so
// the Iteration path cannot drift from the root one).
func TestIterationWaitCancelAndAbandonment(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		f := newFixture(t)
		clock := retryClock(t, f)
		ctx := context.Background()
		id := f.run(t, retryContainer(iterationGraph("continue", []string{"one"}, 50)), nil)
		if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
			t.Fatal(err)
		}
		f.exec.finish(f.exec.awaitStart(t), execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
		zero := 0
		wait, payload := containerWait(t, f, id, "", "worker", &zero)
		if _, err := f.host.CancelRun(ctx, f.scope, id); err != nil {
			t.Fatal(err)
		}
		wakeContainerRetry(t, f, clock, id, payload, wait)
		run, rows := f.await(t, id, terminal)
		worker := iterationRow(rows, "worker", 0)
		if run.Status != store.WorkflowStatusCancelled || worker.Status != store.WorkflowStatusCancelled || strings.Contains(string(worker.Payload), "retry_wait") || starts(f) != 1 {
			t.Fatalf("cancel: run = %s worker = %+v sessions = %d", run.Status, worker, starts(f))
		}
	})
	t.Run("abandoned by another branch", func(t *testing.T) {
		f := newFixture(t)
		clock := retryClock(t, f)
		ctx := context.Background()
		// The root Agent and the Iteration member execute concurrently. Hold the root turn, fail
		// the member's attempt into a retry wait, then fail the root turn: the run failure must
		// abandon the Iteration wait.
		graph := retryContainer(iterationGraph("continue", []string{"one"}, 50))
		graph = strings.Replace(graph, `{"id":"iter"`, `{"id":"outer","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"outer",`+containerRetryDisabled+`}}},
		 {"id":"iter"`, 1)
		graph = strings.Replace(graph, `"edges":[`, `"edges":[{"source":"start","target":"outer"},`, 1)
		id := f.run(t, graph, nil)
		if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
			t.Fatal(err)
		}
		outer := f.exec.awaitStarts(t, 2)["outer"]
		if outer == "" {
			t.Fatal("the outer Agent did not start")
		}
		// Arm the member's retry while the outer turn executes.
		f.await(t, id, func(_ store.WorkflowRun, rows []store.WorkflowNodeRun) bool {
			return iterationRow(rows, "worker", 0).ID != ""
		})
		rows, err := f.s.WorkflowEngineNodeRuns(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.host.engine.FailNode(ctx, id, iterationRow(rows, "worker", 0).ID, workflow.Failure{Kind: workflow.FailureSession, Message: "worker lost"}); err != nil {
			t.Fatal(err)
		}
		zero := 0
		wait, payload := containerWait(t, f, id, "", "worker", &zero)
		f.exec.finish(outer, execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
		run, rows := f.await(t, id, terminal)
		if run.Status != store.WorkflowStatusFailed {
			t.Fatalf("run = %s", run.Status)
		}
		abandoned(t, iterationRow(rows, "worker", 0), wait.ID)
		wakeContainerRetry(t, f, clock, id, payload, wait)
		if starts(f) != 2 {
			t.Fatalf("an abandoned Iteration wait started a session: %d", starts(f))
		}
	})
}
