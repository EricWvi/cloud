package taskworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

const minimalWorkflowGraph = `{
  "nodes": [
    {"id": "start", "type": "workflow", "data": {"kind": "start", "title": "Start"}},
    {"id": "out", "type": "workflow", "data": {"kind": "output", "title": "Out", "outputMode": "text"}}
  ],
  "edges": [
    {"id": "e1", "source": "start", "target": "out"}
  ]
}`

type fakeRuns struct {
	prepareFn func(ctx context.Context, tenant, user, id string, input *string, values map[string]json.RawMessage) error
	startFn   func(ctx context.Context, tenant, user, id string) error
	cancelFn  func(ctx context.Context, tenant, user, id string) error

	mu           sync.Mutex
	subscribes   int
	unsubscribes int
	cancels      []string
	cancelCh     chan string
	subs         map[string][]chan struct{}
}

func newFakeRuns() *fakeRuns {
	return &fakeRuns{
		cancelCh: make(chan string, 8),
		subs:     map[string][]chan struct{}{},
	}
}

func (r *fakeRuns) Prepare(ctx context.Context, tenant, user, id string, input *string, values map[string]json.RawMessage) error {
	if r.prepareFn != nil {
		return r.prepareFn(ctx, tenant, user, id, input, values)
	}
	return nil
}

func (r *fakeRuns) Start(ctx context.Context, tenant, user, id string) error {
	if r.startFn != nil {
		return r.startFn(ctx, tenant, user, id)
	}
	return nil
}

func (r *fakeRuns) Cancel(ctx context.Context, tenant, user, id string) error {
	r.mu.Lock()
	r.cancels = append(r.cancels, id)
	r.mu.Unlock()
	r.cancelCh <- id
	if r.cancelFn != nil {
		return r.cancelFn(ctx, tenant, user, id)
	}
	return nil
}

func (r *fakeRuns) Subscribe(id string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 4)
	r.mu.Lock()
	r.subscribes++
	r.subs[id] = append(r.subs[id], ch)
	r.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			r.mu.Lock()
			r.unsubscribes++
			r.mu.Unlock()
		})
	}
}

func (r *fakeRuns) notify(id string) {
	r.mu.Lock()
	list := append([]chan struct{}(nil), r.subs[id]...)
	r.mu.Unlock()
	for _, ch := range list {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (r *fakeRuns) counts() (subscribes, unsubscribes int, cancels []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.subscribes, r.unsubscribes, append([]string(nil), r.cancels...)
}

type fixture struct {
	s      *store.Store
	tenant string
	user   string
	wfID   string
	snapID string
	task   store.Task
}

func setupFixture(t *testing.T) fixture {
	t.Helper()
	ctx := t.Context()
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "taskworkflow-runner")
	project, err := s.CreateProject(ctx, execution.Scope{TenantID: tenant, UserID: user}, store.Project{
		ID:            "proj-tw",
		Name:          "TaskWorkflow Test Project",
		RepositoryURL: "https://example.invalid/demo.git",
		Branch:        "main",
		Commit:        "0123456789abcdef0123456789abcdef01234567",
		MirrorPath:    "/tmp/proj-tw",
		SnapshotRef:   "refs/ora/snapshots/proj-tw",
	})
	if err != nil {
		t.Fatal(err)
	}
	wf, _, err := s.CreateWorkflow(ctx, tenant, "处理流", minimalWorkflowGraph)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.PublishWorkflow(ctx, tenant, wf.ID, "v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, tenant, user, store.TaskInput{
		Title:     "需要处理",
		ProjectID: project.ID,
		Assignee:  &store.TaskAssigneeRef{Type: "human", ID: user},
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{
		s:      s,
		tenant: tenant,
		user:   user,
		wfID:   wf.ID,
		snapID: snap.ID,
		task:   task,
	}
}

func waitSettled(t *testing.T, f fixture, runID, wantStatus, wantTaskStatus string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		items, err := f.s.TaskWorkflowRuns(t.Context(), f.tenant, f.task.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			if item.WorkflowRunID == runID && item.LinkStatus == wantStatus {
				gotTask, err := f.s.Task(t.Context(), f.tenant, f.task.ID)
				if err != nil {
					t.Fatal(err)
				}
				if gotTask.Status == wantTaskStatus {
					return
				}
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	items, _ := f.s.TaskWorkflowRuns(t.Context(), f.tenant, f.task.ID)
	gotTask, _ := f.s.Task(t.Context(), f.tenant, f.task.ID)
	t.Fatalf("run %s did not settle to %q (task=%+v items=%+v)", runID, wantStatus, gotTask, items)
}

func TestStartFailureRefuseStateRaceCancelsAndKeepsExistingWatch(t *testing.T) {
	f := setupFixture(t)
	runs := newFakeRuns()
	startErr := errors.New("start failed after dispatch")
	runs.startFn = func(context.Context, string, string, string) error {
		return startErr
	}
	runs.cancelFn = func(context.Context, string, string, string) error {
		return errors.New("execution stop unconfirmed")
	}

	h := New(f.s, runs)
	var raced sync.Once
	var runID string
	var raceErr error
	h.beforeRefuse = func() {
		// Simulate StartWorkflowRun committing between TaskWorkflowOutcome seeing "pending"
		// and RefuseTaskWorkflow attempting the CAS back to cancelled.
		raced.Do(func() {
			_, raceErr = f.s.StartWorkflowRun(context.Background(), f.tenant, runID)
		})
	}

	var err error
	in := store.NewTaskWorkflow{
		TenantID:   f.tenant,
		TaskID:     f.task.ID,
		UserID:     f.user,
		Key:        "race-start-failure",
		WorkflowID: f.wfID,
		SnapshotID: f.snapID,
	}
	// Capture runID via Prepare because beforeRefuse runs before RefuseTaskWorkflow inside Start.
	runs.prepareFn = func(_ context.Context, _, _, id string, _ *string, _ map[string]json.RawMessage) error {
		runID = id
		return nil
	}

	gotID, err := h.Start(t.Context(), in, nil)
	if !errors.Is(err, startErr) {
		t.Fatalf("Start err = %v, want %v", err, startErr)
	}
	if raceErr != nil {
		t.Fatalf("StartWorkflowRun in race hook failed: %v", raceErr)
	}
	if gotID == "" || gotID != runID {
		t.Fatalf("gotID=%q runID=%q", gotID, runID)
	}

	subs, unsubs, cancels := runs.counts()
	if subs != 1 || unsubs != 0 {
		t.Fatalf("subscribes=%d unsubscribes=%d, want 1/0 (must reuse existing watch)", subs, unsubs)
	}
	if len(cancels) != 1 || cancels[0] != runID {
		t.Fatalf("cancels=%v, want [%s]", cancels, runID)
	}

	// Cancel was unconfirmed so the handling link stays running until the real run completes.
	if _, err := f.s.FinishWorkflowRun(t.Context(), f.tenant, runID, store.WorkflowStatusSucceeded, nil, nil); err != nil {
		t.Fatal(err)
	}
	runs.notify(runID)

	waitSettled(t, f, runID, store.WorkflowStatusSucceeded, store.TaskReview)
	h.Close()

	_, unsubs, _ = runs.counts()
	if unsubs != 1 {
		t.Fatalf("unsubscribes=%d after Close, want 1", unsubs)
	}
}

func TestPrepareRefuseStateRaceSubscribesCancelsAndWatches(t *testing.T) {
	f := setupFixture(t)
	runs := newFakeRuns()
	prepareErr := errors.New("prepare failed late")
	var runID string
	var raced sync.Once
	var raceErr error
	runs.prepareFn = func(_ context.Context, _, _, id string, _ *string, _ map[string]json.RawMessage) error {
		runID = id
		return prepareErr
	}

	h := New(f.s, runs)
	h.beforeRefuse = func() {
		raced.Do(func() {
			_, raceErr = f.s.StartWorkflowRun(context.Background(), f.tenant, runID)
		})
	}

	gotID, err := h.Start(t.Context(), store.NewTaskWorkflow{
		TenantID:   f.tenant,
		TaskID:     f.task.ID,
		UserID:     f.user,
		Key:        "race-prepare",
		WorkflowID: f.wfID,
		SnapshotID: f.snapID,
	}, nil)
	if !errors.Is(err, prepareErr) {
		t.Fatalf("Start err = %v, want %v", err, prepareErr)
	}
	if raceErr != nil {
		t.Fatalf("StartWorkflowRun in race hook failed: %v", raceErr)
	}
	if gotID != runID {
		t.Fatalf("gotID=%q runID=%q", gotID, runID)
	}

	subs, _, cancels := runs.counts()
	if subs != 1 || len(cancels) != 1 || cancels[0] != runID {
		t.Fatalf("subs=%d cancels=%v, want 1/[%s]", subs, cancels, runID)
	}

	if _, err := f.s.FinishWorkflowRun(t.Context(), f.tenant, runID, store.WorkflowStatusSucceeded, nil, nil); err != nil {
		t.Fatal(err)
	}
	runs.notify(runID)

	waitSettled(t, f, runID, store.WorkflowStatusSucceeded, store.TaskReview)
	h.Close()

	_, unsubs, _ := runs.counts()
	if unsubs != 1 {
		t.Fatalf("unsubscribes=%d after Close, want 1", unsubs)
	}
}

func TestRefuseRetryTransitionToExecutedSubscribesCancelsAndWatches(t *testing.T) {
	f := setupFixture(t)
	id, fresh, err := f.s.CreateTaskWorkflowRun(t.Context(), store.NewTaskWorkflow{
		TenantID:        f.tenant,
		TaskID:          f.task.ID,
		UserID:          f.user,
		Key:             "retry-executed",
		WorkflowID:      f.wfID,
		SnapshotID:      f.snapID,
		ExpectedVersion: f.task.Version,
	})
	if err != nil || !fresh {
		t.Fatalf("CreateTaskWorkflowRun = %q %v %v", id, fresh, err)
	}

	runs := newFakeRuns()
	runs.cancelFn = func(context.Context, string, string, string) error {
		return errors.New("execution stop unconfirmed")
	}
	h := New(f.s, runs)
	h.retryInterval = 10 * time.Millisecond

	var mu sync.Mutex
	attempts := 0
	var startErr error
	h.beforeRefuse = func() {
		mu.Lock()
		attempts++
		attempt := attempts
		mu.Unlock()
		// Attempt 1 uses a canceled context to trigger the background retry path.
		// Attempt 2 executes inside the retry goroutine after the run has already started.
		if attempt == 2 {
			_, startErr = f.s.StartWorkflowRun(context.Background(), f.tenant, id)
		}
	}

	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()
	cause := errors.New("initial dispatch failed")
	executed, gotErr := h.refuse(canceledCtx, f.tenant, f.user, id, cause, false)
	if executed || !errors.Is(gotErr, cause) || !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("refuse(canceled) = (%v, %v)", executed, gotErr)
	}

	select {
	case canceledID := <-runs.cancelCh:
		if canceledID != id {
			t.Fatalf("canceledID=%q want %q", canceledID, id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for retry goroutine to call Cancel")
	}
	if startErr != nil {
		t.Fatalf("StartWorkflowRun in retry hook failed: %v", startErr)
	}

	subs, _, _ := runs.counts()
	if subs != 1 {
		t.Fatalf("subscribes=%d, want 1 from background retry watcher", subs)
	}

	if _, err := f.s.FinishWorkflowRun(t.Context(), f.tenant, id, store.WorkflowStatusSucceeded, nil, nil); err != nil {
		t.Fatal(err)
	}
	runs.notify(id)

	waitSettled(t, f, id, store.WorkflowStatusSucceeded, store.TaskReview)
	h.Close()

	_, unsubs, _ := runs.counts()
	if unsubs != 1 {
		t.Fatalf("unsubscribes=%d after Close, want 1", unsubs)
	}
}
