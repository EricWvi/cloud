package executionruntime

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/taskrun"
	"github.com/wanglongan587/cloud/v2-server/internal/workflowhost"
)

// connectionCut drops actual PostgreSQL connections and refuses reconnections.
// Both reads and writes fail; this does not replace FinishExecutionTurn with a stub.
type connectionCut struct {
	mu    sync.Mutex
	down  bool
	conns []net.Conn
	dial  func(context.Context, string, string) (net.Conn, error)
}

func (c *connectionCut) connect(ctx context.Context, network, address string) (net.Conn, error) {
	c.mu.Lock()
	down := c.down
	c.mu.Unlock()
	if down {
		return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("test PostgreSQL outage")}
	}
	conn, err := c.dial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		_ = conn.Close()
		return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("test PostgreSQL outage")}
	}
	c.conns = append(c.conns, conn)
	return conn, nil
}

func (c *connectionCut) cut() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.down = true
	for _, conn := range c.conns {
		_ = conn.Close()
	}
	c.conns = nil
}

func (c *connectionCut) restore() { c.mu.Lock(); c.down = false; c.mu.Unlock() }

type outageAgent struct {
	started chan execution.Turn
	finish  chan struct{}
}

func (a *outageAgent) Run(ctx context.Context, _ execution.Scope, _ execution.Session, turn execution.Turn, _ func(context.Context, execution.Event) error) (execution.Result, error) {
	a.started <- turn
	select {
	case <-a.finish:
		return execution.Result{Status: execution.StatusCompleted, Answer: "backfilled answer", AnswerComplete: true, StopReason: "end_turn"}, nil
	case <-ctx.Done():
		return execution.Result{}, ctx.Err()
	}
}
func (*outageAgent) End(context.Context, string) error { return nil }
func (*outageAgent) Rebuild(string)                    {}
func (*outageAgent) Reattach(string, string, string)   {}
func (*outageAgent) Close(context.Context) error       { return nil }

const outageGraph = `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"hello","retry":{"enabled":false,"maxRetries":0,"initialDelaySeconds":1}}}},
 {"id":"out","data":{"kind":"output","outputs":[{"name":"answer","variableSelector":["a","output"]}]}}
],"edges":[{"source":"start","target":"a"},{"source":"a","target":"out"}]}`

func TestObserversRecoverWhenPostgresReadsAndWritesFailTogether(t *testing.T) {
	for _, observer := range []string{"stream", "workflow", "task"} {
		t.Run(observer, func(t *testing.T) {
			f := runtimeDatabase(t, nil)
			if err := f.service.Close(f.ctx); err != nil {
				t.Fatal(err)
			}
			f.db.Close()
			cut := &connectionCut{dial: f.config.ConnConfig.DialFunc}
			defer cut.restore()
			cfg := f.config.Copy()
			cfg.ConnConfig.DialFunc = cut.connect
			var err error
			f.db, err = store.OpenConfig(f.ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			agent := &outageAgent{started: make(chan execution.Turn, 1), finish: make(chan struct{})}
			f.service, err = New(f.ctx, Config{Store: f.db, Models: f.models, Agent: agent})
			if err != nil {
				t.Fatal(err)
			}
			f.service.finishRetryDelays = []time.Duration{10 * time.Millisecond}
			f.service.concludeInterval = 20 * time.Millisecond
			readFailed := make(chan struct{})
			var readOnce sync.Once
			f.service.observerReadFailed = func() { readOnce.Do(func() { close(readFailed) }) }
			var first atomic.Bool
			writeFailed := make(chan struct{})
			finish := f.service.finishTurn
			f.service.finishTurn = func(ctx context.Context, scope execution.Scope, ref execution.Ref, result execution.Result) (execution.Result, error) {
				if first.CompareAndSwap(false, true) {
					cut.cut()
					got, err := finish(ctx, scope, ref, result)
					if err == nil {
						t.Error("cut PostgreSQL connection still accepted the result write")
					}
					close(writeFailed)
					return got, err
				}
				return finish(ctx, scope, ref, result)
			}
			project, err := f.db.CreateProject(f.ctx, f.scope, store.Project{ID: "outage-project", Name: "Outage", RepositoryURL: "https://example.test/repo.git", Commit: "fixed", MirrorPath: "/unused"})
			if err != nil {
				t.Fatal(err)
			}
			var settled func() bool
			var ready func() bool
			switch observer {
			case "stream":
				ref, err := f.service.Start(f.ctx, f.scope, execution.StartRequest{Caller: execution.Caller{Kind: execution.CallerWorkflow, ID: "run", NodeID: "a", AttemptID: "attempt"}, Target: execution.Target{ProjectID: project.ID}, ExecutorID: "opencode:fixture", Prompt: execution.TextPrompt("hello")})
				if err != nil {
					t.Fatal(err)
				}
				stream, err := f.service.Subscribe(f.ctx, f.scope, ref.SessionID, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				done := make(chan error, 1)
				go func() {
					for {
						event, err := stream.Next(f.ctx)
						if err != nil {
							done <- err
							return
						}
						if event.Kind == execution.EventTurnEnded && event.Ref == ref {
							if event.Result == nil || event.Result.Answer != "backfilled answer" {
								done <- errors.New("observer lost the real result")
							} else {
								done <- nil
							}
							return
						}
					}
				}()
				settled = func() bool {
					select {
					case err := <-done:
						if err != nil {
							t.Fatal(err)
						}
						return true
					default:
						return false
					}
				}
			case "workflow":
				host, err := workflowhost.NewHost(workflowhost.Config{Store: f.db, Execution: f.service, Catalog: f.service})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(host.Close)
				w, _, err := f.db.CreateWorkflow(f.ctx, f.scope.TenantID, "Outage", outageGraph)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.PublishWorkflow(f.ctx, f.scope.TenantID, w.ID, "v1", nil); err != nil {
					t.Fatal(err)
				}
				run, err := f.db.CreateWorkflowRun(f.ctx, f.scope.TenantID, w.ID, "", store.WorkflowRunInput{ProjectID: project.ID})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := host.StartRun(f.ctx, server.WorkflowRunScope{TenantID: f.scope.TenantID, UserID: f.scope.UserID}, run.ID); err != nil {
					t.Fatal(err)
				}
				ready = func() bool {
					nodes, err := f.db.WorkflowEngineNodeRuns(f.ctx, run.ID)
					if err != nil {
						t.Fatal(err)
					}
					for _, n := range nodes {
						if n.NodeID == "a" && n.SessionID != nil {
							return true
						}
					}
					return false
				}
				settled = func() bool {
					r, err := f.db.WorkflowRun(f.ctx, f.scope.TenantID, run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if r.Status == store.WorkflowStatusFailed {
						t.Fatal("temporary outage was misclassified as a lost Agent")
					}
					return r.Status == store.WorkflowStatusSucceeded && r.Output != nil && *r.Output == `{"answer":"backfilled answer"}`
				}
			case "task":
				host := taskrun.New(f.db, f.service, f.service, nil)
				t.Cleanup(host.Close)
				task, err := f.db.CreateTask(f.ctx, f.scope.TenantID, f.scope.UserID, store.TaskInput{Title: "Outage", ProjectID: project.ID, Assignee: &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: f.scope.UserID}})
				if err != nil {
					t.Fatal(err)
				}
				run, err := host.Start(f.ctx, f.scope, task.ID, "attempt", taskrun.StartInput{ExecutorID: "opencode:fixture"})
				if err != nil {
					t.Fatal(err)
				}
				settled = func() bool {
					r, err := f.db.TaskRun(f.ctx, f.scope.TenantID, task.ID, run.ID)
					if err != nil {
						t.Fatal(err)
					}
					task, err := f.db.Task(f.ctx, f.scope.TenantID, task.ID)
					if err != nil {
						t.Fatal(err)
					}
					return r.LastResult == "completed" && r.State == store.TaskRunIdle && task.Status == store.TaskReview
				}
			}
			var turn execution.Turn
			select {
			case turn = <-agent.started:
			case <-f.ctx.Done():
				t.Fatal(f.ctx.Err())
			}
			if ready != nil {
				outageAwait(t, f.ctx, ready)
			}
			close(agent.finish)
			select {
			case <-writeFailed:
			case <-f.ctx.Done():
				t.Fatal(f.ctx.Err())
			}
			if _, err := f.service.ReadAttempt(f.ctx, f.scope, turn.Caller); !errors.Is(err, execution.ErrUnavailable) {
				t.Fatalf("reads did not fail alongside writes: %v", err)
			}
			outageAwait(t, f.ctx, func() bool {
				f.service.mu.Lock()
				defer f.service.mu.Unlock()
				_, pending := f.service.unsaved[turn.Ref.SessionID]
				return pending
			})
			// Observe a real failed cursor/event query from the waiting caller,
			// rather than merely assuming it ran during a timed outage.
			select {
			case <-readFailed:
			case <-f.ctx.Done():
				t.Fatal("the observer never attempted a read during the outage")
			}
			cut.restore()
			outageAwait(t, f.ctx, settled)
		})
	}
}

func outageAwait(t *testing.T, ctx context.Context, ready func() bool) {
	t.Helper()
	for !ready() {
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
