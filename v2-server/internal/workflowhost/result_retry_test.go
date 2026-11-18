package workflowhost

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/execution/executiontest"
	"github.com/wanglongan587/cloud/v2-server/internal/executionwait"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

type unavailableStream struct{}

func (unavailableStream) Next(context.Context) (execution.Event, error) {
	return execution.Event{}, execution.ErrUnavailable
}
func (unavailableStream) Close() error { return nil }

func TestAwaitTurnReadsBackfilledResultAfterSubscribeOrStreamFailure(t *testing.T) {
	for _, subscribing := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream", true: "subscribe"}[subscribing], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ref := execution.Ref{SessionID: "session", TurnID: "turn"}
				caller := execution.Caller{Kind: execution.CallerWorkflow, ID: "run", NodeID: "node", AttemptID: "attempt"}
				scope := execution.Scope{TenantID: "tenant", UserID: "owner"}
				want := completed("backfilled answer")
				reads := 0
				port := &executiontest.Stub{
					SubscribeFunc: func(context.Context, execution.Scope, string, uint64) (execution.Stream, error) {
						if subscribing {
							return nil, execution.ErrUnavailable
						}
						return unavailableStream{}, nil
					},
					ReadAttemptFunc: func(_ context.Context, gotScope execution.Scope, gotCaller execution.Caller) (execution.Turn, error) {
						if gotScope != scope || !reflect.DeepEqual(gotCaller, caller) {
							t.Errorf("read changed ownership: %+v %+v", gotScope, gotCaller)
						}
						reads++
						switch reads {
						case 1, 2:
							return execution.Turn{}, execution.ErrUnavailable
						case 3:
							return execution.Turn{Ref: ref}, nil // reads recover before the write
						default:
							return execution.Turn{Ref: ref, Result: &want}, nil
						}
					},
				}
				h := &Host{ctx: t.Context(), exec: port}
				got, err := h.awaitTurn(scope, ref, caller)
				if err != nil || !reflect.DeepEqual(got, want) || reads != 4 {
					t.Fatalf("result=%+v reads=%d err=%v", got, reads, err)
				}
			})
		})
	}
}

func TestWorkflowResultReadFailureIsBoundedAndResumable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &Host{ctx: t.Context(), resultRetry: executionwait.Policy{Budget: time.Second}, exec: &executiontest.Stub{
			ReadAttemptFunc: func(context.Context, execution.Scope, execution.Caller) (execution.Turn, error) {
				return execution.Turn{}, execution.ErrUnavailable
			},
		}}
		_, _, _, err := h.recoverTurn(execution.Scope{}, execution.Ref{}, execution.Caller{}, 0, io.EOF)
		if !errors.Is(err, executionwait.ErrExhausted) || publicReason(err) != "execution result could not be read before the retry deadline" {
			t.Fatalf("missing retry deadline diagnostic: %v (%s)", err, publicReason(err))
		}
	})
}

type stubStream struct {
	next  func(context.Context) (execution.Event, error)
	close func() error
}

func (s stubStream) Next(ctx context.Context) (execution.Event, error) { return s.next(ctx) }
func (s stubStream) Close() error {
	if s.close != nil {
		return s.close()
	}
	return nil
}

func TestWorkflowAwaitTurnHealthyLongTurnPastBudgetAfterSubscribeOrStreamFailure(t *testing.T) {
	for _, subscribing := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream", true: "subscribe"}[subscribing], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ref := execution.Ref{SessionID: "session", TurnID: "turn"}
				caller := execution.Caller{Kind: execution.CallerWorkflow, ID: "run", NodeID: "node", AttemptID: "attempt"}
				scope := execution.Scope{TenantID: "tenant", UserID: "owner"}
				want := completed("long healthy answer")
				subscribes := 0
				var cursors []uint64
				start := time.Now()
				port := &executiontest.Stub{
					SubscribeFunc: func(_ context.Context, _ execution.Scope, _ string, after uint64) (execution.Stream, error) {
						subscribes++
						cursors = append(cursors, after)
						if subscribing && subscribes == 1 {
							return nil, execution.ErrUnavailable
						}
						if !subscribing && subscribes == 1 {
							sent := false
							return stubStream{next: func(context.Context) (execution.Event, error) {
								if !sent {
									sent = true
									return execution.Event{Sequence: 7, Ref: ref, Kind: execution.EventTextDelta, TextDelta: "working"}, nil
								}
								return execution.Event{}, execution.ErrUnavailable
							}}, nil
						}
						return stubStream{next: func(ctx context.Context) (execution.Event, error) {
							select {
							case <-time.After(2 * time.Second): // longer than the 1s read budget
								return execution.Event{Sequence: 8, Ref: ref, Kind: execution.EventTurnEnded, Result: &want}, nil
							case <-ctx.Done():
								return execution.Event{}, ctx.Err()
							}
						}}, nil
					},
					ReadAttemptFunc: func(context.Context, execution.Scope, execution.Caller) (execution.Turn, error) {
						return execution.Turn{Ref: ref}, nil // DB healthy; turn is still running
					},
				}
				h := &Host{ctx: t.Context(), resultRetry: executionwait.Policy{Budget: time.Second, Interval: 50 * time.Millisecond}, exec: port}
				got, err := h.awaitTurn(scope, ref, caller)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("result=%+v err=%v", got, err)
				}
				if time.Since(start) < 2*time.Second || subscribes != 2 {
					t.Fatalf("elapsed=%s subscribes=%d cursors=%v", time.Since(start), subscribes, cursors)
				}
				if !subscribing && cursors[1] != 7 {
					t.Fatalf("re-subscribe cursor=%d, want 7", cursors[1])
				}
			})
		})
	}
}

type retryEngineRepo struct {
	runContext func(context.Context, string) (workflow.RunContext, error)
	nodeRuns   func(context.Context, string) ([]workflow.NodeRun, error)
	complete   func(context.Context, string, string, *string, map[string]any, func([]byte, workflow.NodeRun) ([]byte, error)) (bool, error)
	fail       func(context.Context, string, string, string, *string, func(int, []byte) map[string]any) (bool, string, error)
	finish     func(context.Context, string, *string) error
}

func (r retryEngineRepo) RunContext(ctx context.Context, runID string) (workflow.RunContext, error) {
	return r.runContext(ctx, runID)
}
func (r retryEngineRepo) NodeRuns(ctx context.Context, runID string) ([]workflow.NodeRun, error) {
	return r.nodeRuns(ctx, runID)
}
func (r retryEngineRepo) StartRun(context.Context, string, workflow.NodeStart, func([]byte) ([]byte, error)) (bool, error) {
	return false, nil
}
func (r retryEngineRepo) StartNodes(context.Context, string, []workflow.NodeStart) ([]workflow.NodeRun, error) {
	return nil, nil
}
func (r retryEngineRepo) CompleteNode(ctx context.Context, runID, nodeRunID string, output *string, keys map[string]any, fold func([]byte, workflow.NodeRun) ([]byte, error)) (bool, error) {
	return r.complete(ctx, runID, nodeRunID, output, keys, fold)
}
func (r retryEngineRepo) FailNode(ctx context.Context, runID, nodeRunID, message string, output *string, detail func(int, []byte) map[string]any) (bool, string, error) {
	return r.fail(ctx, runID, nodeRunID, message, output, detail)
}
func (r retryEngineRepo) AbandonNode(context.Context, string, string) (bool, error) {
	return false, nil
}
func (r retryEngineRepo) FinishRun(ctx context.Context, runID string, output *string) error {
	return r.finish(ctx, runID, output)
}
func (r retryEngineRepo) UpdateRunInput(context.Context, string, *string, func([]byte) ([]byte, error)) error {
	return nil
}

func TestWorkflowHostFailAndCompleteRetryTransientOutages(t *testing.T) {
	const simpleGraph = `{"nodes":[
		{"id":"start","data":{"kind":"start"}},
		{"id":"a","data":{"kind":"agent","agentConfig":{"prompt":"go"}}},
		{"id":"out","data":{"kind":"output","outputs":[{"name":"answer","variableSelector":["a","output"]}]}}
	],"edges":[{"source":"start","target":"a"},{"source":"a","target":"out"}]}`

	t.Run("fail retries transient outage", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls := 0
			repo := retryEngineRepo{
				runContext: func(context.Context, string) (workflow.RunContext, error) {
					return workflow.RunContext{RunID: "run", Status: workflow.StatusFailed, Graph: simpleGraph}, nil
				},
				fail: func(context.Context, string, string, string, *string, func(int, []byte) map[string]any) (bool, string, error) {
					calls++
					if calls < 3 {
						return false, "", execution.ErrUnavailable
					}
					return false, "", nil
				},
			}
			h := &Host{
				ctx:         t.Context(),
				hub:         NewHub(),
				log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
				resultRetry: executionwait.Policy{Interval: 10 * time.Millisecond},
				engine:      workflow.NewEngine(repo, nil, nil, time.Now),
			}
			h.fail(workflow.AgentDispatch{RunID: "run", NodeRunID: "nr"}, workflow.Failure{Kind: workflow.FailureSession, Message: "lost"})
			if calls != 3 {
				t.Fatalf("fail calls=%d, want 3", calls)
			}
		})
	})

	t.Run("complete resumes scheduling after commit and read outage", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			completeCalls, nodeRunsCalls, finishCalls := 0, 0, 0
			committed := false
			repo := retryEngineRepo{
				runContext: func(context.Context, string) (workflow.RunContext, error) {
					return workflow.RunContext{RunID: "run", Status: workflow.StatusRunning, Graph: simpleGraph}, nil
				},
				complete: func(context.Context, string, string, *string, map[string]any, func([]byte, workflow.NodeRun) ([]byte, error)) (bool, error) {
					completeCalls++
					if !committed {
						committed = true
						// Committed the completion, then scheduling failed to read RunContext/NodeRuns.
						return false, execution.ErrUnavailable
					}
					// Duplicate callback is a no-op; Host.complete must call engine.Resume.
					return false, nil
				},
				nodeRuns: func(context.Context, string) ([]workflow.NodeRun, error) {
					nodeRunsCalls++
					return []workflow.NodeRun{
						{ID: "nr-start", NodeID: "start", NodeType: "start", Status: workflow.StatusSucceeded},
						{ID: "nr-a", NodeID: "a", NodeType: "agent", Status: workflow.StatusSucceeded, Output: str("done")},
						{ID: "nr-out", NodeID: "out", NodeType: "output", Status: workflow.StatusSucceeded, Output: str(`{"answer":"done"}`)},
					}, nil
				},
				finish: func(context.Context, string, *string) error {
					finishCalls++
					return nil
				},
			}
			h := &Host{
				ctx:         t.Context(),
				hub:         NewHub(),
				log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
				resultRetry: executionwait.Policy{Interval: 10 * time.Millisecond},
				engine:      workflow.NewEngine(repo, nil, nil, time.Now),
			}
			h.complete(workflow.AgentDispatch{RunID: "run", NodeRunID: "nr-a"}, workflow.Completion{Output: str("done")})
			if completeCalls != 2 || nodeRunsCalls == 0 || finishCalls != 1 {
				t.Fatalf("completeCalls=%d nodeRunsCalls=%d finishCalls=%d", completeCalls, nodeRunsCalls, finishCalls)
			}
		})
	})
}

func TestInteractiveParkRetriesOutageAndAmbiguousCommit(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(map[bool]string{false: "transient", true: "ambiguous"}[ambiguous], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rowStatus := store.WorkflowStatusRunning
				calls := 0
				var transitions []string
				h := &Host{
					ctx:         t.Context(),
					hub:         NewHub(),
					log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
					resultRetry: executionwait.Policy{Interval: 10 * time.Millisecond},
					moveNode: func(_ context.Context, runID, nodeRunID, from, to string, merge func([]byte) ([]byte, error)) (bool, error) {
						if runID != "run" || nodeRunID != "node-run" {
							t.Fatalf("unexpected ids: %s %s", runID, nodeRunID)
						}
						calls++
						transitions = append(transitions, from+"->"+to)
						if calls == 1 {
							if ambiguous {
								rowStatus = store.WorkflowStatusPending // committed before connection dropped
							}
							return false, execution.ErrUnavailable
						}
						if rowStatus != from {
							return false, nil
						}
						if _, err := merge([]byte("{}")); err != nil {
							t.Fatal(err)
						}
						rowStatus = to
						return true, nil
					},
				}
				st := &runState{}
				n := &interactiveNode{
					d:     workflow.AgentDispatch{RunID: "run", NodeRunID: "node-run"},
					phase: phaseTurn,
					task:  "follow up",
				}
				h.park(st, n)
				if n.phase != phaseParked || rowStatus != store.WorkflowStatusPending {
					t.Fatalf("phase=%v rowStatus=%s calls=%d transitions=%v", n.phase, rowStatus, calls, transitions)
				}
				wantCalls := 2
				if ambiguous {
					wantCalls = 3
				}
				if calls != wantCalls {
					t.Fatalf("calls=%d want=%d transitions=%v", calls, wantCalls, transitions)
				}
			})
		})
	}
}
