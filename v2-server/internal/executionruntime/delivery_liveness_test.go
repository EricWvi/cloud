package executionruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/taskrun"
	"github.com/wanglongan587/cloud/v2-server/internal/workspacefs"
)

func newBlockedDeliveryRuntime(t *testing.T) (*runtimeFixture, *deliveryBarrier, *orchestrationAgent, string, store.WorkspaceLocation) {
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
	t.Cleanup(func() {
		select {
		case barrier.allow <- struct{}{}:
		default:
		}
	})
	if err := os.WriteFile(filepath.Join(at.Path, "change"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f, barrier, agent, p.ID, at
}

// A Stop that arrives after the Agent finished, while its delivery runs, must wait for the
// delivery and must not end the healthy session.
func TestCancelDuringStagedDeliveryKeepsSessionOpen(t *testing.T) {
	f, barrier, agent, project, at := newBlockedDeliveryRuntime(t)
	_, ref, call := startBlockedChat(t, f, agent, project, at)
	call.unblock()
	waitDeliveryRequest(t, f, barrier.at)
	type out struct {
		r   execution.Result
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := f.service.Cancel(f.ctx, f.scope, ref)
		done <- out{r, err}
	}()
	select {
	case o := <-done:
		t.Fatalf("Cancel returned before delivery finished: %+v", o)
	case <-time.After(300 * time.Millisecond):
	}
	barrier.allow <- struct{}{}
	o := <-done
	if o.err != nil || o.r.Status != execution.StatusCompleted {
		t.Fatalf("Cancel = %+v, %v", o.r, o.err)
	}
	session, err := f.service.ReadSession(f.ctx, f.scope, ref.SessionID)
	if err != nil || session.EndedAt != nil {
		t.Fatalf("healthy session ended by Stop during delivery: %+v %v", session.EndedAt, err)
	}
	select {
	case sid := <-agent.ended:
		t.Fatalf("Agent %s stopped", sid)
	default:
	}
}

// A protocol error after a terminal must stop the Agent before the snapshot is collected.
func TestProtocolErrorStopsAgentBeforeSnapshot(t *testing.T) {
	f, barrier, agent, project, at := newBlockedDeliveryRuntime(t)
	_, _, call := startBlockedChat(t, f, agent, project, at)
	call.err = errors.New("history projection failed")
	call.unblock()
	waitDeliveryRequest(t, f, barrier.at)
	select {
	case <-agent.ended:
	default:
		t.Fatal("snapshot collected before the Agent was stopped")
	}
	barrier.allow <- struct{}{}
}

func startBlockedChat(t *testing.T, f *runtimeFixture, agent *orchestrationAgent, project string, at store.WorkspaceLocation) (store.DeliverySource, execution.Ref, *agentCall) {
	t.Helper()
	source, caller := runtimeDeliverySource(t, f, project, execution.CallerChat)
	ref, err := f.service.Start(f.ctx, f.scope, execution.StartRequest{Caller: caller, Target: execution.Target{ProjectID: project}, ExecutorID: "opencode:fixture", Prompt: execution.TextPrompt("hello")})
	if err != nil {
		t.Fatal(err)
	}
	call := nextAgentCall(t, f, agent)
	if err := f.db.RecordWorkspace(f.ctx, call.session.WorkspaceID, at); err != nil {
		t.Fatal(err)
	}
	return source, ref, call
}

func seedDeliveryWork(t *testing.T, f *runtimeFixture, project string, at store.WorkspaceLocation, ended bool) (store.DeliverySource, store.DeliveryWork) {
	t.Helper()
	src, caller := runtimeDeliverySource(t, f, project, execution.CallerChat)
	session := execution.Session{ID: newID(), WorkspaceID: newID(), Target: &execution.Target{ProjectID: project}}
	ref := execution.Ref{SessionID: session.ID, TurnID: newID()}
	turn := execution.Turn{Ref: ref, Caller: caller, Prompt: execution.TextPrompt("hello")}
	if err := f.db.CreateDeliveryExecutionSession(f.ctx, f.scope, session, turn); err != nil {
		t.Fatal(err)
	}
	if at.Path != "" {
		if err := f.db.RecordWorkspace(f.ctx, session.WorkspaceID, at); err != nil {
			t.Fatal(err)
		}
	}
	result := execution.Result{Status: execution.StatusCompleted, AnswerComplete: true}
	stage := f.db.StageDeliveryTerminal
	if ended {
		stage = f.db.FinishExecutionTurn
	}
	if _, err := stage(f.ctx, f.scope, ref, result); err != nil {
		t.Fatal(err)
	}
	work, err := f.db.DeliveryWork(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	return src, work
}

func readDeliveryView(t *testing.T, f *runtimeFixture, src store.DeliverySource) store.DeliveryView {
	t.Helper()
	view, err := f.db.WorkspaceDeliveryView(f.ctx, f.scope, src)
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func waitDeliveryRequest(t *testing.T, f *runtimeFixture, ch <-chan workspacefs.DeliveryRequest) workspacefs.DeliveryRequest {
	t.Helper()
	select {
	case req := <-ch:
		return req
	case <-f.ctx.Done():
		t.Fatal("delivery did not reach Git within the test budget")
		return workspacefs.DeliveryRequest{}
	}
}

func TestDeliveryRetriesTransientRecordWrite(t *testing.T) {
	f, barrier, agent, project, at := newBlockedDeliveryRuntime(t)
	var attempts []store.DeliveryRecord
	f.service.commitDelivery = func(ctx context.Context, work store.DeliveryWork, record store.DeliveryRecord) (store.DeliveryRecord, error) {
		attempts = append(attempts, record)
		if len(attempts) == 1 {
			return store.DeliveryRecord{}, errors.New("transient delivery write")
		}
		return f.db.CommitTurnDelivery(ctx, work, record)
	}
	src, ref, call := startBlockedChat(t, f, agent, project, at)
	call.unblock()
	req := waitDeliveryRequest(t, f, barrier.at)
	barrier.allow <- struct{}{}
	observeTurn(t, f, ref, 0)
	got := readDeliveryView(t, f, src)
	if got.Good == nil || got.Good.Status != store.DeliveryDelivered || len(attempts) != 2 ||
		attempts[0] != attempts[1] || got.Good.ID != req.DeliveryID {
		t.Fatalf("transient write did not retain and retry the same snapshot: view=%+v, attempts=%+v", got, attempts)
	}
}

func TestDeliveryInvalidResultBecomesTerminalFailure(t *testing.T) {
	f, barrier, agent, project, at := newBlockedDeliveryRuntime(t)
	src, ref, call := startBlockedChat(t, f, agent, project, at)
	// Returning a malformed result without emitting it exercises normalization in the worker.
	call.result = &execution.Result{Status: execution.StatusCompleted, Answer: "not complete"}
	call.silent = true
	// Pre-release the Git barrier so the assertion also sees a fallback that never calls Git.
	barrier.allow <- struct{}{}
	go func() {
		select {
		case <-barrier.at:
		case <-f.ctx.Done():
		}
	}()
	call.unblock()
	result, _ := observeTurn(t, f, ref, 0)
	view := readDeliveryView(t, f, src)
	if result.Status != execution.StatusFailed || result.FailureCode != "execution_failed" ||
		view.Latest == nil || view.Latest.Status != store.DeliveryDelivered {
		t.Fatalf("invalid result did not become an execution failure with a snapshot: %+v, %+v", result, view.Latest)
	}
}

func TestDeliveryFinalizationErrorDoesNotStrandTask(t *testing.T) {
	for _, failRecord := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed record unavailable=%t", failRecord), func(t *testing.T) {
			f, barrier, agent, project, at := newBlockedDeliveryRuntime(t)
			f.service.commitDelivery = func(ctx context.Context, work store.DeliveryWork, record store.DeliveryRecord) (store.DeliveryRecord, error) {
				if record.Status != store.DeliveryFailed || failRecord {
					return store.DeliveryRecord{}, execution.ErrInvalidRequest
				}
				return f.db.CommitTurnDelivery(ctx, work, record)
			}
			host := taskrun.New(f.db, f.service, f.service, nil)
			defer host.Close()
			task, err := f.db.CreateTask(f.ctx, f.scope.TenantID, f.scope.UserID, store.TaskInput{
				Title: "Delivery task", ProjectID: project, Assignee: &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: f.scope.UserID},
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
			call.unblock()
			waitDeliveryRequest(t, f, barrier.at)
			barrier.allow <- struct{}{}
			ref := execution.Ref{SessionID: run.SessionID, TurnID: run.LastTurnID}
			deadline := time.After(3 * time.Second)
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				got, err := f.db.Task(f.ctx, f.scope.TenantID, task.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.Status != store.TaskRunning {
					if got.Status != store.TaskPending {
						t.Fatalf("task settled to %s, want pending", got.Status)
					}
					break
				}
				select {
				case <-tick.C:
				case <-deadline:
					t.Fatal("delivery finalization error stranded the task")
				}
			}
			result, _ := observeTurn(t, f, ref, 0)
			view := readDeliveryView(t, f, store.DeliverySource{Kind: execution.CallerTask, ID: run.ID, TaskID: task.ID})
			if failRecord {
				if result.Status != execution.StatusFailed || result.FailureCode != execution.FailureDeliveryUnavailable {
					t.Fatalf("ordinary terminal fallback: %+v", result)
				}
			} else if view.Failed == nil || view.Failed.ReasonCode != execution.FailureDeliveryUnavailable || result.Status != execution.StatusCompleted {
				t.Fatalf("recorded fallback: %+v, %+v", result, view.Failed)
			}
		})
	}
}

type observedDeliveryLocks struct {
	*deliveryBarrier
	requested chan struct{}
}

func (b observedDeliveryLocks) LockWorkspace(ctx context.Context, root string) (func(), error) {
	b.requested <- struct{}{}
	return b.deliveryBarrier.LockWorkspace(ctx, root)
}

func TestPublishDeliverySerializesThroughRecordCommit(t *testing.T) {
	f, barrier, _, project, at := newBlockedDeliveryRuntime(t)
	_, first := seedDeliveryWork(t, f, project, at, false)
	_, second := seedDeliveryWork(t, f, project, at, false)
	locks := make(chan struct{}, 2)
	f.service.delivery = observedDeliveryLocks{barrier, locks}
	committing := make(chan struct{})
	release := make(chan struct{})
	f.service.commitDelivery = func(ctx context.Context, work store.DeliveryWork, record store.DeliveryRecord) (store.DeliveryRecord, error) {
		if work.Ref == first.Ref {
			close(committing)
			select {
			case <-release:
			case <-ctx.Done():
				return store.DeliveryRecord{}, ctx.Err()
			}
		}
		return f.db.CommitTurnDelivery(ctx, work, record)
	}
	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	var publishers sync.WaitGroup
	defer func() { cancel(); publishers.Wait() }()
	publish := func(work store.DeliveryWork) <-chan error {
		done := make(chan error, 1)
		publishers.Go(func() { done <- f.service.publishDelivery(ctx, work, false, 0) })
		return done
	}
	one := publish(first)
	waitDeliveryRequest(t, f, barrier.at)
	select {
	case <-locks:
	default:
		t.Fatal("first delivery did not acquire the workspace lock")
	}
	barrier.allow <- struct{}{}
	select {
	case <-committing:
	case <-ctx.Done():
		t.Fatal("first delivery did not reach its record write")
	}
	two := publish(second)
	select {
	case <-barrier.at:
		t.Fatal("second delivery entered Git before the first record committed")
	case <-locks:
	case <-ctx.Done():
		t.Fatal("workspace lock was not acquired")
	}
	select {
	case <-barrier.at:
		t.Fatal("second delivery entered Git while the first record write was blocked")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-one; err != nil {
		t.Fatal(err)
	}
	waitDeliveryRequest(t, f, barrier.at)
	barrier.allow <- struct{}{}
	if err := <-two; err != nil {
		t.Fatal(err)
	}
}

func TestCancelReadsDeliveryOutsideServiceMutex(t *testing.T) {
	f, barrier, agent, project, at := newBlockedDeliveryRuntime(t)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	reading := make(chan bool, 1)
	f.service.readDeliveryWork = func(read context.Context, ref execution.Ref) (store.DeliveryWork, error) {
		if read == ctx {
			free := f.service.mu.TryLock()
			if free {
				f.service.mu.Unlock()
			}
			reading <- free
		}
		return f.db.DeliveryWork(read, ref)
	}
	_, ref, call := startBlockedChat(t, f, agent, project, at)
	call.unblock()
	waitDeliveryRequest(t, f, barrier.at)
	done := make(chan error, 1)
	go func() {
		_, err := f.service.Cancel(ctx, f.scope, ref)
		done <- err
	}()
	select {
	case free := <-reading:
		if !free {
			t.Error("Cancel read the delivery while holding the service mutex")
		}
	case <-f.ctx.Done():
		t.Fatal("Cancel never read delivery work")
	}
	barrier.allow <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryRegistryReplacementOrphansRef(t *testing.T) {
	f, barrier, agent, project, at := newBlockedDeliveryRuntime(t)
	f.service.commitDelivery = func(ctx context.Context, work store.DeliveryWork, record store.DeliveryRecord) (store.DeliveryRecord, error) {
		if record.Status == store.DeliveryDelivered {
			location := work.Workspace
			location.CloneExecution = "replacement"
			if err := f.db.RecordWorkspace(ctx, work.WorkspaceID, location); err != nil {
				return store.DeliveryRecord{}, err
			}
		}
		return f.db.CommitTurnDelivery(ctx, work, record)
	}
	src, ref, call := startBlockedChat(t, f, agent, project, at)
	call.unblock()
	req := waitDeliveryRequest(t, f, barrier.at)
	barrier.allow <- struct{}{}
	observeTurn(t, f, ref, 0)
	got := readDeliveryView(t, f, src)
	name := "refs/ora/deliveries/" + project + "/" + req.DeliveryID
	if refs := runtimeDeliveryGit(t, req.MirrorPath, "for-each-ref", "--format=%(refname)", name); refs != name {
		t.Fatalf("published ref was not retained: %s", refs)
	}
	if got.Failed == nil || got.Failed.ID == req.DeliveryID || got.Failed.ReasonCode != "workspace_unstable" || got.Good != nil {
		t.Fatalf("replacement did not orphan the old snapshot: %+v, latest=%+v", got, got.Latest)
	}
}

func TestBootDeliveryWithoutRegisteredPath(t *testing.T) {
	f, _, _, project, _ := newBlockedDeliveryRuntime(t)
	src, _ := seedDeliveryWork(t, f, project, store.WorkspaceLocation{}, true)
	if err := f.service.RecoverTurnDeliveries(f.ctx); err != nil {
		t.Fatal(err)
	}
	got := readDeliveryView(t, f, src)
	if got.Failed == nil || got.Failed.ReasonCode != "workspace_not_ready" || got.InProgress {
		t.Fatalf("wrong boot outcome without registered checkout: %+v, latest=%+v", got, got.Latest)
	}
}

func TestBootDeliveryContinuesAfterPersistentItemFailure(t *testing.T) {
	f, _, _, project, _ := newBlockedDeliveryRuntime(t)
	var sources []store.DeliverySource
	items := make(map[string]int)
	for i := range 3 {
		src, work := seedDeliveryWork(t, f, project, store.WorkspaceLocation{}, true)
		items[work.Ref.TurnID] = i
		sources = append(sources, src)
	}
	var attempts [3]int
	f.service.commitDelivery = func(ctx context.Context, work store.DeliveryWork, record store.DeliveryRecord) (store.DeliveryRecord, error) {
		i := items[work.Ref.TurnID]
		attempts[i]++
		if i == 0 || i == 1 && record.ReasonCode != execution.FailureDeliveryUnavailable {
			return store.DeliveryRecord{}, errors.New("persistent item write failure")
		}
		return f.db.CommitTurnDelivery(ctx, work, record)
	}
	if err := f.service.RecoverTurnDeliveries(f.ctx); err != nil {
		t.Fatalf("healthy database failed boot over one item: %v", err)
	}
	first := readDeliveryView(t, f, sources[0])
	second := readDeliveryView(t, f, sources[1])
	third := readDeliveryView(t, f, sources[2])
	if !first.InProgress || first.Latest != nil || second.Failed == nil ||
		second.Failed.ReasonCode != execution.FailureDeliveryUnavailable || third.Failed == nil ||
		third.Failed.ReasonCode != "workspace_not_ready" || attempts != [3]int{6, 4, 1} {
		t.Fatalf("recovery did not isolate item failures: first %+v, second %+v, third %+v, attempts %v", first, second, third, attempts)
	}
}
