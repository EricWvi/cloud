//go:build linux

package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/node"
	"github.com/wanglongan587/cloud/v2-server/internal/nodeservices"
)

// cancelCommand is the payload of a cancel_turn the driver sent.
type cancelCommand struct {
	NodeID    string `json:"node_id"`
	CommandID string `json:"command_id"`
	TurnID    string `json:"turn_id"`
}

// nextCancel waits for the driver's cancel_turn and checks it names turn on the session's
// execution with a command ID the settlement can echo.
func nextCancel(t *testing.T, f *fakeNodeIPC, start node.Message, turn string) cancelCommand {
	t.Helper()
	m := nextIPCCommand(t, f, "cancel_turn")
	var c cancelCommand
	if err := json.Unmarshal(m.Payload, &c); err != nil || m.ExecutionID != start.ExecutionID || c.NodeID != f.id.NodeID || c.CommandID == "" || c.TurnID != turn {
		t.Fatalf("cancel_turn = %+v on %s (err %v), want turn %s on %s", c, m.ExecutionID, err, turn, start.ExecutionID)
	}
	return c
}

// settleCancel sends Node's TurnCancelSettled for the command, as the execution's next event.
func (f *fakeNodeIPC) settleCancel(start node.Message, c cancelCommand, outcome string) {
	f.seq[start.ExecutionID]++
	sequence := f.seq[start.ExecutionID]
	payload, _ := json.Marshal(map[string]string{"command_id": c.CommandID, "turn_id": c.TurnID, "outcome": outcome})
	f.in <- node.Message{Type: "turn_cancel_settled", Version: 1, OperationID: start.OperationID, ExecutionID: start.ExecutionID, Sequence: &sequence, Payload: payload}
}

// startSpec decodes the AgentSessionSpec a start_agent_session carried.
func startSpec(t *testing.T, start node.Message) map[string]any {
	t.Helper()
	var started struct{ Spec map[string]any }
	if err := json.Unmarshal(start.Payload, &started); err != nil {
		t.Fatal(err)
	}
	return started.Spec
}

// activeGuard is the retry guard of the session's running turn.
func activeGuard(t *testing.T, h *ipcHarness, sid string) *retryGuard {
	t.Helper()
	h.driver.mu.Lock()
	a := h.driver.sessions[sid]
	h.driver.mu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active == nil {
		t.Fatal("no running turn")
	}
	return &a.active.guard
}

// cutOnceRetrying records what a stalled turn looks like to the guard: one model call cut at the
// idle limit, and the Agent's retry still running.
func cutOnceRetrying(g *retryGuard) {
	_, cut := g.begin()
	g.finish(cut, false, nodeservices.CutIdle)
	g.begin()
}

// turnEnds collects the TurnEnded publications of a run.
func turnEnds() (Emit, chan execution.Event) {
	events := make(chan execution.Event, 8)
	return func(_ context.Context, event execution.Event) error {
		if event.Kind == execution.EventTurnEnded {
			events <- event
		}
		return nil
	}, events
}

// A stop over a connection that negotiated turn_cancel cancels only the turn: the session chose
// keep_session_on_cancel (and nothing for failures), Node's TurnEnded waits for its settlement,
// a stopped settlement keeps the session, and the next turn goes to the same Agent. No
// EndSession is sent.
func TestDriverIPCStoppedTurnKeepsTheSessionForTheNextTurn(t *testing.T) {
	h := newIPCHarness(t, nil)
	h.turnCancel.Store(true)
	ctx := ipcContext(t)
	turnCtx, stop := context.WithCancelCause(ctx)
	session := h.session("session", "workspace")
	emit, events := turnEnds()
	done := startIPCRun(h, turnCtx, session, "first", emit)
	f := receiveIPC(t, h.created)
	start := nextIPCCommand(t, f, "start_agent_session")
	spec := startSpec(t, start)
	if _, failure := spec["keep_session_on_failure"]; spec["keep_session_on_cancel"] != true || failure {
		t.Fatalf("keep choices in the start spec = %v / %v, want only keep_session_on_cancel", spec["keep_session_on_cancel"], spec["keep_session_on_failure"])
	}
	stop(execution.ErrStopTurn)
	cancel := nextCancel(t, f, start, "first")
	f.finishTurnAs(t, start, "first", "partial", "cancelled")
	receiveIPC(t, f.acked)
	select {
	case event := <-events:
		t.Fatalf("the stopped turn was published before its settlement: %+v", event)
	case got := <-done:
		t.Fatalf("Run returned before the settlement: %+v", got)
	default:
	}
	f.settleCancel(start, cancel, "stopped")
	if event := receiveIPC(t, events); event.Ref.TurnID != "first" || event.Result == nil || event.Result.Status != execution.StatusCancelled || event.Result.Answer != "partial" {
		t.Fatalf("stopped turn publication = %+v", event)
	}
	if ack := receiveIPC(t, f.acked); ack.Type != "turn_cancel_settled" {
		t.Fatalf("acknowledged %s, want the settlement", ack.Type)
	}
	got := receiveIPC(t, done)
	if got.err != nil || got.result.Status != execution.StatusCancelled || !got.result.SessionKept || got.result.AgentLost || got.result.FailureCode != "" {
		t.Fatalf("stopped turn = %+v", got)
	}
	second := startIPCRun(h, ctx, session, "second", emit)
	submit := nextIPCCommand(t, f, "submit_user_turn")
	if submit.ExecutionID != start.ExecutionID {
		t.Fatal("the turn after a stop replaced the physical Agent")
	}
	f.finishTurn(t, submit, "second", "answer")
	if next := receiveIPC(t, second); next.err != nil || next.result.Status != execution.StatusCompleted || next.result.Answer != "answer" {
		t.Fatalf("turn after the stop = %+v", next)
	}
	if sent := sentIPC(f); slices.Contains(sent, "end_session") {
		t.Fatalf("a kept session was ended: %v", sent)
	}
}

// cloud#369: a turn whose model call was cut at the idle limit while the Agent's retry was still
// running fails with the cut when the turn ends cancelled on its own (Node records failures as
// cancelled), but stays cancelled, and its session kept, when the user stopped it.
func TestDriverIPCUserStopAfterACutStaysCancelled(t *testing.T) {
	h := newIPCHarness(t, nil)
	h.turnCancel.Store(true)
	ctx := ipcContext(t)
	turnCtx, stop := context.WithCancelCause(ctx)
	session := h.session("session", "workspace")
	emit, events := turnEnds()
	done := startIPCRun(h, turnCtx, session, "first", emit)
	f := receiveIPC(t, h.created)
	start := nextIPCCommand(t, f, "start_agent_session")
	cutOnceRetrying(activeGuard(t, h, session.ID))
	stop(execution.ErrStopTurn)
	cancel := nextCancel(t, f, start, "first")
	f.finishTurnAs(t, start, "first", "", "cancelled")
	f.settleCancel(start, cancel, "stopped")
	if event := receiveIPC(t, events); event.Result == nil || event.Result.Status != execution.StatusCancelled || event.Result.FailureCode != "" {
		t.Fatalf("a stop after a cut was published as %+v", event.Result)
	}
	got := receiveIPC(t, done)
	if got.err != nil || got.result.Status != execution.StatusCancelled || got.result.FailureCode != "" || !got.result.SessionKept {
		t.Fatalf("user stop after a cut = %+v", got)
	}

	second := startIPCRun(h, ctx, session, "second", emit)
	submit := nextIPCCommand(t, f, "submit_user_turn")
	cutOnceRetrying(activeGuard(t, h, session.ID))
	f.finishTurnAs(t, submit, "second", "", "cancelled")
	if event := receiveIPC(t, events); event.Result == nil || event.Result.Status != execution.StatusFailed || event.Result.FailureCode != nodeservices.CutIdle {
		t.Fatalf("a cut turn nobody stopped was published as %+v", event.Result)
	}
	if next := receiveIPC(t, second); next.err != nil || next.result.Status != execution.StatusFailed || next.result.FailureCode != nodeservices.CutIdle || next.result.SessionKept {
		t.Fatalf("cut turn without a stop = %+v", next)
	}
}

// A Node that will not keep the session settles the stop as session_ending: the turn is the
// user's stop (cut or not), the session is lost, and the driver sends no EndSession of its own;
// End returns once Node's terminal proves the stop.
func TestDriverIPCStopNodeCannotKeepEndsTheSession(t *testing.T) {
	h := newIPCHarness(t, nil)
	h.turnCancel.Store(true)
	ctx := ipcContext(t)
	turnCtx, stop := context.WithCancelCause(ctx)
	session := h.session("session", "workspace")
	emit, events := turnEnds()
	done := startIPCRun(h, turnCtx, session, "first", emit)
	f := receiveIPC(t, h.created)
	start := nextIPCCommand(t, f, "start_agent_session")
	cutOnceRetrying(activeGuard(t, h, session.ID))
	stop(execution.ErrStopTurn)
	cancel := nextCancel(t, f, start, "first")
	f.finishTurnAs(t, start, "first", "partial", "cancelled")
	f.settleCancel(start, cancel, "session_ending")
	if event := receiveIPC(t, events); event.Result == nil || event.Result.Status != execution.StatusCancelled || !event.Result.AgentLost || event.Result.FailureCode != "" {
		t.Fatalf("an unkept stop was published as %+v", event.Result)
	}
	got := receiveIPC(t, done)
	if !errors.Is(got.err, execution.ErrSessionEnded) || got.result.Status != execution.StatusCancelled || !got.result.AgentLost || got.result.SessionKept || got.result.Answer != "partial" {
		t.Fatalf("unkept stop = %+v", got)
	}
	ended := make(chan error, 1)
	go func() { ended <- h.driver.End(ctx, session.ID) }()
	select {
	case err := <-ended:
		t.Fatalf("End returned before Node's terminal: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	f.terminalAs(start, "agent_failed", "turn_boundary_unverified")
	if err := receiveIPC(t, ended); err != nil {
		t.Fatal(err)
	}
	if sent := sentIPC(f); slices.Contains(sent, "end_session") {
		t.Fatalf("the driver ended a session Node was already ending: %v", sent)
	}
}

// A turn Node dropped before its Agent received it has no TurnEnded: the driver records it as
// cancelled itself and the session takes the next turn.
func TestDriverIPCStopOfATurnNotStartedKeepsTheSession(t *testing.T) {
	h := newIPCHarness(t, nil)
	h.turnCancel.Store(true)
	ctx := ipcContext(t)
	turnCtx, stop := context.WithCancelCause(ctx)
	session := h.session("session", "workspace")
	emit, events := turnEnds()
	done := startIPCRun(h, turnCtx, session, "first", emit)
	f := receiveIPC(t, h.created)
	start := nextIPCCommand(t, f, "start_agent_session")
	stop(execution.ErrStopTurn)
	f.settleCancel(start, nextCancel(t, f, start, "first"), "not_started")
	if event := receiveIPC(t, events); event.Result == nil || event.Result.Status != execution.StatusCancelled {
		t.Fatalf("dropped turn publication = %+v", event)
	}
	if got := receiveIPC(t, done); got.err != nil || got.result.Status != execution.StatusCancelled || !got.result.SessionKept {
		t.Fatalf("dropped turn = %+v", got)
	}
	second := startIPCRun(h, ctx, session, "second", emit)
	f.finishTurn(t, nextIPCCommand(t, f, "submit_user_turn"), "second", "answer")
	if next := receiveIPC(t, second); next.err != nil || next.result.Status != execution.StatusCompleted {
		t.Fatalf("turn after a dropped turn = %+v", next)
	}
}

// A turn that ends on its own before the cancel takes effect keeps its own result; Node then
// settles the cancel as already_ended.
func TestDriverIPCStopRacingTheTurnsEndKeepsItsResult(t *testing.T) {
	h := newIPCHarness(t, nil)
	h.turnCancel.Store(true)
	ctx := ipcContext(t)
	turnCtx, stop := context.WithCancelCause(ctx)
	session := h.session("session", "workspace")
	emit, _ := turnEnds()
	done := startIPCRun(h, turnCtx, session, "first", emit)
	f := receiveIPC(t, h.created)
	start := nextIPCCommand(t, f, "start_agent_session")
	stop(execution.ErrStopTurn)
	cancel := nextCancel(t, f, start, "first")
	f.finishTurn(t, start, "first", "complete answer")
	f.settleCancel(start, cancel, "already_ended")
	if got := receiveIPC(t, done); got.err != nil || got.result.Status != execution.StatusCompleted || got.result.Answer != "complete answer" || got.result.SessionKept {
		t.Fatalf("turn that won the race = %+v", got)
	}
	// A settlement for a turn that already has its result changes nothing, and is acknowledged.
	receiveIPC(t, f.acked)
	receiveIPC(t, f.acked)
	f.settleCancel(start, cancel, "already_ended")
	if ack := receiveIPC(t, f.acked); ack.Type != "turn_cancel_settled" {
		t.Fatalf("acknowledged %s, want the late settlement", ack.Type)
	}
}

// Whenever Node cannot settle the stop for a kept session (the session did not negotiate
// turn_cancel, Node refuses the command or does not know the turn, or no settlement arrives in
// time), the stop ends the session through EndSession, as before turn_cancel.
func TestDriverIPCStopFallsBackToEndingTheSession(t *testing.T) {
	for _, tc := range []struct {
		name       string
		turnCancel bool
		answer     func(f *fakeNodeIPC, start node.Message, cancel node.Message)
		wait       time.Duration
	}{
		{name: "not negotiated"},
		{name: "refused", turnCancel: true, answer: func(f *fakeNodeIPC, start, cancel node.Message) {
			var c cancelCommand
			_ = json.Unmarshal(cancel.Payload, &c)
			f.reply(cancel, "session_command_rejected", map[string]string{"command_id": c.CommandID, "reason": "turn_cancel_unsupported"})
		}},
		{name: "unknown turn", turnCancel: true, answer: func(f *fakeNodeIPC, start, cancel node.Message) {
			var c cancelCommand
			_ = json.Unmarshal(cancel.Payload, &c)
			f.settleCancel(start, c, "unknown")
		}},
		{name: "never settled", turnCancel: true, wait: 50 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newIPCHarness(t, nil)
			h.turnCancel.Store(tc.turnCancel)
			if tc.wait != 0 {
				h.driver.turnCancelWait = tc.wait
			}
			ctx := ipcContext(t)
			turnCtx, stop := context.WithCancelCause(ctx)
			session := h.session("session", "workspace")
			done := startIPCRun(h, turnCtx, session, "first", discardIPCEvent)
			f := receiveIPC(t, h.created)
			start := nextIPCCommand(t, f, "start_agent_session")
			if spec := startSpec(t, start); (spec["keep_session_on_cancel"] == true) != tc.turnCancel {
				t.Fatalf("keep_session_on_cancel = %v with turn_cancel negotiated %v", spec["keep_session_on_cancel"], tc.turnCancel)
			}
			stop(execution.ErrStopTurn)
			var kinds []string
			for {
				m := receiveIPC(t, f.sent)
				kinds = append(kinds, m.Type)
				if m.Type == "cancel_turn" && tc.answer != nil {
					tc.answer(f, start, m)
				}
				if m.Type == "end_session" {
					f.terminal(m)
					break
				}
			}
			if slices.Contains(kinds, "cancel_turn") != tc.turnCancel {
				t.Fatalf("commands after the stop = %v", kinds)
			}
			got := receiveIPC(t, done)
			if got.err == nil || got.result.Status != execution.StatusCancelled || got.result.SessionKept {
				t.Fatalf("stop that ended the session = %+v", got)
			}
		})
	}
}
