package workflow

import "encoding/json"

// CheckStart checks a frozen graph and caller inputs without creating a run or executing nodes.
// It shares the exact executable-graph and input policies used by StartAs and UpdateInput.
func CheckStart(graph string, input *string, values map[string]json.RawMessage) error {
	g, err := ParseGraph(graph)
	if err != nil {
		return err
	}
	start, err := validateExecutable(g)
	if err != nil {
		return err
	}
	p := NewRunPayload(g, LocaleZhCN)
	if err := p.MirrorInput(input); err != nil {
		return err
	}
	if err := p.ApplyStartValues(values); err != nil {
		return err
	}
	return p.ValidateStartInputs(start)
}
