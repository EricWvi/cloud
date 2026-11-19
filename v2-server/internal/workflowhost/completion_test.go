package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// interactiveDeliverables is interactiveChain whose A must leave plan.md and change a file under
// out/.
const interactiveDeliverables = `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"a","data":{"kind":"agent","agentConfig":{"schemaVersion":4,"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"起草","interactive":true,
  "completionSignal":"end_turn","completion":{"files":[{"path":"plan.md"},{"path":"out/*.md","mustChange":true}]}}}},
 {"id":"b","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"审阅：{{#a.output#}}"}}}
],"edges":[{"source":"start","target":"a"},{"source":"a","target":"b"}]}`

// payloadOf decodes a node row's payload.
func payloadOf(t *testing.T, row store.WorkflowNodeRun) map[string]any {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(row.Payload, &p); err != nil {
		t.Fatalf("payload %s: %v", row.Payload, err)
	}
	return p
}

// The workflow's own first turn never completes the step by itself (first-turn ADR D1, D2): only
// confirmInitialTurn adopts it, force cannot stand in for the confirmation, the two together are
// refused, and B does not start until the starter confirms.
func TestTheFirstTurnCompletesOnlyOnceConfirmed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	_, nodes := f.await(t, runID, parked("a"))
	p := interactivePayload(t, node(nodes, "a"))
	snap := p["completion"].(map[string]any)
	if p["completable"] != false || !reflect.DeepEqual(blockerKinds(p), []string{blockerTurnUnconfirmed}) || snap["parkKind"] != parkAwaitingUser ||
		snap["turnOrigin"] != originWorkflow || p["answer"] != "初稿一" || snap["completionPolicy"] != "end_turn" {
		t.Fatalf("summary = %v, want the opening turn awaiting confirmation", p)
	}
	for name, req := range map[string]server.WorkflowNodeCompletion{"plain": {}, "force": {Force: true}} {
		if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", req); !refusedWith(err, blockerTurnUnconfirmed) {
			t.Fatalf("%s completion = %v, want turn_unconfirmed", name, err)
		}
	}
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", server.WorkflowNodeCompletion{Force: true, ConfirmInitialTurn: true}); !errors.Is(err, server.ErrWorkflowNodeCompletionInvalid) {
		t.Fatalf("confirm with force = %v, want invalid", err)
	}
	f.exec.mu.Lock()
	started := len(f.exec.started)
	f.exec.mu.Unlock()
	if started != 1 || f.exec.isClosed(a) {
		t.Fatalf("started %d sessions, A closed %v: a refused completion must not move the run", started, f.exec.isClosed(a))
	}

	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", confirm); err != nil {
		t.Fatal(err)
	}
	b := f.exec.awaitStart(t)
	if text := promptText(t, f.exec.request(b).Prompt); !strings.Contains(text, "审阅：初稿一") {
		t.Fatalf("B did not read the confirmed answer:\n%s", text)
	}
	_, nodes = f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusSucceeded
	})
	payload := payloadOf(t, node(nodes, "a"))
	if payload["requester"] != requesterHuman || payload["completion_forced"] != nil {
		t.Fatalf("payload = %v, want a human completion without a forced audit", payload)
	}
}

// A later turn the starter opened completes without confirmation.
func TestAHumanTurnCompletesWithoutConfirmation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "收紧"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.exec.finish(a, completed("定稿"))
	_, nodes := f.await(t, runID, parked("a"))
	p := interactivePayload(t, node(nodes, "a"))
	snap := p["completion"].(map[string]any)
	if p["completable"] != true || len(blockerKinds(p)) != 0 || snap["turnOrigin"] != originHuman || snap["parkKind"] != nil ||
		!reflect.DeepEqual(snap["turnKey"], map[string]any{"userSeq": float64(1), "endedSeq": float64(2)}) {
		t.Fatalf("summary = %v", p)
	}
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", server.WorkflowNodeCompletion{}); err != nil {
		t.Fatal(err)
	}
}

// Force covers how a turn ended and records what it covered (first-turn ADR D3): while the
// session is open it takes the current turn's answer, as desktop does.
func TestForcedCompletionRecordsItsAudit(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, interactiveChain, str("支付重构"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	f.exec.finish(a, agentEndedTurns["refusal"])
	f.await(t, runID, parked("a"))
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", server.WorkflowNodeCompletion{Force: true}); err != nil {
		t.Fatal(err)
	}
	b := f.exec.awaitStart(t)
	if text := promptText(t, f.exec.request(b).Prompt); !strings.Contains(text, "审阅：不行") {
		t.Fatalf("B did not read the forced answer:\n%s", text)
	}
	_, nodes := f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusSucceeded
	})
	payload := payloadOf(t, node(nodes, "a"))
	want := map[string]any{"by": "human", "blockers": []any{map[string]any{"kind": blockerTurnRefused}}, "stop_cause": nil,
		"without_turn": false, "output_source": outputTurnWindow}
	if !reflect.DeepEqual(payload["completion_forced"], want) || payload["requester"] != requesterHuman {
		t.Fatalf("audit = %v, want %v", payload["completion_forced"], want)
	}
}

// v2 stops a turn only by ending the session (until #1111), so the conversation cannot go on: the
// stopped turn blocks a plain completion as desktop's does, and force takes the last answer that
// ended normally, which nothing can improve on any more.
func TestForceAfterAStopTakesTheLastNormalAnswer(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, _ := f.startParked(t, interactiveChain, "初稿一")
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "继续"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusRunning
	})
	if _, err := f.host.StopNodeTurn(ctx, f.scope, runID, "a"); err != nil {
		t.Fatal(err)
	}
	_, nodes := f.await(t, runID, awaitingInput("a"))
	p := interactivePayload(t, node(nodes, "a"))
	if p["session_ended"] != true || !reflect.DeepEqual(blockerKinds(p), []string{blockerTurnStopped}) ||
		p["answer"] != "初稿一" || p["answer_source"] != outputLastNormal {
		t.Fatalf("summary after stop = %v", p)
	}
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", server.WorkflowNodeCompletion{}); !refusedWith(err, blockerTurnStopped) {
		t.Fatalf("plain completion after stop = %v", err)
	}
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", server.WorkflowNodeCompletion{Force: true}); err != nil {
		t.Fatal(err)
	}
	b := f.exec.awaitStart(t)
	if text := promptText(t, f.exec.request(b).Prompt); !strings.Contains(text, "审阅：初稿一") {
		t.Fatalf("B did not read the last normal answer:\n%s", text)
	}
	_, nodes = f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusSucceeded
	})
	audit := payloadOf(t, node(nodes, "a"))["completion_forced"].(map[string]any)
	if audit["output_source"] != outputLastNormal || audit["stop_cause"] != stopUserStopped {
		t.Fatalf("audit = %v", audit)
	}
}

// A completion compares the turn and answer the park recorded with the node's own, and refuses,
// force or not, when they differ (completion ADR D5).
func TestAStaleSnapshotRefusesCompletion(t *testing.T) {
	for name, tamper := range map[string]func(map[string]any){
		"turn":   func(c map[string]any) { c["turnKey"] = map[string]any{"userSeq": 0, "endedSeq": 9} },
		"output": func(c map[string]any) { c["outputDigest"] = "sha256:other" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			runID, a := f.startParked(t, interactiveChain, "初稿一")
			_, nodes := f.await(t, runID, parked("a"))
			row := node(nodes, "a")
			summary := interactivePayload(t, row)
			tamper(summary["completion"].(map[string]any))
			if _, err := f.s.MoveWorkflowEngineNode(ctx, runID, row.ID, store.WorkflowStatusPending, store.WorkflowStatusPending,
				func(node []byte) ([]byte, error) {
					return workflow.MergePayload(node, map[string]any{"interactive": summary})
				}); err != nil {
				t.Fatal(err)
			}
			_, err := f.host.CompleteNode(ctx, f.scope, runID, "a", server.WorkflowNodeCompletion{ConfirmInitialTurn: true})
			var turn *server.WorkflowNodeTurnChanged
			var output *server.WorkflowNodeOutputChanged
			if (name == "turn" && !errors.As(err, &turn)) || (name == "output" && !errors.As(err, &output)) {
				t.Fatalf("completion = %v, want the %s change", err, name)
			}
			if f.exec.isClosed(a) {
				t.Fatal("a stale completion ended the session")
			}
			if _, nodes := f.await(t, runID, parked("a")); node(nodes, "a").Status != store.WorkflowStatusPending {
				t.Fatal("a stale completion moved the node")
			}
		})
	}
}

// Declared deliverables are checked in the run's checkout on every completion: a missing file and
// a mustChange file still as it was when the node started block it, force covers them with an
// audit, and a fixed checkout completes on the next try (deliverable ADR D2-D4).
func TestDeliverablesBlockCompletion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("out/a.md", "before")
	f.host.runCheckout = func(context.Context, string, string) (deliverableCheckout, error) {
		return deliverableCheckout{WorkspaceLocation: store.WorkspaceLocation{Path: root}}, nil
	}
	runID, _ := f.startParked(t, interactiveDeliverables, "初稿一")
	_, nodes := f.await(t, runID, parked("a"))
	if baseline := payloadOf(t, node(nodes, "a"))["completion_baseline"].(map[string]any); baseline["source"] != baselineCheckout || len(baseline["declarations"].(map[string]any)) != 1 || baseline["declarations"].(map[string]any)["out/*.md"] == "" {
		t.Fatalf("baseline = %v, want out/*.md's digest recorded from the checkout", baseline)
	}
	var incomplete *server.WorkflowNodeIncomplete
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", confirm); !errors.As(err, &incomplete) ||
		!reflect.DeepEqual(incomplete.Blockers, []server.WorkflowNodeBlocker{{Kind: blockerFileMissing, Path: "plan.md"}, {Kind: blockerFileUnchanged, Path: "out/*.md"}}) {
		t.Fatalf("completion = %v, want both deliverables blocking", err)
	}
	// A symbolic link never counts as the deliverable.
	if err := os.Symlink(filepath.Join(root, "out/a.md"), filepath.Join(root, "plan.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", confirm); !errors.As(err, &incomplete) || incomplete.Blockers[0].Kind != blockerFileMissing {
		t.Fatalf("completion with a linked plan.md = %v", err)
	}
	if err := os.Remove(filepath.Join(root, "plan.md")); err != nil {
		t.Fatal(err)
	}
	write("plan.md", "计划")
	write("out/b.md", "new file") // a new match counts as a change
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", confirm); err != nil {
		t.Fatalf("completion with the deliverables in place = %v", err)
	}
}

func TestForceCoversMissingDeliverables(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	root := t.TempDir()
	f.host.runCheckout = func(context.Context, string, string) (deliverableCheckout, error) {
		return deliverableCheckout{WorkspaceLocation: store.WorkspaceLocation{Path: root}}, nil
	}
	runID, a := f.startParked(t, interactiveDeliverables, "初稿一")
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "继续"); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitAppend(t)
	f.exec.finish(a, completed("定稿"))
	f.await(t, runID, parked("a"))
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", server.WorkflowNodeCompletion{Force: true}); err != nil {
		t.Fatal(err)
	}
	_, nodes := f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusSucceeded
	})
	audit := payloadOf(t, node(nodes, "a"))["completion_forced"].(map[string]any)
	blockers := audit["blockers"].([]any)
	if len(blockers) != 2 || blockers[0].(map[string]any)["kind"] != blockerFileMissing || audit["output_source"] != outputTurnWindow {
		t.Fatalf("audit = %v, want both deliverables recorded", audit)
	}
}

// The run's first Agent has no checkout before it starts: its baseline is the commit the checkout
// is cloned at.
func TestTheBaseCommitIsTheBaselineOfTheFirstAgent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	root := t.TempDir()
	gitIn := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	gitIn("init", "-q")
	for rel, content := range map[string]string{"plan.md": "计划", "out/a.md": "before"} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755)
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn("add", ".")
	gitIn("commit", "-q", "-m", "base")
	base := gitIn("rev-parse", "HEAD")
	cloned := false
	f.host.runCheckout = func(context.Context, string, string) (deliverableCheckout, error) {
		if !cloned {
			return deliverableCheckout{}, store.ErrNotFound
		}
		return deliverableCheckout{WorkspaceLocation: store.WorkspaceLocation{Path: root, BaseCommit: base}, Mirror: filepath.Join(root, ".git")}, nil
	}
	runID, _ := f.startParked(t, interactiveDeliverables, "初稿一")
	cloned = true
	_, nodes := f.await(t, runID, parked("a"))
	if baseline := payloadOf(t, node(nodes, "a"))["completion_baseline"].(map[string]any); baseline["source"] != baselineCommit {
		t.Fatalf("baseline = %v, want the base commit", baseline)
	}
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", confirm); !refusedWith(err, blockerFileUnchanged) {
		t.Fatalf("completion = %v, want out/*.md unchanged since the base commit", err)
	}
	if err := os.WriteFile(filepath.Join(root, "out/a.md"), []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", confirm); err != nil {
		t.Fatalf("completion after the change = %v", err)
	}
}

// A restart keeps the gate: a recovered opening turn still awaits confirmation.
func TestARecoveredOpeningTurnStillNeedsConfirmation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID, a := f.startParked(t, interactiveChain, "初稿一")
	_, nodes := f.await(t, runID, parked("a"))
	f.exec.recordedTurns(a, node(nodes, "a").ID, completed("初稿一"))
	f.restart(t)
	f.await(t, runID, awaitingInput("a"))
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", server.WorkflowNodeCompletion{}); !refusedWith(err, blockerTurnUnconfirmed) {
		t.Fatalf("completion after the restart = %v, want turn_unconfirmed", err)
	}
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", confirm); err != nil {
		t.Fatalf("confirmation after the restart = %v", err)
	}
}
