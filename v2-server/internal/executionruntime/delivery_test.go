package executionruntime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/chat"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/taskrun"
	"github.com/wanglongan587/cloud/v2-server/internal/workspacefs"
)

type deliveryBarrier struct {
	*workspacefs.DeliveryFS
	at    chan workspacefs.DeliveryRequest
	allow chan struct{}
}

func (b *deliveryBarrier) Deliver(ctx context.Context, req workspacefs.DeliveryRequest) (workspacefs.Snapshot, error) {
	select {
	case b.at <- req:
	case <-ctx.Done():
		return workspacefs.Snapshot{}, ctx.Err()
	}
	select {
	case <-b.allow:
		return b.DeliveryFS.Deliver(ctx, req)
	case <-ctx.Done():
		return workspacefs.Snapshot{}, ctx.Err()
	}
}

func runtimeDeliveryGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.test"}, args...)...)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

func runtimeDeliveryProject(t *testing.T, f *runtimeFixture) (store.Project, store.WorkspaceLocation) {
	t.Helper()
	root := t.TempDir()
	runtimeDeliveryGit(t, root, "init", "-q")
	runtimeDeliveryGit(t, root, "commit", "--allow-empty", "-qm", "base")
	base := runtimeDeliveryGit(t, root, "rev-parse", "HEAD")
	mirror := filepath.Join(t.TempDir(), "mirror.git")
	runtimeDeliveryGit(t, root, "clone", "--mirror", root, mirror)
	p, err := f.db.CreateProject(f.ctx, f.scope, store.Project{ID: newID(), Name: "Delivery", Commit: base, MirrorPath: mirror, RepositoryURL: "https://example.test/fixture.git"})
	if err != nil {
		t.Fatal(err)
	}
	return p, store.WorkspaceLocation{Path: root, BaseCommit: base}
}

func runtimeDeliverySource(t *testing.T, f *runtimeFixture, project string, kind execution.CallerKind) (store.DeliverySource, execution.Caller) {
	t.Helper()
	caller := execution.Caller{Kind: kind, AttemptID: "first"}
	src := store.DeliverySource{Kind: kind}
	switch kind {
	case execution.CallerChat:
		c, err := f.db.CreateChatConversation(f.ctx, store.NewChatConversation{TenantID: f.scope.TenantID, UserID: f.scope.UserID, CreateKey: newID(), ProjectID: project, ExecutorID: "opencode:fixture", Title: "delivery"})
		if err != nil {
			t.Fatal(err)
		}
		src.ID = c.ID
	case execution.CallerTask:
		task, err := f.db.CreateTask(f.ctx, f.scope.TenantID, f.scope.UserID, store.TaskInput{Title: "delivery", ProjectID: project})
		if err != nil {
			t.Fatal(err)
		}
		run, err := f.db.CreateTaskRun(f.ctx, store.NewTaskRun{TenantID: f.scope.TenantID, TaskID: task.ID, StartedBy: f.scope.UserID, StartKey: newID(), ExecutorID: "opencode:fixture", ExecutorName: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		src.ID, src.TaskID = run.ID, task.ID
	case execution.CallerWorkflow:
		w, _, err := f.db.CreateWorkflow(f.ctx, f.scope.TenantID, "delivery", recoveryGraph)
		if err != nil {
			t.Fatal(err)
		}
		snap, err := f.db.PublishWorkflow(f.ctx, f.scope.TenantID, w.ID, "v1", nil)
		if err != nil {
			t.Fatal(err)
		}
		run, err := f.db.CreateWorkflowRun(f.ctx, f.scope.TenantID, w.ID, snap.ID, store.WorkflowRunInput{ProjectID: project})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.db.StartWorkflowRun(f.ctx, f.scope.TenantID, run.ID); err != nil {
			t.Fatal(err)
		}
		node, err := f.db.CreateWorkflowNodeRun(f.ctx, f.scope.TenantID, run.ID, store.WorkflowNodeRunInput{NodeID: "a", NodeType: "agent"})
		if err != nil {
			t.Fatal(err)
		}
		src.ID = run.ID
		caller.NodeID, caller.AttemptID = node.NodeID, node.ID
	}
	caller.ID = src.ID
	return src, caller
}

func TestTurnDeliveryOnceForEachSourceAndOutcome(t *testing.T) {
	f := runtimeDatabase(t, nil)
	p, at := runtimeDeliveryProject(t, f)
	fs, err := workspacefs.NewDeliveryFS(t.TempDir(), workspacefs.DefaultDeliveryObjectBytes)
	if err != nil {
		t.Fatal(err)
	}
	barrier := &deliveryBarrier{fs, make(chan workspacefs.DeliveryRequest), make(chan struct{}, 1)}
	agent := &orchestrationAgent{started: make(chan *agentCall, 1), ended: make(chan string, 20)}
	if err := f.service.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.service, err = New(f.ctx, Config{Store: f.db, Models: f.models, Agent: agent, Delivery: barrier})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind   execution.CallerKind
		result execution.Result
		silent bool
	}{
		{execution.CallerChat, execution.Result{Status: execution.StatusCompleted, AnswerComplete: true}, false},
		{execution.CallerTask, execution.Result{Status: execution.StatusFailed, FailureCode: "fixture"}, true},
		{execution.CallerWorkflow, execution.Result{Status: execution.StatusCancelled}, false},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			src, caller := runtimeDeliverySource(t, f, p.ID, tc.kind)
			ref, err := f.service.Start(f.ctx, f.scope, execution.StartRequest{Caller: caller, Target: execution.Target{ProjectID: p.ID}, ExecutorID: "opencode:fixture", Prompt: execution.TextPrompt("hello")})
			if err != nil {
				t.Fatal(err)
			}
			call := nextAgentCall(t, f, agent)
			if err := f.db.RecordWorkspace(f.ctx, call.session.WorkspaceID, at); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(at.Path, "change"), []byte(tc.kind), 0600); err != nil {
				t.Fatal(err)
			}
			call.result, call.silent = &tc.result, tc.silent
			call.unblock()
			select {
			case <-barrier.at:
			case <-f.ctx.Done():
				t.Fatal(f.ctx.Err())
			}
			allowed := false
			defer func() {
				if !allowed {
					barrier.allow <- struct{}{}
				}
			}()
			page, err := f.service.ReadTurns(f.ctx, f.scope, ref.SessionID, "", 1)
			if err != nil || len(page.Items) != 1 || page.Items[0].Result != nil {
				t.Fatalf("turn visible during delivery: %+v, %v", page, err)
			}
			view, err := f.db.WorkspaceDeliveryView(f.ctx, f.scope, src)
			if err != nil || !view.InProgress || view.Latest != nil {
				t.Fatalf("pending delivery: %+v, %v", view, err)
			}
			if tc.kind == execution.CallerChat {
				_, err := f.service.Append(f.ctx, f.scope, execution.AppendRequest{SessionID: ref.SessionID, Caller: execution.Caller{Kind: tc.kind, ID: src.ID, AttemptID: "next"}, Prompt: execution.TextPrompt("next")})
				if !errors.Is(err, execution.ErrWorkspaceBusy) {
					t.Fatalf("append during delivery: %v", err)
				}
			}
			barrier.allow <- struct{}{}
			allowed = true
			got, _ := observeTurn(t, f, ref, 0)
			if got.Status != tc.result.Status {
				t.Fatalf("actual turn status %s, want %s", got.Status, tc.result.Status)
			}
			view, err = f.db.WorkspaceDeliveryView(f.ctx, f.scope, src)
			if err != nil || view.InProgress || view.Good == nil || view.Latest == nil || view.Good.TurnID != ref.TurnID || view.Good.Status != store.DeliveryDelivered {
				t.Fatalf("final delivery: %+v, %v", view, err)
			}
			if tc.kind == execution.CallerWorkflow && view.Good.NodeRunID != caller.AttemptID {
				t.Fatalf("workflow attempt not retained: %+v", view.Good)
			}
			if _, err := f.db.StageDeliveryTerminal(f.ctx, f.scope, ref, execution.Result{Status: execution.StatusFailed, FailureCode: "duplicate"}); err != nil {
				t.Fatal(err)
			}
			again, err := f.db.WorkspaceDeliveryView(f.ctx, f.scope, src)
			if err != nil || again.Latest == nil || again.Latest.ID != view.Latest.ID {
				t.Fatalf("duplicate changed delivery: %+v, %v", again, err)
			}
		})
	}
	if got := strings.Fields(runtimeDeliveryGit(t, p.MirrorPath, "for-each-ref", "--format=%(refname)", "refs/ora/deliveries")); len(got) != 3 {
		t.Fatalf("delivery refs = %v", got)
	}
}

type stopAfterRef struct {
	*workspacefs.DeliveryFS
	stop context.CancelFunc
}

func (s stopAfterRef) Deliver(ctx context.Context, req workspacefs.DeliveryRequest) (workspacefs.Snapshot, error) {
	snap, err := s.DeliveryFS.Deliver(ctx, req)
	if err == nil && snap.Commit != "" {
		s.stop()
	}
	return snap, err
}

func TestDeliveryRefBeforeRecordCrashAndRestart(t *testing.T) {
	f := runtimeDatabase(t, nil)
	project, at := runtimeDeliveryProject(t, f)
	fs, err := workspacefs.NewDeliveryFS(t.TempDir(), workspacefs.DefaultDeliveryObjectBytes)
	if err != nil {
		t.Fatal(err)
	}
	agent := &orchestrationAgent{started: make(chan *agentCall, 1), ended: make(chan string, 1)}
	if err := f.service.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	run, stop := context.WithCancel(f.ctx)
	defer stop()
	f.service, err = New(run, Config{Store: f.db, Models: f.models, Agent: agent, Delivery: stopAfterRef{fs, stop}})
	if err != nil {
		t.Fatal(err)
	}
	source, caller := runtimeDeliverySource(t, f, project.ID, execution.CallerChat)
	ref, err := f.service.Start(f.ctx, f.scope, execution.StartRequest{Caller: caller, Target: execution.Target{ProjectID: project.ID}, ExecutorID: "opencode:fixture", Prompt: execution.TextPrompt("write")})
	if err != nil {
		t.Fatal(err)
	}
	call := nextAgentCall(t, f, agent)
	if err := f.db.RecordWorkspace(f.ctx, call.session.WorkspaceID, at); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(at.Path, "change"), []byte("recover"), 0600); err != nil {
		t.Fatal(err)
	}
	call.unblock()
	select {
	case <-run.Done():
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	if err := f.service.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	view, err := f.db.WorkspaceDeliveryView(f.ctx, f.scope, source)
	if err != nil || view.Latest != nil || !view.InProgress {
		t.Fatalf("orphan surfaced before recovery: %+v, %v", view, err)
	}
	orphan := runtimeDeliveryGit(t, project.MirrorPath, "for-each-ref", "--format=%(refname)", "refs/ora/deliveries")
	if orphan == "" {
		t.Fatal("ref does not precede record")
	}
	var prior string
	for range 2 {
		f.service, err = New(f.ctx, Config{Store: f.db, Models: f.models, Delivery: fs})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.RecoverTurnDeliveries(f.ctx); err != nil {
			t.Fatal(err)
		}
		view, err = f.db.WorkspaceDeliveryView(f.ctx, f.scope, source)
		if err != nil || view.InProgress || view.Good == nil || view.Good.TurnID != ref.TurnID || view.Good.Status != store.DeliveryDelivered {
			t.Fatalf("boot did not deliver once: %+v, %v", view, err)
		}
		if prior != "" && view.Good.ID != prior {
			t.Fatal("second boot replaced delivery")
		}
		prior = view.Good.ID
		if err := f.service.Close(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Fields(runtimeDeliveryGit(t, project.MirrorPath, "for-each-ref", "--format=%(refname)", "refs/ora/deliveries")); len(got) != 2 || !strings.Contains(strings.Join(got, " "), orphan) {
		t.Fatalf("refs = %v, missing live or original orphan", got)
	}
	page, err := f.db.ReadExecutionTurns(f.ctx, f.scope, ref.SessionID, "", 1)
	if err != nil || len(page.Items) != 1 || page.Items[0].Result == nil || page.Items[0].Result.Status != execution.StatusCompleted {
		t.Fatalf("ACKed result lost on restart: %+v, %v", page, err)
	}
}

func TestInterruptedTurnsAreDeliveredBeforeAdmission(t *testing.T) {
	f := runtimeDatabase(t, nil)
	project, at := runtimeDeliveryProject(t, f)
	fs, err := workspacefs.NewDeliveryFS(t.TempDir(), workspacefs.DefaultDeliveryObjectBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(at.Path, "change"), []byte("interrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	type interrupted struct {
		source store.DeliverySource
		ref    execution.Ref
	}
	var turns []interrupted
	for _, kind := range []execution.CallerKind{execution.CallerTask, execution.CallerWorkflow, execution.CallerChat} {
		src, caller := runtimeDeliverySource(t, f, project.ID, kind)
		session := execution.Session{ID: newID(), WorkspaceID: newID(), Target: &execution.Target{ProjectID: project.ID}}
		ref := execution.Ref{SessionID: session.ID, TurnID: newID()}
		// Exercise upgrade: admitted before delivery enrollment existed.
		if err := f.db.CreateExecutionSession(f.ctx, f.scope, session, execution.Turn{Ref: ref, Caller: caller, Prompt: execution.TextPrompt("interrupted")}); err != nil {
			t.Fatal(err)
		}
		location := at
		if kind == execution.CallerWorkflow {
			location.Path = filepath.Join(at.Path, "removed")
		}
		if err := f.db.RecordWorkspace(f.ctx, session.WorkspaceID, location); err != nil {
			t.Fatal(err)
		}
		if kind == execution.CallerTask {
			if err := f.db.RecordTaskTurn(f.ctx, src.ID, ref.SessionID, ref.TurnID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.TransitionTaskStatus(f.ctx, f.scope.TenantID, src.TaskID, store.TaskPending, store.TaskRunning); err != nil {
				t.Fatal(err)
			}
		}
		turns = append(turns, interrupted{src, ref})
	}
	if err := f.service.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.service, err = New(f.ctx, Config{Store: f.db, Models: f.models, Delivery: fs})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range turns {
		p, err := f.db.ReadExecutionTurns(f.ctx, f.scope, x.ref.SessionID, "", 1)
		if err != nil || len(p.Items) != 1 || p.Items[0].Result == nil || p.Items[0].Result.Status != execution.StatusInterrupted {
			t.Fatalf("not interrupted: %+v %v", p, err)
		}
	}
	if _, err := f.db.InterruptTaskRuns(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.service.RecoverTurnDeliveries(f.ctx); err != nil {
		t.Fatal(err)
	}
	for _, x := range turns {
		view, err := f.db.WorkspaceDeliveryView(f.ctx, f.scope, x.source)
		if err != nil || view.InProgress || view.Latest == nil || view.Latest.TurnID != x.ref.TurnID {
			t.Fatalf("missing boot outcome: %+v %v", view, err)
		}
		if x.source.Kind == execution.CallerWorkflow {
			if view.Latest.Status != store.DeliveryFailed || view.Latest.ReasonCode != "workspace_removed" {
				t.Fatalf("missing checkout outcome: %+v", view.Latest)
			}
		} else if view.Latest.Status != store.DeliveryDelivered {
			t.Fatalf("unexpected boot outcome: %+v", view.Latest)
		}
		if x.source.Kind == execution.CallerTask {
			task, err := f.db.Task(f.ctx, f.scope.TenantID, x.source.TaskID)
			if err != nil || task.Status != store.TaskPending {
				t.Fatalf("interrupted task not pending: %+v %v", task, err)
			}
		}
	}
	if err := f.service.RecoverTurnDeliveries(f.ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Fields(runtimeDeliveryGit(t, project.MirrorPath, "for-each-ref", "--format=%(refname)", "refs/ora/deliveries"))); n != 2 {
		t.Fatalf("boot created %d refs, want 2", n)
	}
}

func TestChatDeliveryFailureKeepsSessionOpen(t *testing.T) {
	f := runtimeDatabase(t, nil)
	p, at := runtimeDeliveryProject(t, f)
	fs, err := workspacefs.NewDeliveryFS(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	agent := &orchestrationAgent{started: make(chan *agentCall, 1), ended: make(chan string, 4)}
	if err := f.service.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.service, err = New(f.ctx, Config{Store: f.db, Models: f.models, Agent: agent, Delivery: fs})
	if err != nil {
		t.Fatal(err)
	}
	service := chat.New(f.db, f.service, f.service, chat.Options{})
	conversation, ref, err := service.Create(f.ctx, f.scope, chat.CreateRequest{Key: "first", ProjectID: p.ID, ExecutorID: "opencode:fixture", Prompt: "write"})
	if err != nil {
		t.Fatal(err)
	}
	call := nextAgentCall(t, f, agent)
	if err := f.db.RecordWorkspace(f.ctx, call.session.WorkspaceID, at); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(at.Path, "change"), []byte("over cap"), 0600); err != nil {
		t.Fatal(err)
	}
	call.unblock()
	result, _ := observeTurn(t, f, ref, 0)
	if result.Status != execution.StatusCompleted || result.AgentLost {
		t.Fatalf("delivery changed result: %+v", result)
	}
	view, err := f.db.WorkspaceDeliveryView(f.ctx, f.scope, store.DeliverySource{Kind: execution.CallerChat, ID: conversation.ID})
	if err != nil || view.Failed == nil || view.Failed.Reason != "改动过大" || view.Good != nil {
		t.Fatalf("failed delivery: %+v %v", view, err)
	}
	_, next, err := service.Send(f.ctx, f.scope, conversation.ID, "second", "continue")
	if err != nil || next.SessionID != ref.SessionID || next.TurnID == ref.TurnID {
		t.Fatalf("next turn: %+v, %v", next, err)
	}
	nextAgentCall(t, f, agent).unblock()
	observeTurn(t, f, next, 0)
}

func TestTaskHostUsesDeliveryOutcome(t *testing.T) {
	f := runtimeDatabase(t, nil)
	project, at := runtimeDeliveryProject(t, f)
	fs, err := workspacefs.NewDeliveryFS(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	agent := &orchestrationAgent{started: make(chan *agentCall, 1), ended: make(chan string, 4)}
	if err := f.service.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.service, err = New(f.ctx, Config{Store: f.db, Models: f.models, Agent: agent, Delivery: fs})
	if err != nil {
		t.Fatal(err)
	}
	host := taskrun.New(f.db, f.service, f.service, nil)
	defer host.Close()
	task, err := f.db.CreateTask(f.ctx, f.scope.TenantID, f.scope.UserID, store.TaskInput{
		Title: "Delivery task", ProjectID: project.ID,
		Assignee: &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: f.scope.UserID},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := host.Start(f.ctx, f.scope, task.ID, "first", taskrun.StartInput{ExecutorID: "opencode:fixture"})
	if err != nil {
		t.Fatal(err)
	}
	call := nextAgentCall(t, f, agent)
	if err := f.db.RecordWorkspace(f.ctx, call.session.WorkspaceID, at); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(at.Path, "change"), []byte("over cap"), 0600); err != nil {
		t.Fatal(err)
	}
	call.unblock()
	result, _ := observeTurn(t, f, execution.Ref{SessionID: run.SessionID, TurnID: run.LastTurnID}, 0)
	if result.Status != execution.StatusCompleted {
		t.Fatalf("turn did not complete: %+v", result)
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		record, err := f.db.Task(f.ctx, f.scope.TenantID, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if record.Status != store.TaskRunning {
			if record.Status != store.TaskPending || record.ActiveRun == nil || record.ActiveRun.State != store.TaskRunIdle {
				t.Fatalf("task did not return to pending after delivery failure: %+v", record)
			}
			break
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("task observer did not settle the delivered turn")
		}
	}
}

// A turn that changes nothing is delivered as empty: the record has no delivery commit, the mirror
// gains no ref, and the turn's terminal is still published.
func TestUnchangedTurnRecordsEmptyWithoutRef(t *testing.T) {
	f := runtimeDatabase(t, nil)
	p, at := runtimeDeliveryProject(t, f)
	fs, err := workspacefs.NewDeliveryFS(t.TempDir(), workspacefs.DefaultDeliveryObjectBytes)
	if err != nil {
		t.Fatal(err)
	}
	agent := &orchestrationAgent{started: make(chan *agentCall, 1), ended: make(chan string, 20)}
	if err := f.service.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.service, err = New(f.ctx, Config{Store: f.db, Models: f.models, Agent: agent, Delivery: fs})
	if err != nil {
		t.Fatal(err)
	}
	src, caller := runtimeDeliverySource(t, f, p.ID, execution.CallerChat)
	ref, err := f.service.Start(f.ctx, f.scope, execution.StartRequest{Caller: caller, Target: execution.Target{ProjectID: p.ID}, ExecutorID: "opencode:fixture", Prompt: execution.TextPrompt("look, change nothing")})
	if err != nil {
		t.Fatal(err)
	}
	call := nextAgentCall(t, f, agent)
	if err := f.db.RecordWorkspace(f.ctx, call.session.WorkspaceID, at); err != nil {
		t.Fatal(err)
	}
	call.result = &execution.Result{Status: execution.StatusCompleted, Answer: "nothing to change", AnswerComplete: true}
	call.unblock()
	got, _ := observeTurn(t, f, ref, 0)
	if got.Status != execution.StatusCompleted {
		t.Fatalf("published terminal = %+v, want completed", got)
	}
	page, err := f.service.ReadTurns(f.ctx, f.scope, ref.SessionID, "", 1)
	if err != nil || len(page.Items) != 1 || page.Items[0].Result == nil || page.Items[0].Result.Status != execution.StatusCompleted {
		t.Fatalf("turn result not published: %+v, %v", page, err)
	}
	view, err := f.db.WorkspaceDeliveryView(f.ctx, f.scope, src)
	if err != nil || view.InProgress || view.HasWork || view.Latest == nil || view.Good == nil || view.Good.ID != view.Latest.ID {
		t.Fatalf("delivery state after an unchanged turn: %+v, %v", view, err)
	}
	if d := view.Latest; d.Status != store.DeliveryEmpty || d.DeliveryCommit != "" || d.TurnID != ref.TurnID || d.BaseCommit != at.BaseCommit {
		t.Fatalf("unchanged turn recorded %+v, want empty with no delivery commit", d)
	}
	if refs := runtimeDeliveryGit(t, p.MirrorPath, "for-each-ref", "--format=%(refname)", "refs/ora/deliveries"); refs != "" {
		t.Fatalf("an empty delivery wrote refs: %s", refs)
	}
}
