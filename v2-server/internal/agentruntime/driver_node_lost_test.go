//go:build linux

package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

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

// startedSession runs a workflow session's first turn to its answer and returns its Node and
// project.
func startedSession(t *testing.T, h *ipcHarness, ctx context.Context, session execution.Session) (*fakeNodeIPC, *projectRuntime) {
	t.Helper()
	done := runWorkflowTurn(h, ctx, session, "first")
	f := receiveIPC(t, h.created)
	f.finishTurn(t, nextIPCCommand(t, f, "start_agent_session"), "first", "ready")
	if got := receiveIPC(t, done); got.err != nil {
		t.Fatalf("first turn: %+v", got)
	}
	h.driver.mu.Lock()
	p := h.driver.projects["project"]
	h.driver.mu.Unlock()
	return f, p
}

// sentCommand reports whether a command of kind is waiting in f's sent queue.
func sentCommand(f *fakeNodeIPC, kind string) bool {
	for {
		select {
		case m := <-f.sent:
			if m.Type == kind {
				return true
			}
		default:
			return false
		}
	}
}

// A turn that finds its Node already gone reports ErrNodeLost: nothing was sent, so the execution
// service may hand the same turn to a new Agent.
func TestDriverIPCTurnAfterItsNodeDiedReportsTheNodeLost(t *testing.T) {
	h := newIPCHarness(t, nil)
	ctx := ipcContext(t)
	session := h.session("held", "run-workspace")
	f, p := startedSession(t, h, ctx, session)
	_ = f.Close()
	receiveIPC(t, p.dead)

	got := receiveIPC(t, runWorkflowTurn(h, ctx, session, "second"))
	if !errors.Is(got.err, execution.ErrNodeLost) || got.result.Status != execution.StatusFailed || got.result.FailureCode != execution.FailureNodeLost {
		t.Fatalf("turn after the Node died = %+v, want ErrNodeLost with %s", got, execution.FailureNodeLost)
	}
	if sentCommand(f, "submit_user_turn") {
		t.Fatal("a turn was sent to a dead Node")
	}
}

// A Node that dies while a turn is being written may have received it: the turn fails with its
// Node but never reports ErrNodeLost, so it is not run again on a new Agent.
func TestDriverIPCTurnSentAsItsNodeDiesIsNotReportedUnsent(t *testing.T) {
	h := newIPCHarness(t, nil)
	ctx := ipcContext(t)
	session := h.session("held", "run-workspace")
	f, p := startedSession(t, h, ctx, session)
	fail := func(m node.Message) error {
		if m.Type != "submit_user_turn" {
			return nil
		}
		// The connection breaks during the write: the Node is dead before its sessions are failed.
		p.deadOnce.Do(func() { close(p.dead) })
		return net.ErrClosed
	}
	f.failSend.Store(&fail)

	got := receiveIPC(t, runWorkflowTurn(h, ctx, session, "second"))
	if errors.Is(got.err, execution.ErrNodeLost) || got.err == nil || got.result.Status != execution.StatusFailed || got.result.FailureCode != execution.FailureNodeLost {
		t.Fatalf("turn whose write failed as its Node died = %+v, want %s without ErrNodeLost", got, execution.FailureNodeLost)
	}
	_ = f.Close()
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

// Only a reattached checkout can be unknown to the Node starting on it. Node refusing a checkout
// this incarnation cloned itself as checkout_unavailable is an ordinary lost session: it is not
// reported as a gone checkout, nothing is forgotten, and the workspace's next session starts on
// the same checkout without cloning again.
func TestDriverIPCUnavailableFreshCheckoutIsAnOrdinaryFailure(t *testing.T) {
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

	done := runWorkflowTurn(h, ctx, h.session("fresh", "run-workspace"), "first")
	f := receiveIPC(t, h.created)
	clone := nextIPCCommand(t, f, "clone_repository")
	start := nextIPCCommand(t, f, "start_agent_session")
	if !commandUsesCheckout(t, start, clone.ExecutionID) {
		t.Fatal("the session did not start on the checkout it cloned")
	}
	f.terminalAs(start, "agent_failed", nodeCheckoutUnavailable)
	got := receiveIPC(t, done)
	if !errors.Is(got.err, execution.ErrSessionEnded) || !got.result.AgentLost || got.result.FailureCode != "agent_ended_before_turn" {
		t.Fatalf("fresh checkout refused as unavailable = %+v, want an ordinary agent_ended_before_turn, not %s", got, execution.FailureWorkspaceMissing)
	}
	mu.Lock()
	if len(forgotten) != 0 {
		t.Fatalf("forgotten = %v, want a checkout this Node cloned kept", forgotten)
	}
	mu.Unlock()

	done = runWorkflowTurn(h, ctx, h.session("sibling", "run-workspace"), "first")
	start = nextIPCCommand(t, f, "start_agent_session")
	if !commandUsesCheckout(t, start, clone.ExecutionID) {
		t.Fatal("the workspace's next session left the checkout it cloned")
	}
	f.finishTurn(t, start, "first", "same checkout")
	if got := receiveIPC(t, done); got.err != nil || got.result.Answer != "same checkout" {
		t.Fatalf("sibling on the same checkout: %+v", got)
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

// A session that broke on a live Node and is brought back (Recovery) runs as a new execution under
// the same session ID. When the broken execution stops afterwards, the checkout's bookkeeping of
// that ID belongs to its successor and stays: the new session is still an owner, so its Skills
// still count among the live siblings'.
func TestDriverIPCReplacedSessionLeavesItsSuccessorsOwnership(t *testing.T) {
	h := newIPCHarness(t, nil)
	ctx := ipcContext(t)
	session := h.session("held", "run-workspace")
	done := runWorkflowTurn(h, ctx, session, "first")
	f := receiveIPC(t, h.created)
	old := nextIPCCommand(t, f, "start_agent_session")
	f.finishTurn(t, old, "first", "ready")
	if got := receiveIPC(t, done); got.err != nil {
		t.Fatalf("first turn: %+v", got)
	}
	// Node rejects a command of the execution: it is broken, though not yet stopped.
	f.seq[old.ExecutionID]++
	sequence := f.seq[old.ExecutionID]
	f.in <- node.Message{Type: "session_command_rejected", Version: 1, OperationID: old.OperationID, ExecutionID: old.ExecutionID, Sequence: &sequence, Payload: []byte(`{}`)}
	h.driver.mu.Lock()
	broken := h.driver.sessions[session.ID]
	h.driver.mu.Unlock()
	for !broken.replaceable() {
		select {
		case <-ctx.Done():
			t.Fatal("the rejected execution never broke")
		case <-time.After(10 * time.Millisecond):
		}
	}

	recovered := session
	recovered.Recovery = &execution.Recovery{Handoff: "<ora_previous_conversation>…</ora_previous_conversation>"}
	done = runWorkflowTurn(h, ctx, recovered, "second")
	next := nextIPCCommand(t, f, "start_agent_session")
	if next.ExecutionID == old.ExecutionID {
		t.Fatal("the broken execution was not replaced")
	}
	f.finishTurn(t, next, "second", "again")
	if got := receiveIPC(t, done); got.err != nil {
		t.Fatalf("recovered turn: %+v", got)
	}
	f.terminal(old)
	receiveIPC(t, broken.stopped)

	p := broken.project
	p.mu.Lock()
	_, owner := p.owners[session.WorkspaceID][session.ID]
	holder := p.holders[session.ID]
	p.mu.Unlock()
	h.driver.mu.Lock()
	current := h.driver.sessions[session.ID]
	h.driver.mu.Unlock()
	if !owner || holder != current || holder == broken {
		t.Fatalf("after the replaced execution stopped: owner=%v holder is successor=%v", owner, holder == current)
	}
}

// A recovered session (Recovery: its Agent was lost with a restart or its Node) projects its Skills
// again when its new Agent starts. A live sibling holding another Skill under the same name
// refuses that start as workflow_skill_name_conflict before anything is sent, reported NotStarted:
// the execution service then fails only this turn and keeps the session for the next one, which
// starts once the sibling has ended.
func TestDriverIPCRecoveredSessionRefusedByASiblingsSkillCanRetry(t *testing.T) {
	h := newIPCHarness(t, func(f *fakeNodeIPC, m node.Message) bool {
		if m.Type == "clone_repository" {
			_ = os.MkdirAll(filepath.Join(f.project.root, "w", "r", m.ExecutionID), 0700)
		}
		return false
	})
	ctx := ipcContext(t)
	review := func(content string) *execution.Skills {
		return &execution.Skills{Documents: []execution.SkillDocument{{Name: "review", Description: "review", Content: content}}}
	}
	sibling := h.session("sibling", "run-workspace")
	sibling.Skills = review("SIBLING-REVIEW")
	done := runWorkflowTurn(h, ctx, sibling, "sibling-1")
	f := receiveIPC(t, h.created)
	siblingStart := nextIPCCommand(t, f, "start_agent_session")
	f.finishTurn(t, siblingStart, "sibling-1", "live")
	if got := receiveIPC(t, done); got.err != nil {
		t.Fatalf("sibling: %+v", got)
	}

	recovered := h.session("recovered", "run-workspace")
	recovered.Skills = review("OWN-REVIEW")
	recovered.Recovery = &execution.Recovery{Handoff: "<ora_previous_conversation>…</ora_previous_conversation>"}
	got := receiveIPC(t, runWorkflowTurn(h, ctx, recovered, "recovered-2"))
	if got.err == nil || !got.result.NotStarted || got.result.FailureCode != "workflow_skill_name_conflict" {
		t.Fatalf("recovered turn beside a clashing sibling = %+v, want it refused before starting", got)
	}
	if sentCommand(f, "start_agent_session") {
		t.Fatal("a refused recovery started an Agent")
	}

	ending := make(chan error, 1)
	go func() { ending <- h.driver.End(ctx, sibling.ID) }()
	nextIPCCommand(t, f, "end_session")
	f.terminal(siblingStart)
	if err := receiveIPC(t, ending); err != nil {
		t.Fatalf("sibling End: %v", err)
	}
	done = runWorkflowTurn(h, ctx, recovered, "recovered-3")
	start := nextIPCCommand(t, f, "start_agent_session")
	f.finishTurn(t, start, "recovered-3", "continued")
	if got := receiveIPC(t, done); got.err != nil || got.result.Answer != "continued" {
		t.Fatalf("retry after the sibling ended: %+v", got)
	}
}
