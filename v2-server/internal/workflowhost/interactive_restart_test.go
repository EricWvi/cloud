package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/executionwait"
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

	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", confirm); err != nil {
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
	// A later turn lost the node's conversation: only a first turn is told a resume clones again.
	if msg := errorMessage(t, node(nodes, "a")); strings.Contains(msg, "fresh clone") {
		t.Fatalf("message %q, want the lost conversation rather than the first-turn hint", msg)
	}
}

// A message the execution service refused before the restart left no turn behind: the summary is
// its only record. Rewriting the summary on recovery keeps the notice and the message's number,
// so the page still offers it back and the next message does not reuse it.
func TestARestartKeepsTheUndeliveredNotice(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	_, nodes := f.await(t, runID, parked("a"))
	accept := f.refuseAppendsOf(a, execution.ErrUnavailable)
	if accepted, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "改一下"); err != nil || accepted.Sent != 1 {
		t.Fatalf("the turn = %+v %v, want message 1", accepted.Sent, err)
	}
	f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusPending && interactivePayload(t, node(n, "a"))["last_turn_error"] == turnErrorNotDelivered
	})
	f.exec.recordedTurns(a, node(nodes, "a").ID, completed("初稿一"))
	f.restart(t)

	f.await(t, runID, awaitingInput("a"))
	if p := interactivePayload(t, f.nodeRow(t, runID, "a")); p["last_turn_error"] != turnErrorNotDelivered || p["sent"] != float64(1) || p["answer"] != "初稿一" {
		t.Fatalf("summary after the restart = %v, want the notice about message 1 kept", p)
	}
	accept()
	if accepted, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "改一下"); err != nil || accepted.Sent != 2 {
		t.Fatalf("the resent message = %+v %v, want message 2", accepted.Sent, err)
	}
}

func TestUnstartedRecoveryKeepsTheNodeWaiting(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	_, nodes := f.await(t, runID, parked("a"))
	f.exec.recordedTurns(a, node(nodes, "a").ID, completed("初稿一"))
	f.restart(t)

	// The new Agent could not be started (Node was busy): nothing ran, so the message never
	// reached an Agent. It is no turn and is reported not delivered, as the execution service
	// marks it, and the starter can try again in the same session.
	f.awaitClaimable(t, runID, "a")
	before := blockerKinds(interactivePayload(t, f.nodeRow(t, runID, "a")))
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再改一版"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.exec.finish(a, execution.Result{Status: execution.StatusFailed, FailureCode: "agent_prepare_failed", NotStarted: true, NotDelivered: true})
	_, nodes = f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusPending && interactivePayload(t, node(n, "a"))["last_turn_error"] == turnErrorNotDelivered
	})
	if p := interactivePayload(t, node(nodes, "a")); p["turns"] != float64(1) || p["sent"] != float64(1) || p["session_ended"] != false || p["answer"] != "初稿一" || !reflect.DeepEqual(blockerKinds(p), before) {
		t.Fatalf("summary = %v, want the session open, one turn and the message reported not delivered", p)
	}
	f.awaitClaimable(t, runID, "a")
	if accepted, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再试一次"); err != nil || accepted.Sent != 2 {
		t.Fatalf("retry after an unstarted recovery = %+v %v, want message 2", accepted.Sent, err)
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

// errorMessage reads the failure message the engine recorded on a node run.
func errorMessage(t *testing.T, n store.WorkflowNodeRun) string {
	t.Helper()
	var p struct {
		ErrorDetail struct {
			Message string `json:"message"`
		} `json:"error_detail"`
	}
	if err := json.Unmarshal(n.Payload, &p); err != nil {
		t.Fatalf("payload %s: %v", n.Payload, err)
	}
	return p.ErrorDetail.Message
}

// A first turn on a reattached checkout Node no longer knows (the successor of a node completed
// after the restart) fails without a conversation to lose: its message says a resume starts on a
// fresh clone, not that a conversation cannot continue.
func TestAFirstTurnOnAnUnknownCheckoutSaysAResumeClonesAgain(t *testing.T) {
	f := newFixture(t)
	runID := f.run(t, interactiveChain, str("支付重构"))
	if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	f.exec.finish(a, execution.Result{Status: execution.StatusFailed, FailureCode: execution.FailureWorkspaceMissing, AgentLost: true})
	run, nodes := f.await(t, runID, terminal)
	got := node(nodes, "a")
	if run.Status != store.WorkflowStatusFailed || errorKind(t, got) != "interactive_session" || !strings.Contains(errorMessage(t, got), "fresh clone") {
		t.Fatalf("run %s node %s, want an interactive_session failure that points at a fresh clone", run.Status, got.Payload)
	}
}

// A node being completed at the restart had its session ended, but its row's summary still read
// the session as open. The recovery writes the summary again, so the page reads it as awaiting
// completion and offers no message box its session would refuse.
func TestARestartDuringCompletionReadsTheSessionAsEnded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	_, nodes := f.await(t, runID, parked("a"))
	if p := interactivePayload(t, node(nodes, "a")); p["session_ended"] != false {
		t.Fatalf("summary before the restart = %v, want the session open", p)
	}
	f.exec.recordedTurns(a, node(nodes, "a").ID, completed("初稿一"))
	// The completion's End was confirmed, then the process stopped before the output was written.
	ended := time.Now()
	f.exec.ReadSessionFunc = func(_ context.Context, _ execution.Scope, id string) (execution.Session, error) {
		if id != a {
			return execution.Session{}, execution.ErrNotFound
		}
		return execution.Session{ID: id, EndedAt: &ended}, nil
	}

	f.restart(t)
	_, nodes = f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusPending && interactivePayload(t, node(n, "a"))["session_ended"] == true
	})
	// The opening turn still awaits the starter's confirmation, which then completes it.
	if p := interactivePayload(t, node(nodes, "a")); p["completable"] != false || p["answer"] != "初稿一" ||
		!reflect.DeepEqual(blockerKinds(p), []string{blockerTurnUnconfirmed}) {
		t.Fatalf("summary after the restart = %v, want the earlier answer awaiting confirmation", p)
	}
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", confirm); err != nil {
		t.Fatalf("complete after the restart: %v", err)
	}
}

// failCatalogReads makes the next n catalog reads fail as the execution service does while it is
// unavailable, then serves the catalog again.
func (f *fixture) failCatalogReads(n int) {
	list := f.exec.ListExecutorsFunc
	var mu sync.Mutex
	f.exec.ListExecutorsFunc = func(ctx context.Context, scope execution.Scope) ([]execution.Executor, error) {
		mu.Lock()
		failing := n > 0
		n--
		mu.Unlock()
		if failing {
			return nil, execution.ErrUnavailable
		}
		return list(ctx, scope)
	}
}

// A catalog read that fails once while the restart rebuilds a parked node says nothing about the
// node's executor: the wait survives, and the starter's next message continues it.
func TestATransientCatalogErrorOnRestartKeepsTheWait(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	rowA := f.nodeRow(t, runID, "a")
	f.exec.recordedTurns(a, rowA.ID, completed("初稿一"))
	f.failCatalogReads(1)

	f.restart(t)
	f.await(t, runID, awaitingInput("a"))
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再改一版"); err != nil {
		t.Fatalf("follow-up after a transient catalog error: %v", err)
	}
	if id := f.exec.awaitAppend(t); id != a {
		t.Fatalf("follow-up went to session %s, want %s", id, a)
	}
	f.exec.finish(a, completed("初稿二"))
	if _, nodes := f.await(t, runID, parked("a")); interactivePayload(t, node(nodes, "a"))["answer"] != "初稿二" {
		t.Fatalf("summary = %v", interactivePayload(t, node(nodes, "a")))
	}
}

// A catalog that stays unreadable aborts startup rather than failing the wait: the row is left as
// it was, and the next start, with the catalog back, recovers the node.
func TestAnUnreadableCatalogOnRestartAbortsStartupAndKeepsTheWait(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	rowA := f.nodeRow(t, runID, "a")
	f.exec.recordedTurns(a, rowA.ID, completed("初稿一"))
	list := f.exec.ListExecutorsFunc
	f.exec.ListExecutorsFunc = func(context.Context, execution.Scope) ([]execution.Executor, error) {
		return nil, execution.ErrUnavailable
	}

	f.host.Close()
	f.host = f.newHost(t)
	f.host.resultRetry = executionwait.Policy{Budget: 100 * time.Millisecond, Interval: 10 * time.Millisecond}
	if err := f.host.Interrupt(ctx); !errors.Is(err, execution.ErrUnavailable) && !errors.Is(err, executionwait.ErrExhausted) {
		t.Fatalf("startup with an unreadable catalog = %v, want it aborted", err)
	}
	if row := f.nodeRow(t, runID, "a"); row.Status != store.WorkflowStatusPending || f.exec.isClosed(a) {
		t.Fatalf("node %+v closed %v, want the wait left for the next start", row, f.exec.isClosed(a))
	}

	f.exec.ListExecutorsFunc = list
	f.restart(t)
	f.await(t, runID, awaitingInput("a"))
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再改一版"); err != nil {
		t.Fatalf("follow-up once the catalog is back: %v", err)
	}
}
