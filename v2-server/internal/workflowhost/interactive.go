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
// can keep talking, and completion waits for a turn that ended normally. Only a lost Agent, the
// starter's stop or another session of the run ends the session; the node then parks for
// completion with the last normal answer, if there is one.
//
// Every transition of one node is claimed under runState.mu through interactiveNode.phase, so a
// follow-up turn, a completion and a stop never act on the same node at once: exactly one claim
// wins and the others see the phase it left. Turns still execute on the run's worker, which owns
// the run's single turn slot in the shared directory. That directory holds one live session: the
// pinned Node refuses a second Agent session on a checkout (node-db session.rs:117-120 at
// aa21210e), so the execution service ends a parked node's session when the run starts another
// Agent, and the node records it (see sessionsReplaced).

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
	// blockerNoCompletedTurn: the session ended (stopped, lost or replaced) before any turn ended
	// normally, so there is no answer to accept and no way to produce one.
	blockerNoCompletedTurn = "no_completed_turn"
	// blockerTurnNotCompleted: the session is open but its last turn did not end normally
	// (refused, truncated, unreadable); another turn can fix it.
	blockerTurnNotCompleted = "turn_not_completed"
	// blockerStructuredOutput: the accepted answer does not satisfy the node's output schema.
	blockerStructuredOutput = "structured_output"
)

// interactiveNode is one root-scope interactive Agent attempt with an open session. Fields other
// than the immutable dispatch data are guarded by the owning runState.mu.
type interactiveNode struct {
	d        workflow.AgentDispatch
	executor resolvedExecutor
	cfg      workflow.AgentConfig
	task     string
	session  string

	phase interactivePhase
	// turns counts the turns that settled; seq numbers the follow-up caller attempts.
	turns, seq int
	// last is the latest settled turn; accepted the latest one that ended normally.
	last, accepted *execution.Result
	// sessionEnded means the session accepts no further turn: the starter stopped a turn (the
	// pinned Node stops a turn only by ending its session), the Agent was lost, or the run started
	// another session in its directory.
	sessionEnded bool
	// stopping marks a turn the starter asked to stop, so its end also ends the session even if
	// the turn completed before the stop reached it.
	stopping bool
}

// followUp is one human turn waiting for the run's turn slot.
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
	s := map[string]any{"turns": n.turns, "session_ended": n.sessionEnded, "completable": reason == ""}
	if reason != "" {
		s["blocker"] = map[string]any{"reason": reason, "detail": detail}
	}
	if n.last != nil {
		s["last_stop_reason"] = n.last.StopReason
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
func (h *Host) settleInteractiveTurn(st *runState, n *interactiveNode, result execution.Result, err error) {
	if st.endTurn(nil) {
		h.end(st.scope, n.session)
		h.finishCancel(st, n.d.RunID)
		return
	}
	if err != nil && h.ctx.Err() != nil {
		return // shutting down; the next start interrupts the run
	}
	st.mu.Lock()
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
		// covers a turn whose result could not be read. Unconfirmed is logged by end.
		h.end(st.scope, n.session)
	}
	if err != nil {
		h.log.Warn("lost an interactive workflow turn", "run", n.d.RunID, "node_run", n.d.NodeRunID, "error", publicReason(err))
	}
	h.park(st, n)
}

// sessionsReplaced records that starting the session of attempt nodeRunID ended every other
// tracked node's session: the execution service ends the run's previous session on Start, since
// the pinned Node holds one Agent session per checkout. A parked node republishes its summary so
// the starter sees that it can only complete; a node with a follow-up queued learns it when the
// turn's Append is refused, and a completing node ends the session itself.
func (h *Host) sessionsReplaced(st *runState, nodeRunID string) {
	st.mu.Lock()
	var parked []*interactiveNode
	for _, n := range st.interactive {
		if n.d.NodeRunID == nodeRunID || n.sessionEnded {
			continue
		}
		n.sessionEnded = true
		if n.phase == phaseParked {
			parked = append(parked, n)
		}
	}
	st.mu.Unlock()
	for _, n := range parked {
		h.refreshSummary(st, n, store.WorkflowStatusPending)
	}
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

// park moves the node back to awaiting input with its summary. A run that stopped running
// meanwhile (a sibling failed it) has no use for the node: its session ends and the row closes.
func (h *Host) park(st *runState, n *interactiveNode) {
	st.mu.Lock()
	keys := map[string]any{"interactive": n.summary(), "task": n.task}
	st.mu.Unlock()
	moved, err := h.store.MoveWorkflowEngineNode(h.ctx, n.d.RunID, n.d.NodeRunID, store.WorkflowStatusRunning, store.WorkflowStatusPending,
		func(node []byte) ([]byte, error) { return workflow.MergePayload(node, keys) })
	if err != nil {
		// The row stays running and the node unreachable until the run ends; cancelling it still
		// ends the session through the sweep.
		h.log.Error("park interactive workflow node", "run", n.d.RunID, "node_run", n.d.NodeRunID, "error", err)
		return
	}
	if !moved {
		// Only a run that stopped running refuses the park of its own running attempt.
		h.end(st.scope, n.session)
		st.mu.Lock()
		if st.interactive[n.d.Node.ID] == n {
			delete(st.interactive, n.d.Node.ID)
		}
		st.mu.Unlock()
		if _, err := h.store.CloseOrphanedWorkflowNode(h.ctx, n.d.RunID, n.d.NodeRunID); err != nil {
			h.log.Error("close orphaned interactive workflow node", "run", n.d.RunID, "node_run", n.d.NodeRunID, "error", err)
		}
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

// SendNodeTurn claims a parked node for a human turn, marks it running and queues the turn on the
// run's worker. The answer arrives over the node's session; the node parks again after it.
func (h *Host) SendNodeTurn(ctx context.Context, scope server.WorkflowRunScope, runID, nodeID, content string) (store.WorkflowRun, error) {
	st, err := h.starter(ctx, scope, runID)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	if st == nil {
		return store.WorkflowRun{}, server.ErrWorkflowNodeNotAwaitingInput
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
		return store.WorkflowRun{}, err
	}
	n.phase = phaseTurn
	n.seq++
	caller := execution.Caller{Kind: execution.CallerWorkflow, ID: runID, NodeID: nodeID, AttemptID: n.d.NodeRunID + "/turn-" + strconv.Itoa(n.seq)}
	st.mu.Unlock()

	moved, err := h.store.MoveWorkflowEngineNode(ctx, runID, n.d.NodeRunID, store.WorkflowStatusPending, store.WorkflowStatusRunning,
		func(node []byte) ([]byte, error) { return node, nil })
	if err != nil || !moved {
		st.mu.Lock()
		n.phase = phaseParked
		st.mu.Unlock()
		if err == nil {
			err = server.ErrWorkflowNodeNotAwaitingInput
		}
		return store.WorkflowRun{}, err
	}
	h.hub.Publish(runID)
	st.mu.Lock()
	if st.cancelling {
		// The run was cancelled after the row moved: the turn would never run, so do not accept
		// it. The cancellation ends the session and closes the row.
		st.mu.Unlock()
		return store.WorkflowRun{}, server.ErrWorkflowNodeNotAwaitingInput
	}
	st.turns = append(st.turns, followUp{node: n, caller: caller, prompt: execution.TextPrompt(content)})
	if !st.working {
		st.working = true
		h.workers.Add(1)
		go h.work(runID, st)
	}
	st.mu.Unlock()
	return h.store.WorkflowRun(ctx, scope.TenantID, runID)
}

// runFollowUp executes one human turn on the worker and parks the node after it.
func (h *Host) runFollowUp(st *runState, f followUp) {
	n := f.node
	if !st.beginStart("", n.d.NodeRunID) {
		return // cancelling: the run's cancellation ends the session and closes the row
	}
	ref, err := h.appendTurn(st.scope, execution.AppendRequest{SessionID: n.session, Caller: f.caller, Prompt: f.prompt})
	if st.endStart(ref, err) {
		if err == nil {
			st.endTurn(nil)
		}
		h.end(st.scope, n.session)
		h.finishCancel(st, n.d.RunID)
		return
	}
	if err != nil {
		if h.ctx.Err() != nil {
			return
		}
		// The turn never started. A session that ended underneath accepts no more turns; any other
		// refusal leaves the node as it was, waiting for another attempt.
		h.log.Warn("send interactive workflow turn", "run", n.d.RunID, "node_run", n.d.NodeRunID, "error", publicReason(err))
		if errors.Is(err, execution.ErrSessionEnded) {
			st.mu.Lock()
			n.sessionEnded = true
			st.mu.Unlock()
		}
		h.park(st, n)
		return
	}
	result, err := h.awaitTurn(st.scope, ref, f.caller)
	h.settleInteractiveTurn(st, n, result, err)
}

// appendTurn is start's busy wait for a follow-up turn.
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
// without further turns; the run itself keeps running. A turn still queued behind another node's
// never starts: it is withdrawn and the session ended the same way.
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
	ref, queued, err := st.turnOf(stopCtx, nodeID)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	switch {
	case queued != nil:
		queued.sessionEnded = true // owned by this call: the job left the queue under st.mu
		if err := h.end(st.scope, queued.session); err != nil {
			return store.WorkflowRun{}, fmt.Errorf("%w: %w", server.ErrWorkflowRunStopUnconfirmed, err)
		}
		h.park(st, queued)
	default:
		if _, err := h.exec.Cancel(stopCtx, st.scope, ref); err != nil {
			return store.WorkflowRun{}, fmt.Errorf("%w: %w", server.ErrWorkflowRunStopUnconfirmed, err)
		}
	}
	return h.store.WorkflowRun(ctx, scope.TenantID, runID)
}

// turnOf finds the turn nodeID is executing (marking the node stopping, so the turn's end ends the
// session), waiting out a start in flight, or withdraws its follow-up from the queue (returned as
// queued, with sessionEnded already set under st.mu).
func (st *runState) turnOf(ctx context.Context, nodeID string) (execution.Ref, *interactiveNode, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for {
		n, err := st.nodeOf(nodeID)
		if err != nil || n.phase != phaseTurn {
			return execution.Ref{}, nil, server.ErrWorkflowNodeTurnNotRunning
		}
		if st.starting != nil && st.activeNodeRun == n.d.NodeRunID {
			starting := st.starting
			st.mu.Unlock()
			select {
			case <-starting:
			case <-ctx.Done():
				st.mu.Lock()
				return execution.Ref{}, nil, fmt.Errorf("%w: %w", server.ErrWorkflowRunStopUnconfirmed, ctx.Err())
			}
			st.mu.Lock()
			continue
		}
		if st.active != nil && st.activeNodeRun == n.d.NodeRunID {
			n.stopping = true
			return *st.active, nil, nil
		}
		for i, f := range st.turns {
			if f.node == n {
				st.turns = append(st.turns[:i:i], st.turns[i+1:]...)
				n.sessionEnded = true
				return execution.Ref{}, n, nil
			}
		}
		// Between dequeue and start, or between the turn's end and its park: nothing to stop.
		return execution.Ref{}, nil, server.ErrWorkflowNodeTurnNotRunning
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
	accepted, ended := *n.accepted, n.sessionEnded
	keys := completionKeys(accepted, n.executor, n.cfg, n.task)
	st.mu.Unlock()
	release := func() {
		st.mu.Lock()
		n.phase = phaseParked
		st.mu.Unlock()
	}

	// Desktop revalidates after shutting the session down: no turn can slip in between the
	// accepted answer and the output that names it.
	if !ended {
		stopCtx, cancel := context.WithTimeout(ctx, h.stopTimeout)
		err := h.exec.End(stopCtx, st.scope, n.session)
		cancel()
		if err != nil {
			release()
			return store.WorkflowRun{}, fmt.Errorf("%w: %w", server.ErrWorkflowRunStopUnconfirmed, err)
		}
		st.mu.Lock()
		n.sessionEnded = true
		st.mu.Unlock()
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
// drops the run's state once nothing else of it is in flight. Its session is already ended.
func (h *Host) closeLost(st *runState, n *interactiveNode) {
	st.mu.Lock()
	if st.interactive[n.d.Node.ID] == n {
		delete(st.interactive, n.d.Node.ID)
	}
	st.mu.Unlock()
	if _, err := h.store.CloseOrphanedWorkflowNode(h.ctx, n.d.RunID, n.d.NodeRunID); err != nil {
		h.log.Error("close orphaned interactive workflow node", "run", n.d.RunID, "node_run", n.d.NodeRunID, "error", err)
	}
	h.hub.Publish(n.d.RunID)
	h.forgetFinished(n.d.RunID, st)
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
		if err := h.exec.End(ctx, st.scope, id); err != nil {
			return err
		}
	}
	return nil
}

// releaseOrphans ends the sessions of nodes parked under a run that stopped running without a
// cancellation (a sibling failed it) and closes their rows, which the failure left pending. Callers
// have read the run as no longer running.
func (h *Host) releaseOrphans(st *runState, runID string) {
	st.mu.Lock()
	nodes := make([]*interactiveNode, 0, len(st.interactive))
	for id, n := range st.interactive {
		if n.phase == phaseCompleting {
			continue // the completion settles it and reports that it lost
		}
		nodes = append(nodes, n)
		delete(st.interactive, id)
	}
	st.mu.Unlock()
	for _, n := range nodes {
		h.end(st.scope, n.session)
		if _, err := h.store.CloseOrphanedWorkflowNode(h.ctx, runID, n.d.NodeRunID); err != nil {
			h.log.Error("close orphaned interactive workflow node", "run", runID, "node_run", n.d.NodeRunID, "error", err)
			continue
		}
		h.hub.Publish(runID)
	}
}
