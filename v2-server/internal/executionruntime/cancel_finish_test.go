package executionruntime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// quietAgent returns its result without publishing a turn-ended event, so the runtime itself
// hands the terminal to the ordinary persistence path after Run returns.
type quietAgent struct{ release chan struct{} }

func (a *quietAgent) Run(ctx context.Context, _ execution.Scope, _ execution.Session, _ execution.Turn, _ func(context.Context, execution.Event) error) (execution.Result, error) {
	select {
	case <-a.release:
	case <-ctx.Done():
		return execution.Result{Status: execution.StatusCancelled}, nil
	}
	return execution.Result{Status: execution.StatusCompleted, Answer: "done", AnswerComplete: true, StopReason: "end_turn"}, nil
}

func (*quietAgent) End(context.Context, string) error { return nil }
func (*quietAgent) Rebuild(string)                    {}
func (*quietAgent) StopTurn(execution.Ref) bool       { return false }
func (*quietAgent) Reattach(string, string, string)   {}
func (*quietAgent) Close(context.Context) error       { return nil }

// A Stop that arrives after Run returned, while the worker is still persisting the terminal, must
// wait for it instead of stopping the session: the worker marks itself staged before it hands the
// result on, and is still registered for the turn while it does.
func TestCancelWhileFinishingReturnedTurnKeepsSession(t *testing.T) {
	f := runtimeDatabase(t, nil)
	if err := f.service.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	agent := &quietAgent{release: make(chan struct{})}
	var err error
	f.service, err = New(f.ctx, Config{Store: f.db, Models: f.models, Agent: agent})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.service.Close(context.Background()) })
	finishing, proceed := make(chan struct{}), make(chan struct{})
	var released sync.Once
	release := func() { released.Do(func() { close(proceed) }) }
	// Runs before the service's Close cleanup, so a failed assertion never leaves the worker parked.
	defer release()
	f.service.finishTurn = func(ctx context.Context, scope execution.Scope, ref execution.Ref, result execution.Result) (execution.Result, error) {
		close(finishing)
		<-proceed
		return f.db.FinishExecutionTurn(ctx, scope, ref, result)
	}
	if _, err := f.db.CreateProject(f.ctx, f.scope, store.Project{ID: "project", Name: "Demo", RepositoryURL: "https://example.com/demo.git", Commit: "fixed", MirrorPath: "/fake/unused"}); err != nil {
		t.Fatal(err)
	}
	ref, err := f.service.Start(f.ctx, f.scope, execution.StartRequest{Caller: modelRequest("chat", "first", "").Caller, Target: execution.Target{ProjectID: "project"}, ExecutorID: "opencode:fixture", Prompt: execution.TextPrompt("first")})
	if err != nil {
		t.Fatal(err)
	}
	close(agent.release)
	<-finishing

	type outcome struct {
		result execution.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := f.service.Cancel(f.ctx, f.scope, ref)
		done <- outcome{r, err}
	}()
	// A Stop that wrongly wins marks the session as ending at once; give it the chance to.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		f.service.mu.Lock()
		ending := f.service.ending[ref.SessionID]
		f.service.mu.Unlock()
		if ending {
			t.Fatal("Stop of a returned turn started ending its session")
		}
		time.Sleep(5 * time.Millisecond)
	}
	release()
	got := <-done
	if got.err != nil || got.result.Status != execution.StatusCompleted {
		t.Fatalf("Stop after Run returned = %+v, %v; want the completed result", got.result, got.err)
	}
	session, err := f.service.ReadSession(f.ctx, f.scope, ref.SessionID)
	if err != nil || session.EndedAt != nil {
		t.Fatalf("Stop after Run returned ended the session: %+v %v", session, err)
	}
}
