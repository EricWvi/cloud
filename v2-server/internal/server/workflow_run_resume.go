package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// WorkflowRunResumer is the execution bridge's resume from failure, desktop's
// preview_resume / resume_from_failure with only the keep-files mode. It is optional beside
// WorkflowRunHost: a bridge without it leaves both routes answering 503.
type WorkflowRunResumer interface {
	PreviewRunResume(context.Context, WorkflowRunScope, string) (WorkflowRunResumePreview, error)
	ResumeRun(context.Context, WorkflowRunScope, string) (store.WorkflowRun, error)
}

// WorkflowRunResumePreview is what a resume would do now. Reason is empty when the run can resume.
type WorkflowRunResumePreview struct {
	Reason                  string
	RunningNodeIDs          []string
	FailedNodes             []WorkflowRunResumeFailedNode
	RestartContainerNodeIDs []string
	ClearNodeIDs            []string
}

// WorkflowRunResumeFailedNode is one failed or cancelled node a resume starts from.
// ContainerNodeID names the Loop or Iteration that restarts in its place, or is empty.
type WorkflowRunResumeFailedNode struct {
	NodeID, NodeRunID, Status, ContainerNodeID string
}

// WorkflowRunNotResumable is a refused resume, with the rule that refused it.
type WorkflowRunNotResumable struct {
	Reason         string
	RunningNodeIDs []string
}

func (e *WorkflowRunNotResumable) Error() string {
	return "workflow run cannot resume from failure: " + e.Reason
}

func (h *handler) workflowRunResumer(w http.ResponseWriter, a access) (WorkflowRunResumer, WorkflowRunScope, bool) {
	host, scope, ok := h.workflowRunHost(w, a)
	if !ok {
		return nil, scope, false
	}
	resumer, ok := host.(WorkflowRunResumer)
	if !ok {
		fault(w, http.StatusServiceUnavailable, "workflow_run_host_unavailable")
		return nil, scope, false
	}
	return resumer, scope, true
}

func orEmpty(items []string) []string {
	if items == nil {
		return []string{}
	}
	return items
}

func (h *handler) previewWorkflowRunResume(w http.ResponseWriter, r *http.Request, a access) {
	resumer, scope, ok := h.workflowRunResumer(w, a)
	if !ok {
		return
	}
	runID, ok := workflowRunID(w, r)
	if !ok {
		return
	}
	p, err := resumer.PreviewRunResume(r.Context(), scope, runID)
	if err != nil {
		workflowRunResumeFault(w, err, "preview workflow run resume")
		return
	}
	failed := make([]map[string]any, len(p.FailedNodes))
	for i, n := range p.FailedNodes {
		failed[i] = map[string]any{"nodeId": n.NodeID, "nodeRunId": n.NodeRunID, "status": n.Status, "containerNodeId": nil}
		if n.ContainerNodeID != "" {
			failed[i]["containerNodeId"] = n.ContainerNodeID
		}
	}
	var reason any
	if p.Reason != "" {
		reason = p.Reason
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resumable":               p.Reason == "",
		"reason":                  reason,
		"runningNodeIds":          orEmpty(p.RunningNodeIDs),
		"failedNodes":             failed,
		"restartContainerNodeIds": orEmpty(p.RestartContainerNodeIDs),
		"clearNodeIds":            orEmpty(p.ClearNodeIDs),
		"rollback":                "keep",
	})
}

func (h *handler) resumeWorkflowRun(w http.ResponseWriter, r *http.Request, a access) {
	resumer, scope, ok := h.workflowRunResumer(w, a)
	if !ok {
		return
	}
	runID, ok := workflowRunID(w, r)
	if !ok {
		return
	}
	run, err := resumer.ResumeRun(r.Context(), scope, runID)
	if err != nil {
		workflowRunResumeFault(w, err, "resume workflow run")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": toWorkflowRunView(run)})
}

// workflowRunResumeFault answers a refusal with desktop's workflow_run_not_resumable and the rule
// that refused; everything else follows the other lifecycle routes.
func workflowRunResumeFault(w http.ResponseWriter, err error, op string) {
	var refused *WorkflowRunNotResumable
	if errors.As(err, &refused) {
		faultWithParams(w, http.StatusConflict, "workflow_run_not_resumable", map[string]any{
			"reason": refused.Reason, "runningNodeIds": orEmpty(refused.RunningNodeIDs),
		})
		return
	}
	workflowRunLifecycleFault(w, err, op)
}
