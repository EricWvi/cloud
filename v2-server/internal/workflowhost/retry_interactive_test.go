package workflowhost

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// interactiveRetryGraph makes the root Agent interactive with an explicit policy that would
// retry; desktop never retries an interactive node, whatever its policy
// (desktop@ebd5b725 crates/application/src/workflow_run/engine/node_runtime.rs:373-379).
func interactiveRetryGraph() string {
	return strings.Replace(retryGraph(`{"enabled":true,"maxRetries":2,"initialDelaySeconds":0}`),
		`"roleId":"reviewer"`, `"roleId":"reviewer","interactive":true`, 1)
}

// A turn failure that would retry an automatic Agent leaves an interactive root Agent parked on
// its only attempt: a person drives the node, so no waiting attempt is inserted and the attempt is
// not soft-deleted into history. Setup failures, which do fail the node, are covered by
// TestInteractiveSetupFailuresAreNotAutoRetryable.
func TestInteractiveRootAgentNeverRetries(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.run(t, interactiveRetryGraph(), str("input"))
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	session := f.exec.awaitStart(t)
	attempt := f.exec.request(session).Caller.AttemptID
	f.exec.finish(session, execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	// The fixture reads the run before its nodes. Wait for both observations to settle;
	// seeing the newly parked node alone can accompany the preceding running run view.
	run, rows := f.await(t, id, func(run store.WorkflowRun, rows []store.WorkflowNodeRun) bool {
		return run.Status == store.WorkflowRunAwaitingInput && parked("a")(run, rows)
	})
	a := node(rows, "a")
	if run.Status != store.WorkflowRunAwaitingInput || a.ID != attempt || node(rows, "b").ID != "" {
		t.Fatalf("run %s, a %+v, b %+v; want the run awaiting input on its only attempt", run.Status, a, node(rows, "b"))
	}
	var p map[string]json.RawMessage
	if err := json.Unmarshal(a.Payload, &p); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"retry_wait", "auto_retry", "retry_chain"} {
		if _, ok := p[key]; ok {
			t.Errorf("parked interactive attempt carries %s: %s", key, a.Payload)
		}
	}
	history, err := f.s.WorkflowRunFailedAttempts(ctx, f.scope.TenantID, id)
	if err != nil || len(history) != 0 {
		t.Fatalf("history = %+v %v; an interactive turn failure must not be replaced", history, err)
	}
	if starts(f) != 1 {
		t.Fatalf("starts = %d, want the single interactive attempt", starts(f))
	}
}

// An interactive root Agent that its starter completes records the run path's unapplied list,
// which equals the analysis of its graph and leaves out both retry (no Ora retries an interactive
// node) and interactive (v2 parks a root Agent for its starter).
func TestInteractiveRootAgentDoesNotListRetry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	graph := interactiveRetryGraph()
	id := f.run(t, graph, str("input"))
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(f.exec.awaitStart(t), completed("draft"))
	f.await(t, id, parked("a"))
	if _, err := f.host.CompleteNode(ctx, f.scope, id, "a", false, "human"); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(f.exec.awaitStart(t), completed("review"))
	run, rows := f.await(t, id, terminal)
	if run.Status != "succeeded" {
		t.Fatalf("run = %s", run.Status)
	}
	var p struct {
		Unapplied []string `json:"unapplied"`
	}
	if err := json.Unmarshal(node(rows, "a").Payload, &p); err != nil {
		t.Fatal(err)
	}
	analysis, err := workflow.AnalyzeWorkflow(graph)
	if err != nil {
		t.Fatal(err)
	}
	var analyzed []string
	for _, entry := range analysis.UnappliedSettings {
		if entry.NodeID == "a" {
			analyzed = entry.Settings
		}
	}
	if !slices.Equal(p.Unapplied, analyzed) {
		t.Fatalf("payload lists %v, analysis %v; want equal lists", p.Unapplied, analyzed)
	}
	for _, setting := range []string{"retry", "interactive"} {
		if slices.Contains(p.Unapplied, setting) {
			t.Fatalf("payload lists %v; %s is applied to a root interactive Agent", p.Unapplied, setting)
		}
	}
}
