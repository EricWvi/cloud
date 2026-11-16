package executionruntime

import (
	"errors"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// A held session outlives a turn its Agent ended without completing, but not a result the
// runtime could not store: nobody can tell that session's caller how its turn ended, so it takes
// no further turn and its Agent is stopped like any other session's.
func TestRuntimeHeldSessionWithAnUnstoredResultTakesNoFurtherTurn(t *testing.T) {
	shortFinishRetries(t)
	f := runtimeDatabase(t, nil)
	if err := f.service.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	refused := execution.Result{Status: execution.StatusFailed, StopReason: "refusal", FailureCode: "agent_turn_incomplete", Answer: "no"}
	a := &failureAgent{result: refused, endEntered: make(chan string, 8), allowEnd: make(chan struct{})}
	a.release()
	var err error
	if f.service, err = New(f.ctx, Config{Store: f.db, Models: f.models, Agent: a}); err != nil {
		t.Fatal(err)
	}
	flaky := &flakyFinish{write: f.service.finishTurn}
	f.service.finishTurn = flaky.finish
	if _, err := f.db.CreateProject(f.ctx, f.scope, store.Project{ID: "project", Name: "Demo", RepositoryURL: "https://example.com/demo.git", Commit: "fixed", MirrorPath: "/fake/unused"}); err != nil {
		t.Fatal(err)
	}
	req := execution.StartRequest{Caller: execution.Caller{Kind: execution.CallerWorkflow, ID: "run", NodeID: "draft", AttemptID: "a1"},
		Target: execution.Target{ProjectID: "project"}, ExecutorID: "opencode:fixture", Prompt: execution.TextPrompt("run"), Lifetime: execution.LifetimeHeld}
	// A refused turn whose result is stored leaves the session open and its Agent running.
	first, err := f.service.Start(f.ctx, f.scope, req)
	if err != nil {
		t.Fatal(err)
	}
	observeTurn(t, f, first, 0)
	turn := func(attempt string) (execution.Ref, error) {
		caller := req.Caller
		caller.AttemptID = attempt
		return f.service.Append(f.ctx, f.scope, execution.AppendRequest{SessionID: first.SessionID, Caller: caller, Prompt: execution.TextPrompt("again")})
	}
	// The next refused turn cannot be stored.
	flaky.failures.Store(-1)
	if _, err := turn("a2"); err != nil {
		t.Fatalf("held session refused a turn after a stored refusal: %v", err)
	}
	if len(a.endEntered) != 0 {
		t.Fatal("a stored refusal stopped the held session's Agent")
	}
	waitUnsaved(t, f, first.SessionID)
	select {
	case sid := <-a.endEntered:
		if sid != first.SessionID {
			t.Fatalf("stopped wrong Agent: %s", sid)
		}
	default:
		t.Fatal("a held session whose result was not stored kept its physical Agent")
	}
	if _, err := turn("a3"); !errors.Is(err, execution.ErrSessionEnded) {
		t.Fatalf("held session with an unstored result accepted a turn: %v", err)
	}
}
