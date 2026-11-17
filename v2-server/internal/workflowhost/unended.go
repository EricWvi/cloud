package workflowhost

import (
	"errors"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
)

// endRetryLimit caps the wait between retries of an End the execution service did not confirm.
const endRetryLimit = 30 * time.Second

// linger keeps the slot of a session whose End was not confirmed after its owner stopped tracking
// it: Node may still hold the session on the run's checkout, and a start admitted in its place
// could be refused with checkout_session_limit. It reports whether the session was added, so the
// caller starts its retry (see retryEnd) once it released mu. Callers hold mu.
func (st *runState) linger(session string) bool {
	if st.unended == nil {
		st.unended = map[string]bool{}
	}
	if st.unended[session] {
		return false
	}
	st.unended[session] = true
	return true
}

// retryEnd ends a lingering session, at once when now is set (nobody tried yet) and otherwise
// after endRetry, then again backing off up to endRetryLimit, until the execution service confirms
// it or the host shuts down. Only then does the session give its slot back; the run's state is
// kept until then (see forgetFinished).
func (h *Host) retryEnd(st *runState, runID, session string, now bool) {
	h.endUntilConfirmed(st, session, now, func() bool { return false }, func() {
		st.mu.Lock()
		delete(st.unended, session)
		st.slotsMayHaveChanged()
		st.mu.Unlock()
		h.forgetFinished(runID, st)
	})
}

// endUntilConfirmed calls End in the background with retryEnd's schedule until it is confirmed,
// then runs confirmed. It stops early, without calling confirmed, when settled reports that
// someone else confirmed the End meanwhile, or when the host shuts down.
func (h *Host) endUntilConfirmed(st *runState, session string, now bool, settled func() bool, confirmed func()) {
	h.workers.Add(1)
	go func() {
		defer h.workers.Done()
		delay := h.endRetry
		first := delay
		if now {
			first = 0
		}
		timer := time.NewTimer(first)
		defer timer.Stop()
		for {
			select {
			case <-h.ctx.Done():
				return
			case <-timer.C:
			}
			// The timer and the shutdown may be ready together; an End now would hold Close up
			// for a whole stop timeout.
			if h.ctx.Err() != nil || settled() {
				return
			}
			if endConfirmed(h.end(st.scope, session)) {
				break
			}
			delay = min(delay*2, endRetryLimit)
			timer.Reset(delay)
		}
		confirmed()
	}()
}

// endConfirmed reports whether an End's outcome proves Node no longer holds the session: it
// succeeded, or the session is unknown or already ended.
func endConfirmed(err error) bool {
	return err == nil || errors.Is(err, execution.ErrNotFound) || errors.Is(err, execution.ErrSessionEnded)
}

// endInteractive ends n's session and, once End is confirmed, gives its slot back. Until then the
// node keeps its slot and End is retried in the background: a stopped turn may leave nothing to
// complete, so no completion would end it again. Whoever stops tracking the node meanwhile hands
// the session to linger instead (see dropInteractive).
func (h *Host) endInteractive(st *runState, n *interactiveNode) {
	if !endConfirmed(h.end(st.scope, n.session)) {
		st.mu.Lock()
		retrying := n.endRetrying
		n.endRetrying = true
		st.mu.Unlock()
		if retrying {
			return
		}
		settled := func() bool {
			st.mu.Lock()
			defer st.mu.Unlock()
			return n.sessionGone || st.interactive[n.d.Node.ID] != n
		}
		h.endUntilConfirmed(st, n.session, false, settled, func() { h.sessionGone(st, n) })
		return
	}
	h.sessionGone(st, n)
}

// sessionGone records that Node no longer holds n's session and gives its slot back.
func (h *Host) sessionGone(st *runState, n *interactiveNode) {
	st.mu.Lock()
	n.sessionGone = true
	st.slotsMayHaveChanged()
	st.mu.Unlock()
}

// dropInteractive stops tracking n, if it is still tracked. A session whose End was not confirmed
// keeps its slot as a lingering session; it reports whether the caller must start its retry.
// Dropping n never frees a slot by itself: a gone session gave its slot back (and woke the
// waiters) when its End was confirmed. Callers hold mu.
func (st *runState) dropInteractive(n *interactiveNode) bool {
	if st.interactive[n.d.Node.ID] != n {
		return false
	}
	delete(st.interactive, n.d.Node.ID)
	return !n.sessionGone && st.linger(n.session)
}

// forgetInteractive is dropInteractive for a caller not holding mu, which then ends a session
// still held: at once when endNow is set (End was not tried yet), otherwise after the retry delay.
func (h *Host) forgetInteractive(st *runState, n *interactiveNode, endNow bool) {
	st.mu.Lock()
	retry := st.dropInteractive(n)
	st.mu.Unlock()
	if retry {
		h.retryEnd(st, n.d.RunID, n.session, endNow)
	}
}
