package workflow

import (
	"sort"
	"strings"
)

// validateLoopReferences rejects references the engine can never assign, so authors learn at
// publication, with the consumer named, what would otherwise fail at runtime as "unassigned".
// The engine publishes only a Loop's named outputs to the outer scope (desktop loop_round.rs
// exit_outputs), resolves its carried variables only inside its body, and never assigns
// anything to a body's round Start. A Loop's own configuration is validated by
// validateLoopBindings and is skipped here.
func validateLoopReferences(d *document) error {
	type loopNames struct{ outputs, variables map[string]bool }
	loops := map[string]loopNames{}
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
	}
	if len(loops) == 0 {
		return nil
	}
	check := func(n *node, field string, parts []string) error {
		producer := d.byID[parts[0]]
		if producer == nil || producer == n || len(parts) < 2 {
			return nil
		}
		selected := strings.Join(parts, ".")
		if producer.kind == "start" && producer.container {
			return invalid("invalid_loop_reference", n.id, field,
				"round Start "+producer.id+" assigns no variables; "+selected+" is never set, read the Loop's variables instead")
		}
		names, isLoop := loops[producer.id]
		if !isLoop {
			return nil
		}
		if n.container && n.owner == producer.id {
			if !names.variables[parts[1]] {
				return invalid("invalid_loop_reference", n.id, field,
					"inside Loop "+producer.id+" only its loop variables ("+listNames(names.variables)+") are readable; "+selected+" is not one")
			}
			return nil
		}
		if !names.outputs[parts[1]] {
			return invalid("invalid_loop_reference", n.id, field,
				"Loop "+producer.id+" publishes only its named outputs ("+listNames(names.outputs)+"); "+selected+" is not one")
		}
		return nil
	}
	for _, n := range d.nodes {
		if !d.active[n.id] || n.kind == "loop" {
			continue
		}
		for _, parts := range n.selectors {
			if err := check(n, "selector", parts); err != nil {
				return err
			}
		}
		for _, text := range n.templates {
			for _, parts := range templateSelectors(text) {
				if err := check(n, "template", parts); err != nil {
					return err
				}
			}
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
