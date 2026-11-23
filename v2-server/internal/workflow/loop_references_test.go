package workflow_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// The engine publishes only a Loop's named outputs and never assigns its round Start, so
// references the editor used to offer must be refused at publication, naming the consumer.
func TestLoopReferences(t *testing.T) {
	outer := func(selector ...string) func(obj) {
		return func(g obj) {
			addNode(g, "out", "output", obj{"outputs": []any{obj{"name": "r", "variableSelector": selector}}})
			connect(g, "loop", "out")
		}
	}
	body := func(prompt string) func(obj) {
		return func(g obj) { dataOf(g, "writer")["agentConfig"].(obj)["prompt"] = prompt }
	}
	for _, test := range []struct {
		name   string
		edit   func(obj)
		nodeID string // empty when the graph is valid
		reason string
	}{
		{"outer named output", outer("loop", "result"), "", ""},
		{"outer loop.output", outer("loop", "output"), "out", "publishes only its named outputs (result); loop.output"},
		{"outer loop variable", outer("loop", "draft"), "out", "loop.draft is not one"},
		{"outer template loop.output", func(g obj) {
			addNode(g, "after", "agent", obj{"agentConfig": obj{"executor": obj{"agentCli": "claude", "modelId": "model"}, "prompt": "Use {{#loop.output#}}"}})
			connect(g, "loop", "after")
		}, "after", "publishes only its named outputs"},
		{"outer aggregator loop.output", func(g obj) {
			addNode(g, "agg", "aggregator", obj{"aggregatorConfig": obj{"variables": []any{[]string{"loop", "output"}}}})
			connect(g, "loop", "agg")
		}, "agg", "publishes only its named outputs"},
		{"body loop variable", body("Improve {{#loop.draft#}} for {{#start.input#}}"), "", ""},
		{"body loop output", body("Improve {{#loop.result#}}"), "writer", "only its loop variables (draft) are readable; loop.result"},
		{"body round start input", body("Improve {{#entry.input#}}"), "writer", "round Start entry assigns no variables"},
	} {
		t.Run(test.name, func(t *testing.T) {
			g := fixture(t, "loop")
			test.edit(g)
			err := workflow.ValidateWorkflow(encode(t, g))
			if test.nodeID == "" {
				if err != nil {
					t.Fatalf("expected a publishable graph, got %v", err)
				}
				return
			}
			var domain *workflow.ValidationError
			if !errors.As(err, &domain) || domain.Code != "invalid_loop_reference" || domain.NodeID != test.nodeID || !strings.Contains(domain.Reason, test.reason) {
				t.Fatalf("expected invalid_loop_reference on %s mentioning %q, got %v", test.nodeID, test.reason, err)
			}
		})
	}
}
