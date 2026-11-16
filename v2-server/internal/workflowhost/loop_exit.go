package workflowhost

import (
	"context"
	"fmt"
)

// CleanupLoopExit fences new starts in this scope before asynchronously draining its active
// turns. It never waits under the engine gate: that turn may need the gate for its late callback.
func (h *Host) CleanupLoopExit(runID, parentID, scopeID string) {
	st := h.state(runID)
	if st == nil {
		return
	}
	st.mu.Lock()
	if st.closedScopes == nil {
		st.closedScopes = map[string]bool{}
	}
	if st.closedScopes[scopeID] {
		st.mu.Unlock()
		return
	}
	st.closedScopes[scopeID] = true
	st.mu.Unlock()
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

func (h *Host) drainLoopScope(ctx context.Context, st *runState, scopeID string) error {
	inScope := func(a *attemptState) bool { return a.scope == scopeID }
	st.mu.Lock()
	refs, err := st.awaitStarts(ctx, inScope)
	if err != nil {
		return err
	}
	st.mu.Lock()
	var done []<-chan struct{}
	for _, a := range st.attempts {
		if inScope(a) && a.ref != nil {
			done = append(done, a.done)
		}
	}
	st.mu.Unlock()
	if err := h.cancelTurns(ctx, st.scope, refs); err != nil {
		return err
	}
	// Each attempt owns its End and is forgotten only after that call. Merely cancelling the
	// turns is not enough to advance while their session/workspace cleanup is still under way.
	for _, d := range done {
		select {
		case <-d:
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
