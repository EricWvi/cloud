package workflowhost

import "time"

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
			if h.end(st.scope, session) == nil {
				break
			}
			delay = min(delay*2, endRetryLimit)
			timer.Reset(delay)
		}
		st.mu.Lock()
		delete(st.unended, session)
		st.slotsMayHaveChanged()
		st.mu.Unlock()
		h.forgetFinished(runID, st)
	}()
}

// endInteractive ends n's session and, once End is confirmed, gives its slot back. An unconfirmed
// End leaves the node holding its slot: whoever stops tracking it hands the session to linger
// (see dropInteractive).
func (h *Host) endInteractive(st *runState, n *interactiveNode) {
	if h.end(st.scope, n.session) != nil {
		return
	}
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
