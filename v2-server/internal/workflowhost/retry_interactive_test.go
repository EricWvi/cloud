package workflowhost

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// interactiveRetryGraph makes the root Agent a interactive with an explicit policy that would
// retry; desktop never retries an interactive node, whatever its policy
// (desktop@ebd5b725 crates/application/src/workflow_run/engine/node_runtime.rs:373-379).
func interactiveRetryGraph() string {
	return strings.Replace(retryGraph(`{"enabled":true,"maxRetries":2,"initialDelaySeconds":0}`),
		`"roleId":"reviewer"`, `"roleId":"reviewer","interactive":true`, 1)
}

// A retryable failure of an interactive root Agent fails the attempt and the run: no waiting
// attempt is inserted and the attempt is not soft-deleted into history.
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
	run, rows := f.await(t, id, terminal)
	a := node(rows, "a")
	if run.Status != "failed" || a.ID != attempt || a.Status != "failed" || node(rows, "b").ID != "" {
		t.Fatalf("run %s, a %+v, b %+v; want the run failed on its only attempt", run.Status, a, node(rows, "b"))
	}
	var p map[string]json.RawMessage
	if err := json.Unmarshal(a.Payload, &p); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"retry_wait", "auto_retry", "retry_chain"} {
		if _, ok := p[key]; ok {
			t.Errorf("failed interactive attempt carries %s: %s", key, a.Payload)
		}
	}
	history, err := f.s.WorkflowRunFailedAttempts(ctx, f.scope.TenantID, id)
	if err != nil || len(history) != 0 {
		t.Fatalf("history = %+v %v; an interactive failure must not be replaced", history, err)
	}
	if starts(f) != 1 {
		t.Fatalf("starts = %d, want the single interactive attempt", starts(f))
	}
}

// An interactive root Agent that succeeds records the run path's unapplied list, which equals the
// analysis of its graph and leaves retry out: it is not a setting v2 skips, since no Ora retries
// an interactive node.
func TestInteractiveRootAgentDoesNotListRetry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	graph := interactiveRetryGraph()
	id := f.run(t, graph, str("input"))
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(f.exec.awaitStart(t), completed("draft"))
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
	if slices.Contains(p.Unapplied, "retry") || slices.Contains(analyzed, "retry") || !slices.Equal(p.Unapplied, analyzed) {
		t.Fatalf("payload lists %v, analysis %v; want equal lists without retry", p.Unapplied, analyzed)
	}
	if !slices.Contains(p.Unapplied, "interactive") {
		t.Fatalf("payload lists %v; the interactive mode itself is still unapplied", p.Unapplied)
	}
}
