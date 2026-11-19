package server_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// fakeFormHost adds the review form to the lifecycle fake; every form call fails with err.
type fakeFormHost struct {
	fakeRunHost
	retries int
}

func (f *fakeFormHost) RetryForm(context.Context, store.Access, string) (store.WorkflowForm, error) {
	f.retries++
	return store.WorkflowForm{}, f.err
}

func (f *fakeFormHost) ReadForm(context.Context, store.Access, string) (store.WorkflowForm, error) {
	return store.WorkflowForm{}, f.err
}

func (f *fakeFormHost) SaveForm(context.Context, store.Access, string, int64, string, json.RawMessage, bool) (store.WorkflowForm, error) {
	return store.WorkflowForm{}, f.err
}

// D30: the form's task creation retry of a task's handling run is refused with its own code, so
// the run page can point back to the task instead of saying the form is closed.
func TestWorkflowFormRetryRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{"handling run", store.ErrTaskHandlingRun, "task_handling"},
		{"decomposition run", store.ErrTaskDecompositionRun, "task_decomposition"},
		{"starter unavailable", store.ErrStarterUnavailable, "starter_unavailable"},
		{"not retryable", store.ErrWorkflowRunState, "workflow_form_closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &fakeFormHost{fakeRunHost: fakeRunHost{err: tc.err}}
			call := workflowServer(t, func(o *server.Options) { o.WorkflowRuns = host })
			tid := workflowTenant(t, call)
			status, failure := call("POST", "/api/v2/tenants/"+tid+"/workflow-runs/11111111-1111-4111-8111-111111111111/form/retry", nil)
			if status != 409 || failure["code"] != tc.code || host.retries != 1 {
				t.Fatalf("form retry = %d %v after %d calls", status, failure, host.retries)
			}
		})
	}
}
