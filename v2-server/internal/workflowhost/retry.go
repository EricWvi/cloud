package workflowhost

import (
	"context"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

var _ workflow.RetryRepository = repository{}

func (r repository) RetryNode(ctx context.Context, runID string, row workflow.NodeRun, f workflow.Failure, failed, next func(int) ([]byte, error)) (workflow.NodeRun, error) {
	run, err := r.s.WorkflowEngineContext(ctx, runID)
	if err != nil {
		return workflow.NodeRun{}, err
	}
	n, err := r.s.RetryWorkflowNodeRun(ctx, run.Run.TenantID, runID, row.ID, store.WorkflowNodeRetry{
		Failure: store.WorkflowNodeFailure{Error: f.Message, Output: f.Output}, FailurePayload: failed, BuildNextPayload: next,
	})
	return nodeRun(n), err
}

func (r repository) WakeNode(ctx context.Context, runID string, row workflow.NodeRun, payload []byte) (workflow.NodeRun, error) {
	run, err := r.s.WorkflowEngineContext(ctx, runID)
	if err != nil {
		return workflow.NodeRun{}, err
	}
	n, err := r.s.WakeWorkflowNodeRun(ctx, run.Run.TenantID, runID, row.ID, payload)
	return nodeRun(n), err
}

func (r repository) AbandonRetry(ctx context.Context, runID, nodeRunID string) (bool, error) {
	return r.s.AbandonWorkflowRetryWait(ctx, runID, nodeRunID)
}
