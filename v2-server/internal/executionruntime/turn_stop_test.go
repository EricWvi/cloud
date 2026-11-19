package executionruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// keepingAgentService replaces the fixture's service with one over an Agent driver that keeps
// sessions after a stop when keep is set, and a project to run it on.
func keepingAgentService(t *testing.T, f *runtimeFixture, keep bool) *orchestrationAgent {
	t.Helper()
	if err := f.service.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	a := &orchestrationAgent{started: make(chan *agentCall, 8), ended: make(chan string, 8), keepOnStop: keep}
	var err error
	f.service, err = New(f.ctx, Config{Store: f.db, Models: f.models, Agent: a})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.CreateProject(f.ctx, f.scope, store.Project{ID: "project", Name: "Demo", RepositoryURL: "https://example.com/demo.git", Commit: "fixed", MirrorPath: "/fake/unused"}); err != nil {
		t.Fatal(err)
	}
	return a
}

// assertNotEnded fails if the driver was asked to end a session.
func assertNotEnded(t *testing.T, a *orchestrationAgent) {
	t.Helper()
	select {
	case sid := <-a.ended:
		t.Fatalf("session %s was ended", sid)
	default:
	}
}

// Stopping a conversation's, a task run's or a workflow node's running turn asks the driver to stop
// only the turn (ErrStopTurn). When the driver keeps the session the stop leaves it live: nothing
// ends it, and the same session takes the next turn.
func TestRuntimeStoppedAgentTurnKeepsTheSession(t *testing.T) {
	for _, caller := range []execution.Caller{
		{Kind: execution.CallerChat, ID: "conversation", AttemptID: "first"},
		{Kind: execution.CallerTask, ID: "run", AttemptID: "first"},
		{Kind: execution.CallerWorkflow, ID: "workflow-run", NodeID: "node", AttemptID: "attempt"},
	} {
		t.Run(string(caller.Kind), func(t *testing.T) {
			f := runtimeDatabase(t, nil)
			a := keepingAgentService(t, f, true)
			request := execution.StartRequest{Caller: caller, Target: execution.Target{ProjectID: "project"}, ExecutorID: "opencode:fixture", Prompt: execution.TextPrompt("run")}
			if caller.Kind == execution.CallerWorkflow {
				request.Lifetime = execution.LifetimeHeld
			}
			first, err := f.service.Start(f.ctx, f.scope, request)
			if err != nil {
				t.Fatal(err)
			}
			call := nextAgentCall(t, f, a)
			stopped, err := f.service.Cancel(f.ctx, f.scope, first)
			if err != nil || stopped.Status != execution.StatusCancelled || stopped.AgentLost {
				t.Fatalf("stop = %+v err=%v", stopped, err)
			}
			if !errors.Is(context.Cause(call.ctx), execution.ErrStopTurn) {
				t.Fatalf("the driver was cancelled with %v, want ErrStopTurn", context.Cause(call.ctx))
			}
			assertNotEnded(t, a)
			session, err := f.service.ReadSession(f.ctx, f.scope, first.SessionID)
			if err != nil || session.EndedAt != nil {
				t.Fatalf("session after a kept stop = %+v err=%v", session, err)
			}
			next := caller
			next.AttemptID = "second"
			second, err := f.service.Append(f.ctx, f.scope, execution.AppendRequest{SessionID: first.SessionID, Caller: next, Prompt: execution.TextPrompt("continue")})
			if err != nil || second.SessionID != first.SessionID {
				t.Fatalf("Append after a kept stop = %+v err=%v", second, err)
			}
			nextAgentCall(t, f, a).unblock()
			if result, _ := observeTurn(t, f, second, 0); result.Status != execution.StatusCompleted {
				t.Fatalf("turn after the stop = %+v", result)
			}
			again, err := f.service.Cancel(f.ctx, f.scope, first)
			if err != nil || again.Status != execution.StatusCancelled {
				t.Fatalf("repeated stop of the stopped turn = %+v err=%v", again, err)
			}
			assertNotEnded(t, a)
		})
	}
}

// Without turn_cancel (the driver cannot keep the session) a stop still ends the session, as do a
// failed turn and an Agent error when the driver keeps sessions after a stop: only a stop the
// driver kept leaves a non-held session live.
func TestRuntimeTurnsTheDriverDoesNotKeepEndTheSession(t *testing.T) {
	for _, tc := range []struct {
		name   string
		keep   bool
		finish func(t *testing.T, f *runtimeFixture, ref execution.Ref, call *agentCall)
		status execution.Status
	}{
		{name: "stop without turn_cancel", finish: func(t *testing.T, f *runtimeFixture, ref execution.Ref, _ *agentCall) {
			if _, err := f.service.Cancel(f.ctx, f.scope, ref); err != nil {
				t.Fatal(err)
			}
		}, status: execution.StatusCancelled},
		{name: "failed turn", keep: true, finish: func(_ *testing.T, _ *runtimeFixture, _ execution.Ref, call *agentCall) {
			call.result = &execution.Result{Status: execution.StatusFailed, FailureCode: "model_idle_timeout", StopReason: "cancelled"}
			call.unblock()
		}, status: execution.StatusFailed},
		{name: "kept result with an Agent error", keep: true, finish: func(_ *testing.T, _ *runtimeFixture, _ execution.Ref, call *agentCall) {
			call.result = &execution.Result{Status: execution.StatusCancelled, StopReason: "cancelled", SessionKept: true}
			call.err = execution.ErrSessionEnded
			call.unblock()
		}, status: execution.StatusCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := runtimeDatabase(t, nil)
			a := keepingAgentService(t, f, tc.keep)
			request := execution.StartRequest{Caller: modelRequest("chat", "first", "").Caller, Target: execution.Target{ProjectID: "project"}, ExecutorID: "opencode:fixture", Prompt: execution.TextPrompt("run")}
			ref, err := f.service.Start(f.ctx, f.scope, request)
			if err != nil {
				t.Fatal(err)
			}
			tc.finish(t, f, ref, nextAgentCall(t, f, a))
			if result, _ := observeTurn(t, f, ref, 0); result.Status != tc.status {
				t.Fatalf("result = %+v, want %s", result, tc.status)
			}
			select {
			case sid := <-a.ended:
				if sid != ref.SessionID {
					t.Fatalf("ended %s", sid)
				}
			case <-f.ctx.Done():
				t.Fatal("the session was not ended")
			}
			if _, err := f.service.Append(f.ctx, f.scope, execution.AppendRequest{SessionID: ref.SessionID, Caller: modelRequest("chat", "second", "").Caller, Prompt: execution.TextPrompt("next")}); !errors.Is(err, execution.ErrSessionEnded) {
				t.Fatalf("Append after an unkept turn = %v", err)
			}
		})
	}
}
