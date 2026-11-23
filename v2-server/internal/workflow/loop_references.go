package workflow

import (
	"fmt"
	"sort"
	"strings"
)

// validateLoopReferences rejects references the engine can never assign, so authors learn at
// publication, with the consumer named, what would otherwise fail at runtime as "unassigned"
// or "has no value in this round". It checks variable names; validateLoopBindings separately
// checks that the producing node is visible at all.
//
// What the engine assigns, by where a reference is resolved:
//   - Outside a Loop (root and Iteration nodes, and a Loop's `initial` selectors, which read the
//     pool the Loop starts in): only the Loop's named outputs (desktop loop_round.rs
//     exit_outputs); there is no `{loop}.output`, and a Loop's initial values cannot read that
//     Loop at all.
//   - In a Loop's round pool (its body members, and its own `feedback`, `until` and `outputs`):
//     its loop variables, not its named outputs, which exist only once the Loop finishes.
//   - A body's round Start assigns nothing, wherever it is read.
//   - An Aggregator inside a body is typed against the body's own declarations only
//     (validateDeclarations), so its candidates must be produced in that body.
//
// A global whose name happens to start with a Loop or round Start ID is resolved as the global,
// so it is never refused here.
func validateLoopReferences(d *document, globals map[string]string) error {
	type loopNames struct{ outputs, variables map[string]bool }
	loops := map[string]loopNames{}
	configs := map[string]loopConfig{}
	for _, n := range d.nodes {
		if !d.active[n.id] || n.kind != "loop" {
			continue
		}
		cfg, _ := (Node{data: n.data}).loopConfig() // validateNode already checked the shape.
		names := loopNames{outputs: map[string]bool{}, variables: map[string]bool{}}
		for _, output := range cfg.Outputs {
			names.outputs[output.Name] = true
		}
		for _, variable := range cfg.Variables {
			names.variables[variable.Name] = true
		}
		loops[n.id] = names
		configs[n.id] = cfg
	}
	if len(loops) == 0 {
		return nil
	}
	// check resolves parts as read by consumer in the round pool of Loop `round`, or outside
	// every Loop when round is "".
	check := func(consumer *node, round, field string, parts []string) error {
		if len(parts) < 2 || globals[qualified(parts)] != "" {
			return nil
		}
		producer := d.byID[parts[0]]
		if producer == nil {
			return nil
		}
		selected := strings.Join(parts, ".")
		if producer.kind == "start" && producer.container {
			hint := ""
			if round != "" {
				hint = ", read the Loop's variables instead"
			}
			return invalid("invalid_loop_reference", consumer.id, field,
				"round Start "+producer.id+" assigns no variables; "+selected+" is never set"+hint)
		}
		names, isLoop := loops[producer.id]
		switch {
		case !isLoop:
			return nil
		case producer == consumer && round == "":
			return invalid("invalid_loop_reference", consumer.id, field,
				"Loop "+producer.id+"'s initial values are read before its first round; "+selected+" cannot read the Loop itself")
		case round == producer.id:
			if !names.variables[parts[1]] {
				return invalid("invalid_loop_reference", consumer.id, field,
					"inside Loop "+producer.id+" only its loop variables ("+listNames(names.variables)+") are readable; "+selected+" is not one")
			}
		case !names.outputs[parts[1]]:
			return invalid("invalid_loop_reference", consumer.id, field,
				"Loop "+producer.id+" publishes only its named outputs ("+listNames(names.outputs)+"); "+selected+" is not one")
		}
		return nil
	}
	checkTemplate := func(consumer *node, round string, template templateRef) error {
		for _, parts := range templateSelectors(template.text) {
			if err := check(consumer, round, template.field, parts); err != nil {
				return err
			}
		}
		return nil
	}
	for _, n := range d.nodes {
		if !d.active[n.id] {
			continue
		}
		if n.kind == "loop" {
			if err := checkLoopConfig(n, configs[n.id], check, checkTemplate); err != nil {
				return err
			}
			continue
		}
		round := ""
		if n.container {
			round = n.owner
		}
		for _, parts := range n.selectors {
			if err := check(n, round, "selector", parts); err != nil {
				return err
			}
		}
		for _, template := range n.templates {
			if err := checkTemplate(n, round, template); err != nil {
				return err
			}
		}
		if n.kind == "aggregator" && round != "" {
			for _, parts := range n.aggregator {
				producer := d.byID[parts[0]]
				if globals[qualified(parts)] != "" || (producer != nil && producer.container && producer.owner == round) {
					continue
				}
				return invalid("invalid_loop_reference", n.id, "aggregatorConfig.variables",
					"inside Loop "+round+" an aggregator reads only variables produced in the Loop body or globals; "+strings.Join(parts, ".")+" is not one")
			}
		}
	}
	return nil
}

// checkLoopConfig applies check to a Loop's own selectors: `initial` reads the pool the Loop
// starts in, while `feedback`, `until` and `outputs` read the Loop's round pool.
func checkLoopConfig(
	loop *node,
	cfg loopConfig,
	check func(consumer *node, round, field string, parts []string) error,
	checkTemplate func(consumer *node, round string, template templateRef) error,
) error {
	for i, variable := range cfg.Variables {
		if variable.Initial.Kind == "variable" {
			if err := check(loop, "", fmt.Sprintf("loopConfig.variables[%d].initial", i), variable.Initial.Selector); err != nil {
				return err
			}
		}
		if err := check(loop, loop.id, fmt.Sprintf("loopConfig.variables[%d].feedback", i), variable.Feedback); err != nil {
			return err
		}
	}
	for _, clause := range cfg.until {
		for _, rule := range clause.rules {
			if err := check(loop, loop.id, "loopConfig.until", rule.selector); err != nil {
				return err
			}
		}
	}
	for _, template := range loop.templates {
		if err := checkTemplate(loop, loop.id, template); err != nil {
			return err
		}
	}
	for i, output := range cfg.Outputs {
		if err := check(loop, loop.id, fmt.Sprintf("loopConfig.outputs[%d].variableSelector", i), output.Selector); err != nil {
			return err
		}
	}
	return nil
}

// templateSelectors lists the qualified `{{#node.variable#}}` references in one template.
// Malformed templates are diagnosed at execution, as on desktop.
func templateSelectors(text string) [][]string {
	var result [][]string
	for {
		_, after, found := strings.Cut(text, "{{#")
		if !found {
			return result
		}
		selected, rest, closed := strings.Cut(after, "#}}")
		if !closed {
			return result
		}
		if strings.Contains(selected, ".") {
			result = append(result, strings.Split(selected, "."))
		}
		text = rest
	}
}

func listNames(names map[string]bool) string {
	if len(names) == 0 {
		return "none"
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	return strings.Join(sorted, ", ")
}
