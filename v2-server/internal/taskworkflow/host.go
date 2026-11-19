// Package taskworkflow owns task handling admission and fenced status flow-back. Execution stays
// with the workflow host; decomposition and inspection associations never move task status.
package taskworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// Runs is the existing workflow execution boundary: input preparation, dispatch, host-side
// cancellation for a start whose run already began executing, and its change subscription.
type Runs interface {
	Prepare(context.Context, string, string, string, *string, map[string]json.RawMessage) error
	Start(context.Context, string, string, string) error
	Cancel(context.Context, string, string, string) error
	Subscribe(string) (<-chan struct{}, func())
}

// Host owns watchers until Close and uses the database to serialize cross-kind admission.
type Host struct {
	db            *store.Store
	runs          Runs
	ctx           context.Context
	stop          context.CancelFunc
	workers       sync.WaitGroup
	mu            sync.Mutex
	locks         map[string]*taskLock
	beforeRefuse  func()
	retryInterval time.Duration
}

// taskLock counts both its holder and queued callers so idle entries can be reclaimed safely.
type taskLock struct {
	mutex sync.Mutex
	users int
}

// New constructs a host. Recover must run before requests; Close precedes workflow host shutdown.
func New(db *store.Store, runs Runs) *Host {
	ctx, stop := context.WithCancel(context.Background())
	return &Host{db: db, runs: runs, ctx: ctx, stop: stop, locks: map[string]*taskLock{}}
}

// Close stops and joins every watcher, leaving unfinished links for recovery.
func (h *Host) Close() { h.stop(); h.workers.Wait() }

// Start freezes the run before executing it as the assignee. Retries of an admitted key return
// the existing run without dispatching again, including when the original request was cancelled.
func (h *Host) Start(ctx context.Context, in store.NewTaskWorkflow, values map[string]json.RawMessage) (string, error) {
	defer h.lock(in.TaskID)()
	task, err := h.db.Task(ctx, in.TenantID, in.TaskID)
	if err != nil {
		return "", err
	}
	if err := store.AuthorizeTaskRunner(ctx, task, in.UserID, h.db.InSquad); err != nil {
		return "", err
	}
	if id, err := h.db.TaskWorkflowRunByKey(ctx, in); err == nil {
		return id, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	var snapshot store.WorkflowSnapshot
	if in.SnapshotID == "" {
		snapshot, err = h.db.PublishedWorkflowSnapshot(ctx, in.TenantID, in.WorkflowID)
	} else {
		snapshot, err = h.db.WorkflowSnapshot(ctx, in.TenantID, in.WorkflowID, in.SnapshotID)
	}
	if err != nil {
		return "", err
	}
	if snapshot.Version == store.WorkflowDraftVersion {
		return "", store.ErrWorkflowDraftSnapshot
	}
	if err := workflow.CheckStart(snapshot.Graph, in.Input, values); err != nil {
		return "", preflightError(err)
	}
	in.SnapshotID = snapshot.ID
	in.ExpectedVersion = task.Version
	id, fresh, err := h.db.CreateTaskWorkflowRun(ctx, in)
	if err != nil || !fresh {
		return id, err
	}
	owned := context.WithoutCancel(ctx)
	if err := h.runs.Prepare(owned, in.TenantID, in.UserID, id, in.Input, values); err != nil {
		_, err = h.refuse(owned, in.TenantID, in.UserID, id, err, true)
		return id, err
	}
	if err := h.db.CheckTaskWorkflowStart(owned, in.TenantID, id); err != nil {
		_, err = h.refuse(owned, in.TenantID, in.UserID, id, err, true)
		return id, err
	}
	events, cancel := h.runs.Subscribe(id)
	if err = h.runs.Start(owned, in.TenantID, in.UserID, id); err != nil {
		if h.startFailure(owned, in.TenantID, in.UserID, id, err) {
			// The run already began executing; keep observing it until its real outcome settles.
			h.watch(in.TenantID, id, events, cancel)
		} else {
			cancel()
		}
		return id, err
	}
	h.watch(in.TenantID, id, events, cancel)
	return id, nil
}

// CommentWorkflowInput is a task comment that validates Start inputs and runs a workflow as the commenter.
type CommentWorkflowInput struct {
	TenantID   string
	TaskID     string
	AuthorID   string
	CreateKey  string
	Body       string
	Mentions   []store.TaskMention
	WorkflowID string
	SnapshotID string
	Input      *string
	Values     map[string]json.RawMessage
}

// CommentWorkflow validates Start inputs against the published workflow snapshot, creates the run
// and comment atomically as the commenter (without altering task status or handling locks), and
// dispatches execution. Retries with the same key return the existing comment without re-running.
func (h *Host) CommentWorkflow(ctx context.Context, in CommentWorkflowInput) (store.TaskComment, error) {
	defer h.lock(in.TaskID)()
	if existing, err := h.db.TaskCommentByKey(ctx, in.TenantID, in.TaskID, in.AuthorID, in.CreateKey); err == nil {
		return existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.TaskComment{}, err
	}
	task, err := h.db.Task(ctx, in.TenantID, in.TaskID)
	if err != nil {
		return store.TaskComment{}, err
	}
	if task.Status == store.TaskDone || task.Status == store.TaskCancelled {
		return store.TaskComment{}, store.ErrInvalidTransition
	}
	mentions, err := h.db.ResolveTaskMentions(ctx, in.TenantID, in.Mentions)
	if err != nil {
		return store.TaskComment{}, err
	}
	var snapshot store.WorkflowSnapshot
	if in.SnapshotID == "" {
		snapshot, err = h.db.PublishedWorkflowSnapshot(ctx, in.TenantID, in.WorkflowID)
	} else {
		snapshot, err = h.db.WorkflowSnapshot(ctx, in.TenantID, in.WorkflowID, in.SnapshotID)
	}
	if err != nil {
		return store.TaskComment{}, err
	}
	if snapshot.Version == store.WorkflowDraftVersion {
		return store.TaskComment{}, store.ErrWorkflowDraftSnapshot
	}
	startInput := in.Input
	if startInput == nil {
		if _, hasExplicitInput := in.Values["input"]; !hasExplicitInput {
			body := in.Body
			startInput = &body
		}
	}
	decodedValues, err := workflow.DecodeFormStartValues(snapshot.Graph, in.Values)
	if err != nil {
		return store.TaskComment{}, preflightError(err)
	}
	if err := workflow.CheckStart(snapshot.Graph, startInput, decodedValues); err != nil {
		return store.TaskComment{}, preflightError(err)
	}
	comment, fresh, err := h.db.CreateTaskWorkflowComment(ctx, store.NewTaskWorkflowComment{
		TenantID:   in.TenantID,
		TaskID:     in.TaskID,
		AuthorID:   in.AuthorID,
		CreateKey:  in.CreateKey,
		Body:       in.Body,
		Mentions:   mentions,
		WorkflowID: in.WorkflowID,
		SnapshotID: snapshot.ID,
		Input:      startInput,
	})
	if err != nil || !fresh {
		return comment, err
	}
	owned := context.WithoutCancel(ctx)
	runID := comment.WorkflowRun.ID
	if err := h.runs.Prepare(owned, in.TenantID, in.AuthorID, runID, startInput, decodedValues); err != nil {
		return store.TaskComment{}, errors.Join(err, h.rejectCommentWorkflow(owned, in, runID, "工作流输入准备失败，请重新发起。"))
	}
	if err := h.runs.Start(owned, in.TenantID, in.AuthorID, runID); err != nil {
		return store.TaskComment{}, errors.Join(err, h.rejectCommentWorkflow(owned, in, runID, "工作流启动失败，请重新发起。"))
	}
	if latest, err := h.db.TaskCommentByKey(owned, in.TenantID, in.TaskID, in.AuthorID, in.CreateKey); err == nil {
		return latest, nil
	}
	return comment, nil
}

func (h *Host) rejectCommentWorkflow(ctx context.Context, in CommentWorkflowInput, runID, message string) error {
	if discarded, err := h.db.DiscardUnstartedCommentWorkflow(ctx, in.TenantID, in.TaskID, in.AuthorID, in.CreateKey, runID); err != nil || discarded {
		return err
	}
	// Execution may already have begun before Start reported an error. Keep its history and
	// never mark it stopped unless the execution host confirms cancellation.
	if err := h.runs.Cancel(ctx, in.TenantID, in.AuthorID, runID); err != nil && !errors.Is(err, store.ErrWorkflowRunState) && !errors.Is(err, store.ErrWorkflowRunNotFound) {
		return err
	}
	if _, err := h.db.CancelWorkflowRun(ctx, in.TenantID, runID); err != nil && !errors.Is(err, store.ErrWorkflowRunState) {
		return err
	}
	return h.db.RecordCommentWorkflowStartFailure(ctx, in.TenantID, runID, message)
}

// watch settles the run's outcome, woken by run events and a periodic tick, until it commits.
func (h *Host) watch(tenant, id string, events <-chan struct{}, cancel func()) {
	h.workers.Add(1)
	go func() {
		defer h.workers.Done()
		defer cancel()
		tick := time.NewTicker(3 * time.Second)
		defer tick.Stop()
		for {
			if status, _, err := h.settle(h.ctx, tenant, id); err != nil && h.ctx.Err() == nil {
				slog.Warn("settle task workflow", "run", id, "error", err)
			} else if status != "" {
				return
			}
			select {
			case <-h.ctx.Done():
				return
			case <-events:
			case <-tick.C:
			}
		}
	}()
}

// stoppedBeforeStart is the reason recorded when a handling run is cancelled before it ever
// executes: the start did not happen, so the task returns to the status it had before (D10).
var stoppedBeforeStart = errors.New("Workflow run was stopped before it started.")

func (h *Host) settle(ctx context.Context, tenant, id string) (string, string, error) {
	status, failure, _, err := h.settleOutcome(ctx, tenant, id)
	return status, failure, err
}

func (h *Host) settleOutcome(ctx context.Context, tenant, id string) (string, string, bool, error) {
	outcome, err := h.db.TaskWorkflowOutcome(ctx, tenant, id)
	if err != nil {
		return "", "", false, err
	}
	const deletedFailure = "Workflow run was deleted."
	if outcome.Status == store.WorkflowStatusPending {
		if !outcome.Deleted {
			return "", "", false, nil
		}
		// Deleting an admission that never executed is a refused start, not a failed execution.
		return store.WorkflowStatusFailed, deletedFailure, true, h.db.RefuseTaskWorkflow(ctx, tenant, id, deletedFailure)
	}
	if outcome.Status == store.WorkflowStatusCancelled && !outcome.Executed {
		// A run stopped before it ever executed is a start that did not happen, whichever of
		// Start's own refusal or a history read settles first: the link records the stop and the
		// task returns to its original status instead of being pulled to pending (D10 over D5).
		return store.WorkflowStatusFailed, stoppedBeforeStart.Error(), outcome.Deleted, h.db.RefuseTaskWorkflow(ctx, tenant, id, stoppedBeforeStart.Error())
	}
	switch outcome.Status {
	case store.WorkflowStatusSucceeded, store.WorkflowStatusFailed, store.WorkflowStatusCancelled:
	default:
		return "", "", outcome.Deleted, nil
	}
	failure := outcome.Failure
	if outcome.Deleted && failure == "" {
		failure = deletedFailure
	}
	return outcome.Status, failure, outcome.Deleted, h.db.SettleTaskWorkflow(ctx, tenant, id, outcome.Status, failure)
}

// startFailure settles a dispatch that returned an error after admission and reports whether the
// run had already begun executing. A run that never executed is a refused start: it is cancelled
// and the original task status restored atomically, with the stop recorded as the reason when the
// run was cancelled before start. A run that executed is stopped through the workflow host — the
// dispatched Agent turn included — and settlement then records its real outcome.
func (h *Host) startFailure(ctx context.Context, tenant, user, id string, cause error) bool {
	outcome, err := h.db.TaskWorkflowOutcome(ctx, tenant, id)
	if err == nil && outcome.Status != store.WorkflowStatusPending {
		if outcome.Status != store.WorkflowStatusCancelled || outcome.Executed {
			h.cancelExecutedStart(ctx, tenant, user, id)
			return true
		}
		executed, _ := h.refuse(ctx, tenant, user, id, stoppedBeforeStart, false)
		return executed
	}
	executed, _ := h.refuse(ctx, tenant, user, id, cause, false)
	return executed
}

// cancelExecutedStart stops a run that already began executing through the workflow host and
// settles its real outcome. An unconfirmed host stop is not fatal: the run's own worker closes it
// once the turn ends and the watcher then settles; a run that already finished settles as-is.
func (h *Host) cancelExecutedStart(ctx context.Context, tenant, user, id string) {
	if err := h.runs.Cancel(ctx, tenant, user, id); err != nil && !errors.Is(err, store.ErrWorkflowRunState) && !errors.Is(err, store.ErrWorkflowRunNotFound) {
		slog.Warn("cancel failed task workflow start", "run", id, "error", err)
	}
	if _, _, err := h.settle(ctx, tenant, id); err != nil {
		slog.Warn("settle task workflow", "run", id, "error", err)
	}
}

// List settles completed runs before returning the task's three kinds of workflow history.
func (h *Host) List(ctx context.Context, tenant, taskID string) ([]store.TaskWorkflowRun, error) {
	items, err := h.db.TaskWorkflowRuns(ctx, tenant, taskID)
	if err != nil {
		return nil, err
	}
	for i, item := range items {
		if item.Kind == "handle" && item.LinkStatus == "running" {
			status, failure, deleted, err := h.settleOutcome(ctx, tenant, item.WorkflowRunID)
			if err != nil {
				return nil, err
			}
			if status != "" {
				items[i].Status = status
				if item.Deleted || deleted {
					items[i].Deleted = true
					items[i].Failure = ""
				} else {
					items[i].Failure = failure
				}
				items[i].LinkStatus = status
			}
		}
	}
	return items, nil
}

// Recover runs before workflow interruption. Completed runs keep their outcome; all still-live
// handling runs are cancelled and marked interrupted, including parked forms and pending starts.
// Comment-initiated runs left pending before execution ever began are discarded so same-key retries
// can start cleanly. Previous-process sessions have already been ended by execution recovery at this point.
func (h *Host) Recover(ctx context.Context) error {
	items, err := h.db.LiveTaskWorkflows(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		if status, _, err := h.settle(ctx, item.Tenant, item.RunID); err != nil {
			return err
		} else if status != "" {
			continue
		}
		if _, err := h.db.CancelWorkflowRun(ctx, item.Tenant, item.RunID); err != nil && !errors.Is(err, store.ErrWorkflowRunState) {
			return err
		}
		if err := h.db.SettleTaskWorkflow(ctx, item.Tenant, item.RunID, "interrupted", "Server restarted; start a new run."); err != nil {
			return err
		}
	}
	pendingComments, err := h.db.PendingCommentWorkflows(ctx)
	if err != nil {
		return err
	}
	for _, comment := range pendingComments {
		if _, err := h.db.DiscardUnstartedCommentWorkflow(ctx, comment.TenantID, comment.TaskID, comment.AuthorID, comment.CreateKey, comment.RunID); err != nil {
			return err
		}
	}
	return nil
}

// StartInputError is a safe preflight refusal for one caller-supplied Start value.
type StartInputError struct{ Variable, Reason string }

func (e *StartInputError) Error() string {
	return "invalid workflow start input: " + e.Variable + ": " + e.Reason
}

// ErrStartGraph reports a graph that the workflow engine would refuse before dispatch.
var ErrStartGraph = errors.New("workflow graph is not executable")

func preflightError(err error) error {
	var input *workflow.InputRejection
	if errors.As(err, &input) {
		return &StartInputError{Variable: input.Variable, Reason: input.Reason}
	}
	var start *workflow.StartError
	if errors.As(err, &start) && start.Code == "workflow_run_input_invalid" {
		return &StartInputError{Variable: start.Variable, Reason: start.Detail}
	}
	return fmt.Errorf("%w: %w", ErrStartGraph, err)
}

func (h *Host) lock(id string) func() {
	h.mu.Lock()
	l := h.locks[id]
	if l == nil {
		l = &taskLock{}
		h.locks[id] = l
	}
	l.users++
	h.mu.Unlock()
	l.mutex.Lock()
	return func() {
		l.mutex.Unlock()
		h.mu.Lock()
		l.users--
		if l.users == 0 {
			delete(h.locks, id)
		}
		h.mu.Unlock()
	}
}

// refuse keeps admission reserved until cancellation and link restoration commit together, and
// retries transient failures, so a transient error cannot abandon an admission. If the run turns
// out to have already begun executing (ErrWorkflowRunState), it hands off to cancelExecutedStart
// and keeps (or opens) a watcher so the real outcome settles.
func (h *Host) refuse(ctx context.Context, tenant, user, id string, cause error, watchOnExecute bool) (bool, error) {
	clean := func(ctx context.Context, subscribeOnExecute bool) (bool, error) {
		if h.beforeRefuse != nil {
			h.beforeRefuse()
		}
		err := h.db.RefuseTaskWorkflow(ctx, tenant, id, cause.Error())
		if errors.Is(err, store.ErrWorkflowRunState) {
			if subscribeOnExecute {
				events, cancel := h.runs.Subscribe(id)
				h.cancelExecutedStart(ctx, tenant, user, id)
				h.watch(tenant, id, events, cancel)
			} else {
				h.cancelExecutedStart(ctx, tenant, user, id)
			}
			return true, nil
		}
		return false, err
	}
	executed, err := clean(ctx, watchOnExecute)
	if err != nil {
		interval := h.retryInterval
		if interval <= 0 {
			interval = time.Second
		}
		h.workers.Add(1)
		go func() {
			defer h.workers.Done()
			tick := time.NewTicker(interval)
			defer tick.Stop()
			for {
				select {
				case <-h.ctx.Done():
					return
				case <-tick.C:
					if _, err := clean(h.ctx, true); err == nil {
						return
					}
				}
			}
		}()
		return false, errors.Join(cause, err)
	}
	return executed, cause
}
