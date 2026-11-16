package workflowhost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

const containerRetryDisabled = `"retry":{"enabled":false,"maxRetries":2,"initialDelaySeconds":10}`

func retryContainer(graph string) string {
	return strings.Replace(graph, containerRetryDisabled, `"retry":{"enabled":true,"maxRetries":1,"initialDelaySeconds":10}`, 1)
}

func iterationRow(rows []store.WorkflowNodeRun, id string, round int) store.WorkflowNodeRun {
	for _, row := range rows {
		if row.NodeID == id && row.Iteration != nil && *row.Iteration == round {
			return row
		}
	}
	return store.WorkflowNodeRun{}
}

func containerWait(t *testing.T, f *fixture, runID, scopeID, nodeID string, iteration *int) (store.WorkflowNodeRun, waitPayload) {
	t.Helper()
	read := func() store.WorkflowNodeRun {
		if scopeID == "" {
			rows, err := f.s.WorkflowEngineNodeRuns(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if iteration == nil {
				return node(rows, nodeID)
			}
			return iterationRow(rows, nodeID, *iteration)
		}
		rows, err := f.s.WorkflowEngineScopeNodes(context.Background(), runID, scopeID)
		if err != nil {
			t.Fatal(err)
		}
		return node(rows, nodeID)
	}
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool {
		row := read()
		return row.ID != "" && row.Status == store.WorkflowStatusRunning && row.StartedAt == nil
	})
	row := read()
	var payload waitPayload
	if err := json.Unmarshal(row.Payload, &payload); err != nil || payload.Wait == nil {
		t.Fatalf("wait payload %s: %v", row.Payload, err)
	}
	if row.SessionID != nil {
		t.Fatal("a container retry wait inherited its failed session")
	}
	return row, payload
}

func wakeContainerRetry(t *testing.T, f *fixture, clock interface{ Store(int64) }, runID string, payload waitPayload, row store.WorkflowNodeRun) {
	t.Helper()
	clock.Store(payload.Wait.Due)
	if err := f.host.engine.WakeRetry(context.Background(), runID, row.ID); err != nil {
		t.Fatal(err)
	}
}

func autoRetryMarker(t *testing.T, row store.WorkflowNodeRun) (retry int) {
	t.Helper()
	var payload struct {
		Auto struct {
			Retry int `json:"retry"`
		} `json:"auto_retry"`
	}
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Auto.Retry
}

func TestLoopRetriesInEachRoundWithFreshBudgetAndAttemptNumbers(t *testing.T) {
	f := newFixture(t)
	clock := retryClock(t, f)
	ctx := context.Background()
	id := f.run(t, retryContainer(loopGraph), nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	sessions := []string{}
	for round, answer := range []string{"draft", "done"} {
		first := f.exec.awaitStart(t)
		sessions = append(sessions, first)
		wantPrompt := []string{"Improve seed", "Improve draft"}[round]
		if text := promptText(t, f.exec.request(first).Prompt); !strings.Contains(text, wantPrompt) {
			t.Fatalf("round %d prompt %s, want %s", round+1, text, wantPrompt)
		}
		f.exec.finish(first, execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
		rounds, err := f.s.WorkflowEngineLoopRounds(ctx, id)
		if err != nil || len(rounds) != round+1 {
			t.Fatalf("rounds %+v: %v", rounds, err)
		}
		wait, payload := containerWait(t, f, id, rounds[round].ID, "writer", nil)
		if payload.Wait.Attempt != 2 || payload.Wait.Retry != 1 || autoRetryMarker(t, wait) != 1 {
			t.Fatalf("round %d did not start a fresh retry budget: %+v %s", round+1, payload.Wait, wait.Payload)
		}
		wakeContainerRetry(t, f, clock, id, payload, wait)
		second := f.exec.awaitStart(t)
		sessions = append(sessions, second)
		if second == first {
			t.Fatal("Loop retry reused its session")
		}
		if text := promptText(t, f.exec.request(second).Prompt); !strings.Contains(text, wantPrompt) {
			t.Fatalf("round %d retry left its scope pool: %s", round+1, text)
		}
		f.exec.finish(second, completed(answer))
	}
	run, _ := f.await(t, id, terminal)
	if run.Status != store.WorkflowStatusSucceeded || run.Output == nil || *run.Output != `{"result":"done"}` {
		t.Fatalf("run %+v", run)
	}
	rounds, err := f.s.WorkflowEngineLoopRounds(ctx, id)
	if err != nil || len(rounds) != 2 {
		t.Fatalf("rounds %+v: %v", rounds, err)
	}
	history, err := f.s.WorkflowRunFailedAttempts(ctx, f.scope.TenantID, id)
	if err != nil || len(history) != 2 {
		t.Fatalf("history %+v: %v", history, err)
	}
	for i, failed := range history {
		var payload waitPayload
		if err := json.Unmarshal(failed.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if failed.ScopeID != rounds[i].ID || payload.Detail.Attempt != 1 || failed.SessionID == nil || *failed.SessionID != sessions[i*2] {
			t.Fatalf("round %d history %+v %s", i+1, failed, failed.Payload)
		}
	}
	if sessions[0] == sessions[2] || sessions[1] == sessions[3] {
		t.Fatal("sessions leaked across Loop rounds")
	}
}

func TestIterationRetryCollectsOnlyTheFinalAttempt(t *testing.T) {
	f := newFixture(t)
	clock := retryClock(t, f)
	ctx := context.Background()
	id := f.run(t, retryContainer(iterationGraph("continue", []string{"one"}, 50)), nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	first := f.exec.awaitStart(t)
	refused := completed("stale")
	refused.StopReason = "refusal"
	f.exec.finish(first, refused)
	zero := 0
	wait, payload := containerWait(t, f, id, "", "worker", &zero)
	wakeContainerRetry(t, f, clock, id, payload, wait)
	second := f.exec.awaitStart(t)
	if second == first {
		t.Fatal("Iteration retry reused its session")
	}
	if text := promptText(t, f.exec.request(second).Prompt); !strings.Contains(text, "item 0: one") {
		t.Fatalf("retry used another round's input: %s", text)
	}
	f.exec.finish(second, completed("final"))
	run, rows := f.await(t, id, terminal)
	if run.Status != store.WorkflowStatusSucceeded || run.Output == nil || *run.Output != `{"failed":0,"items":["final"]}` {
		t.Fatalf("run %+v", run)
	}
	history, err := f.s.WorkflowRunFailedAttempts(ctx, f.scope.TenantID, id)
	if err != nil || len(history) != 1 || history[0].Output == nil || *history[0].Output != "stale" {
		t.Fatalf("history %+v: %v", history, err)
	}
	if final := iterationRow(rows, "worker", 0); final.Output == nil || *final.Output != "final" || autoRetryMarker(t, final) != 1 {
		t.Fatalf("final attempt %+v %s", final, final.Payload)
	}
}

func TestIterationExhaustionAppliesItsErrorStrategy(t *testing.T) {
	for _, strategy := range []string{"continue", "fail"} {
		t.Run(strategy, func(t *testing.T) {
			f := newFixture(t)
			clock := retryClock(t, f)
			ctx := context.Background()
			id := f.run(t, retryContainer(iterationGraph(strategy, []string{"bad", "next"}, 50)), nil)
			if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
				t.Fatal(err)
			}
			first := f.exec.awaitStart(t)
			refused := completed("stale")
			refused.StopReason = "refusal"
			f.exec.finish(first, refused)
			zero := 0
			wait, payload := containerWait(t, f, id, "", "worker", &zero)
			wakeContainerRetry(t, f, clock, id, payload, wait)
			second := f.exec.awaitStart(t)
			finalRefusal := completed("final bad")
			finalRefusal.StopReason = "refusal"
			f.exec.finish(second, finalRefusal)
			if strategy == "continue" {
				next := f.exec.awaitStart(t)
				if text := promptText(t, f.exec.request(next).Prompt); !strings.Contains(text, "item 1: next") {
					t.Fatalf("next round prompt %s", text)
				}
				f.exec.finish(next, completed("good"))
			}
			run, rows := f.await(t, id, terminal)
			if strategy == "continue" {
				if run.Status != store.WorkflowStatusSucceeded || run.Output == nil || *run.Output != `{"failed":1,"items":["good"]}` {
					t.Fatalf("continue run %+v", run)
				}
			} else if run.Status != store.WorkflowStatusFailed || node(rows, "iter").Status != store.WorkflowStatusFailed || iterationRow(rows, "worker", 1).ID != "" {
				t.Fatalf("fail strategy run %+v rows %+v", run, rows)
			}
			final := iterationRow(rows, "worker", 0)
			var detail waitPayload
			if err := json.Unmarshal(final.Payload, &detail); err != nil || detail.Detail.Attempt != 2 {
				t.Fatalf("exhausted attempt %+v %s", final, final.Payload)
			}
			history, err := f.s.WorkflowRunFailedAttempts(ctx, f.scope.TenantID, id)
			if err != nil || len(history) != 1 {
				t.Fatalf("history %+v: %v", history, err)
			}
			p, _, err := workflow.DecodeRunPayload(run.RuntimePayload)
			if err != nil {
				t.Fatal(err)
			}
			if strategy == "continue" && (p.IterationLedger["iter"][0].Output != nil || p.IterationLedger["iter"][0].Status != store.WorkflowStatusFailed) {
				t.Fatalf("failed attempts leaked into the ledger: %+v", p.IterationLedger["iter"][0])
			}
		})
	}
}

func TestInteractiveLoopMemberNeverRetries(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	graph := strings.Replace(retryContainer(loopGraph), `"prompt":"Improve {{#loop.draft#}}"`, `"prompt":"Improve {{#loop.draft#}}","interactive":true`, 1)
	id := f.run(t, graph, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(f.exec.awaitStart(t), execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	run, _ := f.await(t, id, terminal)
	if run.Status != store.WorkflowStatusFailed || starts(f) != 1 {
		t.Fatalf("interactive run %+v, %d sessions", run, starts(f))
	}
	history, err := f.s.WorkflowRunFailedAttempts(ctx, f.scope.TenantID, id)
	if err != nil || len(history) != 0 {
		t.Fatalf("interactive member left retry history %+v: %v", history, err)
	}
}

func TestCancelSettlesLoopRetryWait(t *testing.T) {
	f := newFixture(t)
	clock := retryClock(t, f)
	ctx := context.Background()
	id := f.run(t, retryContainer(loopGraph), nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(f.exec.awaitStart(t), execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	rounds, _ := f.s.WorkflowEngineLoopRounds(ctx, id)
	wait, payload := containerWait(t, f, id, rounds[0].ID, "writer", nil)
	if _, err := f.host.CancelRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	wakeContainerRetry(t, f, clock, id, payload, wait)
	run, _ := f.await(t, id, terminal)
	rows, err := f.s.WorkflowEngineScopeNodes(ctx, id, rounds[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	wait = node(rows, "writer")
	if run.Status != store.WorkflowStatusCancelled || wait.Status != store.WorkflowStatusCancelled || strings.Contains(string(wait.Payload), "retry_wait") || starts(f) != 1 {
		t.Fatalf("cancel run %s wait %+v sessions %d", run.Status, wait, starts(f))
	}
}

func TestLoopSiblingFailureAbandonsWaitingRetry(t *testing.T) {
	f := newFixture(t)
	clock := retryClock(t, f)
	ctx := context.Background()
	graph := strings.Replace(loopGraph, `{"id":"out"`, `{"id":"sibling","data":{"kind":"agent","containerId":"loop","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"sibling",`+containerRetryDisabled+`}}},{"id":"out"`, 1)
	graph = strings.Replace(retryContainer(graph), `{"source":"entry","target":"writer"}`, `{"source":"entry","target":"writer"},{"source":"entry","target":"sibling"}`, 1)
	id := f.run(t, graph, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(f.exec.awaitStart(t), execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	rounds, _ := f.s.WorkflowEngineLoopRounds(ctx, id)
	wait, payload := containerWait(t, f, id, rounds[0].ID, "writer", nil)
	siblingSession := f.exec.awaitStart(t)
	if f.exec.request(siblingSession).Caller.NodeID != "sibling" {
		t.Fatalf("started %s, want sibling", f.exec.request(siblingSession).Caller.NodeID)
	}
	if err := f.host.engine.FailNode(ctx, id, f.exec.request(siblingSession).Caller.AttemptID, workflow.Failure{Kind: workflow.FailureRepository, Message: "sibling broke"}); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(siblingSession, completed("late"))
	run, _ := f.await(t, id, terminal)
	rows, err := f.s.WorkflowEngineScopeNodes(ctx, id, rounds[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.WorkflowStatusFailed {
		t.Fatalf("run %s", run.Status)
	}
	abandoned(t, node(rows, "writer"), wait.ID)
	wakeContainerRetry(t, f, clock, id, payload, wait)
	if starts(f) != 2 {
		t.Fatalf("abandoned Loop wait started another session: %d", starts(f))
	}
}

func TestRestartInterruptsLoopRetryWait(t *testing.T) {
	f := newFixture(t)
	clock := retryClock(t, f)
	ctx := context.Background()
	id := f.run(t, retryContainer(loopGraph), nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(f.exec.awaitStart(t), execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	rounds, _ := f.s.WorkflowEngineLoopRounds(ctx, id)
	wait, payload := containerWait(t, f, id, rounds[0].ID, "writer", nil)
	f.host.Close()
	f.host = f.newHost(t)
	if err := f.host.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	wakeContainerRetry(t, f, clock, id, payload, wait)
	run, _ := f.await(t, id, terminal)
	rows, err := f.s.WorkflowEngineScopeNodes(ctx, id, rounds[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	wait = node(rows, "writer")
	if run.Status != store.WorkflowStatusFailed || errorKind(t, wait) != "interrupted_by_restart" || strings.Contains(string(wait.Payload), "retry_wait") || starts(f) != 1 {
		t.Fatalf("restart run %s wait %+v sessions %d", run.Status, wait, starts(f))
	}
}

func TestLoopExitCancelsWaitingRetryMarker(t *testing.T) {
	f := newFixture(t)
	clock := retryClock(t, f)
	ctx := context.Background()
	// writer precedes exit, so exit becomes ready only after writer's retry is waiting.
	graph := strings.Replace(retryContainer(exitGraph()), `{"source":"entry","target":"writer"}`, `{"source":"entry","target":"writer"},{"source":"writer","target":"exit"}`, 1)
	id := f.run(t, graph, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(f.exec.awaitStart(t), execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	rounds, err := f.s.WorkflowEngineLoopRounds(ctx, id)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("rounds %+v: %v", rounds, err)
	}
	wait, payload := containerWait(t, f, id, rounds[0].ID, "writer", nil)
	// The committed break takes precedence while the sibling waits for its retry.
	if _, err := f.s.StartWorkflowEngineNodes(ctx, id, []store.WorkflowEngineStart{{NodeID: "exit", NodeType: "loopExit", ScopeID: rounds[0].ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.host.engine.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	run, _ := f.await(t, id, terminal)
	if run.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("run %s %v", run.Status, run.Error)
	}
	rows, err := f.s.WorkflowEngineScopeNodes(ctx, id, rounds[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	wait = node(rows, "writer")
	if wait.Status != store.WorkflowStatusCancelled || strings.Contains(string(wait.Payload), "retry_wait") {
		t.Fatalf("LoopExit left a due-looking wait: %+v %s", wait, wait.Payload)
	}
	wakeContainerRetry(t, f, clock, id, payload, wait)
	if starts := starts(f); starts != 1 {
		t.Fatalf("a cancelled LoopExit wait started %d sessions", starts)
	}
}

func TestRestartSettlesIterationRetryWaitByErrorStrategy(t *testing.T) {
	for _, strategy := range []string{"continue", "fail"} {
		t.Run(strategy, func(t *testing.T) {
			f := newFixture(t)
			clock := retryClock(t, f)
			ctx := context.Background()
			id := f.run(t, retryContainer(iterationGraph(strategy, []string{"bad", "next"}, 50)), nil)
			if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
				t.Fatal(err)
			}
			f.exec.finish(f.exec.awaitStart(t), execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
			zero := 0
			wait, payload := containerWait(t, f, id, "", "worker", &zero)
			f.host.Close()
			f.host = f.newHost(t)
			if err := f.host.Interrupt(ctx); err != nil {
				t.Fatal(err)
			}
			if strategy == "continue" {
				next := f.exec.awaitStart(t)
				if text := promptText(t, f.exec.request(next).Prompt); !strings.Contains(text, "item 1: next") {
					t.Fatalf("recovered next round prompt %s", text)
				}
				f.exec.finish(next, completed("good"))
			}
			wakeContainerRetry(t, f, clock, id, payload, wait)
			run, rows := f.await(t, id, terminal)
			wait = iterationRow(rows, "worker", 0)
			if errorKind(t, wait) != "interrupted_by_restart" || strings.Contains(string(wait.Payload), "retry_wait") {
				t.Fatalf("interrupted wait %+v %s", wait, wait.Payload)
			}
			if strategy == "continue" {
				if run.Status != store.WorkflowStatusSucceeded || run.Output == nil || *run.Output != `{"failed":1,"items":["good"]}` {
					t.Fatalf("recovered continue run %+v", run)
				}
				p, _, err := workflow.DecodeRunPayload(run.RuntimePayload)
				if err != nil || p.IterationLedger["iter"][0].Status != store.WorkflowStatusFailed {
					t.Fatalf("interrupted round ledger %+v: %v", p.IterationLedger["iter"], err)
				}
			} else if run.Status != store.WorkflowStatusFailed || node(rows, "iter").Status != store.WorkflowStatusFailed || iterationRow(rows, "worker", 1).ID != "" {
				t.Fatalf("recovered fail run %+v rows %+v", run, rows)
			}
		})
	}
}

func TestIterationRetryAttemptNumbersArePerRound(t *testing.T) {
	f := newFixture(t)
	clock := retryClock(t, f)
	ctx := context.Background()
	id := f.run(t, retryContainer(iterationGraph("continue", []string{"first", "second"}, 50)), nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	for round := range 2 {
		first := f.exec.awaitStart(t)
		f.exec.finish(first, execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
		wait, payload := containerWait(t, f, id, "", "worker", &round)
		if payload.Wait.Attempt != 2 {
			t.Fatalf("round %d wait attempt %d, want 2", round, payload.Wait.Attempt)
		}
		wakeContainerRetry(t, f, clock, id, payload, wait)
		f.exec.finish(f.exec.awaitStart(t), completed(fmt.Sprintf("done %d", round)))
	}
	run, _ := f.await(t, id, terminal)
	if run.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("run %s", run.Status)
	}
	history, err := f.s.WorkflowRunFailedAttempts(ctx, f.scope.TenantID, id)
	if err != nil || len(history) != 2 {
		t.Fatalf("history %+v: %v", history, err)
	}
	for round, failed := range history {
		var payload waitPayload
		if err := json.Unmarshal(failed.Payload, &payload); err != nil || failed.Iteration == nil || *failed.Iteration != round || payload.Detail.Attempt != 1 {
			t.Fatalf("round %d history %+v %s", round, failed, failed.Payload)
		}
	}
}

// L3 (merge review): a failure through another branch settles a container wait in PostgreSQL but
// leaves its in-process timer armed; the resume must cancel it (retryTimers.forget, G2), as the
// README promises, or the abandoned wait's goroutine lives until its deadline.
func TestResumeCancelsTheTimerOfAWaitASiblingFailureAbandoned(t *testing.T) {
	f := newFixture(t)
	retryClock(t, f)
	ctx := context.Background()
	graph := strings.Replace(loopGraph, `{"id":"out"`, `{"id":"sibling","data":{"kind":"agent","containerId":"loop","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"sibling",`+containerRetryDisabled+`}}},{"id":"out"`, 1)
	graph = strings.Replace(retryContainer(graph), `{"source":"entry","target":"writer"}`, `{"source":"entry","target":"writer"},{"source":"entry","target":"sibling"}`, 1)
	id := f.run(t, graph, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(f.exec.awaitStart(t), sessionFailed)
	rounds, err := f.s.WorkflowEngineLoopRounds(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	containerWait(t, f, id, rounds[0].ID, "writer", nil)
	sibling := f.exec.awaitStart(t)
	if err := f.host.engine.FailNode(ctx, id, f.exec.request(sibling).Caller.AttemptID, workflow.Failure{Kind: workflow.FailureRepository, Message: "sibling broke"}); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(sibling, completed("late"))
	f.await(t, id, terminal)
	awaitIdle(t, f, id)
	if fenced, armed := f.host.engine.HeldRetries(id); fenced || armed != 1 {
		t.Fatalf("setup: the failed run holds fenced=%v armed=%d, want the abandoned wait's timer armed", fenced, armed)
	}
	if _, err := f.host.ResumeRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	if fenced, armed := f.host.engine.HeldRetries(id); fenced || armed != 0 {
		t.Fatalf("after the resume: fenced=%v armed=%d, want no stale timer left", fenced, armed)
	}
}
