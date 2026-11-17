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
		_, err := h.recordedResult(execution.Scope{}, execution.Caller{}, io.EOF)
		if !errors.Is(err, executionwait.ErrExhausted) || publicReason(err) != "execution result could not be read before the retry deadline; resume the run to retry" {
			t.Fatalf("missing retry deadline diagnostic: %v (%s)", err, publicReason(err))
		}
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
