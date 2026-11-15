package workflow_test

import (
	"slices"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

func TestRetryUnappliedOnlyOutsideEligibleRootAgents(t *testing.T) {
	policy := obj{"enabled": true, "maxRetries": 2, "initialDelaySeconds": 10}
	for _, interactive := range []bool{false, true} {
		graph := flat()
		addNode(graph, "a", "agent", obj{"agentConfig": obj{"retry": policy, "interactive": interactive}})
		connect(graph, "start", "a")
		g, err := workflow.ParseGraph(encode(t, graph))
		if err != nil {
			t.Fatal(err)
		}
		// A root Agent's retry applies; an interactive one never retries, which is not a v2 gap.
		n, _ := g.Node("a")
		if slices.Contains(n.Agent().Unapplied, "retry") {
			t.Fatalf("interactive %v: root Agent lists retry as unapplied: %v", interactive, n.Agent().Unapplied)
		}
	}
	graph := fixture(t, "loop")
	dataOf(graph, "writer")["agentConfig"] = obj{"retry": policy}
	g, err := workflow.ParseGraph(encode(t, graph))
	if err != nil {
		t.Fatal(err)
	}
	body, ok := g.LoopBody("loop")
	if !ok {
		t.Fatal("missing loop")
	}
	n, _ := body.Node("writer")
	if !slices.Contains(n.Agent().Unapplied, "retry") {
		t.Fatal("Loop retry lost unapplied marker")
	}
	graph = fixture(t, "iteration")
	// The fixture's Agent member is discovered from the executable graph.
	g, err = workflow.ParseGraph(encode(t, graph))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range g.Nodes() {
		if n.Kind == "agent" && n.RegionOwner != "" {
			dataOf(graph, n.ID)["agentConfig"] = obj{"retry": policy}
			g, err = workflow.ParseGraph(encode(t, graph))
			if err != nil {
				t.Fatal(err)
			}
			member, _ := g.Node(n.ID)
			if !slices.Contains(member.Agent().Unapplied, "retry") {
				t.Fatal("Iteration retry lost unapplied marker")
			}
			return
		}
	}
	t.Fatal("fixture has no Iteration Agent")
}
