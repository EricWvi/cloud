package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// WorkflowNodeTurns is the execution bridge's interactive Agent session: a human turn sent to a
// parked node and stopping the turn it is executing. It is separate from WorkflowRunHost because
// these routes are v2-only (api/openapi.json has no node-level turn or stop route); a bridge that
// does not implement it keeps them unavailable.
type WorkflowNodeTurns interface {
	SendNodeTurn(ctx context.Context, scope WorkflowRunScope, runID, nodeID, content string) (store.WorkflowRun, error)
	StopNodeTurn(ctx context.Context, scope WorkflowRunScope, runID, nodeID string) (store.WorkflowRun, error)
}

var (
	// ErrWorkflowRunOwned reports a turn, stop or completion by someone other than the run's
	// starter. The node's session belongs to the starter, so only they can act on it.
	ErrWorkflowRunOwned = errors.New("workflow run started by someone else")
	// ErrWorkflowNodeSessionEnded reports a turn sent to a parked node whose session was stopped.
	ErrWorkflowNodeSessionEnded = errors.New("workflow node session ended")
	// ErrWorkflowNodeTurnNotRunning reports a stop while the node executes no turn.
	ErrWorkflowNodeTurnNotRunning = errors.New("workflow node turn is not running")
)

// WorkflowNodeIncomplete refuses completing a parked node whose turns give no answer to accept.
// Reason is a stable code; Detail is a safe, human-readable explanation.
type WorkflowNodeIncomplete struct {
	Reason string
	Detail string
}

func (e *WorkflowNodeIncomplete) Error() string {
	return fmt.Sprintf("workflow node is incomplete (%s): %s", e.Reason, e.Detail)
}

// maxNodeTurnRunes bounds one human turn like a chat message, so a turn cannot carry a document.
const maxNodeTurnRunes = 16000

func (h *handler) workflowNodeTurns(w http.ResponseWriter, r *http.Request, a access) (WorkflowNodeTurns, WorkflowRunScope, string, string, bool) {
	host, scope, ok := h.workflowRunHost(w, a)
	if !ok {
		return nil, scope, "", "", false
	}
	turns, ok := host.(WorkflowNodeTurns)
	if !ok {
		fault(w, http.StatusServiceUnavailable, "workflow_run_host_unavailable")
		return nil, scope, "", "", false
	}
	runID, ok := workflowRunID(w, r)
	if !ok {
		return nil, scope, "", "", false
	}
	nodeID := r.PathValue("nodeId")
	if nodeID == "" {
		fault(w, http.StatusNotFound, "workflow_node_not_found")
		return nil, scope, "", "", false
	}
	return turns, scope, runID, nodeID, true
}

// sendWorkflowNodeTurn queues a human turn on a parked interactive node. The answer arrives over
// the node's session events; the response only confirms the node is running the turn.
func (h *handler) sendWorkflowNodeTurn(w http.ResponseWriter, r *http.Request, a access) {
	turns, scope, runID, nodeID, ok := h.workflowNodeTurns(w, r, a)
	if !ok {
		return
	}
	var in struct {
		Content string `json:"content"`
	}
	if !decodeWorkflow(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Content) == "" || utf8.RuneCountInString(in.Content) > maxNodeTurnRunes {
		fault(w, http.StatusBadRequest, "invalid_content")
		return
	}
	run, err := turns.SendNodeTurn(r.Context(), scope, runID, nodeID, in.Content)
	if err != nil {
		workflowRunLifecycleFault(w, err, "send workflow node turn")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": toWorkflowRunView(run)})
}

// stopWorkflowNodeTurn stops the turn an interactive node is executing. The pinned Node can only
// stop a turn by ending its session, so the node parks without further turns; the run goes on.
func (h *handler) stopWorkflowNodeTurn(w http.ResponseWriter, r *http.Request, a access) {
	turns, scope, runID, nodeID, ok := h.workflowNodeTurns(w, r, a)
	if !ok {
		return
	}
	run, err := turns.StopNodeTurn(r.Context(), scope, runID, nodeID)
	if err != nil {
		workflowRunLifecycleFault(w, err, "stop workflow node turn")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": toWorkflowRunView(run)})
}

// interactiveNodeFault maps the interactive refusals; false leaves err to the lifecycle mapping.
func interactiveNodeFault(w http.ResponseWriter, err error) bool {
	var incomplete *WorkflowNodeIncomplete
	switch {
	case errors.Is(err, ErrWorkflowRunOwned):
		fault(w, http.StatusConflict, "run_owned")
	case errors.Is(err, ErrWorkflowNodeSessionEnded):
		fault(w, http.StatusConflict, "workflow_node_session_ended")
	case errors.Is(err, ErrWorkflowNodeTurnNotRunning):
		fault(w, http.StatusConflict, "workflow_node_turn_not_running")
	case errors.As(err, &incomplete):
		writeJSON(w, http.StatusConflict, map[string]any{
			"code":      "workflow_node_incomplete",
			"params":    map[string]any{"reason": incomplete.Reason, "detail": incomplete.Detail},
			"requestId": requestID(),
		})
	default:
		return false
	}
	return true
}
