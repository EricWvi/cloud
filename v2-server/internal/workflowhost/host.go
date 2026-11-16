package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// Config wires a Host. Store, Execution and Catalog are required.
type Config struct {
	Store     *store.Store
	Execution execution.Service
	Catalog   execution.Catalog
	// Hub receives run invalidations; nil creates a private one (see Host.Hub).
	Hub    *Hub
	Logger *slog.Logger
	// Now reads the clock failure details and system variables use; nil uses the process clock.
	Now func() time.Time
	// StopTimeout bounds waiting for execution to confirm a cancellation; zero means 30s.
	StopTimeout time.Duration
	// BusyRetry bounds how long an interactive node's follow-up turn waits for its session's
	// previous turn to finish cleaning up; zero means 10s.
	BusyRetry time.Duration
}

// Host executes workflow runs in this process and implements server.WorkflowRunHost. Every run
// is driven by the stateless engine; the host only keeps what cannot outlive the process: who
// started each run (execution sessions are scoped to that user) and its turns in flight.
type Host struct {
	store       *store.Store
	exec        execution.Service
	catalog     execution.Catalog
	hub         *Hub
	log         *slog.Logger
	now         func() time.Time
	stopTimeout time.Duration
	busyRetry   time.Duration
	engine      *workflow.Engine

	ctx     context.Context
	stop    context.CancelFunc
	workers sync.WaitGroup

	mu   sync.Mutex
	runs map[string]*runState
	// lifecycles serializes a run's cancellation and resume: a cancellation that starts while a
	// resume is under way waits for it and then stops the resumed Agent, and a resume is refused
	// while a cancellation is under way. An entry exists only while someone holds or waits for
	// it, and only for a run the caller's tenant was found to own, so the map stays bounded by
	// the lifecycle operations in flight.
	lifecycles map[string]*lifecycleLock
	// resuming, when set (tests only), runs inside ResumeRun once it holds the run's lifecycle
	// lock and has admitted the resume, before the resume transaction.
	resuming func(runID string)
	// dispatching, when set (tests only), runs first in every Agent attempt's goroutine.
	dispatching func(workflow.AgentDispatch)
	// followingUp, when set (tests only), runs first in every accepted follow-up's goroutine.
	followingUp func(nodeID string)
}

// lifecycleLock is one run's cancel/resume lock and the number of callers holding or waiting.
type lifecycleLock struct {
	mutex sync.Mutex
	users int
}

// lockLifecycle waits for the run's cancel/resume lock and returns its release.
func (h *Host) lockLifecycle(runID string) func() {
	l := h.joinLifecycle(runID)
	l.mutex.Lock()
	return func() {
		l.mutex.Unlock()
		h.leaveLifecycle(runID, l)
	}
}

// tryLockLifecycle takes the run's cancel/resume lock only when nobody holds it.
func (h *Host) tryLockLifecycle(runID string) (func(), bool) {
	l := h.joinLifecycle(runID)
	if !l.mutex.TryLock() {
		h.leaveLifecycle(runID, l)
		return nil, false
	}
	return func() {
		l.mutex.Unlock()
		h.leaveLifecycle(runID, l)
	}, true
}

func (h *Host) joinLifecycle(runID string) *lifecycleLock {
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.lifecycles[runID]
	if l == nil {
		l = &lifecycleLock{}
		h.lifecycles[runID] = l
	}
	l.users++
	return l
}

func (h *Host) leaveLifecycle(runID string, l *lifecycleLock) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l.users--
	if l.users == 0 {
		delete(h.lifecycles, runID)
	}
}

var _ server.WorkflowRunHost = (*Host)(nil)

// NewHost returns a ready host. Call Interrupt before serving requests and Close on shutdown.
func NewHost(c Config) (*Host, error) {
	if c.Store == nil || c.Execution == nil || c.Catalog == nil {
		return nil, errors.New("workflow host requires a store, execution and catalog")
	}
	h := &Host{
		store: c.Store, exec: c.Execution, catalog: c.Catalog, hub: c.Hub, log: c.Logger, now: c.Now,
		stopTimeout: c.StopTimeout, busyRetry: c.BusyRetry, runs: map[string]*runState{}, lifecycles: map[string]*lifecycleLock{},
	}
	if h.hub == nil {
		h.hub = NewHub()
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	if h.now == nil {
		h.now = time.Now
	}
	if h.stopTimeout == 0 {
		h.stopTimeout = 30 * time.Second
	}
	if h.busyRetry == 0 {
		h.busyRetry = 10 * time.Second
	}
	h.ctx, h.stop = context.WithCancel(context.Background())
	h.engine = workflow.NewEngine(repository{c.Store}, h, h.hub.Publish, h.now)
	return h, nil
}

// Hub is where run invalidations are published, for the run events route.
func (h *Host) Hub() *Hub { return h.hub }

// Close stops observing executing turns and waits for the run workers. It does not cancel the
// turns themselves: the execution service ends them on its own shutdown, and the next start
// interrupts the runs.
func (h *Host) Close() {
	h.stop()
	h.engine.Close()
	h.workers.Wait()
}

const interruptedMessage = "the server restarted while this run was executing"

// workflowInterrupts keeps pending LoopExit cleanup and running Iterations alive. Every other
// active row must be a running Iteration member; outer work and retry waits still fail the run.
func workflowInterrupts(graph string, nodes []store.WorkflowNodeRun, rounds []store.WorkflowScope) map[string]bool {
	g, err := workflow.ParseGraph(graph)
	if err != nil {
		return nil
	}
	exiting := map[string]bool{}
	for _, round := range rounds {
		if round.ParentLoopNodeRunID != nil && round.Status != nil && *round.Status == store.WorkflowStatusRunning && workflow.HasPendingLoopExit(round.State) {
			exiting[*round.ParentLoopNodeRunID] = true
		}
	}
	running := map[string]bool{}
	for _, row := range nodes {
		if row.Status != store.WorkflowStatusRunning || row.Iteration != nil || row.NodeType != "iteration" {
			continue
		}
		if n, ok := g.Node(row.NodeID); ok && n.Kind == "iteration" {
			running[row.NodeID] = true
		}
	}
	interrupted := map[string]bool{}
	for _, row := range nodes {
		if row.Status != store.WorkflowStatusRunning {
			return nil
		}
		if row.Iteration == nil {
			if row.NodeType == "loop" && exiting[row.ID] {
				if n, ok := g.Node(row.NodeID); ok && n.Kind == "loop" {
					continue
				}
			}
			if row.NodeType == "iteration" && running[row.NodeID] {
				continue
			}
			return nil
		}
		member, ok := g.Node(row.NodeID)
		if !ok || !running[member.RegionOwner] {
			return nil
		}
		interrupted[row.ID] = true
	}
	return interrupted
}

// Interrupt fails attempts a previous process left executing with desktop's interrupted_by_restart
// detail. Pending LoopExit cleanup survives; other root and Loop work fails its run. An interrupted
// Iteration round fails alone, then its owner applies the round's error strategy. Frozen forms
// and deterministic post-review work survive; no interrupted Agent turn is replayed. It must run
// after execution recovery has ended the old process's sessions, before the host serves requests and
// after the owners of task runs have cancelled theirs, so an absorbed run is never task-owned
// (package boot orders both).
func (h *Host) Interrupt(ctx context.Context) error {
	f := workflow.Failure{Kind: workflow.FailureInterruptedByRestart, Message: interruptedMessage}
	now := h.now()
	ids, err := h.store.InterruptWorkflowRunsWithRecovery(ctx, interruptedMessage, func(attempt int, node []byte) ([]byte, error) {
		return workflow.MergePayload(node, map[string]any{"error_detail": f.Detail(attempt, true, now)})
	}, workflowInterrupts)
	if err != nil {
		return fmt.Errorf("interrupt workflow runs: %w", err)
	}
	for _, id := range ids {
		current, err := h.store.WorkflowEngineContext(ctx, id)
		if err != nil {
			return fmt.Errorf("read interrupted run %s: %w", id, err)
		}
		// The sweep fails every run it cannot resume (no durable starter), so only absorbed
		// Iteration or pending LoopExit runs are still running here.
		if current.Run.Status != store.WorkflowStatusRunning {
			continue
		}
		actor, err := h.store.WorkflowStarter(ctx, id)
		if err != nil {
			return fmt.Errorf("read starter of recoverable workflow %s: %w", id, err)
		}
		if actor == "" {
			// Defensive: the sweep already excludes unstartered runs; reaching here means the
			// starter vanished between the sweep and this read, so fail rather than resume as
			// someone else.
			h.log.Error("recoverable workflow lost its starter; failing the run", "run", id)
			message := interruptedMessage
			if _, err := h.store.FinishWorkflowRun(ctx, current.Run.TenantID, id, store.WorkflowStatusFailed, nil, &message); err != nil {
				h.log.Error("fail recoverable workflow without a starter", "run", id, "error", err)
			}
			continue
		}
		st := h.register(id, execution.Scope{TenantID: current.Run.TenantID, UserID: actor})
		err = h.engine.Resume(ctx, id)
		st.started()
		h.forgetFinished(id, st)
		if err != nil {
			h.log.Error("recover interrupted workflow", "run", id, "error", err)
		}
	}
	reviews, err := h.store.RecoverableFormRuns(ctx)
	if err != nil {
		return err
	}
	for _, id := range reviews {
		if err := h.resumeReviewRun(ctx, id); err != nil {
			h.log.Error("recover review run", "run", id, "error", err)
		}
	}
	if len(ids) != 0 {
		h.log.Info("interrupted workflow runs left by the previous process", "runs", len(ids))
	}
	return nil
}

// runState is one started run's process-local execution state.
type runState struct {
	scope execution.Scope

	mu         sync.Mutex
	starters   int
	cancelling bool
	// generation counts the resumes of this run in this process. A dispatch carries the generation
	// it was dispatched in, so one dispatched before a cancellation and a resume can never start a
	// turn for the attempt the resume soft-deleted (see beginStart).
	generation uint64
	// dispatched counts the goroutines of attempts and follow-up turns that have not returned.
	dispatched int
	// attempts are the turns starting or executing, by caller attempt: Agent attempts and the
	// follow-up turns of interactive nodes. They execute concurrently.
	attempts map[string]*attemptState
	// startsChanged, when set, is closed as soon as any turn's Start returns.
	startsChanged      chan struct{}
	closedScopes       map[string]bool
	scopeCleanupErrors map[string]error

	// interactive holds the root-scope interactive Agent nodes whose session is open between
	// turns, by node ID. Process-local on purpose: a restart fails parked nodes.
	interactive map[string]*interactiveNode
}

// attemptState is one turn from just before its Start (or Append) until it ended.
type attemptState struct {
	nodeRun string // the node run the turn belongs to, so a node's stop finds its turn
	scope   string
	// starting is closed when the in-flight Start returns, so a cancellation can learn its ref.
	starting chan struct{}
	ref      *execution.Ref
	// done is closed once the turn ended (or never started).
	done chan struct{}
	// unstopped marks a turn whose Start a cancellation gave up waiting for: its attempt stops
	// the turn itself once the Start returns.
	unstopped bool
}

// register records who is starting a run and counts the start under way. A run that already
// started keeps its first owner; a pending run's state is dropped again when every start of it
// failed (see forgetFinished), so a later start by someone else executes under their scope.
func (h *Host) register(runID string, scope execution.Scope) *runState {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.runs[runID]
	if st == nil {
		st = &runState{scope: scope}
		h.runs[runID] = st
	}
	st.mu.Lock()
	st.starters++
	st.mu.Unlock()
	return st
}

// started ends one start counted by register.
func (st *runState) started() {
	st.mu.Lock()
	st.starters--
	st.mu.Unlock()
}

func (h *Host) state(runID string) *runState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.runs[runID]
}

func executionScope(s server.WorkflowRunScope) execution.Scope {
	return execution.Scope{TenantID: s.TenantID, UserID: s.UserID}
}

// StartRun authorizes the run against the caller's tenant, then starts it and its first wave.
func (h *Host) StartRun(ctx context.Context, scope server.WorkflowRunScope, runID string) (store.WorkflowRun, error) {
	run, err := h.store.WorkflowRun(ctx, scope.TenantID, runID)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	if run.Status != store.WorkflowStatusPending {
		return store.WorkflowRun{}, server.ErrWorkflowRunNotStartable
	}
	st := h.register(runID, executionScope(scope))
	// The run outlives the request once it starts; a disconnecting client must not abandon the
	// first scheduling pass halfway.
	err = h.engine.StartAs(context.WithoutCancel(ctx), runID, st.scope.UserID)
	st.started()
	// A run without Agents can finish within its first pass, and a refused start leaves the run
	// pending without an owner.
	h.forgetFinished(runID, st)
	if err != nil {
		return store.WorkflowRun{}, lifecycleError(err)
	}
	return h.store.WorkflowRun(ctx, scope.TenantID, runID)
}

// UpdateRunInput replaces the kickoff text and Start values while the run is still pending.
func (h *Host) UpdateRunInput(ctx context.Context, scope server.WorkflowRunScope, runID string, input *string, values map[string]json.RawMessage) (store.WorkflowRun, error) {
	if _, err := h.store.WorkflowRun(ctx, scope.TenantID, runID); err != nil {
		return store.WorkflowRun{}, err
	}
	if err := h.engine.UpdateInput(ctx, runID, input, values); err != nil {
		if errors.Is(err, store.ErrWorkflowRunState) {
			return store.WorkflowRun{}, fmt.Errorf("%w: %w", server.ErrWorkflowRunNotEditable, err)
		}
		return store.WorkflowRun{}, lifecycleError(err)
	}
	h.hub.Publish(runID)
	return h.store.WorkflowRun(ctx, scope.TenantID, runID)
}

// CancelRun stops every executing Agent turn of the run at once and waits for execution to confirm
// all of them before closing the run as cancelled. Without confirmation the run stays open and the
// caller learns the stop is unconfirmed; the run still never advances, and an attempt closes it as
// cancelled once its turn ends. When no turn was executing and only a parked interactive session could not
// be ended, no worker follows up: the run stays open, refusing every node action, until the
// caller cancels again.
func (h *Host) CancelRun(ctx context.Context, scope server.WorkflowRunScope, runID string) (store.WorkflowRun, error) {
	// Look the run up in the caller's tenant before taking its lock, so an unknown or foreign id
	// never creates lifecycle state.
	run, err := h.store.WorkflowRun(ctx, scope.TenantID, runID)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	defer h.lockLifecycle(runID)()
	// The status decides whether there is execution to stop, so it is read again together with
	// the retry fence, under the run gate: a resume that committed after the read above has
	// returned the run to running and dispatched its Agents, which must be stopped too.
	status, err := h.engine.FenceRetriesForCancel(ctx, runID)
	if err != nil {
		return store.WorkflowRun{}, err // the engine dropped the fence again
	}
	run.Status = status
	st := h.state(runID)
	if st != nil && (run.Status == store.WorkflowStatusPending || run.Status == store.WorkflowStatusRunning || run.Status == store.WorkflowRunAwaitingInput) {
		stopCtx, cancel := context.WithTimeout(ctx, h.stopTimeout)
		defer cancel()
		st.mu.Lock()
		st.cancelling = true // no further turn starts
		st.mu.Unlock()
		if _, err := h.stopTurns(stopCtx, st, func(*attemptState) bool { return true }); err != nil {
			return store.WorkflowRun{}, fmt.Errorf("%w: %w", server.ErrWorkflowRunStopUnconfirmed, err)
		}
		// Parked interactive nodes keep their sessions open between turns; a cancelled run
		// must not leave them open.
		if err := h.endParked(stopCtx, st); err != nil {
			return store.WorkflowRun{}, fmt.Errorf("%w: %w", server.ErrWorkflowRunStopUnconfirmed, err)
		}
	}
	run, err = h.store.CancelWorkflowRun(ctx, scope.TenantID, runID)
	if err == nil || errors.Is(err, store.ErrWorkflowRunState) {
		h.engine.ReleaseRetries(runID) // the run is closed; its waits are settled
	}
	if err != nil {
		return store.WorkflowRun{}, err
	}
	h.hub.Publish(runID)
	if st != nil {
		st.mu.Lock()
		st.interactive = nil // their sessions ended above and CancelWorkflowRun closed their rows
		st.mu.Unlock()
		h.forgetFinished(runID, st)
	}
	return run, nil
}

// stopTurns cancels the selected turns concurrently and reports an error unless execution confirmed
// every stop: a run's sibling Agents end together, not one after another. Turns already executing
// are cancelled at once; a turn whose Start is still in flight is cancelled as soon as that Start
// returns. If ctx ends first, every turn executing by then is still cancelled, the stops issued
// are awaited and ctx's error is returned; a turn whose Start is still in flight is marked so its
// attempt stops it once the Start returns (see runAgent). It returns the turns it cancelled.
func (h *Host) stopTurns(ctx context.Context, st *runState, selected func(*attemptState) bool) ([]*attemptState, error) {
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		errs    []error
		stopped []*attemptState
	)
	issued := map[*attemptState]bool{}
	defer wg.Wait()
	for giveUp := false; ; {
		st.mu.Lock()
		starting := false
		for _, a := range st.attempts {
			if !selected(a) {
				continue
			}
			if giveUp && a.starting != nil {
				a.unstopped = true
				continue
			}
			if a.ref != nil && !issued[a] && giveUp {
				// Started just as ctx ended: still stop it, past this call's deadline.
				issued[a] = true
				stopped = append(stopped, a)
				ref := *a.ref
				h.workers.Add(1)
				go func() {
					defer h.workers.Done()
					late, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.stopTimeout)
					defer cancel()
					if _, err := h.exec.Cancel(late, st.scope, ref); err != nil {
						h.log.Warn("stop a turn that started as its cancellation timed out", "session", ref.SessionID, "error", publicReason(err))
					}
				}()
				continue
			}
			if a.ref != nil && !issued[a] {
				issued[a] = true
				stopped = append(stopped, a)
				ref := *a.ref
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := h.exec.Cancel(ctx, st.scope, ref); err != nil {
						mu.Lock()
						errs = append(errs, err)
						mu.Unlock()
					}
				}()
			}
			if a.starting != nil {
				starting = true
			}
		}
		var changed chan struct{}
		if starting {
			if st.startsChanged == nil {
				st.startsChanged = make(chan struct{})
			}
			changed = st.startsChanged
		}
		st.mu.Unlock()
		if giveUp {
			wg.Wait()
			return stopped, errors.Join(append(errs, ctx.Err())...)
		}
		if !starting {
			wg.Wait()
			return stopped, errors.Join(errs...)
		}
		select {
		case <-changed: // any Start returned: stop its turn without waiting for the others
		case <-ctx.Done():
			giveUp = true // one more pass under mu: cancel what started, mark what still starts
		}
	}
}

// lifecycleError translates the engine's refusals into the HTTP layer's lifecycle errors.
func lifecycleError(err error) error {
	var start *workflow.StartError
	var rejection *workflow.InputRejection
	switch {
	case errors.As(err, &rejection):
		return &server.WorkflowRunInputInvalid{Variable: rejection.Variable, Reason: rejection.Reason}
	case errors.As(err, &start) && start.Variable != "":
		return &server.WorkflowRunInputInvalid{Variable: start.Variable, Reason: start.Detail}
	case errors.As(err, &start), errors.Is(err, workflow.ErrNotStartable):
		return fmt.Errorf("%w: %w", server.ErrWorkflowRunNotStartable, err)
	}
	return err
}
