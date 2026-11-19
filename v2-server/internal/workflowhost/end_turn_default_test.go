package workflowhost

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// v2 cannot hand an Agent the host completion tool yet, so a node whose agentConfig asks for the
// agent completion policy, or for its auto-accept, runs as end_turn: the same session request and
// the same completion gate, with both settings recorded as saved but not applied.

// policyChain is interactiveChain at schemaVersion 4 with A's completion settings set to policy.
func policyChain(policy string) string {
	return `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"a","data":{"kind":"agent","title":"起草","agentConfig":{"schemaVersion":4,"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"起草：{{#start.input#}}","interactive":true,` + policy + `}}},
 {"id":"b","data":{"kind":"agent","title":"审阅","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"审阅：{{#a.output#}}"}}},
 {"id":"out","data":{"kind":"output","outputs":[{"name":"review","variableSelector":["b","output"]}]}}
],"edges":[{"source":"start","target":"a"},{"source":"a","target":"b"},{"source":"b","target":"out"}]}`
}

// sessionRequest is the request that started session id, without the caller identity that differs
// between any two attempts.
func (f *fixture) sessionRequest(id string) execution.StartRequest {
	r := f.exec.request(id)
	r.Caller = execution.Caller{}
	return r
}

// unappliedOf is the settings A's completed attempt records as saved but not applied.
func unappliedOf(t *testing.T, row store.WorkflowNodeRun) []string {
	t.Helper()
	raw, _ := payloadOf(t, row)["unapplied"].([]any)
	var out []string
	for _, v := range raw {
		out = append(out, v.(string))
	}
	return out
}

// An agent-policy node starts exactly as an end_turn node does (no host MCP server, nothing that
// would let the Agent report completion), parks through the end_turn gate, its opening turn
// awaiting the starter's confirmation, and completes on that confirmation alone.
func TestAnAgentPolicyNodeCompletesThroughTheEndTurnGate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, policyChain(`"completionSignal":"agent"`), "初稿一")
	_, endTurn := f.startParked(t, policyChain(`"completionSignal":"end_turn"`), "初稿一")
	if mcp := f.exec.request(a).MCP; mcp != nil && (len(mcp.Bindings) != 0 || len(mcp.Packages) != 0) {
		t.Fatalf("the agent-policy session carries MCP servers: %v", mcp)
	}
	if !reflect.DeepEqual(f.sessionRequest(a), f.sessionRequest(endTurn)) {
		t.Fatal("the agent-policy session request differs from the end_turn node's")
	}

	_, nodes := f.await(t, runID, parked("a"))
	p := interactivePayload(t, node(nodes, "a"))
	if snap := p["completion"].(map[string]any); snap["completionPolicy"] != "end_turn" ||
		!reflect.DeepEqual(blockerKinds(p), []string{blockerTurnUnconfirmed}) {
		t.Fatalf("summary = %v, want the end_turn gate awaiting the opening turn's confirmation", p)
	}
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", server.WorkflowNodeCompletion{}); !refusedWith(err, blockerTurnUnconfirmed) {
		t.Fatalf("plain completion = %v, want turn_unconfirmed", err)
	}
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", confirm); err != nil {
		t.Fatalf("confirmation = %v", err)
	}
	_, nodes = f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusSucceeded
	})
	row := node(nodes, "a")
	if row.Output == nil || *row.Output != "初稿一" || payloadOf(t, row)["completion_forced"] != nil {
		t.Fatalf("A = %+v, want the confirmed answer without a forced completion", row)
	}
	if got := unappliedOf(t, row); !slices.Contains(got, "completionSignal") {
		t.Fatalf("unapplied = %v, want completionSignal", got)
	}
}

// Auto-accept has nothing to accept without the completion tool: neither the opening turn nor a
// starter's turn completes the node by itself; the starter does, and the attempt records both
// settings as not applied.
func TestAutoAcceptLeavesTheNodeToTheStarter(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, policyChain(`"completionSignal":"agent","autoAcceptAgentCompletion":true`), "初稿一")
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "再改一版"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.exec.finish(a, completed("初稿二"))
	_, nodes := f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return parked("a")(store.WorkflowRun{}, n) && interactivePayload(t, node(n, "a"))["turns"] == float64(2)
	})
	if p := interactivePayload(t, node(nodes, "a")); p["completable"] != true || len(blockerKinds(p)) != 0 {
		t.Fatalf("summary = %v, want the starter's turn ready for the starter to complete", p)
	}
	f.exec.mu.Lock()
	started := len(f.exec.started)
	f.exec.mu.Unlock()
	if started != 1 {
		t.Fatalf("started %d sessions, want only A's: nothing accepted the turn and started B", started)
	}

	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", server.WorkflowNodeCompletion{}); err != nil {
		t.Fatalf("the starter's completion = %v", err)
	}
	_, nodes = f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusSucceeded
	})
	row := node(nodes, "a")
	if row.Output == nil || *row.Output != "初稿二" || payloadOf(t, row)["requester"] != requesterHuman {
		t.Fatalf("A = %+v, want the starter's completion of the latest answer", row)
	}
	if got := unappliedOf(t, row); !slices.Contains(got, "completionSignal") || !slices.Contains(got, "autoAcceptAgentCompletion") {
		t.Fatalf("unapplied = %v, want completionSignal and autoAcceptAgentCompletion", got)
	}
}
