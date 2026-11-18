package workflowhost

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// Interactive Agent nodes, desktop's legacy end_turn completion (desktop@ebd5b725
// docs/workflows/workflow-runs.md "Interactive nodes"): after its first turn the node parks as
// pending with its session open; the run's starter sends further turns into the same session or
// completes the node, which accepts the last answer that ended normally. The Agent completion
// signal, confirmation gates, forced completion and deliverable checks are not implemented.
//
// How a turn ended never fails the node (workflow-runs.md:125-126, 152-155): a refused, truncated,
// unknown or unrequested cancelled turn keeps it parked with its held session open, so the starter
// can keep talking, and completion waits for a turn that ended normally. Only a lost Agent or the
// starter's stop ends the session; the node then parks for completion with the last normal
// answer, if there is one.
//
// Every transition of one node is claimed under runState.mu through interactiveNode.phase, so a
// follow-up turn, a completion and a stop never act on the same node at once: exactly one claim
// wins and the others see the phase it left. A follow-up turn executes at once, concurrently with
// the run's other Agents, as each node's turn is its own session's: the directory holds several
// live sessions (the Node's shared_checkout_sessions capability, which the Agent driver requires),
// and a parked node keeps its session while the run's other Agents start, run and end theirs
// beside it, so the starter's next message reaches the same session whatever ran in between.

// interactivePhase is what an interactive node is doing.
type interactivePhase int

const (
	// phaseTurn: a turn is queued or executing; the row is running.
	phaseTurn interactivePhase = iota
	// phaseParked: no turn in flight; the row is pending (awaiting input).
	phaseParked
	// phaseCompleting: a completion claimed the parked node; the row is still pending.
	phaseCompleting
)

// Blocker reasons a parked node reports and a completion refuses with.
const (
	// blockerNoCompletedTurn: the session ended (stopped or lost) before any turn ended normally,
	// so there is no answer to accept and no way to produce one.
	blockerNoCompletedTurn = "no_completed_turn"
	// blockerTurnNotCompleted: the session is open but its last turn did not end normally
	// (refused, truncated, unreadable); another turn can fix it.
	blockerTurnNotCompleted = "turn_not_completed"
	// blockerStructuredOutput: the accepted answer does not satisfy the node's output schema.
	blockerStructuredOutput = "structured_output"
)

// turnErrorNotDelivered is payload.interactive.last_turn_error for a message the starter sent
// that never reached the node's session.
const turnErrorNotDelivered = "not_delivered"

// interactiveNode is one root-scope interactive Agent attempt with an open session. Fields other
// than the immutable dispatch data are guarded by the owning runState.mu.
type interactiveNode struct {
	d        workflow.AgentDispatch
	executor resolvedExecutor
	cfg      workflow.AgentConfig
	task     string
	session  string

	phase interactivePhase
	// turns counts the turns that settled; seq numbers the follow-up caller attempts, that is
	// the messages the starter sent (the summary's sent).
	turns, seq int
	// last is the latest settled turn; accepted the latest one that ended normally.
	last, accepted *execution.Result
	// sessionEnded means the session accepts no further turn: the starter stopped a turn (the
	// pinned Node stops a turn only by ending its session) or the Agent was lost.
	sessionEnded bool
	// sessionGone means Node no longer holds the session: its End returned (or the session was
	// already gone). Until then the node keeps its slot of the run's checkout (see sessionsInUse).
	sessionGone bool
	// endRetrying marks a background retry of an unconfirmed End (see endInteractive).
	endRetrying bool
	// stopping marks a turn the starter asked to stop, so its end also ends the session even if
	// the turn completed before the stop reached it.
	stopping bool
	// undelivered means the last message the starter sent was accepted (SendNodeTurn returned)
	// and then refused by the execution service, so it never ran. The summary says so until the
	// next message is claimed; nothing else records the lost message.
	undelivered bool
}

// followUp is one human turn of an interactive node.
type followUp struct {
	node   *interactiveNode
	caller execution.Caller
	prompt execution.Prompt
}

// endedNormally is the legacy end_turn rule: only a complete EndTurn answer can be accepted.
func endedNormally(r execution.Result) bool {
	return r.Status == execution.StatusCompleted && r.AnswerComplete && r.StopReason == "end_turn"
}

// track registers a node whose first turn is executing.
func (st *runState) track(n *interactiveNode) *interactiveNode {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.interactive == nil {
		st.interactive = map[string]*interactiveNode{}
	}
	st.interactive[n.d.Node.ID] = n
	return n
}

// blocker reports why n cannot complete now, or "" with the answer and structured value a
// completion would write. Callers hold st.mu.
func (n *interactiveNode) blocker() (reason, detail, answer string, structured any) {
	accepted := n.accepted
	if !n.sessionEnded && (n.last == nil || !endedNormally(*n.last)) {
		stop := "no turn settled"
		if n.last != nil {
			stop = fmt.Sprintf("the last turn ended as %s (%s)", n.last.Status, n.last.StopReason)
		}
		return blockerTurnNotCompleted, stop + "; send another message so the Agent can finish", "", nil
	}
	if accepted == nil {
		return blockerNoCompletedTurn, "no turn ended normally before the session stopped, so there is no answer to accept; cancel the run instead", "", nil
	}
	if n.cfg.Structured != nil {
		value, err := workflow.ParseStructuredOutput(accepted.Answer, n.cfg.Structured)
		if err != nil {
			return blockerStructuredOutput, err.Error(), "", nil
		}
		structured = value
	}
	return "", "", accepted.Answer, structured
}

// summary is payload.interactive, what the run page shows about a parked node. Callers hold st.mu.
func (n *interactiveNode) summary() map[string]any {
	reason, detail, _, _ := n.blocker()
	// sent numbers the starter's messages claimed so far; last_turn_error is about the latest
	// one, so a page can tell whether a notice concerns a message it sent itself.
	s := map[string]any{"turns": n.turns, "sent": n.seq, "session_ended": n.sessionEnded, "completable": reason == ""}
	if reason != "" {
		s["blocker"] = map[string]any{"reason": reason, "detail": detail}
	}
	if n.last != nil {
		s["last_stop_reason"] = n.last.StopReason
	}
	if n.undelivered {
		s["last_turn_error"] = turnErrorNotDelivered
	}
	if n.accepted != nil {
		// Members who cannot read the starter's session still see what completing would accept.
		s["answer"] = n.accepted.Answer
	}
	return s
}

// settleInteractiveTurn records how a turn of n ended and parks the node again; a turn's outcome
// never fails the node. The session ends only when the turn lost its Agent (its result could not
// be read, or the execution service reports AgentLost) or the starter stopped it; any other turn
// leaves the held session open, and completion waits for a turn that ended normally.
//
// The one exception is a first turn whose session was never set up: its Agent never started (the
// project, checkout, plugin or Skills could not be prepared) or Node refused it at the checkout
// (the run already holds its limit of live sessions, for example). The node then fails like any
// other session that could not start, resumable once the cause is gone, instead of parking with
// neither a conversation nor an answer. A session that was lost after its Agent started still
// parks, as desktop keeps a crashed turn awaiting input.
func (h *Host) settleInteractiveTurn(st *runState, a *attemptState, n *interactiveNode, result execution.Result, err error) {
	if cancelling, last := st.endTurn(a, nil); cancelling {
		h.end(st.scope, n.session)
		if last {
			h.finishCancel(st, n.d.RunID)
		}
		return
	}
	if err != nil && h.ctx.Err() != nil {
		return // shutting down; the next start interrupts the run
	}
	st.mu.Lock()
	// A turn the starter stopped is never a setup failure, even when the stop caught the session
	// still being prepared: it parks like any stopped turn.
	if err == nil && n.turns == 0 && !n.stopping && result.AgentLost && (result.NotStarted || execution.CheckoutRefused(result.FailureCode)) {
		// Nothing is running, so End is idempotent; until it is confirmed the session keeps
		// its slot.
		lingering := st.dropInteractive(n)
		st.mu.Unlock()
		if lingering {
			h.endLingering(st, n.d.RunID, n.session)
		}
		message := "the Agent session could not be set up: " + result.FailureCode
		if execution.CheckoutRefused(result.FailureCode) {
			message = "Node refused the Agent session on the run's directory: " + result.FailureCode
		}
		h.fail(n.d, workflow.Failure{Kind: workflow.FailureInteractiveSession, Message: message})
		return
	}
	if err == nil && result.FailureCode == execution.FailureWorkspaceMissing {
		// The node's session outlived a restart, but the checkout its Agent worked in is gone (or
		// unknown to Node): nothing can continue the conversation or produce what it was asked for.
		// As for a first turn that could not be set up, the session keeps its slot until its End
		// is confirmed (the execution service has ended it, so End is idempotent).
		lingering := st.dropInteractive(n)
		st.mu.Unlock()
		if lingering {
			h.endLingering(st, n.d.RunID, n.session)
		}
		h.fail(n.d, workflow.Failure{Kind: workflow.FailureInteractiveSession, Message: "the run's checkout no longer exists, so the node's conversation cannot continue"})
		return
	}
	if err == nil {
		n.turns++
		n.last = &result
		if endedNormally(result) {
			n.accepted = &result
		}
	}
	ended := err != nil || result.AgentLost || n.stopping
	n.stopping = false
	if ended {
		n.sessionEnded = true
	}
	st.mu.Unlock()
	if ended {
		// A stop or a lost Agent has already ended the session; ending again is idempotent and
		// covers a turn whose result could not be read. Like an automatic attempt, the slot frees
		// only once End is confirmed; until then the node keeps it, and completing the node or
		// the run's end ends the session again.
		h.endInteractive(st, n)
	}
	if err != nil {
		h.log.Warn("lost an interactive workflow turn", "run", n.d.RunID, "node_run", n.d.NodeRunID, "error", publicReason(err))
	}
	h.park(st, n)
}

// refreshSummary rewrites payload.interactive of n's row, which has status, in place: the run
// page reads the node as an interactive session from it, from its first turn on.
func (h *Host) refreshSummary(st *runState, n *interactiveNode, status string) {
	st.mu.Lock()
	keys := map[string]any{"interactive": n.summary()}
	st.mu.Unlock()
	if _, err := h.store.MoveWorkflowEngineNode(h.ctx, n.d.RunID, n.d.NodeRunID, status, status,
		func(node []byte) ([]byte, error) { return workflow.MergePayload(node, keys) }); err != nil {
		h.log.Error("record the session summary of an interactive workflow node", "run", n.d.RunID, "node_run", n.d.NodeRunID, "error", err)
	}
	h.hub.Publish(n.d.RunID)
}

func (h *Host) moveEngineNode(ctx context.Context, runID, nodeRunID, from, to string, merge func(node []byte) ([]byte, error)) (bool, error) {
	if h.moveNode != nil {
		return h.moveNode(ctx, runID, nodeRunID, from, to, merge)
	}
	return h.store.MoveWorkflowEngineNode(ctx, runID, nodeRunID, from, to, merge)
}

// park moves the node back to awaiting input with its summary. A run that stopped running
// meanwhile (a sibling failed it) has no use for the node: its session ends and the row closes.
func (h *Host) park(st *runState, n *interactiveNode) {
	st.mu.Lock()
	keys := map[string]any{"interactive": n.summary(), "task": n.task}
	st.mu.Unlock()
	var moved bool
	writeUncertain := false
	err := h.resultRetry.Store(h.ctx, func(ctx context.Context) error {
		var err error
		moved, err = h.moveEngineNode(ctx, n.d.RunID, n.d.NodeRunID, store.WorkflowStatusRunning, store.WorkflowStatusPending,
			func(node []byte) ([]byte, error) { return workflow.MergePayload(node, keys) })
		if err != nil {
			writeUncertain = true
			return err
		}
		if !moved && writeUncertain {
			// An earlier attempt may have committed before its connection broke. Refreshing in
			// pending succeeds only while the run is still running and the row already parked.
			moved, err = h.moveEngineNode(ctx, n.d.RunID, n.d.NodeRunID, store.WorkflowStatusPending, store.WorkflowStatusPending,
				func(node []byte) ([]byte, error) { return workflow.MergePayload(node, keys) })
		}
		return err
	})
	if err != nil {
		if h.ctx.Err() == nil {
			// The row stays running and the node unreachable until the run ends; cancelling it still
			// ends the session through the sweep.
			h.log.Error("park interactive workflow node", "run", n.d.RunID, "node_run", n.d.NodeRunID, "error", err)
		}
		return
	}
	if !moved {
		// Only a run that stopped running refuses the park of its own running attempt.
		h.endInteractive(st, n)
		h.forgetInteractive(st, n)
		h.closeOrphan(n)
		h.hub.Publish(n.d.RunID)
		return
	}
	st.mu.Lock()
	n.phase = phaseParked
	st.mu.Unlock()
	h.hub.Publish(n.d.RunID)
}

// starter authorizes a turn, stop or completion: the node's session belongs to whoever started the
// run, so nobody else can act on it. Other members still read the run.
func (h *Host) starter(ctx context.Context, scope server.WorkflowRunScope, runID string) (*runState, error) {
	if _, err := h.store.WorkflowRun(ctx, scope.TenantID, runID); err != nil {
		return nil, err
	}
	starter, err := h.store.WorkflowStarter(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("read workflow run starter: %w", err)
	}
	if starter == "" {
		return nil, nil // never started: nothing to talk to yet
	}
	if starter != scope.UserID {
		return nil, server.ErrWorkflowRunOwned
	}
	return h.state(runID), nil
}

// nodeOf finds a tracked node, or reports it as not awaiting input. Callers hold st.mu.
func (st *runState) nodeOf(nodeID string) (*interactiveNode, error) {
	if n := st.interactive[nodeID]; n != nil && !st.cancelling {
		return n, nil
	}
	return nil, server.ErrWorkflowNodeNotAwaitingInput
}

// SendNodeTurn claims a parked node for a human turn, marks it running and starts the turn. The
// answer arrives over the node's session; the node parks again after it. It answers with the
// number the message was claimed under, the value payload.interactive.sent has from now on.
func (h *Host) SendNodeTurn(ctx context.Context, scope server.WorkflowRunScope, runID, nodeID, content string) (server.WorkflowNodeTurnAccepted, error) {
	st, err := h.starter(ctx, scope, runID)
	if err != nil {
		return server.WorkflowNodeTurnAccepted{}, err
	}
	if st == nil {
		return server.WorkflowNodeTurnAccepted{}, server.ErrWorkflowNodeNotAwaitingInput
	}
	st.mu.Lock()
	n, err := st.nodeOf(nodeID)
	switch {
	case err != nil:
	case n.phase != phaseParked:
		err = server.ErrWorkflowNodeNotAwaitingInput
	case n.sessionEnded:
		err = server.ErrWorkflowNodeSessionEnded
	}
	if err != nil {
		st.mu.Unlock()
		return server.WorkflowNodeTurnAccepted{}, err
	}
	n.phase = phaseTurn
	n.seq++
	sent := n.seq
	caller := execution.Caller{Kind: execution.CallerWorkflow, ID: runID, NodeID: nodeID, AttemptID: n.d.NodeRunID + "/turn-" + strconv.Itoa(sent)}
	// This message replaces the one that was not delivered, so the row stops saying so as it
	// starts running; if this one is refused too, the park writes the notice again.
	undelivered := n.undelivered
	n.undelivered = false
	keys := map[string]any{"interactive": n.summary()}
	st.mu.Unlock()

	moved, err := h.store.MoveWorkflowEngineNode(ctx, runID, n.d.NodeRunID, store.WorkflowStatusPending, store.WorkflowStatusRunning,
		func(node []byte) ([]byte, error) { return workflow.MergePayload(node, keys) })
	if err != nil || !moved {
		st.mu.Lock()
		n.phase = phaseParked
		// The row was not rewritten and no turn was registered: this message was never the
		// starter's next one, so neither the notice nor the count moves.
		n.undelivered = undelivered
		n.seq--
		st.mu.Unlock()
		if err == nil {
			err = server.ErrWorkflowNodeNotAwaitingInput
		}
		return server.WorkflowNodeTurnAccepted{}, err
	}
	h.hub.Publish(runID)
	st.mu.Lock()
	if st.cancelling {
		// The run was cancelled after the row moved: the turn would never run, so do not accept
		// it. The cancellation ends the session and closes the row.
		st.mu.Unlock()
		return server.WorkflowNodeTurnAccepted{}, server.ErrWorkflowNodeNotAwaitingInput
	}
	// Registered before the goroutine runs, so a stop arriving first finds the turn starting.
	a := st.register(caller.AttemptID, n.d.NodeRunID, "", st.generation)
	if a == nil {
		st.mu.Unlock()
		return server.WorkflowNodeTurnAccepted{}, server.ErrWorkflowNodeNotAwaitingInput
	}
	st.dispatched++
	h.workers.Add(1)
	go func() {
		defer h.settled(runID, st)
		h.runFollowUp(st, a, followUp{node: n, caller: caller, prompt: execution.TextPrompt(content)})
	}()
	st.mu.Unlock()
	run, err := h.store.WorkflowRun(ctx, scope.TenantID, runID)
	if err != nil {
		return server.WorkflowNodeTurnAccepted{}, err
	}
	return server.WorkflowNodeTurnAccepted{Run: run, Sent: sent}, nil
}

// runFollowUp executes one human turn registered as a and parks the node after it.
func (h *Host) runFollowUp(st *runState, a *attemptState, f followUp) {
	n := f.node
	if h.followingUp != nil {
		h.followingUp(n.d.Node.ID)
	}
	// Another node may have failed the run since the turn was accepted. Like an attempt (see
	// runAgent), check with the turn registered, and run no turn for a run that stopped; the park
	// is then refused, which ends the session and closes the row.
	if h.runStopped(h.ctx, n.d.RunID) {
		if st.releaseStart(a) {
			h.end(st.scope, n.session) // a cancellation stopped the run; close it once
			h.finishCancel(st, n.d.RunID)
			return
		}
		// Accepted and not run. The park is normally refused and closes the node; should the
		// run be running again by then, the node waits on and says the message was lost.
		st.mu.Lock()
		n.undelivered = true
		st.mu.Unlock()
		h.park(st, n)
		return
	}
	ref, err := h.appendTurn(st.scope, execution.AppendRequest{SessionID: n.session, Caller: f.caller, Prompt: f.prompt})
	if cancelling, last, _ := st.endStart(a, ref, err); cancelling {
		h.end(st.scope, n.session)
		if err == nil {
			_, last = st.endTurn(a, nil)
		}
		if last {
			h.finishCancel(st, n.d.RunID)
		}
		return
	}
	if err != nil {
		if h.ctx.Err() != nil {
			return
		}
		// The turn never started. A session that ended underneath accepts no more turns; any other
		// refusal leaves the node waiting for another attempt. SendNodeTurn has already answered
		// the starter, so the summary is the only place that can say the message was lost.
		h.log.Warn("send interactive workflow turn", "run", n.d.RunID, "node_run", n.d.NodeRunID, "error", publicReason(err))
		st.mu.Lock()
		n.undelivered = true
		if errors.Is(err, execution.ErrSessionEnded) {
			n.sessionEnded, n.sessionGone = true, true
			st.slotsMayHaveChanged()
		}
		st.mu.Unlock()
		h.park(st, n)
		return
	}
	result, err := h.awaitTurn(st.scope, ref, f.caller)
	h.settleInteractiveTurn(st, a, n, result, err)
}

// appendTurn waits out ErrWorkspaceBusy while the session's previous turn finishes cleaning up:
// its terminal event is published before the execution worker releases the session.
func (h *Host) appendTurn(scope execution.Scope, req execution.AppendRequest) (execution.Ref, error) {
	deadline := time.Now().Add(h.busyRetry)
	for {
		ref, err := h.exec.Append(h.ctx, scope, req)
		if !errors.Is(err, execution.ErrWorkspaceBusy) || time.Now().After(deadline) {
			return ref, err
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-h.ctx.Done():
			return execution.Ref{}, h.ctx.Err()
		}
	}
}

// StopNodeTurn stops the turn an interactive node is executing. The pinned Node can only stop a
// turn by ending its session (execution.Service.Cancel), so the node then parks for completion
// without further turns; the run itself keeps running. A turn still starting is awaited first.
func (h *Host) StopNodeTurn(ctx context.Context, scope server.WorkflowRunScope, runID, nodeID string) (store.WorkflowRun, error) {
	st, err := h.starter(ctx, scope, runID)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	if st == nil {
		return store.WorkflowRun{}, server.ErrWorkflowNodeTurnNotRunning
	}
	stopCtx, cancel := context.WithTimeout(ctx, h.stopTimeout)
	defer cancel()
	ref, err := st.turnOf(stopCtx, nodeID)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	if _, err := h.exec.Cancel(stopCtx, st.scope, ref); err != nil {
		return store.WorkflowRun{}, fmt.Errorf("%w: %w", server.ErrWorkflowRunStopUnconfirmed, err)
	}
	return h.store.WorkflowRun(ctx, scope.TenantID, runID)
}

// turnOf finds the turn nodeID is executing, marking the node stopping so the turn's end ends the
// session, and waits out a start in flight.
func (st *runState) turnOf(ctx context.Context, nodeID string) (execution.Ref, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for {
		n, err := st.nodeOf(nodeID)
		if err != nil || n.phase != phaseTurn {
			return execution.Ref{}, server.ErrWorkflowNodeTurnNotRunning
		}
		var turn *attemptState
		for _, a := range st.attempts {
			if a.nodeRun == n.d.NodeRunID {
				turn = a
			}
		}
		if turn != nil && turn.starting != nil {
			starting := turn.starting
			st.mu.Unlock()
			select {
			case <-starting:
			case <-ctx.Done():
				st.mu.Lock()
				return execution.Ref{}, fmt.Errorf("%w: %w", server.ErrWorkflowRunStopUnconfirmed, ctx.Err())
			}
			st.mu.Lock()
			continue
		}
		if turn != nil && turn.ref != nil {
			n.stopping = true
			return *turn.ref, nil
		}
		// Before its registration, or between the turn's end and its park: nothing to stop.
		return execution.Ref{}, server.ErrWorkflowNodeTurnNotRunning
	}
}

// CompleteNode completes a parked interactive node as desktop's legacy end_turn completion: it
// accepts the last answer that ended normally (validated against the node's output schema),
// ends the session, then writes the output once and schedules successors. force is accepted for
// contract compatibility but forced completion is not implemented, so it changes nothing.
func (h *Host) CompleteNode(ctx context.Context, scope server.WorkflowRunScope, runID, nodeID string, _ bool, _ string) (store.WorkflowRun, error) {
	st, err := h.starter(ctx, scope, runID)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	if st == nil {
		return store.WorkflowRun{}, server.ErrWorkflowNodeNotAwaitingInput
	}
	st.mu.Lock()
	n, err := st.nodeOf(nodeID)
	if err == nil && n.phase != phaseParked {
		err = server.ErrWorkflowNodeNotAwaitingInput
	}
	if err != nil {
		st.mu.Unlock()
		return store.WorkflowRun{}, err
	}
	reason, detail, answer, structured := n.blocker()
	if reason != "" {
		st.mu.Unlock()
		return store.WorkflowRun{}, &server.WorkflowNodeIncomplete{Reason: reason, Detail: detail}
	}
	n.phase = phaseCompleting
	accepted, gone := *n.accepted, n.sessionGone
	keys := completionKeys(accepted, n.executor, n.cfg, n.task)
	st.mu.Unlock()
	// release gives the claim back and republishes the summary, which may have changed while the
	// claim was held (the session ended); a row that is no longer pending is left as it is.
	release := func() {
		st.mu.Lock()
		n.phase = phaseParked
		st.mu.Unlock()
		h.refreshSummary(st, n, store.WorkflowStatusPending)
	}

	// Desktop revalidates after shutting the session down: no turn can slip in between the
	// accepted answer and the output that names it. A session Node may still hold is ended
	// again, including after an earlier completion's End went unconfirmed: the retry confirms
	// the end before the node gives up its slot.
	if !gone {
		stopCtx, cancel := context.WithTimeout(ctx, h.stopTimeout)
		err := h.exec.End(stopCtx, st.scope, n.session)
		cancel()
		if endConfirmed(err) {
			err = nil
		}
		// Confirmed or not, the execution service takes no further turn for a session it was
		// asked to end, so the node stops offering the conversation either way: a message sent
		// now would be accepted and then lost.
		// An unconfirmed End keeps the slot: Node may still hold the session.
		st.mu.Lock()
		n.sessionEnded = true
		if err == nil {
			n.sessionGone = true
			st.slotsMayHaveChanged()
		}
		st.mu.Unlock()
		if err != nil {
			release()
			if h.runStopped(context.WithoutCancel(ctx), runID) {
				// A sibling failed the run meanwhile and its sweep left this node to the
				// completion: close it now, or it would stay pending under the failed run.
				h.closeLost(st, n)
			}
			return store.WorkflowRun{}, fmt.Errorf("%w: %w", server.ErrWorkflowRunStopUnconfirmed, err)
		}
	}
	st.mu.Lock()
	keys["interactive"] = map[string]any{"turns": n.turns, "session_ended": true, "completable": true, "answer": answer}
	st.mu.Unlock()
	completion := workflow.Completion{Output: &answer, Structured: structured, Keys: keys}
	// The run outlives the request: a disconnecting client must not abandon the successor wave,
	// nor the reads that settle the claim once the completion may have committed.
	settle := context.WithoutCancel(ctx)
	engineErr := h.engine.CompleteNode(settle, runID, n.d.NodeRunID, completion)
	// The row decides who won, not the engine's error: the completion can commit and the successor
	// wave then meet a cancellation that landed in between, which is still this completion's win.
	status, err := h.nodeStatus(settle, scope.TenantID, runID, n.d.NodeRunID)
	if err != nil {
		release()
		return store.WorkflowRun{}, errors.Join(engineErr, err)
	}
	if status != store.WorkflowStatusSucceeded && status != store.WorkflowStatusCancelled && status != store.WorkflowStatusFailed {
		if h.runStopped(settle, runID) {
			// A sibling failed the run while the session was ending, and releaseOrphans left this
			// node to the completion: close its row as the orphan path does and drop the run.
			h.closeLost(st, n)
			return store.WorkflowRun{}, server.ErrWorkflowNodeNotAwaitingInput
		}
		release() // nothing committed; the starter can try again
		if engineErr == nil {
			engineErr = server.ErrWorkflowNodeNotAwaitingInput
		}
		return store.WorkflowRun{}, engineErr
	}
	st.mu.Lock()
	if st.interactive[nodeID] == n {
		delete(st.interactive, nodeID)
		st.slotsMayHaveChanged()
	}
	st.mu.Unlock()
	h.forgetFinished(runID, st)
	if status != store.WorkflowStatusSucceeded {
		// The run stopped running before the completion committed (it was cancelled or a sibling
		// failed it), so this completion did not win.
		return store.WorkflowRun{}, server.ErrWorkflowNodeNotAwaitingInput
	}
	if engineErr != nil {
		h.log.Warn("schedule after interactive completion", "run", runID, "node_run", n.d.NodeRunID, "error", engineErr)
	}
	return h.store.WorkflowRun(ctx, scope.TenantID, runID)
}

// runStopped reports whether the run is no longer running. A read error counts as running, so
// the node stays claimable and the starter can try again.
func (h *Host) runStopped(ctx context.Context, runID string) bool {
	c, err := h.store.WorkflowEngineContext(ctx, runID)
	return err == nil && c.Run.Status != workflow.StatusRunning
}

// closeLost forgets a node whose run stopped running without settling it, closes its row and
// drops the run's state once nothing else of it is in flight. Its session was ended, or lingers
// until a retry confirms the End (see linger).
func (h *Host) closeLost(st *runState, n *interactiveNode) {
	h.forgetInteractive(st, n) // its completion already tried to end the session
	h.closeOrphan(n)
	h.hub.Publish(n.d.RunID)
	h.forgetFinished(n.d.RunID, st)
}

// closeOrphan cancels n's attempt under its stopped run and reports whether the attempt is
// settled: false only while the run is running again (a resume from failure won the race), when
// the attempt is still the run's and must be left as it is.
func (h *Host) closeOrphan(n *interactiveNode) bool {
	_, err := h.store.CloseOrphanedWorkflowNode(h.ctx, n.d.RunID, n.d.NodeRunID)
	if errors.Is(err, store.ErrWorkflowRunActive) {
		return false
	}
	if err != nil {
		h.log.Error("close orphaned interactive workflow node", "run", n.d.RunID, "node_run", n.d.NodeRunID, "error", err)
	}
	return true
}

// nodeStatus reads one attempt's stored status.
func (h *Host) nodeStatus(ctx context.Context, tenant, runID, nodeRunID string) (string, error) {
	nodes, err := h.store.WorkflowRunNodes(ctx, tenant, runID)
	if err != nil {
		return "", err
	}
	for _, row := range nodes {
		if row.ID == nodeRunID {
			return row.Status, nil
		}
	}
	return "", store.ErrWorkflowNodeRunNotFound
}

// endParked ends the session of every tracked node for a run that is closing. Callers have marked
// the run cancelling, so no turn or completion can claim a node any more.
func (h *Host) endParked(ctx context.Context, st *runState) error {
	st.mu.Lock()
	sessions := make([]string, 0, len(st.interactive))
	for _, n := range st.interactive {
		sessions = append(sessions, n.session)
	}
	st.mu.Unlock()
	for _, id := range sessions {
		if err := h.exec.End(ctx, st.scope, id); !endConfirmed(err) {
			return err
		}
	}
	return nil
}

// releaseOrphans closes the rows of nodes parked under a run that stopped running without a
// cancellation (a sibling failed it), which the failure left pending, and ends their sessions.
// Callers have read the run as no longer running, but a resume from failure can return it to
// running at any moment. So each row is closed first, in a transaction that refuses once the run
// runs again, and a node stays tracked until then: ResumeRun, which releases the orphans itself
// before it admits the resume, never finds the run's state empty while another caller is still
// about to end a session, and a node the resumed run kept is left alone with its session.
//
// A node with a turn in flight is left to that turn, like an automatic sibling that keeps running
// when another node fails the run: an executing turn finishes and its park is refused, which ends
// the session and closes the row; a turn not yet executing sees the stopped run and does not run.
func (h *Host) releaseOrphans(st *runState, runID string) {
	st.mu.Lock()
	inFlight := map[string]bool{}
	for _, a := range st.attempts {
		inFlight[a.nodeRun] = true
	}
	nodes := make([]*interactiveNode, 0, len(st.interactive))
	for _, n := range st.interactive {
		if n.phase == phaseCompleting || inFlight[n.d.NodeRunID] {
			continue // the completion or the turn settles it
		}
		nodes = append(nodes, n)
	}
	st.mu.Unlock()
	for _, n := range nodes {
		if !h.closeOrphan(n) {
			continue // the run was resumed meanwhile; the attempt is no longer an orphan
		}
		// End first and only then give the slot back, as park does: a resume releases the
		// orphans before it admits its fan-out, which must not be refused by Node at the
		// checkout's session limit while Node still holds this session.
		h.endInteractive(st, n)
		h.forgetInteractive(st, n)
		h.hub.Publish(runID)
	}
}
