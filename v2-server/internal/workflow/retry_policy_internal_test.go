package workflow

import (
	"encoding/json"
	"testing"
)

// retryPolicyOf budgets and paces retries while retryWouldRun decides whether a policy retries at
// all; both must read every policy shape the same way, with desktop's defaults
// (desktop@ebd5b725 crates/application/src/workflow_run/engine/retry.rs:22-31, 66-74).
func TestRetryPolicyAgreesWithRetryWouldRun(t *testing.T) {
	policy := func(enabled bool, max, initial string) object {
		return object{"enabled": enabled, "maxRetries": json.Number(max), "initialDelaySeconds": json.Number(initial)}
	}
	for name, tc := range map[string]struct {
		config object
		want   retryPolicy
		runs   bool
	}{
		"missing":          {object{}, retryPolicy{true, 2, 10}, true},
		"null":             {object{"retry": nil}, retryPolicy{true, 2, 10}, true},
		"enabled with 0":   {object{"retry": policy(true, "0", "5")}, retryPolicy{true, 0, 5}, false},
		"enabled with N":   {object{"retry": policy(true, "4", "7")}, retryPolicy{true, 4, 7}, true},
		"disabled":         {object{"retry": policy(false, "3", "5")}, retryPolicy{false, 3, 5}, false},
		"malformed budget": {object{"retry": object{"enabled": true, "maxRetries": "2"}}, retryPolicy{true, 0, 0}, false},
	} {
		t.Run(name, func(t *testing.T) {
			got := retryPolicyOf(tc.config)
			if got != tc.want {
				t.Fatalf("retryPolicyOf = %+v, want %+v", got, tc.want)
			}
			if would := retryWouldRun(tc.config); would != tc.runs || would != (got.enabled && got.max > 0) {
				t.Fatalf("retryWouldRun = %v, want %v; the policy %+v disagrees", would, tc.runs, got)
			}
		})
	}
}

// retryApplies is the one answer to whether v2 retries an Agent: only a policy that would retry,
// on a non-interactive Agent in the root graph.
func TestRetryAppliesOnlyToNonInteractiveRootAgents(t *testing.T) {
	interactive := object{"interactive": true, "retry": object{"enabled": true, "maxRetries": json.Number("2"), "initialDelaySeconds": json.Number("1")}}
	disabled := object{"retry": object{"enabled": false, "maxRetries": json.Number("2"), "initialDelaySeconds": json.Number("1")}}
	root, region, loop := placement{}, placement{region: "iter"}, placement{loop: "loop"}
	for name, tc := range map[string]struct {
		config  object
		at      placement
		applies bool
	}{
		"default policy at the root":     {object{}, root, true},
		"default policy in an Iteration": {object{}, region, false},
		"default policy in a Loop":       {object{}, loop, false},
		"interactive with a policy":      {interactive, root, false},
		"interactive in a Loop":          {interactive, loop, false},
		"disabled at the root":           {disabled, root, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := retryApplies(tc.config, tc.at); got != tc.applies {
				t.Fatalf("retryApplies = %v, want %v", got, tc.applies)
			}
		})
	}
}
