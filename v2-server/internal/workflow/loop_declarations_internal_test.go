package workflow

import (
	"os"
	"testing"
)

// Publication must not declare `{loop}.output`: the engine never assigns it. The reference
// check refuses such selectors first, so this reads the static catalog directly.
func TestLoopDeclaresOnlyNamedOutputs(t *testing.T) {
	source, err := os.ReadFile("testdata/loop.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := project(string(source))
	if err != nil {
		t.Fatal(err)
	}
	scopes, err := buildScopes(doc)
	if err != nil {
		t.Fatal(err)
	}
	globals, err := validateGlobals(doc.raw, true)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := scopes[""].declarations(globals)
	if err != nil {
		t.Fatal(err)
	}
	if _, declared := catalog["loop.output"]; declared {
		t.Error("loop.output is declared, but the engine publishes only named outputs")
	}
	if got := catalog["loop.result"]; got != (declaration{typ: "any", writer: "loop"}) {
		t.Errorf("loop.result = %+v, want the named output typed any", got)
	}
}
