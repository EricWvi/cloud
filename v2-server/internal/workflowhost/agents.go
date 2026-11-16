package workflowhost

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// Dispatch starts one Agent attempt in its own goroutine and returns at once. A run's attempts
// execute concurrently, as desktop's: each is its own execution session in the run's shared
// directory, and Node admits several live sessions on one checkout.
func (h *Host) Dispatch(d workflow.AgentDispatch) {
	if d.Node.Kind == "form" || d.Node.Kind == "createTasks" {
		h.workers.Add(1)
		go func() { defer h.workers.Done(); h.dispatchReview(d) }()
		return
	}
	st := h.state(d.RunID)
	if st == nil {
		// Only StartRun admits runs, and it registers the owner first; reaching here is a bug,
		// which still must not leave the attempt running forever. The engine holds the run's
		// gate while dispatching, so the failure is recorded after Dispatch returns.
		h.workers.Add(1)
		go func() {
			defer h.workers.Done()
			h.fail(d, workflow.Failure{Kind: workflow.FailureSession, Message: "the run has no execution owner in this process"})
		}()
		return
	}
	st.mu.Lock()
	generation := st.generation
	st.dispatched++
	st.mu.Unlock()
	h.workers.Add(1)
	go func() {
		defer h.settled(d.RunID, st)
		if h.dispatching != nil {
			h.dispatching(d)
		}
		st.mu.Lock()
		dropped := st.cancelling || generation != st.generation || h.ctx.Err() != nil
		st.mu.Unlock()
		// An attempt dispatched before a cancellation request, or one a resume replaced, is
		// dropped: the run closes as cancelled together with its row, or the resume soft-deleted
		// it. One whose run ended meanwhile (a sibling failed it) is closed without a turn.
		if dropped || h.abandoned(d) {
			return
		}
		h.runAgent(st, d, generation)
	}()
}

// settled ends one goroutine counted in runState.dispatched.
func (h *Host) settled(runID string, st *runState) {
	st.mu.Lock()
	st.dispatched--
	st.mu.Unlock()
	h.forgetFinished(runID, st)
	h.workers.Done()
}

// forgetFinished drops the state of a run that is not running while nothing of it is in flight,
// so the process does not keep every run it ever executed. A running run keeps its owner for the
// next wave. A run still pending once no start is under way was refused (bad input, an unsupported
// node): forgetting its starter lets the next start record its own owner instead of executing under
// the scope of whoever tried first.
func (h *Host) forgetFinished(runID string, st *runState) {
	c, err := h.store.WorkflowEngineContext(h.ctx, runID)
	if err != nil || c.Run.Status == workflow.StatusRunning {
		return
	}
	h.releaseOrphans(st, runID)
	h.mu.Lock()
	defer h.mu.Unlock()
	st.mu.Lock()
	idle := st.dispatched == 0 && st.starters == 0 && len(st.attempts) == 0 && len(st.interactive) == 0
	st.mu.Unlock()
	if idle && h.runs[runID] == st {
		delete(h.runs, runID)
	}
}

// abandoned asks the engine whether the attempt's run ended while it was queued. If the check
// itself fails, the attempt still executes: its outcome is then recorded as a late callback, the
// behavior before this check existed.
func (h *Host) abandoned(d workflow.AgentDispatch) bool {
	abandoned, err := h.engine.Abandon(h.ctx, d.RunID, d.NodeRunID)
	if err != nil {
		h.log.Warn("check workflow run before starting an Agent", "run", d.RunID, "node_run", d.NodeRunID, "error", err)
	}
	return abandoned
}

func (h *Host) fail(d workflow.AgentDispatch, f workflow.Failure) {
	if err := h.engine.FailNode(h.ctx, d.RunID, d.NodeRunID, f); err != nil {
		h.log.Error("record workflow node failure", "run", d.RunID, "node_run", d.NodeRunID, "error", err)
	}
}

func (h *Host) complete(d workflow.AgentDispatch, c workflow.Completion) {
	if err := h.engine.CompleteNode(h.ctx, d.RunID, d.NodeRunID, c); err != nil {
		h.log.Error("record workflow node completion", "run", d.RunID, "node_run", d.NodeRunID, "error", err)
	}
}

// finishCancel closes a run whose cancellation was requested once its turn is known to have
// ended, covering a cancellation whose stop the execution service did not confirm in time.
func (h *Host) finishCancel(st *runState, runID string) {
	_, err := h.store.CancelWorkflowRun(h.ctx, st.scope.TenantID, runID)
	if err == nil || errors.Is(err, store.ErrWorkflowRunState) {
		h.engine.ReleaseRetries(runID) // the run is closed; its waits are settled
	}
	if err != nil {
		if h.ctx.Err() == nil { // at shutdown the next start interrupts the run instead
			h.log.Error("close cancelled workflow run", "run", runID, "error", err)
		}
		return
	}
	h.hub.Publish(runID)
}

// runAgent drives one Agent attempt as desktop's drive_agent_node: resolve the executor, render
// the prompt, start the session, bind it to the attempt, wait for the owning turn's result, end
// the session and report the outcome to the engine.
func (h *Host) runAgent(st *runState, d workflow.AgentDispatch, generation uint64) {
	cfg := d.Node.Agent()
	// Interactive mode applies to root-scope nodes only. Desktop rejects it inside Iterations (graph
	// validation already does here) but parks Loop members too; v2 does not, so a Loop member runs
	// as an automatic node, and the workflow package's unapplied rule lists it for that member.
	interactive := cfg.Interactive && d.ScopeID == "" && d.Iteration == nil
	fail := func(f workflow.Failure) {
		if interactive {
			f = f.Interactive() // a person drives the node: it never fails as auto-retryable
		}
		h.fail(d, f)
	}
	skills, err := h.store.ResolveWorkflowSkills(h.ctx, st.scope, d.ProjectID, cfg.Skills)
	if err != nil {
		fail(workflow.Failure{Kind: workflow.FailureMissingAgentConfig, Message: err.Error()})
		return
	}
	mcp, err := h.store.ResolveWorkflowMCP(h.ctx, st.scope, d.ProjectID, cfg.MCPs)
	if err != nil {
		fail(mcpFailure(err))
		return
	}
	executor, failure := h.resolveExecutor(st.scope, cfg)
	if failure != nil {
		fail(*failure)
		return
	}
	task, err := workflow.RenderTemplate(cfg.Prompt, &d.Pool)
	if err != nil {
		fail(workflow.Failure{Kind: workflow.FailurePromptTemplate, Message: err.Error()})
		return
	}
	caller := execution.Caller{Kind: execution.CallerWorkflow, ID: d.RunID, NodeID: d.Node.ID, AttemptID: d.NodeRunID}
	request := execution.StartRequest{
		MCP:    mcp,
		Skills: skills, Caller: caller, Target: execution.Target{ProjectID: d.ProjectID},
		ExecutorID: executor.executor.ID, Prompt: agentPrompt(d, task),
	}
	if interactive {
		// The session outlives its turns until completion, a stop or the run's end ends it.
		request.Lifetime = execution.LifetimeHeld
	}
	a := st.beginStart(d.NodeRunID, d.NodeRunID, d.ScopeID, generation)
	if a == nil {
		return
	}
	// Resolving the executor and rendering the prompt took a while after the attempt's abandon
	// check: the run may have ended (a sibling failed it) or been cancelled meanwhile. Check again
	// with the attempt registered, so nothing can end the run between this check and the start
	// without waiting for the start to return.
	if h.abandoned(d) {
		if st.releaseStart(a) {
			h.finishCancel(st, d.RunID)
		}
		return
	}
	ref, err := h.exec.Start(h.ctx, st.scope, request)
	cancelling, last, unstopped := st.endStart(a, ref, err)
	if err != nil {
		if cancelling {
			// The run closes once its last turn is gone, never while a sibling's stop is
			// still unconfirmed.
			if last {
				h.finishCancel(st, d.RunID)
			}
			return
		}
		var setup *execution.MCPFailure
		if errors.As(err, &setup) {
			fail(mcpFailure(err))
			return
		}
		fail(workflow.Failure{Kind: workflow.FailureSession, Message: "execution refused to start the Agent: " + publicReason(err)})
		return
	}

	// Desktop publishes the binding only after the owning prompt is accepted; Start returning a
	// Ref is that acceptance here. A refused binding (the attempt was cancelled or already
	// bound elsewhere) must not leave the new session running unowned.
	if _, err := h.store.BindWorkflowNodeSession(h.ctx, st.scope.TenantID, d.RunID, d.NodeRunID, ref.SessionID); err != nil {
		endErr := h.end(st.scope, ref.SessionID)
		if cancelling, last := st.endTurn(a, endErr); cancelling {
			if last {
				h.finishCancel(st, d.RunID)
			}
			return
		}
		fail(workflow.Failure{Kind: workflow.FailureSessionBindingRejected, Message: "the attempt refused its session binding"})
		return
	}
	h.hub.Publish(d.RunID)
	if unstopped {
		// The run's cancellation gave up waiting for this Start: stop the turn it returned. The
		// turn then ends as cancelled like any other, and the last one closes the run.
		h.workers.Add(1)
		go func() {
			defer h.workers.Done()
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), h.stopTimeout)
			defer cancel()
			if _, err := h.exec.Cancel(stopCtx, st.scope, ref); err != nil {
				h.log.Warn("stop a turn started after its run was cancelled", "run", d.RunID, "node_run", d.NodeRunID, "error", publicReason(err))
			}
		}()
	}

	if interactive {
		// Desktop parks an interactive node after its first turn instead of ending the session.
		n := st.track(&interactiveNode{d: d, executor: executor, cfg: cfg, task: task, session: ref.SessionID, phase: phaseTurn})
		h.refreshSummary(st, n, store.WorkflowStatusRunning) // the starter can stop the first turn too
		result, err := h.awaitTurn(st.scope, ref, caller)
		h.settleInteractiveTurn(st, a, n, result, err)
		return
	}
	result, err := h.awaitTurn(st.scope, ref, caller)
	endErr := h.end(st.scope, ref.SessionID)
	if cancelling, last := st.endTurn(a, endErr); cancelling {
		if last {
			h.finishCancel(st, d.RunID)
		}
		return
	}
	if err != nil {
		if h.ctx.Err() != nil {
			return // shutting down; the next start interrupts the run
		}
		fail(workflow.Failure{Kind: workflow.FailureSession, Message: "lost the Agent turn: " + publicReason(err)})
		return
	}
	h.settle(d, result, executor, cfg, task)
}

// beginStart registers a turn about to start under key (the turn's caller attempt) for the node
// run nodeRun, or returns nil once cancellation was requested, the turn's Loop scope was closed,
// or the dispatch belongs to a generation a resume has since replaced.
func (st *runState) beginStart(key, nodeRun, scopeID string, generation uint64) *attemptState {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.register(key, nodeRun, scopeID, generation)
}

// register is beginStart for a caller holding mu.
func (st *runState) register(key, nodeRun, scopeID string, generation uint64) *attemptState {
	if st.cancelling || st.closedScopes[scopeID] || generation != st.generation {
		return nil
	}
	if st.attempts == nil {
		st.attempts = map[string]*attemptState{}
	}
	a := &attemptState{nodeRun: nodeRun, scope: scopeID, starting: make(chan struct{}), done: make(chan struct{})}
	st.attempts[key] = a
	return a
}

// releaseStart forgets a turn beginStart registered that will not start, and reports whether it
// was the cancelled run's last turn (see endTurn).
func (st *runState) releaseStart(a *attemptState) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.forget(a)
	return st.lastCancelled()
}

// endStart publishes the started turn to a waiting cancellation and reports whether one was
// requested meanwhile, whether a refused Start left the cancelled run with no turn (see endTurn),
// and whether a cancellation gave up waiting for this Start, so that nobody stops the turn it
// returned unless its attempt does (see stopTurns).
func (st *runState) endStart(a *attemptState, ref execution.Ref, err error) (cancelling, last, unstopped bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err != nil {
		st.forget(a)
		return st.cancelling, st.lastCancelled(), false
	}
	a.ref = &ref
	close(a.starting)
	a.starting = nil
	st.startResolved()
	return st.cancelling, false, a.unstopped
}

// endTurn forgets the turn that ended and reports whether cancellation was requested, and whether
// this was the cancelled run's last turn. Only that last one closes the run: a sibling whose stop
// is not confirmed yet keeps the run open until its own turn ends.
func (st *runState) endTurn(a *attemptState, endErr error) (cancelling, last bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if endErr != nil && a.scope != "" {
		if st.scopeCleanupErrors == nil {
			st.scopeCleanupErrors = map[string]error{}
		}
		st.scopeCleanupErrors[a.scope] = endErr
	}
	st.forget(a)
	return st.cancelling, st.lastCancelled()
}

// startResolved wakes whoever waits for any of the run's Starts to return (see stopTurns).
// Callers hold mu.
func (st *runState) startResolved() {
	if st.startsChanged != nil {
		close(st.startsChanged)
		st.startsChanged = nil
	}
}

// lastCancelled reports whether cancellation was requested and no turn is left. Callers hold mu.
func (st *runState) lastCancelled() bool {
	return st.cancelling && len(st.attempts) == 0
}

// forget removes the turn and wakes whoever waits for its start or its end. Callers hold mu.
func (st *runState) forget(a *attemptState) {
	for key, current := range st.attempts {
		if current == a {
			delete(st.attempts, key)
		}
	}
	if a.starting != nil {
		close(a.starting)
		a.starting = nil
		st.startResolved()
	}
	select {
	case <-a.done:
	default:
		close(a.done)
	}
}

// awaitTurn follows the session until the owning turn's terminal event. If the stream ends or
// breaks first, the recorded attempt is the authority: it carries the result once written.
func (h *Host) awaitTurn(scope execution.Scope, ref execution.Ref, caller execution.Caller) (execution.Result, error) {
	stream, err := h.exec.Subscribe(h.ctx, scope, ref.SessionID, 0)
	if err != nil {
		return h.recordedResult(scope, caller, err)
	}
	defer stream.Close()
	for {
		event, err := stream.Next(h.ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = execution.ErrSessionEnded
			}
			return h.recordedResult(scope, caller, err)
		}
		if event.Kind == execution.EventTurnEnded && event.Ref.TurnID == ref.TurnID && event.Result != nil {
			return *event.Result, nil
		}
	}
}

func (h *Host) recordedResult(scope execution.Scope, caller execution.Caller, cause error) (execution.Result, error) {
	if h.ctx.Err() != nil {
		return execution.Result{}, h.ctx.Err()
	}
	turn, err := h.exec.ReadAttempt(h.ctx, scope, caller)
	if err == nil && turn.Result != nil {
		return *turn.Result, nil
	}
	return execution.Result{}, cause
}

// end stops the node's session, as desktop's stop_session after the turn; the transcript stays
// readable. Failing to confirm is logged: the outcome is already decided by the turn's result.
func (h *Host) end(scope execution.Scope, sessionID string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), h.stopTimeout)
	defer cancel()
	if err := h.exec.End(ctx, scope, sessionID); err != nil {
		h.log.Warn("end workflow node session", "session", sessionID, "error", err)
		return err
	}
	return nil
}

// settle maps the turn's result onto the engine like desktop's report_outcome. Only an
// authoritative complete answer can succeed; the stop reason then decides as desktop does.
func (h *Host) settle(d workflow.AgentDispatch, r execution.Result, ex resolvedExecutor, cfg workflow.AgentConfig, task string) {
	if r.MCPFailure != nil {
		h.fail(d, mcpFailure(r.MCPFailure))
		return
	}
	var output *string
	if r.Answer != "" {
		output = &r.Answer
	}
	if r.Status != execution.StatusCompleted || !r.AnswerComplete {
		reason := string(r.Status)
		if r.FailureCode != "" {
			reason += ": " + r.FailureCode
		} else if r.Status == execution.StatusCompleted {
			reason = "the final answer could not be read completely"
		}
		h.fail(d, workflow.Failure{Kind: workflow.FailureSession, Message: "the Agent turn did not complete (" + reason + ")", Output: output})
		return
	}
	switch r.StopReason {
	case "end_turn", "max_tokens", "max_turn_requests":
		answer := r.Answer
		completion := workflow.Completion{Output: &answer, Keys: completionKeys(r, ex, cfg, task)}
		if cfg.Structured != nil {
			// desktop's apply_output_contract: the raw answer stays the output, the validated
			// object becomes {node}.structured_output, and a nonconforming answer fails the node
			// while keeping the answer inspectable.
			structured, err := workflow.ParseStructuredOutput(answer, cfg.Structured)
			if err != nil {
				h.fail(d, workflow.Failure{Kind: workflow.FailureStructuredOutput, Message: err.Error(), Output: &answer})
				return
			}
			completion.Structured = structured
		}
		h.complete(d, completion)
	case "refusal":
		h.fail(d, workflow.Failure{Kind: workflow.FailureAgentRefusal, Message: "agent refused the request", Output: output})
	case "":
		h.fail(d, workflow.Failure{Kind: workflow.FailureSessionEndedWithoutReason, Message: "the Agent turn ended without a stop reason", Output: output})
	default:
		h.fail(d, workflow.Failure{Kind: workflow.FailureUnknownStopReason, Message: "agent stopped for a reason this Ora version does not recognize", Output: output})
	}
}

func mcpFailure(err error) workflow.Failure {
	// Infrastructure and authorization errors are retryable execution failures. Never expose
	// their raw text (which may contain SQL or credentials) as a public MCP diagnostic.
	failure := workflow.Failure{Kind: workflow.FailureSession, Message: "execution could not resolve MCP configuration"}
	var setup *execution.MCPFailure
	if errors.As(err, &setup) && execution.IsMCPFailureCode(setup.Code) {
		failure.Kind = workflow.FailureMissingAgentConfig
		failure.Message, failure.Code = setup.Code, setup.Code
		failure.PluginID, failure.SettingID, failure.Transport = setup.PluginID, setup.SettingID, setup.Transport
	}
	return failure
}

// completionKeys are the attempt payload keys a successful Agent writes: desktop's stop_reason,
// plus what v2 adds so the run detail can show the executor actually used, the task instruction
// as rendered from the variable pool (the attempt input keeps the template) and the configured
// capabilities v2 ran without.
func completionKeys(r execution.Result, ex resolvedExecutor, cfg workflow.AgentConfig, task string) map[string]any {
	unapplied := cfg.Unapplied
	if unapplied == nil {
		unapplied = []string{}
	}
	return map[string]any{
		"stop_reason": r.StopReason,
		"executor": map[string]any{
			"id": ex.executor.ID, "agentId": ex.executor.AgentID, "name": ex.executor.AgentName,
			"pluginVersion": ex.executor.PluginVersion,
		},
		"model": map[string]any{
			"id": ex.executor.Model.ID, "name": ex.executor.Model.Name,
			"requested": cfg.ModelID, "fallback": false,
		},
		"unapplied": unapplied,
		"skills":    cfg.Skills,
		"task":      task,
	}
}

type resolvedExecutor struct {
	executor execution.Executor
}

// resolveExecutor maps the node's agentCli to an installed executor's AgentID and its modelId to
// one of that Agent's models by ID or name. An unknown model fails with desktop's
// model-not-found error; an unknown Agent keeps the existing missing-ref failure.
func (h *Host) resolveExecutor(scope execution.Scope, cfg workflow.AgentConfig) (resolvedExecutor, *workflow.Failure) {
	if cfg.AgentCLI == "" {
		return resolvedExecutor{}, &workflow.Failure{Kind: workflow.FailureMissingAgentRef, Message: "the node selects no Agent"}
	}
	executors, err := h.catalog.ListExecutors(h.ctx, scope)
	if err != nil {
		return resolvedExecutor{}, &workflow.Failure{Kind: workflow.FailureSession, Message: "list executors: " + publicReason(err)}
	}
	var first *execution.Executor
	for i := range executors {
		e := executors[i]
		if e.AgentID != cfg.AgentCLI {
			continue
		}
		if cfg.ModelID != "" && (e.Model.ID == cfg.ModelID || e.Model.Name == cfg.ModelID) {
			return resolvedExecutor{executor: e}, nil
		}
		if first == nil {
			first = &executors[i]
		}
	}
	if first == nil {
		return resolvedExecutor{}, &workflow.Failure{Kind: workflow.FailureMissingAgentRef, Message: fmt.Sprintf("agent %s is not available in v2", cfg.AgentCLI)}
	}
	modelLabel := "model"
	if cfg.ModelID != "" {
		modelLabel += " " + cfg.ModelID
	}
	return resolvedExecutor{}, &workflow.Failure{Kind: workflow.FailureWorkflowModelNotFound, Message: fmt.Sprintf("%s is not advertised by agent %s", modelLabel, cfg.AgentCLI)}
}

// publicReason names an execution error by its stable sentinel, never infrastructure detail.
func publicReason(err error) string {
	for _, known := range []error{
		execution.ErrUnavailable, execution.ErrNotFound, execution.ErrInvalidRequest, execution.ErrWorkspaceBusy,
		execution.ErrConflict, execution.ErrUnsupported, execution.ErrStopUnconfirmed, execution.ErrSessionEnded,
		context.Canceled, context.DeadlineExceeded,
	} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "execution error"
}
