package workflowhost

import (
	"context"
	"fmt"
)

// CleanupLoopExit fences new starts in this scope before asynchronously draining its active
// turns. It never waits under the engine gate: that turn may need the gate for its late callback.
func (h *Host) CleanupLoopExit(runID, parentID, scopeID string) {
	st := h.state(runID)
	if st == nil || !st.closeScope(scopeID) {
		return
	}
	h.workers.Add(1)
	go func() {
		defer h.workers.Done()
		ctx, cancel := context.WithTimeout(h.ctx, h.stopTimeout)
		defer cancel()
		err := h.drainLoopScope(ctx, st, scopeID)
		if h.ctx.Err() != nil {
			return
		} // The next host replays the persisted exit after the old process has stopped.
		if err != nil {
			err = fmt.Errorf("LoopExit could not confirm session cleanup: %s", publicReason(err))
		}
		if finishErr := h.engine.FinishLoopExit(h.ctx, runID, parentID, scopeID, err); finishErr != nil {
			h.log.Error("settle workflow LoopExit", "run", runID, "scope", scopeID, "error", finishErr)
		}
		h.forgetFinished(runID, st)
	}()
}

// closeScope fences new starts in a Loop scope and wakes the attempts waiting for a session slot,
// so one of the scope's drops out. It reports false when the scope was already closed.
func (st *runState) closeScope(scopeID string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closedScopes == nil {
		st.closedScopes = map[string]bool{}
	}
	if st.closedScopes[scopeID] {
		return false
	}
	st.closedScopes[scopeID] = true
	st.slotsMayHaveChanged()
	return true
}

// StopFailedScope stops the turns still executing in a Loop round that a node's failure closed.
// The failure already cancelled their attempts and failed the run, so a turn left running would
// only keep changing the run's directory and holding a session slot for a result that is
// discarded. Desktop lets those turns run to their end; v2 stops them, as it does on LoopExit,
// but nothing waits for the stops: the round's outcome is already settled. Like CleanupLoopExit
// it is called under the engine gate and returns at once.
func (h *Host) StopFailedScope(runID, scopeID string) {
	st := h.state(runID)
	if st == nil || !st.closeScope(scopeID) {
		return
	}
	h.workers.Add(1)
	go func() {
		defer h.workers.Done()
		ctx, cancel := context.WithTimeout(h.ctx, h.stopTimeout)
		defer cancel()
		if _, err := h.stopTurns(ctx, st, func(a *attemptState) bool { return a.scope == scopeID }); err != nil && h.ctx.Err() == nil {
			h.log.Warn("stop the turns of a failed Loop round", "run", runID, "scope", scopeID, "error", publicReason(err))
		}
	}()
}

func (h *Host) drainLoopScope(ctx context.Context, st *runState, scopeID string) error {
	stopped, err := h.stopTurns(ctx, st, func(a *attemptState) bool { return a.scope == scopeID })
	if err != nil {
		return err
	}
	// Each attempt owns its End and is forgotten only after that call. Merely cancelling the
	// turns is not enough to advance while their session/workspace cleanup is still under way.
	for _, a := range stopped {
		select {
		case <-a.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return st.scopeCleanupError(scopeID)
}

func (st *runState) scopeCleanupError(scopeID string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.scopeCleanupErrors[scopeID]
}
