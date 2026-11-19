package workflowhost

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/executionruntime"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// refusingDriver stands in for the Agent driver behind the real execution service. The first turn
// answers 初稿一 and a follow-up answers with its prompt, and while refusal is set a follow-up ends as the driver ends a turn it
// refused or lost (agentruntime's driver_undelivered_test.go pins what it reports). Each turn
// records a checkout for its workspace, so a session kept across a restart can start a new Agent.
type refusingDriver struct {
	db  *store.Store
	dir string

	mu sync.Mutex
	// refusal, when set, is how the next follow-ups end; err is returned with it, unpublished,
	// like a refusal the driver reports without Node.
	refusal *driverRefusal
	// got lists the prompts an Agent received.
	got []string
}

type driverRefusal struct {
	result execution.Result
	err    error
}

func (d *refusingDriver) Run(ctx context.Context, _ execution.Scope, session execution.Session, turn execution.Turn, emit func(context.Context, execution.Event) error) (execution.Result, error) {
	if err := d.db.RecordWorkspace(ctx, session.WorkspaceID, store.WorkspaceLocation{Path: d.dir, BaseCommit: "fixed", CloneExecution: "c-held"}); err != nil {
		return execution.Result{}, err
	}
	text, _ := turn.Prompt.Text()
	d.mu.Lock()
	refusal := d.refusal
	if !strings.Contains(turn.Caller.AttemptID, "/turn-") {
		refusal = nil // the workflow's own first turn always reaches the Agent
	}
	if refusal == nil || !refusal.result.NotDelivered {
		d.got = append(d.got, text)
	}
	d.mu.Unlock()
	if refusal != nil {
		result := refusal.result
		if refusal.err != nil {
			return result, refusal.err
		}
		return result, emit(ctx, execution.Event{Kind: execution.EventTurnEnded, Result: &result})
	}
	result := completed("初稿一")
	if strings.Contains(turn.Caller.AttemptID, "/turn-") {
		result = completed("答复：" + text)
	}
	return result, emit(ctx, execution.Event{Kind: execution.EventTurnEnded, Result: &result})
}

func (*refusingDriver) End(context.Context, string) error { return nil }
func (*refusingDriver) Rebuild(string)                    {}
func (*refusingDriver) Reattach(string, string, string)   {}
func (*refusingDriver) Close(context.Context) error       { return nil }

func (d *refusingDriver) refuse(r *driverRefusal) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.refusal = r
}

func (d *refusingDriver) prompts() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.got...)
}

// executionWorkflow runs the fixture's host on the real execution service over driver. restart
// replaces both as a new process would, over the same store.
func executionWorkflow(t *testing.T) (f *fixture, driver *refusingDriver, restart func()) {
	t.Helper()
	f = newFixture(t)
	f.host.Close()
	driver = &refusingDriver{db: f.s, dir: t.TempDir()}
	var runtime *executionruntime.Service
	start := func() {
		var err error
		runtime, err = executionruntime.New(t.Context(), executionruntime.Config{Store: f.s, Models: executionruntime.WithFixtureModels(nil), Agent: driver})
		if err != nil {
			t.Fatal(err)
		}
		if f.host, err = NewHost(Config{Store: f.s, Execution: runtime, Catalog: runtime}); err != nil {
			t.Fatal(err)
		}
	}
	stop := func() {
		f.host.Close()
		if err := runtime.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}
	start()
	t.Cleanup(func() { stop() })
	return f, driver, func() {
		stop()
		start()
		if err := f.host.Interrupt(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

// startParkedOn starts interactiveChain on the real execution service and waits for A to park
// with its first answer.
func (f *fixture) startParkedOn(t *testing.T) (runID, session string) {
	t.Helper()
	runID = f.run(t, interactiveChain, str("支付重构"))
	if _, err := f.host.StartRun(t.Context(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	_, nodes := f.await(t, runID, awaitingInput("a"))
	if p := interactivePayload(t, node(nodes, "a")); p["turns"] != float64(1) || p["answer"] != "初稿一" {
		t.Fatalf("parked A = %v", p)
	}
	return runID, *node(nodes, "a").SessionID
}

// undelivered waits for A to park again saying its last message was not delivered.
func undelivered(t *testing.T) func(store.WorkflowRun, []store.WorkflowNodeRun) bool {
	return func(run store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return awaitingInput("a")(run, n) && interactivePayload(t, node(n, "a"))["last_turn_error"] == turnErrorNotDelivered
	}
}

// A follow-up the execution service accepts (Append succeeds and records the turn) and the Agent
// driver then refuses before the Agent gets it is no turn: the node keeps the turn count and the
// answer of its last delivered turn and says the message was not delivered. When the refusal
// ended the session, the summary says that too and no message can follow; otherwise the starter
// sends it again in the same session.
func TestFollowUpRefusedByTheDriverIsNotDelivered(t *testing.T) {
	for name, tc := range map[string]struct {
		refusal driverRefusal
		ended   bool
	}{
		// begin found the session ended (Node ended it while the node waited, or it broke).
		"session already ended in the driver": {refusal: driverRefusal{
			result: execution.Result{Status: execution.StatusFailed, FailureCode: "agent_session_unavailable", NotDelivered: true}, err: execution.ErrSessionEnded}, ended: true},
		// Node answered submit_user_turn with session_command_rejected(session_ended).
		"Node refused the command": {refusal: driverRefusal{
			result: execution.Result{Status: execution.StatusFailed, FailureCode: "agent_session_ended", NotDelivered: true}, err: errors.New("agent_session_ended")}, ended: true},
		// Node refused the command for an MCP generation that never came; the session stays.
		"MCP refresh never came": {refusal: driverRefusal{
			result: execution.Result{Status: execution.StatusFailed, FailureCode: "mcp_refresh_wait_timeout", NotDelivered: true}}},
	} {
		t.Run(name, func(t *testing.T) {
			f, driver, _ := executionWorkflow(t)
			ctx := t.Context()
			runID, session := f.startParkedOn(t)
			before := interactivePayload(t, f.nodeRow(t, runID, "a"))
			driver.refuse(&tc.refusal)
			if accepted, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "改一下"); err != nil || accepted.Sent != 1 {
				t.Fatalf("the follow-up = %+v %v, want it accepted as message 1", accepted.Sent, err)
			}
			_, nodes := f.await(t, runID, undelivered(t))
			// The execution service accepted the message: its turn is recorded, with the
			// driver's verdict that the Agent never got it.
			turns, err := f.s.ReadExecutionTurns(ctx, executionScope(f.scope), session, "", 10)
			if err != nil || len(turns.Items) != 2 || turns.Items[1].Result == nil || !turns.Items[1].Result.NotDelivered {
				t.Fatalf("recorded turns = %+v %v, want the follow-up recorded as not delivered", turns.Items, err)
			}
			// Apart from the notice, the message number and whether the session ended, the node
			// is exactly as its first turn left it: the same turn count, answer and completion.
			p := interactivePayload(t, node(nodes, "a"))
			want := maps.Clone(before)
			want["sent"], want["last_turn_error"], want["session_ended"] = float64(1), turnErrorNotDelivered, tc.ended
			if !reflect.DeepEqual(p, want) {
				t.Fatalf("summary = %v\nwant %v", p, want)
			}
			if got := driver.prompts(); len(got) != 1 {
				t.Fatalf("an Agent got %q, want only the first turn", got)
			}
			f.awaitClaimable(t, runID, "a")
			driver.refuse(nil)
			if tc.ended {
				if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "改一下"); !errors.Is(err, server.ErrWorkflowNodeSessionEnded) {
					t.Fatalf("a message after the session ended = %v, want %v", err, server.ErrWorkflowNodeSessionEnded)
				}
				if s, err := f.s.ReadExecutionSession(ctx, executionScope(f.scope), session); err != nil || s.EndedAt == nil {
					t.Fatalf("session = %+v %v, want it ended", s, err)
				}
				// The earlier answer still completes the node.
				if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", confirm); err != nil {
					t.Fatalf("completing with the earlier answer: %v", err)
				}
				return
			}
			// The same session takes the message again: it is message 2, the next number.
			if accepted, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "改一下"); err != nil || accepted.Sent != 2 {
				t.Fatalf("the resent message = %+v %v, want message 2", accepted.Sent, err)
			}
			_, nodes = f.await(t, runID, func(run store.WorkflowRun, n []store.WorkflowNodeRun) bool {
				return awaitingInput("a")(run, n) && interactivePayload(t, node(n, "a"))["turns"] == float64(2)
			})
			if p := interactivePayload(t, node(nodes, "a")); p["last_turn_error"] != nil || p["sent"] != float64(2) || p["answer"] != "答复：改一下" || p["session_ended"] != false {
				t.Fatalf("summary after the delivered resend = %v", p)
			}
		})
	}
}

// A follow-up that may have reached the Agent before its session was lost still counts as a
// turn, as before: the node parks with the session ended and no undelivered notice.
func TestFollowUpLostAfterItsAgentGotItStillCounts(t *testing.T) {
	f, driver, _ := executionWorkflow(t)
	ctx := t.Context()
	runID, _ := f.startParkedOn(t)
	driver.refuse(&driverRefusal{result: execution.Result{Status: execution.StatusFailed, FailureCode: "agent_ended_before_turn", AgentLost: true}, err: execution.ErrSessionEnded})
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "改一下"); err != nil {
		t.Fatal(err)
	}
	_, nodes := f.await(t, runID, func(run store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return awaitingInput("a")(run, n) && interactivePayload(t, node(n, "a"))["turns"] == float64(2)
	})
	if p := interactivePayload(t, node(nodes, "a")); p["session_ended"] != true || p["last_turn_error"] != nil {
		t.Fatalf("summary = %v, want the lost turn counted with the session ended", p)
	}
}

// The undelivered message is recorded among the session's turns, so the node rebuilt after a
// restart must not count it, must keep the notice and must give the next message the next number.
func TestARestartDoesNotCountAFollowUpTheDriverRefused(t *testing.T) {
	f, driver, restart := executionWorkflow(t)
	ctx := t.Context()
	runID, session := f.startParkedOn(t)
	driver.refuse(&driverRefusal{result: execution.Result{Status: execution.StatusFailed, FailureCode: "mcp_refresh_wait_timeout", NotDelivered: true}})
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "改一下"); err != nil {
		t.Fatal(err)
	}
	f.await(t, runID, undelivered(t))
	f.awaitClaimable(t, runID, "a")
	driver.refuse(nil)
	restart()

	_, nodes := f.await(t, runID, undelivered(t))
	if p := interactivePayload(t, node(nodes, "a")); p["turns"] != float64(1) || p["sent"] != float64(1) || p["session_ended"] != false || p["answer"] != "初稿一" {
		t.Fatalf("summary after the restart = %v, want one turn and the notice about message 1", p)
	}
	if accepted, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再改一下"); err != nil || accepted.Sent != 2 {
		t.Fatalf("the next message = %+v %v, want message 2", accepted.Sent, err)
	}
	_, nodes = f.await(t, runID, func(run store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return awaitingInput("a")(run, n) && interactivePayload(t, node(n, "a"))["turns"] == float64(2)
	})
	if p := interactivePayload(t, node(nodes, "a")); p["last_turn_error"] != nil || p["sent"] != float64(2) || p["answer"] != "答复：再改一下" {
		t.Fatalf("summary after the message following the restart = %v", p)
	}
	if turns, err := f.s.ReadExecutionTurns(ctx, executionScope(f.scope), session, "", 10); err != nil || len(turns.Items) != 3 {
		t.Fatalf("recorded turns = %d %v, want the first turn, the undelivered message and the new one", len(turns.Items), err)
	}
}
