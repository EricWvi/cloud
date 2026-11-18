package workflowhost

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// The completion gate of an interactive node, after desktop's default mode minus the Agent's
// completion signal (desktop 0fcc3b30 crates/backend/src/workflow/run/completion_snapshot.rs and
// interactive/completion.rs; specs decisions/desktop/core/workflow/20261021-interactive-node-
// completion.md, 20261102-first-turn-confirmation.md, 20261023-interactive-deliverable-
// contract.md). Every park records a snapshot of the settled turn: its identity, the digest of its
// answer, who opened it, why it stopped, and the blockers that keep it from completing. A
// completion re-derives the snapshot, refuses one that changed, adds the live deliverable checks,
// and lets only an explicit confirmation lift the workflow's own opening turn and only force cover
// the other blockers, recording what force covered.

// Blocker kinds, desktop's CompletionBlockerKind.
const (
	blockerTurnStopped     = "turn_stopped"
	blockerTurnTruncated   = "turn_truncated"
	blockerTurnRefused     = "turn_refused"
	blockerTurnUnknown     = "turn_unknown"
	blockerTurnUnconfirmed = "turn_unconfirmed"
	// blockerStructuredInvalid: the answer does not satisfy the node's output schema.
	blockerStructuredInvalid = "structured_invalid"
	blockerFileMissing       = "file_missing"
	blockerFileUnchanged     = "file_unchanged"
)

// Turn origins: the workflow opens the first turn, the starter every later one.
const (
	originWorkflow = "workflow"
	originHuman    = "human"
)

// Stop causes, desktop's StopCause, set only for a turn that did not settle normally.
const (
	stopUserStopped   = "user_stopped"
	stopSessionLost   = "session_crashed"
	stopTimedOut      = "prompt_timed_out"
	stopCauseUnknown  = "unknown"
	parkAwaitingUser  = "awaiting_user"
	parkAbnormal      = "abnormal"
	warningFileCheck  = "file_check_unavailable"
	requesterHuman    = "human"
	outputLastNormal  = "last_completed_turn"
	outputTurnWindow  = "turn_window"
	outputWithoutTurn = "none"
)

// completionBlocker is one reason a parked node cannot complete without force.
type completionBlocker struct {
	Kind   string         `json:"kind"`
	Params map[string]any `json:"params,omitempty"`
}

// turnKey identifies the settled turn the starter sees: UserSeq is the number of the message that
// opened it (0 for the workflow's opening turn), EndedSeq how many turns had settled with it.
type turnKey struct {
	UserSeq  int `json:"userSeq"`
	EndedSeq int `json:"endedSeq"`
}

// completionSnapshot is payload.interactive.completion, desktop's CompletionSnapshot.
type completionSnapshot struct {
	TurnKey      *turnKey            `json:"turnKey"`
	StopReason   string              `json:"stopReason,omitempty"`
	TurnOrigin   string              `json:"turnOrigin"`
	StopCause    string              `json:"stopCause,omitempty"`
	ParkKind     string              `json:"parkKind,omitempty"`
	Blockers     []completionBlocker `json:"blockers"`
	Warnings     []string            `json:"warnings"`
	OutputDigest string              `json:"outputDigest,omitempty"`
	// Policy is the policy v2 applies, end_turn until it can hand the Agent its completion tool.
	Policy string `json:"completionPolicy"`
}

// approval is what the starter explicitly decided on one completion request.
type approval struct {
	confirmInitialTurn, force bool
}

// precise reports the blockers that describe how the turn itself ended.
func precise(kind string) bool {
	switch kind {
	case blockerTurnStopped, blockerTurnTruncated, blockerTurnRefused, blockerTurnUnknown, blockerStructuredInvalid:
		return true
	}
	return false
}

// turnBlocker classifies how a settled turn ended, with the stop cause desktop records for it.
// stopped says the starter asked to stop the turn.
func turnBlocker(r execution.Result, stopped bool) (kind, cause string) {
	switch {
	case endedNormally(r):
		return "", ""
	case r.StopReason == "max_tokens" || r.StopReason == "max_turn_requests":
		return blockerTurnTruncated, ""
	case r.StopReason == "refusal":
		return blockerTurnRefused, ""
	case stopped:
		return blockerTurnStopped, stopUserStopped
	case r.AgentLost:
		return blockerTurnUnknown, stopSessionLost
	case r.FailureCode == "model_max_duration":
		// The model call ran into its time limit and the turn was cut.
		return blockerTurnStopped, stopTimedOut
	case r.Status == execution.StatusCancelled || r.StopReason == "cancelled":
		return blockerTurnStopped, stopCauseUnknown
	}
	return blockerTurnUnknown, stopCauseUnknown
}

// answerDigest is desktop's outputDigest of a final answer.
func answerDigest(answer string) string {
	sum := sha256.Sum256([]byte(answer))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// snapshot derives the completion snapshot of n's settled turn. Callers hold st.mu.
func (n *interactiveNode) snapshot() completionSnapshot {
	s := completionSnapshot{TurnOrigin: n.lastOrigin, Policy: workflow.CompletionSignalEndTurn, Blockers: []completionBlocker{}, Warnings: []string{}}
	if n.baseline != nil && n.baseline.unavailable() {
		s.Warnings = append(s.Warnings, warningFileCheck)
	}
	if n.last == nil || n.unsettled {
		// No turn settled: its result could not be read, or it never ran.
		if n.last == nil {
			s.TurnOrigin = originWorkflow
		}
		s.Blockers = append(s.Blockers, completionBlocker{Kind: blockerTurnUnknown})
		s.StopCause = stopCauseUnknown
		if n.lastStopCause != "" {
			s.StopCause = n.lastStopCause
		}
		s.ParkKind = parkKind(s.Blockers, s.StopCause)
		return s
	}
	r := *n.last
	s.TurnKey = &turnKey{UserSeq: n.lastSeq, EndedSeq: n.turns}
	s.StopReason = r.StopReason
	s.OutputDigest = answerDigest(r.Answer)
	if kind, cause := turnBlocker(r, n.lastStopCause == stopUserStopped); kind != "" {
		s.Blockers = append(s.Blockers, completionBlocker{Kind: kind})
		s.StopCause = cause
	}
	// A turn the Agent itself ended has an answer to judge; a lost one has only a fragment.
	if n.cfg.Structured != nil && !r.AgentLost && r.StopReason != "" {
		if _, err := workflow.ParseStructuredOutput(r.Answer, n.cfg.Structured); err != nil {
			s.Blockers = append(s.Blockers, completionBlocker{Kind: blockerStructuredInvalid, Params: map[string]any{"message": err.Error()}})
		}
	}
	// The workflow's own opening turn never completes the step by itself: its end_turn says the
	// reply finished, which may have been a question. A more precise blocker names the reason
	// instead (first-turn ADR D1).
	if n.lastOrigin == originWorkflow && len(s.Blockers) == 0 {
		s.Blockers = append(s.Blockers, completionBlocker{Kind: blockerTurnUnconfirmed})
	}
	s.ParkKind = parkKind(s.Blockers, s.StopCause)
	return s
}

// parkKind is desktop's park_kind: awaiting_user for the starter's participation (an unconfirmed
// opening turn, their own stop), abnormal for everything else that blocks, absent when ready.
func parkKind(blockers []completionBlocker, cause string) string {
	switch {
	case len(blockers) == 0:
		return ""
	case len(blockers) == 1 && blockers[0].Kind == blockerTurnUnconfirmed:
		return parkAwaitingUser
	case len(blockers) == 1 && blockers[0].Kind == blockerTurnStopped && cause == stopUserStopped:
		return parkAwaitingUser
	}
	return parkAbnormal
}

// gated is a completion the gate admitted: what it writes and whether force covered blockers.
type gated struct {
	answer     string
	structured any
	// forced is the audit payload.completion_forced records, nil for a normal completion.
	forced map[string]any
}

// gate decides a completion of snapshot s plus the live deliverable blockers. Callers hold st.mu.
// Only confirmInitialTurn lifts turn_unconfirmed and force covers every other blocker but never
// that one (first-turn ADR D2); both at once only make sense for the agent policy, which v2 does
// not apply yet, so they are refused as desktop refuses them without a signal gate.
func (n *interactiveNode) gate(s completionSnapshot, files []completionBlocker, a approval) (gated, error) {
	if a.force && a.confirmInitialTurn {
		return gated{}, server.ErrWorkflowNodeCompletionInvalid
	}
	effective := append(append([]completionBlocker{}, s.Blockers...), files...)
	var unfulfilled []completionBlocker
	for _, b := range effective {
		if a.confirmInitialTurn && b.Kind == blockerTurnUnconfirmed {
			continue
		}
		unfulfilled = append(unfulfilled, b)
	}
	if len(unfulfilled) == 0 {
		g := gated{answer: n.last.Answer}
		if n.cfg.Structured != nil {
			// No structured_invalid blocker, so the answer parses.
			g.structured, _ = workflow.ParseStructuredOutput(g.answer, n.cfg.Structured)
		}
		return g, nil
	}
	for _, b := range unfulfilled {
		if !a.force || b.Kind == blockerTurnUnconfirmed {
			return gated{}, incomplete(unfulfilled)
		}
	}
	// Forced. While the session is open the starter could have continued, so the current turn's
	// answer is taken as desktop takes it. Once the session ended (v2 stops a turn only by ending
	// the session, until #1111), nothing can improve on the last answer that ended normally, so
	// that one is taken when there is one; the audit says which.
	g := gated{}
	source := outputWithoutTurn
	switch {
	case n.sessionEnded && n.accepted != nil:
		g.answer, source = n.accepted.Answer, outputLastNormal
	case n.last != nil && !n.unsettled:
		g.answer, source = n.last.Answer, outputTurnWindow
	}
	if n.cfg.Structured != nil {
		if value, err := workflow.ParseStructuredOutput(g.answer, n.cfg.Structured); err == nil {
			g.structured = value
		}
	}
	g.forced = map[string]any{
		"by": requesterHuman, "blockers": unfulfilled, "stop_cause": nullable(s.StopCause),
		"without_turn": n.last == nil || n.unsettled, "output_source": source,
	}
	return g, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// incomplete is the refusal listing the blockers the request left unresolved.
func incomplete(blockers []completionBlocker) error {
	public := make([]server.WorkflowNodeBlocker, len(blockers))
	for i, b := range blockers {
		public[i] = server.WorkflowNodeBlocker{Kind: b.Kind}
		if path, ok := b.Params["path"].(string); ok {
			public[i].Path = path
		}
		if message, ok := b.Params["message"].(string); ok {
			public[i].Message = message
		}
	}
	return &server.WorkflowNodeIncomplete{Blockers: public}
}

// storedSnapshot reads the snapshot the last park wrote, nil for a node that parked before
// snapshots existed (or never parked).
func storedSnapshot(payload []byte) *completionSnapshot {
	var p struct {
		Interactive struct {
			Completion *completionSnapshot `json:"completion"`
		} `json:"interactive"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return nil
	}
	return p.Interactive.Completion
}

// stale refuses a completion whose settled turn or answer is no longer the one the last park
// recorded (completion ADR D5): force never covers it.
func stale(nodeID string, stored *completionSnapshot, live completionSnapshot) error {
	if stored == nil || stored.TurnKey == nil {
		return nil
	}
	if live.TurnKey == nil || *live.TurnKey != *stored.TurnKey {
		var actual *server.WorkflowNodeTurnKey
		if live.TurnKey != nil {
			actual = &server.WorkflowNodeTurnKey{UserSeq: live.TurnKey.UserSeq, EndedSeq: live.TurnKey.EndedSeq}
		}
		return &server.WorkflowNodeTurnChanged{NodeID: nodeID,
			Expected: &server.WorkflowNodeTurnKey{UserSeq: stored.TurnKey.UserSeq, EndedSeq: stored.TurnKey.EndedSeq}, Actual: actual}
	}
	if stored.OutputDigest != "" && stored.OutputDigest != live.OutputDigest {
		return &server.WorkflowNodeOutputChanged{NodeID: nodeID, Expected: stored.OutputDigest, Actual: live.OutputDigest}
	}
	return nil
}
