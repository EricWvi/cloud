// Package executionruntime wires v2's single-process execution service to PostgreSQL and
// model/Agent drivers. A process-local mutex owns admission; there is no distributed scheduler.
package executionruntime

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/modelturn"
	"github.com/wanglongan587/cloud/v2-server/internal/nodeservices"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/strictjson"
)

// AgentDriver owns project Nodes and their physical Agent sessions. Run may publish the
// terminal result through emit before returning, so durable Node events can be ACKed afterward.
type AgentDriver interface {
	Run(context.Context, execution.Scope, execution.Session, execution.Turn, func(context.Context, execution.Event) error) (execution.Result, error)
	End(context.Context, string) error
	// Rebuild marks the Node serving a session for replacement after End could not confirm
	// its stop. The project's next session, or a later End, stops the old process tree first.
	Rebuild(string)
	Close(context.Context) error
}

// ModelChoice separates the public immutable selection from the private provider binding.
type ModelChoice struct {
	Model   execution.Model
	Binding nodeservices.Binding
}

// Config is trusted startup configuration. Agent may be nil for model-only operation.
type Config struct {
	Store  *store.Store
	Models map[string]ModelChoice
	Agent  AgentDriver
}

// pending is a turn result whose write still failed after retries. The service keeps storing it
// until the database takes it, so a recovered database records the real outcome and observers
// waiting on the durable turn end are released without a restart.
type pending struct {
	ref    execution.Ref
	result execution.Result
}

// Limits of the per-session failure handling. Each Service copies them at construction, so a
// test shortens its own instance without touching another's.
var (
	// How long one turn retries its result write before only its own session fails: a short
	// database outage, not an indefinite wait.
	defaultFinishRetryDelays = []time.Duration{time.Second, 2 * time.Second}
	// The pace of the writes that continue after a worker's own retries are exhausted.
	defaultConcludeInterval = 5 * time.Second
	// The service's own budget for one physical stop. Callers may wait less, and a caller
	// without a deadline never waits longer.
	defaultStopTimeout = 30 * time.Second
)

// stopCall is one shared attempt to confirm a session's physical stop.
type stopCall struct {
	done chan struct{}
	err  error
}

type worker struct {
	cancel context.CancelFunc
	done   chan struct{}
	ref    execution.Ref
	scope  execution.Scope
	group  string
	err    error
}

// Service owns all execution workers. Close must finish before the database is closed.
type Service struct {
	db         *store.Store
	models     map[string]ModelChoice
	agent      AgentDriver
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	workers    map[string]*worker
	groups     map[string]string
	workspaces map[string]string
	// held lists the live LifetimeHeld sessions this process started.
	held              map[string]bool
	ending            map[string]bool
	unsaved           map[string]pending
	stops             map[string]*stopCall
	concluding        sync.WaitGroup
	finishRetryDelays []time.Duration
	concludeInterval  time.Duration
	stopTimeout       time.Duration
	finishTurn        func(context.Context, execution.Scope, execution.Ref, execution.Result) (execution.Result, error)
	closed            bool
}

var _ execution.Service = (*Service)(nil)
var _ execution.Catalog = (*Service)(nil)

// New closes old persisted sessions before admitting work. Models may be empty when the
// deployment has no real provider, leaving the catalog empty until one is configured. Node
// process recovery belongs to AgentDriver initialization; callers must construct that driver
// before exposing this service.
func New(ctx context.Context, cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, execution.ErrUnavailable
	}
	models := make(map[string]ModelChoice, len(cfg.Models))
	for id, choice := range cfg.Models {
		if id == "" || choice.Model.ID != id || choice.Model.Name == "" || choice.Model.BindingVersion == "" || choice.Binding.Provider == nil || (choice.Binding.Protocol != nodeservices.OpenAICompletions && choice.Binding.Protocol != nodeservices.AnthropicMessages) {
			return nil, execution.ErrInvalidRequest
		}
		models[id] = choice
	}
	if err := cfg.Store.RecoverExecutions(ctx); err != nil {
		return nil, err
	}
	owned, cancel := context.WithCancel(ctx)
	return &Service{db: cfg.Store, models: models, agent: cfg.Agent, ctx: owned, cancel: cancel, finishTurn: cfg.Store.FinishExecutionTurn,
		finishRetryDelays: defaultFinishRetryDelays, concludeInterval: defaultConcludeInterval, stopTimeout: defaultStopTimeout,
		workers: map[string]*worker{}, groups: map[string]string{}, workspaces: map[string]string{}, held: map[string]bool{}, ending: map[string]bool{}, unsaved: map[string]pending{}, stops: map[string]*stopCall{}}, nil
}

// Start validates the project and freezes a binding before recording the first Agent turn.
func (s *Service) Start(ctx context.Context, scope execution.Scope, req execution.StartRequest) (execution.Ref, error) {
	// Own the nested slices before admission; callers cannot mutate the retained snapshot.
	if req.Skills != nil {
		if req.Caller.Kind != execution.CallerWorkflow {
			return execution.Ref{}, execution.ErrInvalidRequest
		}
		owned := *req.Skills
		owned.Documents = slices.Clone(owned.Documents)
		owned.Plugins = slices.Clone(owned.Plugins)
		req.Skills = &owned
	}
	switch req.Lifetime {
	case execution.LifetimeTurn:
	case execution.LifetimeHeld:
		if req.Caller.Kind != execution.CallerWorkflow {
			return execution.Ref{}, execution.ErrInvalidRequest
		}
	default:
		return execution.Ref{}, execution.ErrInvalidRequest
	}
	if req.MCP != nil && req.Caller.Kind != execution.CallerWorkflow {
		return execution.Ref{}, execution.ErrInvalidRequest
	}
	if err := req.MCP.Validate(); err != nil {
		return execution.Ref{}, err
	}
	// Controller generation admission and lifecycle events are not implemented yet. Reject
	// before copying packages, stopping an idle predecessor, or persisting a Session/Turn.
	// The driver's independent Node capability gate remains defense in depth.
	if req.MCP != nil && len(req.MCP.Bindings) != 0 {
		return execution.Ref{}, &execution.MCPFailure{Code: "mcp_lifecycle_not_ready"}
	}
	req.MCP = req.MCP.Clone()
	return s.start(ctx, scope, req.Caller, req.Prompt, &req.Target, req.ExecutorID, req.AgentID, req.ModelID, req.Skills, req.MCP, req.Lifetime)
}

// StartModel starts an independent model conversation without creating a directory or Node.
func (s *Service) StartModel(ctx context.Context, scope execution.Scope, req execution.ModelStartRequest) (execution.Ref, error) {
	return s.start(ctx, scope, req.Caller, req.Prompt, nil, "", "", req.ModelID, nil, nil, execution.LifetimeTurn)
}

func (s *Service) start(ctx context.Context, scope execution.Scope, caller execution.Caller, prompt execution.Prompt, target *execution.Target, executorID, agentID, modelID string, skills *execution.Skills, mcp *execution.MCPSelection, lifetime execution.Lifetime) (execution.Ref, error) {
	if err := validate(caller, prompt); err != nil {
		return execution.Ref{}, err
	}
	if target != nil {
		if target.ProjectID == "" {
			return execution.Ref{}, execution.ErrInvalidRequest
		}
		if executorID == "" && (agentID == "" || modelID == "") {
			return execution.Ref{}, execution.ErrInvalidRequest
		}
	} else if executorID != "" || agentID != "" {
		return execution.Ref{}, execution.ErrInvalidRequest
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return execution.Ref{}, execution.ErrUnavailable
	}
	// Admission is where execution starts on the user's behalf, so it applies tenant authorization;
	// the store repeats it inside the transaction that records the session or turn.
	if _, err := s.db.TenantAccess(ctx, scope.TenantID, scope.UserID); err != nil {
		return execution.Ref{}, publicError(err)
	}
	// Retained attempts remain readable even when the original runtime has ended.
	if old, err := s.db.ReadExecutionAttempt(ctx, scope, caller); err == nil {
		session, e := s.db.ReadExecutionSession(ctx, scope, old.Ref.SessionID)
		if e != nil {
			return execution.Ref{}, publicError(e)
		}
		if !sameJSON(session.MCP, mcp) || !reflect.DeepEqual(session.Skills, skills) || !sameJSON(old.Prompt, prompt) || !reflect.DeepEqual(session.Target, target) || (target == nil && session.Model.ID != modelID) || (target != nil && !sameAgentSelection(session.Executor, executorID, agentID, modelID)) {
			return execution.Ref{}, execution.ErrConflict
		}
		return old.Ref, nil
	} else if !errors.Is(err, execution.ErrNotFound) {
		return execution.Ref{}, publicError(err)
	}
	sessionID := newID()
	group := turnGroup(scope, caller, sessionID)
	if s.groups[group] != "" {
		return execution.Ref{}, execution.ErrWorkspaceBusy
	}
	var executor *execution.Executor
	if target != nil {
		if s.agent == nil {
			return execution.Ref{}, execution.ErrUnavailable
		}
		if _, err := s.db.GetProject(ctx, scope, target.ProjectID); err != nil {
			return execution.Ref{}, publicError(err)
		}
		var ok bool
		executor, ok = s.resolveExecutor(executorID, agentID, modelID)
		if !ok {
			return execution.Ref{}, execution.ErrNotFound
		}
		modelID = executor.Model.ID
	}
	choice, ok := s.models[modelID]
	// A direct model turn speaks OpenAI chat completions; Messages bindings serve Agents only.
	if !ok || (target == nil && choice.Binding.Protocol != nodeservices.OpenAICompletions) {
		return execution.Ref{}, execution.ErrNotFound
	}
	session := execution.Session{MCP: mcp, Skills: skills, ID: sessionID, Target: target, Executor: executor, Model: choice.Model, CreatedAt: time.Now().UTC()}
	if target != nil {
		session.WorkspaceID = newID()
		if caller.Kind == execution.CallerWorkflow {
			key := groupKey(scope, caller) + "/" + target.ProjectID
			if id := s.workspaces[key]; id != "" {
				session.WorkspaceID = id
			} else {
				s.workspaces[key] = session.WorkspaceID
			}
			// The run's sessions share this directory and stay live side by side: a held session
			// (an interactive node waiting for its starter) is not ended when the next node
			// starts. The Node admits several live sessions per checkout (its
			// shared_checkout_sessions capability, which the Agent driver requires) and refuses
			// only the session over its own limit, without touching the others. Each session
			// takes one turn at a time; the run's sessions run theirs concurrently (see turnGroup).
		}
	}
	turn := execution.Turn{Ref: execution.Ref{SessionID: session.ID, TurnID: newID()}, Caller: caller, Prompt: copyPrompt(prompt), StartedAt: time.Now().UTC()}
	if err := s.db.CreateExecutionSession(ctx, scope, session, turn); err != nil {
		return execution.Ref{}, publicError(err)
	}
	if lifetime == execution.LifetimeHeld {
		s.held[session.ID] = true
	}
	s.launch(scope, session, turn, group)
	return turn.Ref, nil
}

// resolveExecutor accepts either the legacy combined binding ID or the independent Agent and
// model IDs. Mixed forms must describe the same binding; display names never select execution.
func (s *Service) resolveExecutor(executorID, agentID, modelID string) (*execution.Executor, bool) {
	if executorID == "" && (agentID == "" || modelID == "") {
		return nil, false
	}
	for id, choice := range s.models {
		for _, a := range agentExecutors {
			e, ok := executorFor(a, choice)
			if !ok {
				continue
			}
			if executorID != "" && e.ID != executorID {
				continue
			}
			if executorID == "" && (e.AgentID != agentID || id != modelID) {
				continue
			}
			if executorID != "" && ((agentID != "" && e.AgentID != agentID) || (modelID != "" && id != modelID)) {
				continue
			}
			copy := e
			return &copy, true
		}
	}
	return nil, false
}

// sameAgentSelection compares an existing frozen executor snapshot with either the legacy
// combined ID or the independent Agent/model IDs without consulting the mutable catalog.
func sameAgentSelection(e *execution.Executor, executorID, agentID, modelID string) bool {
	if e == nil || (executorID == "" && agentID == "" && modelID == "") {
		return false
	}
	if executorID != "" && e.ID != executorID {
		return false
	}
	if agentID != "" && e.AgentID != agentID {
		return false
	}
	if modelID != "" && e.Model.ID != modelID {
		return false
	}
	return true
}

// Append continues a live logical session. A durable terminal event may precede driver cleanup;
// the next turn waits for that worker without releasing its directory or requiring another event.
// A server restart never reconstructs old workers.
func (s *Service) Append(ctx context.Context, scope execution.Scope, req execution.AppendRequest) (execution.Ref, error) {
	if err := validate(req.Caller, req.Prompt); err != nil {
		return execution.Ref{}, err
	}
	// A new turn starts execution on the user's behalf, so it applies tenant authorization like
	// start; the store repeats it inside the transaction that records the turn.
	if _, err := s.db.TenantAccess(ctx, scope.TenantID, scope.UserID); err != nil {
		return execution.Ref{}, publicError(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if s.closed || s.ctx.Err() != nil {
			return execution.Ref{}, execution.ErrUnavailable
		}
		session, err := s.db.ReadExecutionSession(ctx, scope, req.SessionID)
		if err != nil {
			return execution.Ref{}, publicError(err)
		}
		if old, e := s.db.ReadExecutionAttempt(ctx, scope, req.Caller); e == nil {
			if old.Ref.SessionID != req.SessionID || !sameJSON(old.Prompt, req.Prompt) {
				return execution.Ref{}, execution.ErrConflict
			}
			return old.Ref, nil
		} else if !errors.Is(e, execution.ErrNotFound) {
			return execution.Ref{}, publicError(e)
		}
		if session.EndedAt != nil || s.ending[session.ID] {
			return execution.Ref{}, execution.ErrSessionEnded
		}
		first, err := s.db.ReadExecutionTurns(ctx, scope, session.ID, "", 1)
		if err != nil {
			return execution.Ref{}, publicError(err)
		}
		if len(first.Items) != 1 || first.Items[0].Caller.Kind != req.Caller.Kind || first.Items[0].Caller.ID != req.Caller.ID {
			return execution.Ref{}, execution.ErrConflict
		}
		group := turnGroup(scope, req.Caller, session.ID)
		if active := s.groups[group]; active != "" {
			prior := s.workers[active]
			if prior == nil {
				return execution.Ref{}, execution.ErrWorkspaceBusy
			}
			retained, err := s.readTurn(ctx, scope, prior.ref)
			if err != nil {
				return execution.Ref{}, err
			}
			if retained.Result == nil {
				return execution.Ref{}, execution.ErrWorkspaceBusy
			}
			// Publication precedes the Node ACK and worker cleanup. Keep ownership
			// intact, but do not tell an observer of that terminal event to wait for
			// a second event that will never arrive. Cleanup itself needs this mutex.
			s.mu.Unlock()
			var waitErr error
			select {
			case <-ctx.Done():
				waitErr = ctx.Err()
			case <-s.ctx.Done():
				waitErr = execution.ErrUnavailable
			case <-prior.done:
			}
			s.mu.Lock()
			if waitErr != nil {
				return execution.Ref{}, waitErr
			}
			// Another Append, End, Close or permission change may have won while
			// unlocked. Recheck admission and attempt identity before writing anything.
			continue
		}
		turn := execution.Turn{Ref: execution.Ref{SessionID: session.ID, TurnID: newID()}, Caller: req.Caller, Prompt: copyPrompt(req.Prompt), StartedAt: time.Now().UTC()}
		if err := s.db.AppendExecutionTurn(ctx, scope, session.ID, turn); err != nil {
			return execution.Ref{}, publicError(err)
		}
		s.launch(scope, session, turn, group)
		return turn.Ref, nil
	}
}

// launch is called under mu; workers use the backend's lifetime, never an HTTP request context.
func (s *Service) launch(scope execution.Scope, session execution.Session, turn execution.Turn, group string) {
	ctx, cancel := context.WithCancel(s.ctx)
	w := &worker{cancel: cancel, done: make(chan struct{}), ref: turn.Ref, scope: scope, group: group}
	held := s.held[session.ID]
	s.workers[turn.Ref.TurnID] = w
	s.groups[group] = turn.Ref.TurnID
	go func() {
		defer cancel()
		emit := func(_ context.Context, event execution.Event) error {
			write, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			event.Ref = turn.Ref
			if event.Kind == execution.EventTurnEnded {
				if event.Result == nil {
					return execution.ErrInvalidRequest
				}
				_, err := s.db.FinishExecutionTurn(write, scope, turn.Ref, *event.Result)
				return err
			}
			_, err := s.db.AppendExecutionEvent(write, scope, event)
			return err
		}
		var result execution.Result
		var agentErr error
		if session.Target != nil {
			result, agentErr = s.agent.Run(ctx, scope, session, turn, emit)
			if agentErr != nil && result.Status == "" {
				result = failedResult(ctx, agentErr)
			}
		} else {
			messages, err := s.messages(ctx, scope, turn)
			if err != nil {
				result = failedResult(ctx, err)
			} else {
				result, _ = modelturn.Run(ctx, s.models[session.Model.ID].Binding, messages, func(c context.Context, delta string) error {
					return emit(c, execution.Event{Kind: execution.EventTextDelta, TextDelta: delta})
				})
			}
		}
		if result.Status == "" {
			result = failedResult(ctx, execution.ErrUnavailable)
		}
		// An Agent error means the session is gone; the driver marks the results it publishes
		// itself, and this marks the ones only the runtime writes.
		result.AgentLost = result.AgentLost || agentErr != nil
		result, err := s.finish(scope, turn.Ref, result)
		// Failures stay with this session: nothing here stops admission for other sessions.
		// A held session survives a turn the Agent itself ended without completing: the driver
		// reports no error then, and the Agent is still there to take the next turn.
		broken := session.Target != nil && (agentErr != nil || (result.Status != execution.StatusCompleted && !held))
		if err != nil || broken {
			// An unstored result or a failed/cancelled physical Agent is never advertised as
			// resumable, held or not. Keep admission occupied until stop is confirmed.
			s.mu.Lock()
			s.ending[session.ID] = true
			if err != nil {
				// A held session's caller reads AgentLost to learn that no turn can follow, and
				// a result that could not be stored ends the session as a lost Agent does.
				result.AgentLost = result.AgentLost || held
				s.unsaved[session.ID] = pending{ref: turn.Ref, result: result}
			}
			s.mu.Unlock()
			stopped := true
			if session.Target != nil && s.stopAgent(context.Background(), session.ID) != nil {
				stopped = false
				err = errors.Join(err, execution.ErrStopUnconfirmed)
			}
			write, stop := context.WithTimeout(context.Background(), 10*time.Second)
			concludeErr := s.conclude(write, scope, session.ID, stopped)
			stop()
			if concludeErr != nil {
				// Callers wait on the durable turn end, so the write must not depend on
				// anyone calling End: keep trying until the database takes it.
				err = errors.Join(err, concludeErr)
				s.concludeLater(scope, session.ID, stopped)
			}
		}
		s.mu.Lock()
		w.err = err
		delete(s.groups, group)
		delete(s.workers, turn.Ref.TurnID)
		close(w.done)
		s.mu.Unlock()
	}()
}

// finish retries a short database outage and returns the result it tried to store. A result
// the store rejects as invalid can never be written, so it becomes a failed turn rather than
// a turn that never ends.
func (s *Service) finish(scope execution.Scope, ref execution.Ref, result execution.Result) (execution.Result, error) {
	for attempt := 0; ; attempt++ {
		write, stop := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := s.finishTurn(write, scope, ref, result)
		stop()
		if errors.Is(err, execution.ErrInvalidRequest) && result.Status != execution.StatusFailed {
			result = execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"}
			attempt--
			continue
		}
		if err == nil || attempt >= len(s.finishRetryDelays) {
			return result, err
		}
		select {
		case <-s.ctx.Done():
			return result, err
		case <-time.After(s.finishRetryDelays[attempt]):
		}
	}
}

// conclude stores the result a session's worker could not write and, once its physical stop is
// confirmed, ends the session. Both writes are idempotent, so the worker, its background retry
// and End may race. It never holds mu across database I/O.
func (s *Service) conclude(ctx context.Context, scope execution.Scope, sid string, stopped bool) error {
	s.mu.Lock()
	p, unsaved := s.unsaved[sid]
	s.mu.Unlock()
	if unsaved {
		if _, err := s.finishTurn(ctx, scope, p.ref, p.result); err != nil {
			return err
		}
		s.mu.Lock()
		delete(s.unsaved, sid)
		s.mu.Unlock()
	}
	if !stopped {
		// The Node is marked for rebuild; only an End that obtains stop proof ends the session.
		return nil
	}
	if err := s.db.EndExecutionSession(ctx, scope, sid); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.held, sid)
	s.mu.Unlock()
	return nil
}

// concludeLater repeats conclude after the worker has gone, until it succeeds, the store rejects
// the write for good (the session no longer exists or the request is invalid), or the service
// closes. Only workers call it, so Close can join every retry.
func (s *Service) concludeLater(scope execution.Scope, sid string, stopped bool) {
	s.concluding.Add(1)
	go func() {
		defer s.concluding.Done()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(s.concludeInterval):
			}
			write, stop := context.WithTimeout(s.ctx, 10*time.Second)
			err := s.conclude(write, scope, sid, stopped)
			stop()
			if err == nil || errors.Is(err, store.ErrNotFound) || errors.Is(err, execution.ErrNotFound) || errors.Is(err, execution.ErrInvalidRequest) {
				return
			}
		}
	}()
}

// stopAgent confirms a session's physical stop within the service's own budget. Concurrent
// callers share one attempt and may stop waiting when their context ends; the attempt itself
// continues. So a caller that gives up early never marks a healthy Node, a caller without a
// deadline never waits forever, and a stop the Node does not confirm within the budget marks
// only that project's Node for rebuild. The caller must not hold mu.
func (s *Service) stopAgent(ctx context.Context, sid string) error {
	s.mu.Lock()
	call := s.stops[sid]
	if call == nil {
		call = &stopCall{done: make(chan struct{})}
		s.stops[sid] = call
		go func() {
			budget, cancel := context.WithTimeout(context.Background(), s.stopTimeout)
			call.err = s.agent.End(budget, sid)
			cancel()
			if call.err != nil {
				// Only this project's Node is untrusted; its next session or End retires it.
				s.agent.Rebuild(sid)
			}
			s.mu.Lock()
			delete(s.stops, sid)
			s.mu.Unlock()
			close(call.done)
		}()
	}
	s.mu.Unlock()
	select {
	case <-call.done:
		if call.err != nil {
			return execution.ErrStopUnconfirmed
		}
		return nil
	case <-ctx.Done():
		return execution.ErrStopUnconfirmed
	}
}

func (s *Service) messages(ctx context.Context, scope execution.Scope, current execution.Turn) ([]modelturn.Message, error) {
	var messages []modelturn.Message
	cursor := ""
	for {
		page, err := s.db.ReadExecutionTurns(ctx, scope, current.Ref.SessionID, cursor, 100)
		if err != nil {
			return nil, err
		}
		for _, turn := range page.Items {
			text, err := turn.Prompt.Text()
			if err != nil {
				return nil, err
			}
			messages = append(messages, modelturn.Message{Role: "user", Content: text})
			if turn.Ref.TurnID == current.Ref.TurnID {
				return messages, nil
			}
			if turn.Result != nil {
				messages = append(messages, modelturn.Message{Role: "assistant", Content: turn.Result.Answer})
			}
		}
		if page.NextCursor == "" {
			return nil, execution.ErrNotFound
		}
		cursor = page.NextCursor
	}
}

// Cancel ends an Agent session because the pinned Node has only EndSession cancellation.
// Model-only cancellation stops the requested turn and leaves its conversation open.
func (s *Service) Cancel(ctx context.Context, scope execution.Scope, ref execution.Ref) (execution.Result, error) {
	turn, err := s.readTurn(ctx, scope, ref)
	if err != nil {
		return execution.Result{}, err
	}
	if turn.Result != nil {
		return *turn.Result, nil
	}
	session, err := s.db.ReadExecutionSession(ctx, scope, ref.SessionID)
	if err != nil {
		return execution.Result{}, publicError(err)
	}
	if session.Target != nil {
		// Serialize with Append: a late retry for an already finished turn must not
		// stop a newer turn. Once cancellation wins, no new turn can enter this session.
		s.mu.Lock()
		turn, err = s.readTurn(ctx, scope, ref)
		if err == nil && turn.Result == nil {
			s.ending[ref.SessionID] = true
		}
		s.mu.Unlock()
		if err != nil {
			return execution.Result{}, err
		}
		if turn.Result != nil {
			return *turn.Result, nil
		}
		if err := s.End(ctx, scope, ref.SessionID); err != nil {
			return execution.Result{}, err
		}
		turn, err = s.readTurn(ctx, scope, ref)
		if err != nil {
			return execution.Result{}, err
		}
		if turn.Result == nil {
			return execution.Result{}, execution.ErrStopUnconfirmed
		}
		return *turn.Result, nil
	}
	s.mu.Lock()
	w := s.workers[ref.TurnID]
	if w != nil {
		w.cancel()
	}
	s.mu.Unlock()
	if w == nil {
		// Completion can win after the initial read and remove the worker. Its durable
		// result is still the authority; absence from the map is not a stop failure.
		// Only a result its worker could not write is concluded here, and a model-only session
		// has nothing physical left to stop. A turn that merely finished keeps its session open.
		s.mu.Lock()
		_, unsaved := s.unsaved[ref.SessionID]
		s.mu.Unlock()
		if unsaved {
			if err := s.conclude(ctx, scope, ref.SessionID, true); err != nil {
				return execution.Result{}, publicError(err)
			}
		}
		turn, err = s.readTurn(ctx, scope, ref)
		if err != nil {
			return execution.Result{}, err
		}
		if turn.Result != nil {
			return *turn.Result, nil
		}
		return execution.Result{}, execution.ErrStopUnconfirmed
	}
	select {
	case <-ctx.Done():
		return execution.Result{}, ctx.Err()
	case <-w.done:
	}
	turn, err = s.readTurn(ctx, scope, ref)
	if err != nil {
		return execution.Result{}, err
	}
	if turn.Result == nil {
		return execution.Result{}, execution.ErrStopUnconfirmed
	}
	return *turn.Result, nil
}

// End stops and joins the active worker before persisting the session terminal event.
func (s *Service) End(ctx context.Context, scope execution.Scope, sid string) error {
	session, err := s.db.ReadExecutionSession(ctx, scope, sid)
	if err != nil {
		return publicError(err)
	}
	if session.EndedAt != nil {
		return nil
	}
	s.mu.Lock()
	s.ending[sid] = true
	var wait []*worker
	for _, w := range s.workers {
		if w.ref.SessionID == sid {
			w.cancel()
			wait = append(wait, w)
		}
	}
	s.mu.Unlock()
	for _, w := range wait {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.done:
		}
		if w.err != nil {
			return execution.ErrStopUnconfirmed
		}
	}
	if session.Target != nil {
		if err := s.stopAgent(ctx, sid); err != nil {
			return execution.ErrStopUnconfirmed
		}
	}
	return publicError(s.conclude(ctx, scope, sid, true))
}

// Close shuts down workers and physical Agents. Persisted sessions are ended by recovery on
// the next start, so no old conversation is advertised as resumable across process restarts.
func (s *Service) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	var wait []*worker
	for _, w := range s.workers {
		wait = append(wait, w)
	}
	s.mu.Unlock()
	for _, w := range wait {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.done:
		}
	}
	var err error
	if s.agent != nil {
		err = s.agent.Close(ctx)
	}
	// Background result writes use the database; they stop with s.ctx and are joined here.
	concluded := make(chan struct{})
	go func() {
		s.concluding.Wait()
		close(concluded)
	}()
	select {
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	case <-concluded:
	}
	return err
}

func (s *Service) ReadSession(ctx context.Context, scope execution.Scope, id string) (execution.Session, error) {
	v, e := s.db.ReadExecutionSession(ctx, scope, id)
	return v, publicError(e)
}
func (s *Service) ReadTurns(ctx context.Context, scope execution.Scope, id, cursor string, limit int) (execution.TurnPage, error) {
	v, e := s.db.ReadExecutionTurns(ctx, scope, id, cursor, limit)
	return v, publicError(e)
}
func (s *Service) ReadAttempt(ctx context.Context, scope execution.Scope, caller execution.Caller) (execution.Turn, error) {
	v, e := s.db.ReadExecutionAttempt(ctx, scope, caller)
	return v, publicError(e)
}
func (s *Service) readTurn(ctx context.Context, scope execution.Scope, ref execution.Ref) (execution.Turn, error) {
	cursor := ""
	for {
		p, err := s.ReadTurns(ctx, scope, ref.SessionID, cursor, 100)
		if err != nil {
			return execution.Turn{}, err
		}
		for _, t := range p.Items {
			if t.Ref == ref {
				return t, nil
			}
		}
		if p.NextCursor == "" {
			return execution.Turn{}, execution.ErrNotFound
		}
		cursor = p.NextCursor
	}
}

func (s *Service) ListModels(ctx context.Context, scope execution.Scope) ([]execution.Model, error) {
	// Consumers use the catalog as an execution-readiness gate, so a closing runtime must
	// not keep advertising selections. One session's failure never closes it for others.
	s.mu.Lock()
	unavailable := s.closed || s.ctx.Err() != nil
	s.mu.Unlock()
	if unavailable {
		return nil, execution.ErrUnavailable
	}
	items := make([]execution.Model, 0, len(s.models))
	for _, c := range s.models {
		if c.Binding.Protocol == nodeservices.OpenAICompletions {
			items = append(items, c.Model)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}
func (s *Service) ListExecutors(ctx context.Context, scope execution.Scope) ([]execution.Executor, error) {
	// ListModels supplies the scope and readiness checks; Messages bindings are not in it.
	if _, err := s.ListModels(ctx, scope); err != nil {
		return nil, err
	}
	items := []execution.Executor{}
	if s.agent == nil {
		return items, nil
	}
	ids := slices.Sorted(maps.Keys(s.models))
	// OpenCode stays first, so pages that preselect the first executor keep their default.
	for _, a := range agentExecutors {
		for _, id := range ids {
			if e, ok := executorFor(a, s.models[id]); ok {
				items = append(items, e)
			}
		}
	}
	return items, nil
}
func groupKey(scope execution.Scope, caller execution.Caller) string {
	return scope.TenantID + "/" + scope.UserID + "/" + string(caller.Kind) + "/" + caller.ID
}

// turnGroup is the admission unit that allows one active turn. A conversation, schedule or task
// caller owns one session, so its caller is the unit. A workflow run hosts concurrent Agent
// attempts, each in its own session, so each session is its own unit.
func turnGroup(scope execution.Scope, caller execution.Caller, session string) string {
	if caller.Kind == execution.CallerWorkflow {
		return "session/" + session
	}
	return groupKey(scope, caller)
}
func validate(c execution.Caller, p execution.Prompt) error {
	if c.ID == "" || c.AttemptID == "" || (c.Kind != execution.CallerChat && c.Kind != execution.CallerWorkflow && c.Kind != execution.CallerSchedule && c.Kind != execution.CallerTask) {
		return execution.ErrInvalidRequest
	}
	if c.Kind != execution.CallerWorkflow && (c.NodeID != "" || len(c.Rounds) != 0) {
		return execution.ErrInvalidRequest
	}
	for _, r := range c.Rounds {
		if r.ContainerID == "" || r.Index < 0 {
			return execution.ErrInvalidRequest
		}
	}
	size := 0
	for _, raw := range p {
		size += len(raw)
		if size > 1<<20 || !strictjson.Valid(raw) {
			return execution.ErrInvalidRequest
		}
	}
	_, err := p.Text()
	return err
}
func copyPrompt(p execution.Prompt) execution.Prompt {
	v := make(execution.Prompt, len(p))
	for i, b := range p {
		v[i] = append(json.RawMessage(nil), b...)
	}
	return v
}
func sameJSON(a, b any) bool {
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	var x, y any
	if json.Unmarshal(aa, &x) != nil || json.Unmarshal(bb, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func failedResult(ctx context.Context, err error) execution.Result {
	if errors.Is(ctx.Err(), context.Canceled) {
		return execution.Result{Status: execution.StatusCancelled, StopReason: "cancelled"}
	}
	return execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"}
}
func publicError(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, execution.ErrNotFound, execution.ErrInvalidRequest, execution.ErrConflict, execution.ErrWorkspaceBusy, execution.ErrSessionEnded, execution.ErrStopUnconfirmed} {
		if errors.Is(err, known) {
			return known
		}
	}
	if errors.Is(err, store.ErrNotFound) {
		return execution.ErrNotFound
	}
	return execution.ErrUnavailable
}
