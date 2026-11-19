package workflowhost

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// recordedTurnsAt is recordedTurns with explicit follow-up numbers: seqs[i] numbers results[i],
// 0 for the node run's own first turn. A follow-up refused at Append left no turn, so the
// numbers may have gaps.
func (f *fakeExecution) recordedTurnsAt(session, nodeRun string, seqs []int, results ...execution.Result) {
	f.recordedTurns(session, nodeRun, results...)
	f.ReadTurnsFunc = func(_ context.Context, _ execution.Scope, id, _ string, _ int) (execution.TurnPage, error) {
		page := execution.TurnPage{}
		for i, r := range results {
			attempt := nodeRun
			if seqs[i] > 0 {
				attempt = nodeRun + "/turn-" + strconv.Itoa(seqs[i])
			}
			result := r
			page.Items = append(page.Items, execution.Turn{Caller: execution.Caller{Kind: execution.CallerWorkflow, AttemptID: attempt}, Result: &result})
		}
		return page, nil
	}
}

// rewritePayload changes a parked row's payload while no host runs, as a row written by an
// earlier build or left by a crash would read.
func (f *fixture) rewritePayload(t *testing.T, runID, nodeRunID string, change func(payload map[string]any)) {
	t.Helper()
	moved, err := f.s.MoveWorkflowEngineNode(context.Background(), runID, nodeRunID, store.WorkflowStatusPending, store.WorkflowStatusPending, func(b []byte) ([]byte, error) {
		var p map[string]any
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, err
		}
		change(p)
		return json.Marshal(p)
	})
	if err != nil || !moved {
		t.Fatalf("rewrite the payload of %s: %v %v", nodeRunID, moved, err)
	}
}

// withoutSent drops what a summary written before `sent` existed lacks.
func withoutSent(p map[string]any) {
	i := p["interactive"].(map[string]any)
	delete(i, "sent")
	delete(i, "last_turn_error")
}

// lastAppend is the caller attempt of the latest accepted follow-up.
func (f *fakeExecution) lastAppend() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.appended[len(f.appended)-1].Caller.AttemptID
}

// A summary without `sent` (written by an earlier build) leaves the follow-up numbering to the
// recorded turns. Follow-ups refused at Append consumed their numbers without leaving a turn, so
// the numbers have gaps: the next message is numbered after the highest recorded one, not after
// the count of turns, which would reuse a number a recorded turn already has.
func TestARecoveredNodeNumbersAfterTheHighestRecordedFollowUp(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	rowA := f.nodeRow(t, runID, "a")
	f.host.Close()
	f.rewritePayload(t, runID, rowA.ID, withoutSent)
	f.exec.recordedTurnsAt(a, rowA.ID, []int{0, 1, 3}, completed("初稿一"), completed("初稿二"), completed("初稿四"))
	f.host = f.newHost(t)
	if err := f.host.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	f.await(t, runID, awaitingInput("a"))
	if p := interactivePayload(t, f.nodeRow(t, runID, "a")); p["sent"] != float64(3) || p["turns"] != float64(3) {
		t.Fatalf("summary after the restart = %v, want three turns and message 3 as the latest", p)
	}
	if got, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再改"); err != nil || got.Sent != 4 {
		t.Fatalf("next message = %+v %v, want message 4", got.Sent, err)
	}
	f.exec.awaitAppend(t)
	if got := f.exec.lastAppend(); got != rowA.ID+"/turn-4" {
		t.Fatalf("follow-up attempt = %q, want %s/turn-4 after the recorded turn-3", got, rowA.ID)
	}
}

// Only a parked interactive node writes a session summary, so a pending row with a session but
// no `interactive` summary is not a wait the restart can continue: it fails with its run instead
// of being taken for one.
func TestAParkedRowWithoutASummaryFailsOnRestart(t *testing.T) {
	f := newFixture(t)
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	rowA := f.nodeRow(t, runID, "a")
	f.host.Close()
	f.rewritePayload(t, runID, rowA.ID, func(p map[string]any) { delete(p, "interactive") })
	f.exec.recordedTurns(a, rowA.ID, completed("初稿一"))
	f.host = f.newHost(t)
	if err := f.host.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusFailed || errorKind(t, node(nodes, "a")) != "interrupted_by_restart" {
		t.Fatalf("run %s node %+v, want a row without a summary failed by the restart", run.Status, node(nodes, "a"))
	}
}

// interactiveBesideIteration parks A while the Iteration's worker executes, so a restart absorbs
// the interrupted round and the next round starts a new session after the restart.
func interactiveBesideIteration() string {
	graph := strings.Replace(iterationGraph("continue", []string{"one", "two"}, 50), `{"id":"iter"`, `{"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"起草","interactive":true}}},
 {"id":"iter"`, 1)
	return strings.Replace(graph, `"edges":[`, `"edges":[{"source":"start","target":"a"},`, 1)
}

// A recovered node holds its session slot before its run resumes: the run's other work resumed
// by the restart (here the absorbed Iteration's next round) queues behind it under a limit of one
// session instead of starting beside it, one session over the Node's limit.
func TestARecoveredNodeHoldsItsSlotBeforeTheRunResumes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, interactiveBesideIteration(), nil)
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	sessions := f.exec.awaitStarts(t, 2)
	f.exec.finish(sessions["a"], completed("初稿"))
	_, nodes := f.await(t, runID, func(_ store.WorkflowRun, rows []store.WorkflowNodeRun) bool {
		return node(rows, "a").Status == store.WorkflowStatusPending && iterationRow(rows, "worker", 0).SessionID != nil
	})
	f.exec.recordedTurns(sessions["a"], node(nodes, "a").ID, completed("初稿"))

	f.host.Close()
	f.host = f.newHost(t)
	f.host.sessionLimit = 1
	// Were the slot counted only after the run resumed, the next round could start before the
	// node is rebuilt: hold the rebuild until that start, or a moment when it never comes.
	workerStarted := make(chan struct{})
	var startOnce, readOnce sync.Once
	f.exec.beforeStart = func(req execution.StartRequest) error {
		if req.Caller.NodeID == "worker" {
			startOnce.Do(func() { close(workerStarted) })
		}
		return nil
	}
	read := f.exec.ReadSessionFunc
	f.exec.ReadSessionFunc = func(ctx context.Context, scope execution.Scope, id string) (execution.Session, error) {
		readOnce.Do(func() {
			select {
			case <-workerStarted:
			case <-time.After(300 * time.Millisecond):
			}
		})
		return read(ctx, scope, id)
	}
	if err := f.host.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	f.await(t, runID, func(_ store.WorkflowRun, rows []store.WorkflowNodeRun) bool {
		return slotWait(t, iterationRow(rows, "worker", 1)) != nil
	})
	select {
	case <-workerStarted:
		t.Fatal("the next round started beside the recovered node over the session limit")
	default:
	}

	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", confirm); err != nil {
		t.Fatal(err)
	}
	second := f.exec.awaitStart(t)
	if f.exec.request(second).Caller.NodeID != "worker" {
		t.Fatalf("started %+v, want the queued round once the node gave its slot back", f.exec.request(second).Caller)
	}
	f.exec.finish(second, completed("two"))
	if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("run = %+v", run)
	}
}

// Execution recovery keeps the session of every node parked in a running run; when the sweep
// fails the run (an Agent was executing beside the parked node), the parked node's session has
// nothing left to talk to and is ended at startup.
func TestRestartEndsTheSessionsOfTheNodesItFails(t *testing.T) {
	f, admin := newFixtureWithAdmin(t)
	ctx := context.Background()
	runID := f.run(t, interactiveBeside, str("支付"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	sessions := f.exec.awaitStarts(t, 2)
	f.exec.finish(sessions["a"], completed("初稿"))
	_, nodes := f.await(t, runID, parked("a"))
	f.exec.recordedTurns(sessions["a"], node(nodes, "a").ID, completed("初稿"))
	// The fake execution service keeps no rows; the sweep finds the sessions it left open there.
	for _, id := range []string{sessions["a"], sessions["c"]} {
		if _, err := admin.Exec(ctx, `INSERT INTO v2_execution_sessions (id, tenant_id, user_id, data) VALUES ($1, $2, $3, '{}')`, id, f.scope.TenantID, f.scope.UserID); err != nil {
			t.Fatal(err)
		}
	}

	f.restart(t)
	if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusFailed {
		t.Fatalf("run = %+v, want it failed by the restart", run)
	}
	f.exec.mu.Lock()
	ended := slices.Clone(f.exec.ended)
	f.exec.mu.Unlock()
	if !slices.Contains(ended, sessions["a"]) {
		t.Fatalf("ended %v, want the session of the parked node the restart failed (%s)", ended, sessions["a"])
	}
}

// Message 1 is delivered and message 2 refused before the restart. The notice is about message 2:
// restarts keep it and `sent: 2` (the recorded turn-1 must not lower it), the resent message is
// 3, and once it is delivered a restart no longer shows the notice.
func TestARestartKeepsTheNoticeAboutTheLatestMessage(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	rowA := f.nodeRow(t, runID, "a")
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再改一版"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.exec.finish(a, completed("初稿二"))
	f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusPending && interactivePayload(t, node(n, "a"))["turns"] == float64(2)
	})
	f.awaitClaimable(t, runID, "a")
	accept := f.refuseAppendsOf(a, execution.ErrUnavailable)
	if got, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "第三条"); err != nil || got.Sent != 2 {
		t.Fatalf("message = %v %v, want 2", got.Sent, err)
	}
	f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusPending && interactivePayload(t, node(n, "a"))["last_turn_error"] == turnErrorNotDelivered
	})
	f.exec.recordedTurns(a, rowA.ID, completed("初稿一"), completed("初稿二"))
	for i := range 2 {
		f.restart(t)
		f.await(t, runID, awaitingInput("a"))
		if p := interactivePayload(t, f.nodeRow(t, runID, "a")); p["last_turn_error"] != turnErrorNotDelivered || p["sent"] != float64(2) || p["turns"] != float64(2) {
			t.Fatalf("restart %d summary = %v, want the notice about message 2 and two turns", i+1, p)
		}
	}
	accept()
	if got, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "第三条"); err != nil || got.Sent != 3 {
		t.Fatalf("resent = %v %v, want 3", got.Sent, err)
	}
	f.exec.awaitAppend(t)
	f.exec.finish(a, completed("初稿三"))
	f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusPending && interactivePayload(t, node(n, "a"))["turns"] == float64(3)
	})
	f.exec.recordedTurnsAt(a, rowA.ID, []int{0, 1, 3}, completed("初稿一"), completed("初稿二"), completed("初稿三"))
	f.restart(t)
	f.await(t, runID, awaitingInput("a"))
	if p := interactivePayload(t, f.nodeRow(t, runID, "a")); p["last_turn_error"] != nil || p["sent"] != float64(3) {
		t.Fatalf("after a delivered message and a restart summary = %v, want no notice and sent 3", p)
	}
}

// A summary written before `sent` existed (an upgrade from an earlier build) numbers the next
// message after the recorded follow-ups, and a normally recovered node has no notice.
func TestARestartNumbersAfterASummaryWithoutSent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	rowA := f.nodeRow(t, runID, "a")
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再改一版"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.exec.finish(a, completed("初稿二"))
	f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusPending && interactivePayload(t, node(n, "a"))["turns"] == float64(2)
	})
	f.host.Close()
	f.rewritePayload(t, runID, rowA.ID, withoutSent)
	f.exec.recordedTurns(a, rowA.ID, completed("初稿一"), completed("初稿二"))
	f.host = f.newHost(t)
	if err := f.host.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	f.await(t, runID, awaitingInput("a"))
	if p := interactivePayload(t, f.nodeRow(t, runID, "a")); p["sent"] != float64(1) || p["last_turn_error"] != nil {
		t.Fatalf("summary = %v, want sent 1 from the recorded follow-up and no notice", p)
	}
	if got, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "第三条"); err != nil || got.Sent != 2 {
		t.Fatalf("next = %v %v, want message 2", got.Sent, err)
	}
}
