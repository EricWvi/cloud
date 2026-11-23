package taskrun_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/execution/executiontest"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
	"github.com/wanglongan587/cloud/v2-server/internal/taskrun"
)

// port models the fixed-Node rules task runs depend on: each Start makes a session owned by its
// scope, a session runs one turn at a time, a turn ends when the test finishes it, and cancelling a
// running turn ends the whole session, unless keepOnCancel models a Node that keeps it.
type port struct {
	mu       sync.Mutex
	next     int
	sessions map[string]*session
	turns    map[string]*execution.Turn
	attempts map[string]execution.Ref
	// keepOnCancel makes Cancel stop only the turn, as over a Node that negotiated turn_cancel.
	keepOnCancel bool
	// cancelGate, when set, holds every Cancel until it is closed.
	cancelGate chan struct{}
}

type session struct {
	owner   execution.Scope
	ended   bool
	running string
	events  []execution.Event
	wake    chan struct{}
}

func newPort() *port {
	return &port{sessions: map[string]*session{}, turns: map[string]*execution.Turn{}, attempts: map[string]execution.Ref{}}
}

func (p *port) publish(s *session, ev execution.Event) {
	ev.Sequence = uint64(len(s.events) + 1)
	s.events = append(s.events, ev)
	close(s.wake)
	s.wake = make(chan struct{})
}

func (p *port) admit(scope execution.Scope, sessionID string, c execution.Caller, prompt execution.Prompt) execution.Ref {
	p.next++
	ref := execution.Ref{SessionID: sessionID, TurnID: fmt.Sprintf("turn-%d", p.next)}
	p.turns[ref.TurnID] = &execution.Turn{Ref: ref, Caller: c, Prompt: prompt, StartedAt: time.Now()}
	p.attempts[scope.UserID+"/"+c.ID+"/"+c.AttemptID] = ref
	p.sessions[sessionID].running = ref.TurnID
	return ref
}

// owned returns the session when scope owns it; anyone else gets ErrNotFound, as the service does.
func (p *port) owned(scope execution.Scope, id string) (*session, error) {
	s := p.sessions[id]
	if s == nil || s.owner != scope {
		return nil, execution.ErrNotFound
	}
	return s, nil
}

// finish ends the session's running turn with status.
func (p *port) finish(t *testing.T, turnID string, status execution.Status) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	turn := p.turns[turnID]
	s := p.sessions[turn.Ref.SessionID]
	if s.running != turnID {
		t.Fatalf("turn %s is not running", turnID)
	}
	p.end(s, turn, status)
}

func (p *port) end(s *session, turn *execution.Turn, status execution.Status) execution.Result {
	r := execution.Result{Status: status, AnswerComplete: status == execution.StatusCompleted}
	turn.Result = &r
	s.running = ""
	p.publish(s, execution.Event{Ref: turn.Ref, Kind: execution.EventTurnEnded, Result: &r})
	return r
}

func (p *port) runtime() *executiontest.Stub {
	return &executiontest.Stub{
		ListExecutorsFunc: func(context.Context, execution.Scope) ([]execution.Executor, error) {
			return []execution.Executor{
				{ID: "opencode:fixture", Name: "OpenCode / Fixture", AgentID: "official/ora-space.opencode"},
				{ID: "opencode:configured", Name: "OpenCode / Real", AgentID: "official/ora-space.opencode"},
				{ID: "claude:fixture", Name: "Claude Code / Fixture", AgentID: "official/ora-space.claude"},
			}, nil
		},
		StartFunc: func(_ context.Context, scope execution.Scope, req execution.StartRequest) (execution.Ref, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if ref, ok := p.attempts[scope.UserID+"/"+req.Caller.ID+"/"+req.Caller.AttemptID]; ok {
				return ref, nil
			}
			id := fmt.Sprintf("session-%d", len(p.sessions)+1)
			p.sessions[id] = &session{owner: scope, wake: make(chan struct{})}
			return p.admit(scope, id, req.Caller, req.Prompt), nil
		},
		AppendFunc: func(_ context.Context, scope execution.Scope, req execution.AppendRequest) (execution.Ref, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			s, e := p.owned(scope, req.SessionID)
			switch {
			case e != nil:
				return execution.Ref{}, e
			case s.ended:
				return execution.Ref{}, execution.ErrSessionEnded
			case s.running != "":
				return execution.Ref{}, execution.ErrWorkspaceBusy
			}
			return p.admit(scope, req.SessionID, req.Caller, req.Prompt), nil
		},
		CancelFunc: func(_ context.Context, scope execution.Scope, ref execution.Ref) (execution.Result, error) {
			if p.cancelGate != nil {
				<-p.cancelGate
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			s, e := p.owned(scope, ref.SessionID)
			if e != nil {
				return execution.Result{}, e
			}
			turn := p.turns[ref.TurnID]
			if turn.Result != nil {
				return *turn.Result, nil
			}
			r := p.end(s, turn, execution.StatusCancelled)
			if p.keepOnCancel {
				return r, nil
			}
			s.ended = true
			p.publish(s, execution.Event{Kind: execution.EventSessionEnded})
			return r, nil
		},
		ReadSessionFunc: func(_ context.Context, scope execution.Scope, id string) (execution.Session, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			s, e := p.owned(scope, id)
			if e != nil {
				return execution.Session{}, e
			}
			read := execution.Session{ID: id}
			if s.ended {
				at := time.Now()
				read.EndedAt = &at
			}
			return read, nil
		},
		EndFunc: func(_ context.Context, scope execution.Scope, id string) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			s, e := p.owned(scope, id)
			if e != nil {
				return e
			}
			if !s.ended {
				s.ended = true
				p.publish(s, execution.Event{Kind: execution.EventSessionEnded})
			}
			return nil
		},
		SubscribeFunc: func(_ context.Context, scope execution.Scope, id string, after uint64) (execution.Stream, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if _, e := p.owned(scope, id); e != nil {
				return nil, e
			}
			return &stream{p: p, id: id, at: after, done: make(chan struct{})}, nil
		},
		ReadTurnsFunc: func(_ context.Context, scope execution.Scope, id, _ string, _ int) (execution.TurnPage, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if _, e := p.owned(scope, id); e != nil {
				return execution.TurnPage{}, e
			}
			page := execution.TurnPage{}
			for _, t := range p.turns {
				if t.Ref.SessionID == id {
					page.Items = append(page.Items, *t)
				}
			}
			return page, nil
		},
		ReadAttemptFunc: func(_ context.Context, scope execution.Scope, c execution.Caller) (execution.Turn, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			ref, ok := p.attempts[scope.UserID+"/"+c.ID+"/"+c.AttemptID]
			if !ok {
				return execution.Turn{}, execution.ErrNotFound
			}
			return *p.turns[ref.TurnID], nil
		},
	}
}

type stream struct {
	p    *port
	id   string
	at   uint64
	once sync.Once
	done chan struct{}
}

func (s *stream) Next(ctx context.Context) (execution.Event, error) {
	for {
		s.p.mu.Lock()
		sess := s.p.sessions[s.id]
		if int(s.at) < len(sess.events) {
			ev := sess.events[s.at]
			s.at++
			s.p.mu.Unlock()
			return ev, nil
		}
		if sess.ended {
			s.p.mu.Unlock()
			return execution.Event{}, io.EOF
		}
		wake := sess.wake
		s.p.mu.Unlock()
		select {
		case <-wake:
		case <-s.done:
			return execution.Event{}, errors.New("closed")
		case <-ctx.Done():
			return execution.Event{}, ctx.Err()
		}
	}
}

func (s *stream) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

type fixture struct {
	ctx           context.Context
	store         *store.Store
	port          *port
	host          *taskrun.Host
	tenant        string
	owner, member execution.Scope
	project       string
	agents        map[string]string // plugin identifier to Space Agent id
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s := storetest.Open(t)
	ctx := context.Background()
	owner, tenant := storetest.Login(t, s, "task-owner")
	token := storetest.SignIn(t, s, "task-member", "鲍勃", store.RoleMember)
	member, e := s.SessionUser(ctx, token)
	if e != nil {
		t.Fatal(e)
	}
	f := &fixture{ctx: ctx, store: s, port: newPort(), tenant: tenant,
		owner: execution.Scope{TenantID: tenant, UserID: owner}, member: execution.Scope{TenantID: tenant, UserID: member.ID},
		project: "project-task-runs", agents: map[string]string{}}
	if _, e := s.CreateProject(ctx, f.owner, store.Project{ID: f.project, Name: "演示", RepositoryURL: "https://example.test/r.git",
		Commit: "abc", MirrorPath: "/tmp/mirror"}); e != nil {
		t.Fatal(e)
	}
	agents, e := s.CollaborationAgents(ctx, tenant)
	if e != nil {
		t.Fatal(e)
	}
	for _, a := range agents {
		f.agents[a.Identifier] = a.ID
	}
	f.host = taskrun.New(s, f.port.runtime(), f.port.runtime(), nil)
	t.Cleanup(f.host.Close)
	return f
}

// task creates a task assigned to the plugin's Space Agent, or unassigned with an empty plugin.
func (f *fixture) task(t *testing.T, plugin string) store.Task {
	t.Helper()
	in := store.TaskInput{Title: "修复登录页", Description: "按钮错位", ProjectID: f.project}
	if plugin != "" {
		in.Assignee = &store.TaskAssigneeRef{Type: store.AssigneeAgent, ID: f.agents[plugin]}
	}
	task, e := f.store.CreateTask(f.ctx, f.tenant, f.owner.UserID, in)
	if e != nil {
		t.Fatal(e)
	}
	return task
}

func (f *fixture) read(t *testing.T, id string) store.Task {
	t.Helper()
	task, e := f.store.Task(f.ctx, f.tenant, id)
	if e != nil {
		t.Fatal(e)
	}
	return task
}

// await polls until the task reaches status: the turn is settled by a background observer.
func (f *fixture) await(t *testing.T, id, status string) store.Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		task := f.read(t, id)
		if task.Status == status && (task.ActiveRun == nil || task.ActiveRun.State != store.TaskRunRunning) {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("task stayed %s (run %+v), want %s", task.Status, task.ActiveRun, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

const opencode = "official/ora-space.opencode"

func TestStartRunsTheAgentAndHandsTheTaskToReview(t *testing.T) {
	f := newFixture(t)
	task := f.task(t, opencode)

	run, e := f.host.Start(f.ctx, f.member, task.ID, "start-1", taskrun.StartInput{Note: "先看样式"})
	if e != nil {
		t.Fatal(e)
	}
	// The default is the Agent's configured model, not its fixture, and the run is the starter's.
	if run.ExecutorID != "opencode:configured" || run.StartedBy != f.member.UserID || run.State != store.TaskRunRunning {
		t.Fatalf("run = %+v", run)
	}
	if got := f.read(t, task.ID); got.Status != store.TaskRunning || got.ActiveRun == nil || got.ActiveRun.StartedByName != "鲍勃" {
		t.Fatalf("task while running = %+v %+v", got, got.ActiveRun)
	}
	// The session belongs to the starter: the turn ran under their scope.
	if prompt, e := f.host.TurnPrompt(f.ctx, run, run.LastTurnID); e != nil || prompt != "请完成下面这个任务。\n\n任务：修复登录页\n\n按钮错位\n\n补充说明：先看样式" {
		t.Fatalf("prompt = %q %v", prompt, e)
	}
	// Nobody moves the task by hand while the Agent works.
	if _, e := f.store.SetTaskStatus(f.ctx, storetest.Access(t, f.store, f.tenant, f.owner.UserID), task.ID, store.TaskReview, f.read(t, task.ID).Version); !errors.Is(e, store.ErrRunActive) {
		t.Fatalf("manual move while running = %v", e)
	}
	// A retry with the same key returns the same run instead of starting the Agent again.
	if again, e := f.host.Start(f.ctx, f.member, task.ID, "start-1", taskrun.StartInput{}); e != nil || again.ID != run.ID {
		t.Fatalf("retried start = %+v %v", again, e)
	}

	f.port.finish(t, run.LastTurnID, execution.StatusCompleted)
	got := f.await(t, task.ID, store.TaskReview)
	if got.ActiveRun == nil || got.ActiveRun.State != store.TaskRunIdle {
		t.Fatalf("run after the turn = %+v", got.ActiveRun)
	}
}

func TestOnlyTheStarterFollowsUp(t *testing.T) {
	f := newFixture(t)
	task := f.task(t, opencode)
	run, e := f.host.Start(f.ctx, f.owner, task.ID, "start", taskrun.StartInput{ExecutorID: "opencode:fixture"})
	if e != nil {
		t.Fatal(e)
	}
	// While the turn runs, a follow-up is refused rather than queued.
	if _, e := f.host.Comment(f.ctx, f.owner, task.ID, "busy", taskrun.CommentInput{Body: "还有", ToAgent: true}); !errors.Is(e, store.ErrRunActive) {
		t.Fatalf("follow-up while running = %v", e)
	}
	f.port.finish(t, run.LastTurnID, execution.StatusCompleted)
	f.await(t, task.ID, store.TaskReview)

	// Another member comments and mentions people, but cannot spend the starter's identity.
	plain, e := f.host.Comment(f.ctx, f.member, task.ID, "plain", taskrun.CommentInput{Body: "看起来不错",
		Mentions: []store.TaskMention{{Type: "user", ID: f.owner.UserID}}})
	if e != nil || plain.RunID != "" || len(plain.Mentions) != 1 || plain.Mentions[0].DisplayName != "task-owner" {
		t.Fatalf("plain comment = %+v %v", plain, e)
	}
	if _, e := f.host.Comment(f.ctx, f.member, task.ID, "other", taskrun.CommentInput{Body: "再改改", ToAgent: true}); !errors.Is(e, taskrun.ErrRunOwned) {
		t.Fatalf("follow-up by another member = %v", e)
	}
	if _, e := f.host.Comment(f.ctx, f.member, task.ID, "bad", taskrun.CommentInput{Body: "x",
		Mentions: []store.TaskMention{{Type: "user", ID: "00000000-0000-0000-0000-000000000000"}}}); !errors.Is(e, store.ErrInvalidMention) {
		t.Fatalf("unknown mention = %v", e)
	}

	// The starter's follow-up is the session's next turn and puts the task back in 进行中.
	follow, e := f.host.Comment(f.ctx, f.owner, task.ID, "follow", taskrun.CommentInput{Body: "按钮再大一点", ToAgent: true})
	if e != nil || follow.RunID != run.ID {
		t.Fatalf("follow-up = %+v %v", follow, e)
	}
	if got := f.read(t, task.ID); got.Status != store.TaskRunning || got.ActiveRun.ID != run.ID {
		t.Fatalf("task after follow-up = %+v", got)
	}
	runs, comments, e := f.store.TaskTimeline(f.ctx, f.tenant, task.ID)
	if e != nil || len(runs) != 1 || len(comments) != 2 {
		t.Fatalf("timeline = %d runs, %d comments, %v", len(runs), len(comments), e)
	}
}

func TestStopEndsTheRunAndReturnsTheTask(t *testing.T) {
	f := newFixture(t)
	task := f.task(t, opencode)
	run, e := f.host.Start(f.ctx, f.member, task.ID, "start", taskrun.StartInput{})
	if e != nil {
		t.Fatal(e)
	}
	// A member who neither started the run nor created the task cannot stop it.
	token := storetest.SignIn(t, f.store, "task-bystander", "卡萝", store.RoleMember)
	bystander, e := f.store.SessionUser(f.ctx, token)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.host.Stop(f.ctx, execution.Scope{TenantID: f.tenant, UserID: bystander.ID}, task.ID, run.ID); !errors.Is(e, store.ErrForbidden) {
		t.Fatalf("bystander stop = %v, want forbidden", e)
	}
	// The task's creator can; the session is still read and cancelled under the starter's scope.
	stopped, e := f.host.Stop(f.ctx, f.owner, task.ID, run.ID)
	if e != nil || stopped.State != store.TaskRunEnded || stopped.EndReason != store.TaskRunStopped || stopped.LastResult != "cancelled" {
		t.Fatalf("stopped = %+v %v", stopped, e)
	}
	if got := f.await(t, task.ID, store.TaskPending); got.ActiveRun != nil {
		t.Fatalf("task after stop = %+v", got)
	}
	if _, e := f.host.Stop(f.ctx, f.owner, task.ID, run.ID); !errors.Is(e, taskrun.ErrNoLiveRun) {
		t.Fatalf("second stop = %v", e)
	}
}

// When the session outlives a stopped turn (the Node kept it), Stop settles only the turn: the run
// stays live and idle with the task back in 待处理, and the starter's next comment follows up in
// the same session.
func TestStopKeepsTheRunWhenItsSessionOutlivesTheTurn(t *testing.T) {
	f := newFixture(t)
	f.port.keepOnCancel = true
	task := f.task(t, opencode)
	run, e := f.host.Start(f.ctx, f.member, task.ID, "start", taskrun.StartInput{})
	if e != nil {
		t.Fatal(e)
	}
	stopped, e := f.host.Stop(f.ctx, f.owner, task.ID, run.ID)
	if e != nil || !stopped.Live() || stopped.TurnActive() || stopped.EndReason != "" || stopped.LastResult != "cancelled" {
		t.Fatalf("stopped = %+v %v", stopped, e)
	}
	if got := f.await(t, task.ID, store.TaskPending); got.ActiveRun == nil || got.ActiveRun.ID != run.ID {
		t.Fatalf("task after a kept stop = %+v", got)
	}
	follow, e := f.host.Comment(f.ctx, f.member, task.ID, "follow", taskrun.CommentInput{Body: "继续", ToAgent: true})
	if e != nil || follow.RunID != run.ID {
		t.Fatalf("follow-up after the stop = %+v %v", follow, e)
	}
	again, e := f.store.TaskRun(f.ctx, f.tenant, task.ID, run.ID)
	if e != nil || again.SessionID != run.SessionID || !again.TurnActive() {
		t.Fatalf("run after the follow-up = %+v %v", again, e)
	}
	// Stopping the run's idle session ends it.
	f.port.finish(t, again.LastTurnID, execution.StatusCompleted)
	f.await(t, task.ID, store.TaskReview)
	if ended, e := f.host.Stop(f.ctx, f.owner, task.ID, run.ID); e != nil || ended.State != store.TaskRunEnded || ended.EndReason != store.TaskRunStopped {
		t.Fatalf("stop of the idle run = %+v %v", ended, e)
	}
}

// Two stops of the same running turn arrive at once and the session outlives the stop: the second
// Stop finds the turn it was sent for already stopped and leaves the idle run alone, instead of
// ending it as a Stop of the idle run would.
func TestConcurrentStopsOfAKeptTurnKeepTheRun(t *testing.T) {
	f := newFixture(t)
	f.port.keepOnCancel = true
	task := f.task(t, opencode)
	run, e := f.host.Start(f.ctx, f.member, task.ID, "start", taskrun.StartInput{})
	if e != nil {
		t.Fatal(e)
	}
	f.port.cancelGate = make(chan struct{})
	read := make(chan struct{}, 2)
	taskrun.SetStopRead(f.host, func() { read <- struct{}{} })
	stopped := make(chan error, 2)
	for range 2 {
		go func() {
			_, e := f.host.Stop(f.ctx, f.owner, task.ID, run.ID)
			stopped <- e
		}()
	}
	<-read
	<-read // both saw the turn running
	close(f.port.cancelGate)
	for range 2 {
		if e := <-stopped; e != nil {
			t.Fatal(e)
		}
	}
	got, e := f.store.TaskRun(f.ctx, f.tenant, task.ID, run.ID)
	if e != nil || !got.Live() || got.TurnActive() || got.EndReason != "" || got.LastResult != "cancelled" {
		t.Fatalf("run after two stops of one kept turn = %+v %v", got, e)
	}
}

// A Stop sent while turn A ran, that gets the task's lock only after another stop kept the
// session and the starter's next comment started turn B, does not stop B: it was sent for A.
func TestALateStopDoesNotStopTheNextTurn(t *testing.T) {
	f := newFixture(t)
	f.port.keepOnCancel = true
	task := f.task(t, opencode)
	run, e := f.host.Start(f.ctx, f.member, task.ID, "start", taskrun.StartInput{})
	if e != nil {
		t.Fatal(e)
	}
	var reads atomic.Int32
	seen, hold := make(chan struct{}), make(chan struct{})
	taskrun.SetStopRead(f.host, func() {
		if reads.Add(1) == 1 { // the late stop: it has seen A running, and waits
			close(seen)
			<-hold
		}
	})
	late := make(chan error, 1)
	go func() {
		_, e := f.host.Stop(f.ctx, f.owner, task.ID, run.ID)
		late <- e
	}()
	<-seen
	if _, e := f.host.Stop(f.ctx, f.owner, task.ID, run.ID); e != nil {
		t.Fatal(e)
	}
	follow, e := f.host.Comment(f.ctx, f.member, task.ID, "follow", taskrun.CommentInput{Body: "继续", ToAgent: true})
	if e != nil || follow.RunID != run.ID {
		t.Fatalf("follow-up after the kept stop = %+v %v", follow, e)
	}
	next, e := f.store.TaskRun(f.ctx, f.tenant, task.ID, run.ID)
	if e != nil || !next.TurnActive() || next.LastTurnID == run.LastTurnID {
		t.Fatalf("run running the follow-up = %+v %v", next, e)
	}
	close(hold)
	if e := <-late; e != nil {
		t.Fatal(e)
	}
	got, e := f.store.TaskRun(f.ctx, f.tenant, task.ID, run.ID)
	if e != nil || !got.TurnActive() || got.LastTurnID != next.LastTurnID || got.EndReason != "" {
		t.Fatalf("run after the late stop = %+v %v, want the follow-up still running", got, e)
	}
}

func TestRerunReplacesTheIdleRun(t *testing.T) {
	f := newFixture(t)
	task := f.task(t, opencode)
	first, e := f.host.Start(f.ctx, f.owner, task.ID, "first", taskrun.StartInput{})
	if e != nil {
		t.Fatal(e)
	}
	// A rerun cannot pull the session from under a running turn.
	if _, e := f.host.Start(f.ctx, f.member, task.ID, "early", taskrun.StartInput{}); !errors.Is(e, store.ErrRunActive) {
		t.Fatalf("rerun while running = %v", e)
	}
	f.port.finish(t, first.LastTurnID, execution.StatusFailed)
	f.await(t, task.ID, store.TaskReview)

	second, e := f.host.Start(f.ctx, f.member, task.ID, "second", taskrun.StartInput{})
	if e != nil || second.StartedBy != f.member.UserID {
		t.Fatalf("rerun = %+v %v", second, e)
	}
	old, e := f.store.TaskRun(f.ctx, f.tenant, task.ID, first.ID)
	if e != nil || old.EndReason != store.TaskRunReplaced || old.LastResult != "failed" {
		t.Fatalf("replaced run = %+v %v", old, e)
	}
	if got := f.read(t, task.ID); got.Status != store.TaskRunning || got.ActiveRun.ID != second.ID {
		t.Fatalf("task after rerun = %+v", got)
	}
}

// A starter who has lost tenant access is refused before the start changes anything: the idle
// run someone else left keeps its session instead of being replaced on the way to the refusal.
func TestRevokedStarterLeavesTheIdleRunInPlace(t *testing.T) {
	f := newFixture(t)
	task := f.task(t, opencode)
	first, e := f.host.Start(f.ctx, f.owner, task.ID, "first", taskrun.StartInput{})
	if e != nil {
		t.Fatal(e)
	}
	f.port.finish(t, first.LastTurnID, execution.StatusCompleted)
	f.await(t, task.ID, store.TaskReview)
	// Retiring the account is how a person leaves the team today; either way access is gone.
	if e := f.store.RetireUsers(f.ctx, []store.RetiredUser{{Email: storetest.Email("task-member")}}); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.TenantAccess(f.ctx, f.tenant, f.member.UserID); !errors.Is(e, store.ErrNotFound) {
		t.Fatalf("retired member access = %v", e)
	}
	if _, e := f.host.Start(f.ctx, f.member, task.ID, "revoked", taskrun.StartInput{}); !errors.Is(e, store.ErrNotFound) {
		t.Fatalf("revoked start = %v", e)
	}
	old, e := f.store.TaskRun(f.ctx, f.tenant, task.ID, first.ID)
	if e != nil || old.EndReason != "" {
		t.Fatalf("idle run after a refused start = %+v %v", old, e)
	}
	if got := f.read(t, task.ID); got.ActiveRun == nil || got.ActiveRun.ID != first.ID {
		t.Fatalf("task after a refused start = %+v", got)
	}
}

func TestClosingTheTaskEndsTheIdleRun(t *testing.T) {
	f := newFixture(t)
	task := f.task(t, opencode)
	run, e := f.host.Start(f.ctx, f.owner, task.ID, "start", taskrun.StartInput{})
	if e != nil {
		t.Fatal(e)
	}
	f.port.finish(t, run.LastTurnID, execution.StatusCompleted)
	got := f.await(t, task.ID, store.TaskReview)
	if _, e := f.store.SetTaskStatus(f.ctx, storetest.Access(t, f.store, f.tenant, f.owner.UserID), task.ID, store.TaskDone, got.Version); e != nil {
		t.Fatal(e)
	}
	f.host.Release(f.ctx, f.tenant, task.ID)
	if ended, e := f.store.TaskRun(f.ctx, f.tenant, task.ID, run.ID); e != nil || ended.EndReason != store.TaskRunClosed {
		t.Fatalf("run of a done task = %+v %v", ended, e)
	}
	if _, e := f.host.Start(f.ctx, f.owner, task.ID, "again", taskrun.StartInput{}); !errors.Is(e, taskrun.ErrTaskClosed) {
		t.Fatalf("start on a done task = %v", e)
	}
}

func TestRunsNeedAnAgentAndItsOwnExecutor(t *testing.T) {
	f := newFixture(t)
	if _, e := f.host.Start(f.ctx, f.owner, f.task(t, "").ID, "k", taskrun.StartInput{}); !errors.Is(e, taskrun.ErrNotAgentTask) {
		t.Fatalf("unassigned = %v", e)
	}
	task := f.task(t, opencode)
	if _, e := f.host.Start(f.ctx, f.owner, task.ID, "k", taskrun.StartInput{ExecutorID: "claude:fixture"}); !errors.Is(e, taskrun.ErrNoExecutor) {
		t.Fatalf("another agent's executor = %v", e)
	}
	list, e := f.host.Executors(f.ctx, f.member, task.ID)
	if e != nil || len(list) != 2 {
		t.Fatalf("executors = %+v %v", list, e)
	}
	// A refused start leaves no run behind.
	if got := f.read(t, task.ID); got.ActiveRun != nil || got.Status != store.TaskPending {
		t.Fatalf("task after refused start = %+v", got)
	}
}

func TestSquadMembersRunTheAgentTheyPickOnTheSquadsTask(t *testing.T) {
	f := newFixture(t)
	squad, e := f.store.CreateSquad(f.ctx, f.tenant, f.owner.UserID, store.SquadInput{Name: "Web 前端组"})
	if e != nil {
		t.Fatal(e)
	}
	task, e := f.store.CreateTask(f.ctx, f.tenant, f.owner.UserID, store.TaskInput{Title: "改版登录页", ProjectID: f.project,
		Assignee: &store.TaskAssigneeRef{Type: store.AssigneeSquad, ID: squad.ID}})
	if e != nil {
		t.Fatal(e)
	}

	// Someone outside the squad only watches.
	if _, e := f.host.Executors(f.ctx, f.member, task.ID); !errors.Is(e, taskrun.ErrNotAssignee) {
		t.Fatalf("executors outside the squad = %v", e)
	}
	if _, e := f.host.Start(f.ctx, f.member, task.ID, "k-out", taskrun.StartInput{}); !errors.Is(e, taskrun.ErrNotAssignee) {
		t.Fatalf("start outside the squad = %v", e)
	}

	// A member picks from every executor they can run and works under their own identity.
	if _, e := f.store.AddSquadMember(f.ctx, storetest.Access(t, f.store, f.tenant, f.owner.UserID), squad.ID, store.SquadMemberInput{Type: "human", UserID: f.member.UserID, Version: squad.Version}); e != nil {
		t.Fatal(e)
	}
	list, e := f.host.Executors(f.ctx, f.member, task.ID)
	if e != nil || len(list) != 3 {
		t.Fatalf("member's executors = %+v %v", list, e)
	}
	run, e := f.host.Start(f.ctx, f.member, task.ID, "k-member", taskrun.StartInput{ExecutorID: "claude:fixture"})
	if e != nil || run.ExecutorID != "claude:fixture" || run.StartedBy != f.member.UserID {
		t.Fatalf("member's run = %+v %v", run, e)
	}
	f.port.finish(t, run.LastTurnID, execution.StatusCompleted)
	f.await(t, task.ID, store.TaskReview)
}

func TestAPersonRunsTheAgentTheyPickOnTheirOwnTask(t *testing.T) {
	f := newFixture(t)
	task, e := f.store.CreateTask(f.ctx, f.tenant, f.owner.UserID, store.TaskInput{Title: "修复登录页", ProjectID: f.project,
		Assignee: &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: f.member.UserID}})
	if e != nil {
		t.Fatal(e)
	}

	// Everyone else only watches: listing, starting and a comment to the Agent are refused alike.
	if _, e := f.host.Executors(f.ctx, f.owner, task.ID); !errors.Is(e, taskrun.ErrNotAssignee) {
		t.Fatalf("executors for another person = %v", e)
	}
	if _, e := f.host.Start(f.ctx, f.owner, task.ID, "k-owner", taskrun.StartInput{}); !errors.Is(e, taskrun.ErrNotAssignee) {
		t.Fatalf("start by another person = %v", e)
	}
	if _, e := f.host.Comment(f.ctx, f.owner, task.ID, "c-owner", taskrun.CommentInput{Body: "我来", ToAgent: true}); !errors.Is(e, taskrun.ErrNotAssignee) {
		t.Fatalf("comment to the Agent by another person = %v", e)
	}

	// The assignee picks from every executor they can run, of any Agent.
	list, e := f.host.Executors(f.ctx, f.member, task.ID)
	if e != nil || len(list) != 3 {
		t.Fatalf("assignee's executors = %+v %v", list, e)
	}
	run, e := f.host.Start(f.ctx, f.member, task.ID, "k-member", taskrun.StartInput{ExecutorID: "claude:fixture"})
	if e != nil || run.ExecutorID != "claude:fixture" || run.StartedBy != f.member.UserID {
		t.Fatalf("assignee's run = %+v %v", run, e)
	}
	if got := f.read(t, task.ID); got.Status != store.TaskRunning {
		t.Fatalf("task while running = %+v", got)
	}
	f.port.finish(t, run.LastTurnID, execution.StatusCompleted)
	f.await(t, task.ID, store.TaskReview)

	// The task's creator may still stop the run, though they could not start it.
	if _, e := f.host.Stop(f.ctx, f.owner, task.ID, run.ID); e != nil {
		t.Fatalf("stop by the creator = %v", e)
	}
}

// Each of the three who may stop a run may do so on their own standing: whoever started it, the
// task's creator without being the owner, and the owner without having created it.
func TestStopIsForTheStarterTheCreatorAndTheOwner(t *testing.T) {
	f := newFixture(t)
	starter, e := f.store.SessionUser(f.ctx, storetest.SignIn(t, f.store, "task-starter", "卡萝", store.RoleMember))
	if e != nil {
		t.Fatal(e)
	}
	as := execution.Scope{TenantID: f.tenant, UserID: starter.ID}
	// The member creates the Agent task; neither the starter nor the owner created it.
	task, e := f.store.CreateTask(f.ctx, f.tenant, f.member.UserID, store.TaskInput{Title: "修复登录页", ProjectID: f.project,
		Assignee: &store.TaskAssigneeRef{Type: store.AssigneeAgent, ID: f.agents[opencode]}})
	if e != nil {
		t.Fatal(e)
	}
	for i, stopper := range []struct {
		name  string
		scope execution.Scope
	}{{"the starter", as}, {"the creator", f.member}, {"the owner", f.owner}} {
		run, e := f.host.Start(f.ctx, as, task.ID, fmt.Sprintf("start-%d", i), taskrun.StartInput{})
		if e != nil {
			t.Fatalf("start before %s stops = %v", stopper.name, e)
		}
		stopped, e := f.host.Stop(f.ctx, stopper.scope, task.ID, run.ID)
		if e != nil || stopped.State != store.TaskRunEnded || stopped.EndReason != store.TaskRunStopped {
			t.Fatalf("stop by %s = %+v %v", stopper.name, stopped, e)
		}
		f.await(t, task.ID, store.TaskPending)
	}
}

func TestInterruptEndsRunsLeftByThePreviousProcess(t *testing.T) {
	f := newFixture(t)
	task := f.task(t, opencode)
	run, e := f.host.Start(f.ctx, f.owner, task.ID, "start", taskrun.StartInput{})
	if e != nil {
		t.Fatal(e)
	}
	f.host.Close()
	next := taskrun.New(f.store, f.port.runtime(), f.port.runtime(), nil)
	t.Cleanup(next.Close)
	if e := next.Interrupt(f.ctx); e != nil {
		t.Fatal(e)
	}
	got, e := f.store.TaskRun(f.ctx, f.tenant, task.ID, run.ID)
	if e != nil || got.EndReason != store.TaskRunInterrupted || got.LastResult != "interrupted" {
		t.Fatalf("interrupted run = %+v %v", got, e)
	}
	if task := f.read(t, task.ID); task.Status != store.TaskPending || task.ActiveRun != nil {
		t.Fatalf("task after restart = %+v", task)
	}
}

func TestStartRecordsAnAcceptedTurnAfterTheRequestIsGone(t *testing.T) {
	f := newFixture(t)
	task := f.task(t, opencode)
	ctx, cancel := context.WithCancel(f.ctx)
	runtime := f.port.runtime()
	start := runtime.StartFunc
	runtime.StartFunc = func(c context.Context, scope execution.Scope, req execution.StartRequest) (execution.Ref, error) {
		ref, e := start(c, scope, req)
		cancel() // the page left just as the service accepted the turn
		return ref, e
	}
	host := taskrun.New(f.store, runtime, runtime, nil)
	t.Cleanup(host.Close)

	run, e := host.Start(ctx, f.owner, task.ID, "start-1", taskrun.StartInput{})
	if e != nil || run.SessionID == "" || run.State != store.TaskRunRunning {
		t.Fatalf("run = %+v %v", run, e)
	}
	// The run knows its session, so it can still be stopped instead of holding the task.
	if _, e := host.Stop(f.ctx, f.owner, task.ID, run.ID); e != nil {
		t.Fatalf("stop = %v", e)
	}
	f.await(t, task.ID, store.TaskPending)
}
