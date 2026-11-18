package taskworkflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
	"github.com/wanglongan587/cloud/v2-server/internal/taskworkflow"
)

const simpleWorkflowGraph = `{"nodes":[{"id":"start","data":{"kind":"start"}}],"edges":[]}`

type commentCancelCall struct {
	tenant string
	user   string
	runID  string
}

type commentRunsStub struct {
	prepareFn   func(context.Context, string, string, string, *string, map[string]json.RawMessage) error
	startFn     func(context.Context, string, string, string) error
	cancelFn    func(context.Context, string, string, string) error
	cancelCalls []commentCancelCall
}

func (p *commentRunsStub) Prepare(ctx context.Context, tenant, user, id string, input *string, values map[string]json.RawMessage) error {
	if p.prepareFn != nil {
		return p.prepareFn(ctx, tenant, user, id, input, values)
	}
	return nil
}

func (p *commentRunsStub) Start(ctx context.Context, tenant, user, id string) error {
	if p.startFn != nil {
		return p.startFn(ctx, tenant, user, id)
	}
	return nil
}

func (p *commentRunsStub) Cancel(ctx context.Context, tenant, user, id string) error {
	p.cancelCalls = append(p.cancelCalls, commentCancelCall{tenant: tenant, user: user, runID: id})
	if p.cancelFn != nil {
		return p.cancelFn(ctx, tenant, user, id)
	}
	return nil
}

func (p *commentRunsStub) Subscribe(string) (<-chan struct{}, func()) {
	return make(chan struct{}), func() {}
}

func openCommentWorkflowFixture(t *testing.T) (*store.Store, string, string, store.Task, store.Workflow) {
	t.Helper()
	ctx := context.Background()
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "comment-owner")
	if _, err := s.CreateProject(ctx, execution.Scope{TenantID: tenant, UserID: user}, store.Project{
		ID: "proj-comment", Name: "Demo", RepositoryURL: "fixture://demo", Commit: "abc", MirrorPath: "/tmp/comment-wf",
	}); err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, tenant, user, store.TaskInput{Title: "Comment WF", ProjectID: "proj-comment"})
	if err != nil {
		t.Fatal(err)
	}
	wf, _, err := s.CreateWorkflow(ctx, tenant, "PublishCheck", simpleWorkflowGraph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishWorkflow(ctx, tenant, wf.ID, "v1", nil); err != nil {
		t.Fatal(err)
	}
	return s, tenant, user, task, wf
}

func TestCommentWorkflowCancelContract(t *testing.T) {
	ctx := context.Background()

	t.Run("calls host Cancel and marks cancelled when stop confirms", func(t *testing.T) {
		s, tenant, user, task, wf := openCommentWorkflowFixture(t)
		startErr := errors.New("start failed after dispatch")
		port := &commentRunsStub{
			startFn: func(ctx context.Context, tenant, _, runID string) error {
				if _, err := s.StartWorkflowRun(ctx, tenant, runID); err != nil {
					return err
				}
				return startErr
			},
		}
		h := taskworkflow.New(s, port)
		defer h.Close()

		in := taskworkflow.CommentWorkflowInput{
			TenantID: tenant, TaskID: task.ID, AuthorID: user, CreateKey: "confirm-stop", Body: "@PublishCheck", WorkflowID: wf.ID,
		}
		if _, err := h.CommentWorkflow(ctx, in); !errors.Is(err, startErr) {
			t.Fatalf("CommentWorkflow err = %v, want %v", err, startErr)
		}
		replay, err := h.CommentWorkflow(ctx, in)
		if err != nil || replay.WorkflowRun == nil {
			t.Fatalf("replay = %+v %v", replay, err)
		}
		if len(port.cancelCalls) != 1 || port.cancelCalls[0] != (commentCancelCall{tenant: tenant, user: user, runID: replay.WorkflowRun.ID}) {
			t.Fatalf("host Cancel calls = %+v, want single call for run %s", port.cancelCalls, replay.WorkflowRun.ID)
		}
		if replay.WorkflowRun.Status != store.WorkflowStatusCancelled || replay.WorkflowRun.Error == nil || *replay.WorkflowRun.Error != "工作流启动失败，请重新发起。" {
			t.Fatalf("cancelled replay = %+v", replay.WorkflowRun)
		}
	})

	t.Run("does not mark cancelled or record failure when host Cancel fails", func(t *testing.T) {
		s, tenant, user, task, wf := openCommentWorkflowFixture(t)
		startErr := errors.New("start failed after dispatch")
		cancelErr := errors.New("stop unconfirmed")
		port := &commentRunsStub{
			startFn: func(ctx context.Context, tenant, _, runID string) error {
				if _, err := s.StartWorkflowRun(ctx, tenant, runID); err != nil {
					return err
				}
				return startErr
			},
			cancelFn: func(context.Context, string, string, string) error {
				return cancelErr
			},
		}
		h := taskworkflow.New(s, port)
		defer h.Close()

		in := taskworkflow.CommentWorkflowInput{
			TenantID: tenant, TaskID: task.ID, AuthorID: user, CreateKey: "unconfirmed-stop", Body: "@PublishCheck", WorkflowID: wf.ID,
		}
		err := func() error { _, e := h.CommentWorkflow(ctx, in); return e }()
		if !errors.Is(err, startErr) || !errors.Is(err, cancelErr) {
			t.Fatalf("CommentWorkflow err = %v, want joined startErr + cancelErr", err)
		}
		replay, err := h.CommentWorkflow(ctx, in)
		if err != nil || replay.WorkflowRun == nil {
			t.Fatalf("replay = %+v %v", replay, err)
		}
		if replay.WorkflowRun.Status != store.WorkflowStatusRunning || replay.WorkflowRun.Error != nil {
			t.Fatalf("unconfirmed stop illegally altered run: %+v", replay.WorkflowRun)
		}
	})

	t.Run("preserves finished outcome when host Cancel returns ErrWorkflowRunState", func(t *testing.T) {
		s, tenant, user, task, wf := openCommentWorkflowFixture(t)
		startErr := errors.New("start failed after completion")
		engineErr := "authoritative failure"
		port := &commentRunsStub{
			startFn: func(ctx context.Context, tenant, _, runID string) error {
				if _, err := s.StartWorkflowRun(ctx, tenant, runID); err != nil {
					return err
				}
				if _, err := s.FinishWorkflowRun(ctx, tenant, runID, store.WorkflowStatusFailed, nil, &engineErr); err != nil {
					return err
				}
				return startErr
			},
			cancelFn: func(context.Context, string, string, string) error {
				return store.ErrWorkflowRunState
			},
		}
		h := taskworkflow.New(s, port)
		defer h.Close()

		in := taskworkflow.CommentWorkflowInput{
			TenantID: tenant, TaskID: task.ID, AuthorID: user, CreateKey: "already-finished", Body: "@PublishCheck", WorkflowID: wf.ID,
		}
		if _, err := h.CommentWorkflow(ctx, in); !errors.Is(err, startErr) {
			t.Fatalf("CommentWorkflow err = %v, want %v", err, startErr)
		}
		replay, err := h.CommentWorkflow(ctx, in)
		if err != nil || replay.WorkflowRun == nil {
			t.Fatalf("replay = %+v %v", replay, err)
		}
		if replay.WorkflowRun.Status != store.WorkflowStatusFailed || replay.WorkflowRun.Error == nil || *replay.WorkflowRun.Error != engineErr {
			t.Fatalf("finished outcome overwritten: %+v", replay.WorkflowRun)
		}
	})
}

func TestTaskWorkflowRecoverDiscardsPendingCommentWorkflow(t *testing.T) {
	ctx := context.Background()
	s, tenant, user, task, wf := openCommentWorkflowFixture(t)

	// 1. Crash window: comment & pending run committed, never reached Start.
	crashed, fresh, err := s.CreateTaskWorkflowComment(ctx, store.NewTaskWorkflowComment{
		TenantID: tenant, TaskID: task.ID, AuthorID: user, CreateKey: "crash-pending", Body: "@PublishCheck", WorkflowID: wf.ID,
	})
	if err != nil || !fresh || crashed.WorkflowRun == nil {
		t.Fatalf("seed crashed comment: fresh=%t err=%v", fresh, err)
	}

	// 2. Started comment run must NOT be discarded by TaskWorkflows.Recover.
	started, fresh, err := s.CreateTaskWorkflowComment(ctx, store.NewTaskWorkflowComment{
		TenantID: tenant, TaskID: task.ID, AuthorID: user, CreateKey: "already-started", Body: "@PublishCheck", WorkflowID: wf.ID,
	})
	if err != nil || !fresh || started.WorkflowRun == nil {
		t.Fatalf("seed started comment: fresh=%t err=%v", fresh, err)
	}
	if _, err := s.StartWorkflowRun(ctx, tenant, started.WorkflowRun.ID); err != nil {
		t.Fatal(err)
	}

	// 3. Soft-deleted pending comment run must remain deleted, not hard-removed.
	deleted, fresh, err := s.CreateTaskWorkflowComment(ctx, store.NewTaskWorkflowComment{
		TenantID: tenant, TaskID: task.ID, AuthorID: user, CreateKey: "deleted-pending", Body: "@PublishCheck", WorkflowID: wf.ID,
	})
	if err != nil || !fresh || deleted.WorkflowRun == nil {
		t.Fatalf("seed deleted comment: fresh=%t err=%v", fresh, err)
	}
	if err := s.DeleteWorkflowRun(ctx, tenant, deleted.WorkflowRun.ID); err != nil {
		t.Fatal(err)
	}

	port := &commentRunsStub{
		startFn: func(ctx context.Context, tenant, _, runID string) error {
			_, err := s.StartWorkflowRun(ctx, tenant, runID)
			return err
		},
	}
	h := taskworkflow.New(s, port)
	defer h.Close()

	if err := h.Recover(ctx); err != nil {
		t.Fatalf("Recover = %v", err)
	}

	if _, err := s.TaskCommentByKey(ctx, tenant, task.ID, user, "crash-pending"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("crashed pending comment survived Recover: %v", err)
	}
	if _, err := s.WorkflowRun(ctx, tenant, crashed.WorkflowRun.ID); !errors.Is(err, store.ErrWorkflowRunNotFound) {
		t.Fatalf("crashed pending run survived Recover: %v", err)
	}
	if got, err := s.TaskCommentByKey(ctx, tenant, task.ID, user, "already-started"); err != nil || got.WorkflowRun == nil || got.WorkflowRun.Status != store.WorkflowStatusRunning {
		t.Fatalf("started comment modified by Recover: %+v %v", got, err)
	}
	if got, err := s.TaskCommentByKey(ctx, tenant, task.ID, user, "deleted-pending"); err != nil || got.WorkflowRun == nil || !got.WorkflowRun.Deleted {
		t.Fatalf("deleted pending comment removed by Recover: %+v %v", got, err)
	}

	// Retrying the crashed request with the exact same CreateKey must now start cleanly.
	retried, err := h.CommentWorkflow(ctx, taskworkflow.CommentWorkflowInput{
		TenantID: tenant, TaskID: task.ID, AuthorID: user, CreateKey: "crash-pending", Body: "@PublishCheck", WorkflowID: wf.ID,
	})
	if err != nil || retried.WorkflowRun == nil || retried.WorkflowRun.ID == crashed.WorkflowRun.ID || retried.WorkflowRun.Status != store.WorkflowStatusRunning {
		t.Fatalf("same-key retry after Recover = %+v %v", retried, err)
	}
}
