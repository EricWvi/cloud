//go:build linux

package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/node"
)

// runWorkflowTurn runs one turn of a workflow session through the driver.
func runWorkflowTurn(h *ipcHarness, ctx context.Context, session execution.Session, turn string) <-chan ipcRunOutcome {
	done := make(chan ipcRunOutcome, 1)
	go func() {
		result, err := h.driver.Run(ctx, h.scope, session, execution.Turn{
			Ref: execution.Ref{SessionID: session.ID, TurnID: turn}, Prompt: execution.TextPrompt("prompt"),
			Caller: execution.Caller{Kind: execution.CallerWorkflow, ID: "run", AttemptID: turn},
		}, discardIPCEvent)
		done <- ipcRunOutcome{result, err}
	}()
	return done
}

// A reattached checkout whose clone execution Node no longer knows is gone for Node: the session
// fails as workspace_missing, its record is dropped, and the workspace's next session clones again.
func TestDriverIPCUnknownReattachedCheckoutIsForgotten(t *testing.T) {
	h := newIPCHarness(t, nil)
	ctx := ipcContext(t)
	var mu sync.Mutex
	var forgotten [][2]string
	h.driver.opts.ForgetCheckout = func(_ context.Context, workspace, clone string) error {
		mu.Lock()
		defer mu.Unlock()
		forgotten = append(forgotten, [2]string{workspace, clone})
		return nil
	}
	h.driver.Reattach("run-workspace", "c-earlier", t.TempDir())

	done := runWorkflowTurn(h, ctx, h.session("recovered", "run-workspace"), "first")
	f := receiveIPC(t, h.created)
	start := nextIPCCommand(t, f, "start_agent_session")
	if !commandUsesCheckout(t, start, "c-earlier") {
		t.Fatal("the session did not start on the reattached checkout")
	}
	f.terminalAs(start, "agent_failed", nodeCheckoutUnavailable)
	got := receiveIPC(t, done)
	if !errors.Is(got.err, execution.ErrSessionEnded) || !got.result.AgentLost || got.result.FailureCode != execution.FailureWorkspaceMissing {
		t.Fatalf("session on an unknown checkout = %+v, want it ended as %s", got, execution.FailureWorkspaceMissing)
	}
	mu.Lock()
	if len(forgotten) != 1 || forgotten[0] != [2]string{"run-workspace", "c-earlier"} {
		t.Fatalf("forgotten = %v, want the reattached checkout", forgotten)
	}
	mu.Unlock()
	h.driver.mu.Lock()
	_, kept := h.driver.recorded["run-workspace"]
	h.driver.mu.Unlock()
	if kept {
		t.Fatal("the unknown checkout is still reattached")
	}

	done = runWorkflowTurn(h, ctx, h.session("successor", "run-workspace"), "first")
	nextIPCCommand(t, f, "clone_repository")
	start = nextIPCCommand(t, f, "start_agent_session")
	if commandUsesCheckout(t, start, "c-earlier") {
		t.Fatal("the successor started on the forgotten checkout")
	}
	f.finishTurn(t, start, "first", "fresh clone")
	if got := receiveIPC(t, done); got.err != nil || got.result.Answer != "fresh clone" {
		t.Fatalf("successor on a fresh clone: %+v", got)
	}
}

// commandUsesCheckout reports whether a start_agent_session command names the clone execution.
func commandUsesCheckout(t *testing.T, command node.Message, clone string) bool {
	t.Helper()
	var payload struct {
		Spec struct {
			Checkout string `json:"checkout_execution_id"`
		}
	}
	if err := json.Unmarshal(command.Payload, &payload); err != nil || payload.Spec.Checkout == "" {
		t.Fatalf("start without a checkout: %v", err)
	}
	return payload.Spec.Checkout == clone
}
