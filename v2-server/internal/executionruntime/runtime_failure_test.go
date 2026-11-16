package executionruntime

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// failureAgent controls only the stop-confirmation boundary. It is not Node evidence.
type failureAgent struct {
	result         execution.Result
	runErr, endErr error
	publishFirst   bool
	endEntered     chan string
	allowEnd       chan struct{}
	once           sync.Once
	mu             sync.Mutex
	rebuilt        []string
}

func (a *failureAgent) release() { a.once.Do(func() { close(a.allowEnd) }) }
func (a *failureAgent) Run(ctx context.Context, _ execution.Scope, _ execution.Session, _ execution.Turn, emit func(context.Context, execution.Event) error) (execution.Result, error) {
	if a.publishFirst {
		if err := emit(ctx, execution.Event{Kind: execution.EventTurnEnded, Result: &a.result}); err != nil {
			return execution.Result{}, err
		}
	}
	return a.result, a.runErr
}
func (a *failureAgent) End(ctx context.Context, sid string) error {
	select {
	case a.endEntered <- sid:
	default:
	}
	select {
	case <-a.allowEnd:
		return a.endErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (a *failureAgent) Rebuild(sid string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rebuilt = append(a.rebuilt, sid)
}
func (a *failureAgent) rebuilds() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.rebuilt)
}
func (a *failureAgent) Close(context.Context) error { return nil }

func TestRuntimeAgentFailureRequiresStopBeforeSessionEnd(t *testing.T) {
	for _, tc := range []struct {
		name           string
		result         execution.Result
		runErr, endErr error
		publishFirst   bool
	}{
		{name: "failed_result", result: execution.Result{Status: execution.StatusFailed, Answer: "partial", FailureCode: "fixture_failed"}},
		{name: "run_error_after_durable_success", result: execution.Result{Status: execution.StatusCompleted, Answer: "complete", AnswerComplete: true, StopReason: "end_turn"}, runErr: errors.New("fixture transport ended"), publishFirst: true},
		{name: "stop_unconfirmed", result: execution.Result{Status: execution.StatusFailed, Answer: "partial", FailureCode: "fixture_failed"}, endErr: execution.ErrStopUnconfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := runtimeDatabase(t, nil)
			if err := f.service.Close(f.ctx); err != nil {
				t.Fatal(err)
			}
			a := &failureAgent{result: tc.result, runErr: tc.runErr, endErr: tc.endErr, publishFirst: tc.publishFirst, endEntered: make(chan string, 8), allowEnd: make(chan struct{})}
			t.Cleanup(a.release)
			var err error
			f.service, err = New(f.ctx, Config{Store: f.db, Models: f.models, Agent: a})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.CreateProject(f.ctx, f.scope, store.Project{ID: "project", Name: "Demo", RepositoryURL: "https://example.com/demo.git", Commit: "fixed", MirrorPath: "/fake/unused"}); err != nil {
				t.Fatal(err)
			}
			req := execution.StartRequest{Caller: execution.Caller{Kind: execution.CallerWorkflow, ID: "workflow", NodeID: "first", AttemptID: "first"}, Target: execution.Target{ProjectID: "project"}, ExecutorID: "opencode:fixture", Prompt: execution.TextPrompt("run")}
			ref, err := f.service.Start(f.ctx, f.scope, req)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case sid := <-a.endEntered:
				if sid != ref.SessionID {
					t.Fatalf("stopped wrong Agent: %s", sid)
				}
			case <-f.ctx.Done():
				t.Fatal(f.ctx.Err())
			}
			f.service.mu.Lock()
			w := f.service.workers[ref.TurnID]
			f.service.mu.Unlock()
			if w == nil {
				t.Fatal("worker released before physical stop confirmation")
			}
			session, err := f.service.ReadSession(f.ctx, f.scope, ref.SessionID)
			if err != nil || session.EndedAt != nil {
				t.Fatalf("unconfirmed stop advertised ended: %+v err=%v", session, err)
			}
			turn, err := f.service.ReadAttempt(f.ctx, f.scope, req.Caller)
			if err != nil || turn.Result == nil || !reflect.DeepEqual(*turn.Result, tc.result) {
				t.Fatalf("first durable result=%+v err=%v", turn, err)
			}
			next := req
			next.Caller.NodeID = "next"
			next.Caller.AttemptID = "next"
			// A workflow run's other sessions may start beside it (they share the directory), but
			// the failed session itself takes no new turn until its stop is confirmed: its turn
			// keeps the session's admission occupied, and Append refuses it.
			f.service.mu.Lock()
			occupied := f.service.groups[turnGroup(f.scope, req.Caller, ref.SessionID)] == ref.TurnID
			f.service.mu.Unlock()
			if !occupied {
				t.Fatal("the failed session released its admission before stop confirmation")
			}
			for _, attempt := range []string{"next", "again"} {
				retry := next.Caller
				retry.AttemptID = attempt
				if _, err := f.service.Append(f.ctx, f.scope, execution.AppendRequest{SessionID: ref.SessionID, Caller: retry, Prompt: next.Prompt}); !errors.Is(err, execution.ErrSessionEnded) && !errors.Is(err, execution.ErrWorkspaceBusy) {
					t.Fatalf("broken physical session accepted Append %s before its stop was confirmed: %v", attempt, err)
				}
			}
			a.release()
			select {
			case <-w.done:
			case <-f.ctx.Done():
				t.Fatal(f.ctx.Err())
			}
			session, err = f.service.ReadSession(f.ctx, f.scope, ref.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			events, err := f.db.ReadExecutionEvents(f.ctx, f.scope, ref.SessionID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			if tc.endErr != nil {
				if session.EndedAt != nil || len(events) != 1 || events[0].Kind != execution.EventTurnEnded {
					t.Fatalf("failed physical stop published session end: session=%+v events=%+v", session, events)
				}
				if got := a.rebuilds(); !slices.Equal(got, []string{ref.SessionID}) {
					t.Fatalf("unconfirmed stop did not mark only its project's Node for rebuild: %v", got)
				}
				// Nothing is fenced. The run's next node shares the directory with the session
				// that could not be stopped; the driver retires the marked Node before serving it.
				nextRef, err := f.service.Start(f.ctx, f.scope, next)
				if err != nil {
					t.Fatalf("stop failure fenced the run's next node: %v", err)
				}
				observeTurn(t, f, nextRef, 0)
				nextSession, err := f.service.ReadSession(f.ctx, f.scope, nextRef.SessionID)
				if err != nil || nextSession.WorkspaceID != session.WorkspaceID {
					t.Fatalf("next node left the run's directory: %+v err=%v", nextSession, err)
				}
				// Models, executors and other projects keep running as well.
				if result, _ := observeTurn(t, f, startModel(t, f, modelRequest("another", "attempt", "hello")), 0); result.Status != execution.StatusCompleted {
					t.Fatalf("another conversation after a stop failure: %+v", result)
				}
				if _, err := f.service.ListModels(f.ctx, f.scope); err != nil {
					t.Fatalf("one stop failure withdrew model readiness: %v", err)
				}
				if _, err := f.service.ListExecutors(f.ctx, f.scope); err != nil {
					t.Fatalf("one stop failure withdrew Agent readiness: %v", err)
				}
				if _, err := f.db.CreateProject(f.ctx, f.scope, store.Project{ID: "other", Name: "Other", RepositoryURL: "https://example.com/other.git", Commit: "fixed", MirrorPath: "/fake/other"}); err != nil {
					t.Fatal(err)
				}
				other := execution.StartRequest{Caller: execution.Caller{Kind: execution.CallerChat, ID: "other-chat", AttemptID: "other"}, Target: execution.Target{ProjectID: "other"}, ExecutorID: "opencode:fixture", Prompt: execution.TextPrompt("run")}
				otherRef, err := f.service.Start(f.ctx, f.scope, other)
				if err != nil {
					t.Fatalf("other project's new conversation was refused: %v", err)
				}
				if result, _ := observeTurn(t, f, otherRef, 0); !reflect.DeepEqual(result, tc.result) {
					t.Fatalf("other project's conversation did not run: %+v", result)
				}
			} else {
				if session.EndedAt == nil || len(events) != 2 || events[1].Kind != execution.EventSessionEnded {
					t.Fatalf("confirmed stop did not end session: session=%+v events=%+v", session, events)
				}
				if _, err := f.service.Append(f.ctx, f.scope, execution.AppendRequest{SessionID: ref.SessionID, Caller: next.Caller, Prompt: next.Prompt}); !errors.Is(err, execution.ErrSessionEnded) {
					t.Fatalf("the stopped session accepted a turn: %v", err)
				}
				nextRef, err := f.service.Start(f.ctx, f.scope, next)
				if err != nil {
					t.Fatal(err)
				}
				observeTurn(t, f, nextRef, 0)
				nextSession, err := f.service.ReadSession(f.ctx, f.scope, nextRef.SessionID)
				if err != nil || nextSession.WorkspaceID != session.WorkspaceID {
					t.Fatalf("confirmed directory not reusable: %+v err=%v", nextSession, err)
				}
			}
			persisted, err := f.service.ReadAttempt(f.ctx, f.scope, req.Caller)
			if err != nil || persisted.Result == nil || !reflect.DeepEqual(*persisted.Result, tc.result) {
				t.Fatalf("cleanup overwrote the original result: %+v err=%v", persisted, err)
			}
		})
	}
}

// flakyFinish fails the turn-result write while down is set and counts every attempt.
type flakyFinish struct {
	write    func(context.Context, execution.Scope, execution.Ref, execution.Result) (execution.Result, error)
	failures atomic.Int32 // remaining failures; negative fails forever
	calls    atomic.Int32
}

func (f *flakyFinish) finish(ctx context.Context, scope execution.Scope, ref execution.Ref, result execution.Result) (execution.Result, error) {
	f.calls.Add(1)
	if n := f.failures.Load(); n != 0 {
		if n > 0 {
			f.failures.Add(-1)
		}
		return execution.Result{}, errors.New("database unavailable")
	}
	return f.write(ctx, scope, ref, result)
}

func shortFinishRetries(t *testing.T) {
	t.Helper()
	old := finishRetryDelays
	finishRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { finishRetryDelays = old })
}

func waitUnsaved(t *testing.T, f *runtimeFixture, sid string) {
	t.Helper()
	for {
		f.service.mu.Lock()
		_, unsaved := f.service.unsaved[sid]
		idle := len(f.service.workers) == 0
		f.service.mu.Unlock()
		if unsaved && idle {
			return
		}
		select {
		case <-f.ctx.Done():
			t.Fatal("result write failure was not retained for its session")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestRuntimeRetriesAResultWriteBeforeFailingTheSession(t *testing.T) {
	shortFinishRetries(t)
	f := runtimeDatabase(t, nil)
	flaky := &flakyFinish{write: f.service.finishTurn}
	flaky.failures.Store(int32(len(finishRetryDelays)))
	f.service.finishTurn = flaky.finish
	ref := startModel(t, f, modelRequest("chat", "first", "hello"))
	if result, _ := observeTurn(t, f, ref, 0); result.Status != execution.StatusCompleted {
		t.Fatalf("retried write: %+v", result)
	}
	if got := flaky.calls.Load(); got != int32(len(finishRetryDelays))+1 {
		t.Fatalf("write attempts=%d", got)
	}
	if _, err := f.service.Append(f.ctx, f.scope, execution.AppendRequest{SessionID: ref.SessionID, Caller: execution.Caller{Kind: execution.CallerChat, ID: "chat", AttemptID: "second"}, Prompt: execution.TextPrompt("again")}); err != nil {
		t.Fatalf("a recovered write still failed its session: %v", err)
	}
}

func TestRuntimeResultWriteFailureOnlyFailsItsSession(t *testing.T) {
	shortFinishRetries(t)
	f := runtimeDatabase(t, nil)
	flaky := &flakyFinish{write: f.service.finishTurn}
	flaky.failures.Store(-1)
	f.service.finishTurn = flaky.finish
	ref := startModel(t, f, modelRequest("chat", "first", "hello"))
	waitUnsaved(t, f, ref.SessionID)
	if got := flaky.calls.Load(); got != int32(len(finishRetryDelays))+1 {
		t.Fatalf("write attempts=%d", got)
	}
	if _, err := f.service.Append(f.ctx, f.scope, execution.AppendRequest{SessionID: ref.SessionID, Caller: execution.Caller{Kind: execution.CallerChat, ID: "chat", AttemptID: "second"}, Prompt: execution.TextPrompt("again")}); !errors.Is(err, execution.ErrSessionEnded) {
		t.Fatalf("session with an unstored result accepted Append: %v", err)
	}

	// The database recovers: other conversations and the catalog were never fenced.
	flaky.failures.Store(0)
	if _, err := f.service.ListModels(f.ctx, f.scope); err != nil {
		t.Fatalf("one write failure withdrew model readiness: %v", err)
	}
	if result, _ := observeTurn(t, f, startModel(t, f, modelRequest("other", "first", "hello")), 0); result.Status != execution.StatusCompleted {
		t.Fatalf("another conversation after a write failure: %+v", result)
	}

	// Ending the failed session first stores the result its worker could not write.
	if err := f.service.End(f.ctx, f.scope, ref.SessionID); err != nil {
		t.Fatal(err)
	}
	turn, err := f.service.ReadAttempt(f.ctx, f.scope, execution.Caller{Kind: execution.CallerChat, ID: "chat", AttemptID: "first"})
	if err != nil || turn.Result == nil || turn.Result.Status != execution.StatusCompleted {
		t.Fatalf("End did not store the unsaved result: %+v err=%v", turn, err)
	}
	session, err := f.service.ReadSession(f.ctx, f.scope, ref.SessionID)
	if err != nil || session.EndedAt == nil {
		t.Fatalf("failed session did not end: %+v err=%v", session, err)
	}
}

func TestRuntimeAgentResultWriteFailureStopsOnlyThatAgent(t *testing.T) {
	shortFinishRetries(t)
	f := runtimeDatabase(t, nil)
	if err := f.service.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	completed := execution.Result{Status: execution.StatusCompleted, Answer: "complete", AnswerComplete: true, StopReason: "end_turn"}
	a := &failureAgent{result: completed, endEntered: make(chan string, 8), allowEnd: make(chan struct{})}
	a.release()
	var err error
	f.service, err = New(f.ctx, Config{Store: f.db, Models: f.models, Agent: a})
	if err != nil {
		t.Fatal(err)
	}
	flaky := &flakyFinish{write: f.service.finishTurn}
	flaky.failures.Store(-1)
	f.service.finishTurn = flaky.finish
	for _, id := range []string{"project", "other"} {
		if _, err := f.db.CreateProject(f.ctx, f.scope, store.Project{ID: id, Name: id, RepositoryURL: "https://example.com/" + id + ".git", Commit: "fixed", MirrorPath: "/fake/" + id}); err != nil {
			t.Fatal(err)
		}
	}
	chat := func(id, project string) execution.StartRequest {
		return execution.StartRequest{Caller: execution.Caller{Kind: execution.CallerChat, ID: id, AttemptID: "first"}, Target: execution.Target{ProjectID: project}, ExecutorID: "opencode:fixture", Prompt: execution.TextPrompt("run")}
	}
	ref, err := f.service.Start(f.ctx, f.scope, chat("chat", "project"))
	if err != nil {
		t.Fatal(err)
	}
	waitUnsaved(t, f, ref.SessionID)
	select {
	case sid := <-a.endEntered:
		if sid != ref.SessionID {
			t.Fatalf("stopped wrong Agent: %s", sid)
		}
	default:
		t.Fatal("a session whose result was not stored kept its physical Agent")
	}
	if got := a.rebuilds(); len(got) != 0 {
		t.Fatalf("confirmed stop still marked the Node for rebuild: %v", got)
	}
	flaky.failures.Store(0)
	otherRef, err := f.service.Start(f.ctx, f.scope, chat("other-chat", "other"))
	if err != nil {
		t.Fatalf("other project's new conversation was refused: %v", err)
	}
	if result, _ := observeTurn(t, f, otherRef, 0); !reflect.DeepEqual(result, completed) {
		t.Fatalf("other project's conversation: %+v", result)
	}
	if _, err := f.service.Cancel(f.ctx, f.scope, ref); err != nil {
		t.Fatalf("Cancel after the database recovered: %v", err)
	}
	turn, err := f.service.ReadAttempt(f.ctx, f.scope, chat("chat", "project").Caller)
	if err != nil || turn.Result == nil || !reflect.DeepEqual(*turn.Result, completed) {
		t.Fatalf("Cancel did not store the original result: %+v err=%v", turn, err)
	}
}

func shortStopTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := stopTimeout
	stopTimeout = d
	t.Cleanup(func() { stopTimeout = old })
}

// silentStopFixture runs turns that complete normally on an Agent whose End stays unconfirmed
// until the test releases it, as a Node that swallows end_session would.
func silentStopFixture(t *testing.T) (*runtimeFixture, *failureAgent, execution.Result) {
	t.Helper()
	f := runtimeDatabase(t, nil)
	if err := f.service.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	completed := execution.Result{Status: execution.StatusCompleted, Answer: "complete", AnswerComplete: true, StopReason: "end_turn"}
	a := &failureAgent{result: completed, endEntered: make(chan string, 8), allowEnd: make(chan struct{})}
	t.Cleanup(a.release)
	var err error
	if f.service, err = New(f.ctx, Config{Store: f.db, Models: f.models, Agent: a}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"project", "other"} {
		if _, err := f.db.CreateProject(f.ctx, f.scope, store.Project{ID: id, Name: id, RepositoryURL: "https://example.com/" + id + ".git", Commit: "fixed", MirrorPath: "/fake/" + id}); err != nil {
			t.Fatal(err)
		}
	}
	return f, a, completed
}

func projectChat(id, project string) execution.StartRequest {
	return execution.StartRequest{Caller: execution.Caller{Kind: execution.CallerChat, ID: id, AttemptID: "first"}, Target: execution.Target{ProjectID: project}, ExecutorID: "opencode:fixture", Prompt: execution.TextPrompt("run")}
}

func workflowNode(node string) execution.StartRequest {
	return execution.StartRequest{Caller: execution.Caller{Kind: execution.CallerWorkflow, ID: "workflow", NodeID: node, AttemptID: node}, Target: execution.Target{ProjectID: "project"}, ExecutorID: "opencode:fixture", Prompt: execution.TextPrompt("run")}
}

func startAgentTurn(t *testing.T, f *runtimeFixture, req execution.StartRequest, want execution.Result) execution.Ref {
	t.Helper()
	ref, err := f.service.Start(f.ctx, f.scope, req)
	if err != nil {
		t.Fatal(err)
	}
	if result, _ := observeTurn(t, f, ref, 0); !reflect.DeepEqual(result, want) {
		t.Fatalf("turn %+v: %+v", req.Caller, result)
	}
	return ref
}

func TestRuntimeStopBudgetAfterACompletedTurnMarksOnlyThatNode(t *testing.T) {
	shortStopTimeout(t, 150*time.Millisecond)
	f, a, completed := silentStopFixture(t)

	// An explicit End after a normally completed turn: the service's budget, not the caller's
	// far deadline, bounds the wait, and running out of it marks the Node.
	chat := startAgentTurn(t, f, projectChat("chat", "project"), completed)
	began := time.Now()
	if err := f.service.End(context.Background(), f.scope, chat.SessionID); !errors.Is(err, execution.ErrStopUnconfirmed) {
		t.Fatalf("End on a silent Node: %v", err)
	}
	if waited := time.Since(began); waited > 5*time.Second {
		t.Fatalf("End without a caller deadline waited %s", waited)
	}
	if got := a.rebuilds(); !slices.Equal(got, []string{chat.SessionID}) {
		t.Fatalf("unconfirmed End did not mark its Node for rebuild: %v", got)
	}

	// A workflow run is not held up by a node it could not stop: the session stays open, only
	// its Node is marked, and the run's next node starts beside it on the same directory.
	first := startAgentTurn(t, f, workflowNode("first"), completed)
	if err := f.service.End(context.Background(), f.scope, first.SessionID); !errors.Is(err, execution.ErrStopUnconfirmed) {
		t.Fatalf("End of a workflow node on a silent Node: %v", err)
	}
	if got := a.rebuilds(); !slices.Equal(got, []string{chat.SessionID, first.SessionID}) {
		t.Fatalf("unconfirmed End of a workflow node did not mark its Node for rebuild: %v", got)
	}
	stuck, err := f.service.ReadSession(f.ctx, f.scope, first.SessionID)
	if err != nil || stuck.EndedAt != nil {
		t.Fatalf("unconfirmed stop advertised ended: %+v err=%v", stuck, err)
	}
	next := startAgentTurn(t, f, workflowNode("next"), completed)
	if session, err := f.service.ReadSession(f.ctx, f.scope, next.SessionID); err != nil || session.WorkspaceID != stuck.WorkspaceID {
		t.Fatalf("next node left the run's directory: %+v err=%v", session, err)
	}

	// Once the stop is confirmed, the same Ends succeed.
	a.release()
	for _, sid := range []string{chat.SessionID, first.SessionID} {
		if err := f.service.End(f.ctx, f.scope, sid); err != nil {
			t.Fatal(err)
		}
		if session, err := f.service.ReadSession(f.ctx, f.scope, sid); err != nil || session.EndedAt == nil {
			t.Fatalf("confirmed stop did not end the session: %+v err=%v", session, err)
		}
	}
}

func TestRuntimeImpatientCallerNeverMarksASlowNode(t *testing.T) {
	f, a, completed := silentStopFixture(t)
	chat := startAgentTurn(t, f, projectChat("chat", "project"), completed)
	impatient, cancel := context.WithTimeout(f.ctx, 20*time.Millisecond)
	defer cancel()
	if err := f.service.End(impatient, f.scope, chat.SessionID); !errors.Is(err, execution.ErrStopUnconfirmed) {
		t.Fatalf("End that stopped waiting: %v", err)
	}
	// The Node was only slow: its confirmation still settles the shared attempt.
	a.release()
	if err := f.service.End(f.ctx, f.scope, chat.SessionID); err != nil {
		t.Fatal(err)
	}
	if got := a.rebuilds(); len(got) != 0 {
		t.Fatalf("a caller's short deadline marked a healthy Node: %v", got)
	}
}

// A stop the Node has not confirmed yet is waited for outside the admission lock, so it holds
// up nothing but its own session: not the catalog, other projects, or its run's next node.
func TestRuntimePendingStopHoldsUpNothingButItsOwnSession(t *testing.T) {
	f, a, completed := silentStopFixture(t)
	first := startAgentTurn(t, f, workflowNode("first"), completed)
	ended := make(chan error, 1)
	go func() { ended <- f.service.End(f.ctx, f.scope, first.SessionID) }()
	select {
	case sid := <-a.endEntered:
		if sid != first.SessionID {
			t.Fatalf("End stopped the wrong Agent: %s", sid)
		}
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}

	prompt, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	if _, err := f.service.ListModels(prompt, f.scope); err != nil {
		t.Fatalf("catalog during a pending stop: %v", err)
	}
	if _, err := f.service.ListExecutors(prompt, f.scope); err != nil {
		t.Fatalf("executors during a pending stop: %v", err)
	}
	if _, err := f.service.StartModel(prompt, f.scope, modelRequest("model-chat", "first", "hello")); err != nil {
		t.Fatalf("model conversation during a pending stop: %v", err)
	}
	other, err := f.service.Start(prompt, f.scope, projectChat("other-chat", "other"))
	if err != nil {
		t.Fatalf("other project during a pending stop: %v", err)
	}
	if result, _ := observeTurn(t, f, other, 0); !reflect.DeepEqual(result, completed) {
		t.Fatalf("other project's turn during a pending stop: %+v", result)
	}
	next, err := f.service.Start(prompt, f.scope, workflowNode("next"))
	if err != nil {
		t.Fatalf("the run's next node during a pending stop: %v", err)
	}
	if result, _ := observeTurn(t, f, next, 0); !reflect.DeepEqual(result, completed) {
		t.Fatalf("the run's next node: %+v", result)
	}
	if _, err := f.service.Append(prompt, f.scope, execution.AppendRequest{SessionID: first.SessionID, Caller: workflowNode("late").Caller, Prompt: execution.TextPrompt("late")}); !errors.Is(err, execution.ErrSessionEnded) {
		t.Fatalf("session accepted a turn while being ended: %v", err)
	}
	select {
	case err := <-ended:
		t.Fatalf("End returned before its stop was confirmed: %v", err)
	default:
	}

	a.release()
	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("End after the stop was confirmed: %v", err)
		}
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
}
