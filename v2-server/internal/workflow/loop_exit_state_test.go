package workflow

import (
	"encoding/json"
	"testing"
)

func TestLoopExitStateRejectsMalformedFrozenFacts(t *testing.T) {
	for _, tc := range []struct {
		name, field, value string
	}{
		{"missing node", "nodeRunId", ""},
		{"null node", "nodeRunId", "null"},
		{"non-string node", "nodeRunId", "1"},
		{"missing time", "requestedAt", ""},
		{"null time", "requestedAt", "null"},
		{"string time", "requestedAt", `"1"`},
		{"fractional time", "requestedAt", "1.5"},
		{"overflow time", "requestedAt", "9223372036854775808"},
		{"missing result", "result", ""},
		{"null result", "result", "null"},
		{"array result", "result", "[]"},
		{"empty result", "result", "{}"},
		{"unknown variant", "result", `{"Bogus":{}}`},
		{"two variants", "result", `{"Ok":{},"Err":"failed"}`},
		{"null output", "result", `{"Ok":null}`},
		{"array output", "result", `{"Ok":[]}`},
		{"string output", "result", `{"Ok":"value"}`},
		{"null error", "result", `{"Err":null}`},
		{"object error", "result", `{"Err":{}}`},
		{"number error", "result", `{"Err":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exit := map[string]json.RawMessage{
				"nodeRunId": json.RawMessage(`"exit"`), "requestedAt": json.RawMessage("0"), "result": json.RawMessage(`{"Ok":{}}`),
			}
			if tc.value == "" {
				delete(exit, tc.field)
			} else {
				exit[tc.field] = json.RawMessage(tc.value)
			}
			raw, err := json.Marshal(map[string]any{"variablePool": newPool(), "exit": exit})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeLoopState(raw); err == nil || HasPendingLoopExit(raw) {
				t.Fatalf("malformed exit accepted by runtime or boot: %s, %v", raw, err)
			}
		})
	}
}

func TestLoopExitStatePreservesFrozenResult(t *testing.T) {
	for _, result := range []string{`{"Ok":{}}`, `{"Ok":{"count":9007199254740993}}`, `{"Err":"export failed"}`, `{"Err":""}`} {
		t.Run(result, func(t *testing.T) {
			raw := []byte(`{"variablePool":{"catalog":{},"values":{}},"exit":{"nodeRunId":"exit","requestedAt":0,"result":` + result + `}}`)
			state, err := decodeLoopState(raw)
			if err != nil || !HasPendingLoopExit(raw) || state.Exit.NodeRunID != "exit" || state.Exit.RequestedAt != 0 {
				t.Fatalf("valid exit rejected: %+v, %v", state, err)
			}
			encoded, err := json.Marshal(state.Exit.Result)
			if err != nil || string(encoded) != result {
				t.Fatalf("frozen result changed: %s, %v", encoded, err)
			}
		})
	}
}
