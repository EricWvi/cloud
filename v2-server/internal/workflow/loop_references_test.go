package workflow_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// The engine publishes only a Loop's named outputs, resolves its loop variables only in its
// round pool and never assigns its round Start, so references the editor used to offer must be
// refused at publication, naming the consumer and the field it came from.
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
	until := func(selector ...string) func(obj) {
		return func(g obj) {
			loopConfig(g)["until"] = obj{"logic": "and", "conditions": []any{obj{"variableSelector": selector, "operator": "not_empty"}}}
		}
	}
	// A second Loop after the fixture's, whose initial value reads selector.
	second := func(selector ...string) func(obj) {
		return func(g obj) {
			addNode(g, "loop2", "loop", obj{"loopConfig": obj{
				"maxIterations": 2,
				"variables":     []any{obj{"name": "draft", "valueType": "any", "initial": obj{"kind": "variable", "selector": selector}, "feedback": []string{"loop2", "draft"}}},
				"until":         obj{"logic": "and", "conditions": []any{}},
				"outputs":       []any{obj{"name": "final", "variableSelector": []string{"loop2", "draft"}}},
			}})
			addNode(g, "entry2", "start", obj{"containerId": "loop2"})
			connect(g, "loop", "loop2")
		}
	}
	aggregate := func(selector ...string) func(obj) {
		return func(g obj) {
			addNode(g, "agg", "aggregator", obj{"containerId": "loop", "aggregatorConfig": obj{"variables": []any{selector}}})
			connect(g, "writer", "agg")
		}
	}
	global := func(name string, edit func(obj)) func(obj) {
		return func(g obj) {
			g["globalVariables"] = []any{obj{"name": name, "valueType": "string", "value": "g"}}
			edit(g)
		}
	}
	for _, test := range []struct {
		name   string
		edit   func(obj)
		nodeID string // empty when the graph is valid
		field  string
		reason string
	}{
		{"outer named output", outer("loop", "result"), "", "", ""},
		{"outer loop.output", outer("loop", "output"), "out", "selector", "publishes only its named outputs (result); loop.output"},
		{"outer loop variable", outer("loop", "draft"), "out", "selector", "loop.draft is not one"},
		{"outer template loop.output", func(g obj) {
			addNode(g, "after", "agent", obj{"agentConfig": obj{"executor": obj{"agentCli": "claude", "modelId": "model"}, "prompt": "Use {{#loop.output#}}"}})
			connect(g, "loop", "after")
		}, "after", "agentConfig.prompt", "publishes only its named outputs"},
		{"outer condition value loop.output", func(g obj) {
			addNode(g, "branch", "condition", obj{"cases": []any{obj{"id": "c1", "logic": "and", "conditions": []any{
				obj{"variableSelector": []string{"start", "input"}, "operator": "equals", "value": "{{#loop.output#}}"},
			}}}})
			connect(g, "loop", "branch")
		}, "branch", "cases", "publishes only its named outputs"},
		{"outer aggregator loop.output", func(g obj) {
			addNode(g, "agg", "aggregator", obj{"aggregatorConfig": obj{"variables": []any{[]string{"loop", "output"}}}})
			connect(g, "loop", "agg")
		}, "agg", "selector", "publishes only its named outputs"},
		{"body loop variable", body("Improve {{#loop.draft#}} for {{#start.input#}}"), "", "", ""},
		{"body loop output", body("Improve {{#loop.result#}}"), "writer", "agentConfig.prompt", "only its loop variables (draft) are readable; loop.result"},
		{"body round start input", body("Improve {{#entry.input#}}"), "writer", "agentConfig.prompt", "round Start entry assigns no variables"},

		// The Loop's own configuration: until, feedback and outputs read the round pool.
		{"until reads a loop variable", until("loop", "draft"), "", "", ""},
		{"until reads the round Start", until("entry", "input"), "loop", "loopConfig.until", "round Start entry assigns no variables"},
		{"until reads a named output", until("loop", "result"), "loop", "loopConfig.until", "only its loop variables (draft) are readable; loop.result"},
		{"until value template reads the round Start", func(g obj) {
			loopConfig(g)["until"] = obj{"logic": "and", "conditions": []any{obj{"variableSelector": []string{"writer", "output"}, "operator": "equals", "value": "{{#entry.input#}}"}}}
		}, "loop", "loopConfig.until", "round Start entry"},
		{"feedback reads the round Start", func(g obj) { carried(g)["feedback"] = []string{"entry", "input"} }, "loop", "loopConfig.variables[0].feedback", "round Start entry"},
		{"output binds a loop variable", func(g obj) {
			loopConfig(g)["outputs"] = []any{obj{"name": "result", "variableSelector": []string{"loop", "draft"}}}
		}, "", "", ""},
		{"output binds the round Start", func(g obj) {
			loopConfig(g)["outputs"] = []any{obj{"name": "result", "variableSelector": []string{"entry", "input"}}}
		}, "loop", "loopConfig.outputs[0].variableSelector", "round Start entry"},

		// initial reads the pool the Loop starts in.
		{"initial reads the upstream named output", second("loop", "result"), "", "", ""},
		{"initial reads the upstream loop.output", second("loop", "output"), "loop2", "loopConfig.variables[0].initial", "publishes only its named outputs (result); loop.output"},
		{"initial reads its own Loop", func(g obj) {
			carried(g)["initial"] = obj{"kind": "variable", "selector": []string{"loop", "draft"}}
		}, "loop", "loopConfig.variables[0].initial", "cannot read the Loop itself"},
		{"initial reads a round Start", second("entry", "input"), "loop2", "loopConfig.variables[0].initial", "round Start entry assigns no variables; entry.input is never set"},

		// A body aggregator is typed against the body's own declarations.
		{"body aggregator reads a body product", aggregate("writer", "output"), "", "", ""},
		{"body aggregator reads a loop variable", aggregate("loop", "draft"), "agg", "aggregatorConfig.variables", "produced in the Loop body or globals; loop.draft"},
		{"body aggregator reads the Loop's upstream", aggregate("start", "input"), "agg", "aggregatorConfig.variables", "start.input is not one"},

		// A global named like a Loop variable resolves as the global.
		{"global shadowing loop.output", global("loop.output", outer("loop", "output")), "", "", ""},
		{"global shadowing the round Start", global("entry.input", body("Use {{#entry.input#}}")), "", "", ""},
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
			if !errors.As(err, &domain) || domain.Code != "invalid_loop_reference" || domain.NodeID != test.nodeID || domain.Field != test.field || !strings.Contains(domain.Reason, test.reason) {
				t.Fatalf("expected invalid_loop_reference on %s %s mentioning %q, got %v", test.nodeID, test.field, test.reason, err)
			}
		})
	}
}
