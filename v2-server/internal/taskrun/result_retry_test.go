package taskrun

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/execution/executiontest"
	"github.com/wanglongan587/cloud/v2-server/internal/executionwait"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

type resultStore struct {
	Store
	settle      func(context.Context, string, string, string, string) (bool, error)
	move        func(context.Context, string, string, string, string) (store.Task, error)
	read        func(context.Context, string, string, string) (store.TaskRun, error)
	task        func(context.Context, string, string) (store.Task, error)
	markEnding  func(context.Context, string, string) error
	clearEnding func(context.Context, string) error
	endRun      func(context.Context, string, string) error
}

func (s resultStore) Task(ctx context.Context, tenant, id string) (store.Task, error) {
	return s.task(ctx, tenant, id)
}
func (s resultStore) MarkTaskRunEnding(ctx context.Context, runID, reason string) error {
	return s.markEnding(ctx, runID, reason)
}
func (s resultStore) ClearTaskRunEnding(ctx context.Context, runID string) error {
	return s.clearEnding(ctx, runID)
}
func (s resultStore) EndTaskRun(ctx context.Context, runID, reason string) error {
	return s.endRun(ctx, runID, reason)
}

func (s resultStore) TaskRun(ctx context.Context, tenant, task, run string) (store.TaskRun, error) {
	return s.read(ctx, tenant, task, run)
}

func (s resultStore) SettleTaskTurn(ctx context.Context, run, turn, result, code string) (bool, error) {
	return s.settle(ctx, run, turn, result, code)
}
func (s resultStore) TransitionTaskStatus(ctx context.Context, tenant, task, from, to string) (store.Task, error) {
	return s.move(ctx, tenant, task, from, to)
}

func TestTaskObserverSettlesBackfilledResultAndRetainsTimedOutFailure(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "backfill", true: "timeout"}[timeout], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ref := execution.Ref{SessionID: "session", TurnID: "turn"}
				run := store.TaskRun{ID: "run", TaskID: "task", TenantID: "tenant", StartedBy: "owner"}
				scope := Owner(run)
				reads, writes, moves := 0, 0, 0
				start := time.Now()
				var gotResult, gotCode string
				port := &executiontest.Stub{
					SubscribeFunc: func(context.Context, execution.Scope, string, uint64) (execution.Stream, error) {
						return nil, execution.ErrUnavailable
					},
					ReadTurnsFunc: func(_ context.Context, gotScope execution.Scope, session, cursor string, _ int) (execution.TurnPage, error) {
						if gotScope != scope || session != ref.SessionID {
							t.Errorf("changed read owner: %+v %s", gotScope, session)
						}
						reads++
						if timeout || reads < 3 {
							return execution.TurnPage{}, execution.ErrUnavailable
						}
						if cursor == "" {
							return execution.TurnPage{NextCursor: "second"}, nil
						}
						return execution.TurnPage{Items: []execution.Turn{{Ref: ref, Result: &execution.Result{Status: execution.StatusCompleted}}}}, nil
					},
				}
				db := resultStore{
					settle: func(_ context.Context, id, turn, result, code string) (bool, error) {
						writes++
						if id != run.ID || turn != ref.TurnID {
							t.Errorf("settled wrong turn: %s %s", id, turn)
						}
						// The same outage also prevents writing the observer's timeout.
						if timeout && time.Since(start) < 2*time.Second {
							return false, execution.ErrUnavailable
						}
						gotResult, gotCode = result, code
						return true, nil
					},
					read: func(context.Context, string, string, string) (store.TaskRun, error) {
						return store.TaskRun{
							ID: run.ID, TenantID: run.TenantID, TaskID: run.TaskID, StartedBy: run.StartedBy,
							SessionID: ref.SessionID, LastTurnID: ref.TurnID, LastResult: gotResult, FailureCode: gotCode,
						}, nil
					},
					move: func(_ context.Context, tenant, task, from, to string) (store.Task, error) {
						moves++
						if tenant != run.TenantID || task != run.TaskID || from != store.TaskRunning || to != store.TaskReview {
							t.Errorf("wrong status move: %s %s %s %s", tenant, task, from, to)
						}
						if moves == 1 {
							return store.Task{}, execution.ErrUnavailable
						}
						return store.Task{}, nil
					},
				}
				h := New(db, port, port, slog.New(slog.NewTextHandler(io.Discard, nil)))
				defer h.Close()
				h.resultRetry = executionwait.Policy{Budget: time.Second, Interval: 100 * time.Millisecond}
				h.watch(scope, run, ref)
				h.workers.Wait()
				want := string(execution.StatusCompleted)
				code := ""
				if timeout {
					want, code = string(execution.StatusFailed), "execution_result_unavailable"
				}
				if gotResult != want || gotCode != code || moves != 2 || reads < 2 {
					t.Fatalf("result=%s code=%s reads=%d writes=%d moves=%d", gotResult, gotCode, reads, writes, moves)
				}
			})
		})
	}
}

func TestTaskObserverCloseInterruptsPendingReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{})
		port := &executiontest.Stub{
			SubscribeFunc: func(context.Context, execution.Scope, string, uint64) (execution.Stream, error) {
				return nil, execution.ErrUnavailable
			},
			ReadTurnsFunc: func(ctx context.Context, _ execution.Scope, _, _ string, _ int) (execution.TurnPage, error) {
				close(entered)
				<-ctx.Done()
				return execution.TurnPage{}, ctx.Err()
			},
		}
		h := New(nil, port, port, nil)
		h.watch(execution.Scope{}, store.TaskRun{}, execution.Ref{})
		<-entered
		h.Close() // must join the observer without settling a shutdown as failure
	})
}

func TestTaskObserverCloseInterruptsPendingWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{})
		ref := execution.Ref{TurnID: "turn"}
		port := &executiontest.Stub{
			SubscribeFunc: func(context.Context, execution.Scope, string, uint64) (execution.Stream, error) {
				return nil, execution.ErrUnavailable
			},
			ReadTurnsFunc: func(context.Context, execution.Scope, string, string, int) (execution.TurnPage, error) {
				return execution.TurnPage{Items: []execution.Turn{{Ref: ref, Result: &execution.Result{Status: execution.StatusCompleted}}}}, nil
			},
		}
		db := resultStore{settle: func(ctx context.Context, _, _, _, _ string) (bool, error) {
			close(entered)
			<-ctx.Done()
			return false, ctx.Err()
		}}
		h := New(db, port, port, nil)
		h.watch(execution.Scope{}, store.TaskRun{}, ref)
		<-entered
		h.Close()
	})
}

func TestTaskSettlementRecoversAmbiguousWriteWithoutMovingANewerTurn(t *testing.T) {
	for _, tc := range []struct {
		name      string
		turnID    string
		result    string
		code      string
		wantMoves int
	}{
		{name: "same turn", turnID: "turn", result: "completed", wantMoves: 1},
		{name: "newer turn", turnID: "newer", result: "completed", wantMoves: 0},
		{name: "changed result", turnID: "turn", result: "cancelled", wantMoves: 0},
		{name: "changed failure code", turnID: "turn", result: "completed", code: "other", wantMoves: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				writes, moves := 0, 0
				run := store.TaskRun{ID: "run", TenantID: "tenant", TaskID: "task"}
				db := resultStore{
					settle: func(context.Context, string, string, string, string) (bool, error) {
						writes++
						if writes == 1 {
							return false, execution.ErrUnavailable // committed, but reply was lost
						}
						return false, nil
					},
					read: func(context.Context, string, string, string) (store.TaskRun, error) {
						r := run
						r.LastTurnID, r.LastResult, r.FailureCode = tc.turnID, tc.result, tc.code
						return r, nil
					},
					move: func(context.Context, string, string, string, string) (store.Task, error) {
						moves++
						return store.Task{}, nil
					},
				}
				h := New(db, nil, nil, nil)
				defer h.Close()
				h.settle(t.Context(), run, "turn", execution.Result{Status: execution.StatusCompleted}, false)
				if writes != 2 || moves != tc.wantMoves {
					t.Fatalf("writes=%d moves=%d want moves=%d", writes, moves, tc.wantMoves)
				}
			})
		})
	}
}

type stubTaskStream struct {
	next func(context.Context) (execution.Event, error)
}

func (s stubTaskStream) Next(ctx context.Context) (execution.Event, error) { return s.next(ctx) }
func (s stubTaskStream) Close() error                                      { return nil }

func TestTaskAwaitHealthyLongTurnPastBudgetAfterSubscribeOrStreamFailure(t *testing.T) {
	for _, subscribing := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream", true: "subscribe"}[subscribing], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ref := execution.Ref{SessionID: "session", TurnID: "turn"}
				run := store.TaskRun{ID: "run", TaskID: "task", TenantID: "tenant", StartedBy: "owner"}
				scope := Owner(run)
				subscribes := 0
				var cursors []uint64
				var gotResult, gotCode string
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
							return stubTaskStream{next: func(context.Context) (execution.Event, error) {
								if !sent {
									sent = true
									return execution.Event{Sequence: 5, Ref: ref, Kind: execution.EventTextDelta, TextDelta: "working"}, nil
								}
								return execution.Event{}, execution.ErrUnavailable
							}}, nil
						}
						return stubTaskStream{next: func(ctx context.Context) (execution.Event, error) {
							select {
							case <-time.After(2 * time.Second): // exceeds the 1s read budget
								return execution.Event{Sequence: 6, Ref: ref, Kind: execution.EventTurnEnded, Result: &execution.Result{Status: execution.StatusCompleted}}, nil
							case <-ctx.Done():
								return execution.Event{}, ctx.Err()
							}
						}}, nil
					},
					ReadTurnsFunc: func(context.Context, execution.Scope, string, string, int) (execution.TurnPage, error) {
						return execution.TurnPage{Items: []execution.Turn{{Ref: ref}}}, nil // healthy still-running turn
					},
				}
				db := resultStore{
					settle: func(_ context.Context, _, _, result, code string) (bool, error) {
						gotResult, gotCode = result, code
						return true, nil
					},
					move: func(context.Context, string, string, string, string) (store.Task, error) {
						return store.Task{}, nil
					},
				}
				h := New(db, port, port, slog.New(slog.NewTextHandler(io.Discard, nil)))
				defer h.Close()
				h.resultRetry = executionwait.Policy{Budget: time.Second, Interval: 50 * time.Millisecond}
				h.watch(scope, run, ref)
				h.workers.Wait()
				if gotResult != string(execution.StatusCompleted) || gotCode != "" || time.Since(start) < 2*time.Second || subscribes != 2 {
					t.Fatalf("result=%s code=%s elapsed=%s subscribes=%d cursors=%v", gotResult, gotCode, time.Since(start), subscribes, cursors)
				}
				if !subscribing && cursors[1] != 5 {
					t.Fatalf("re-subscribe cursor=%d, want 5", cursors[1])
				}
			})
		})
	}
}

func TestTaskEndBoundsSettleAndStillEndsSessionOnOutage(t *testing.T) {
	s := storetest.Open(t)
	owner, tenant := storetest.Login(t, s, "end-owner")
	proj, err := s.CreateProject(t.Context(), execution.Scope{TenantID: tenant, UserID: owner}, store.Project{
		ID: "p-end", Name: "End", RepositoryURL: "https://example.test/repo.git", Commit: "c", MirrorPath: "/unused",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(t.Context(), tenant, owner, store.TaskInput{Title: "End test", ProjectID: proj.ID})
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.CreateTaskRun(t.Context(), store.NewTaskRun{
		TenantID: tenant, TaskID: task.ID, StartedBy: owner, StartKey: "k", ExecutorID: "opencode:fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTaskTurn(t.Context(), created.ID, "session", "turn"); err != nil {
		t.Fatal(err)
	}
	run, err := s.TaskRun(t.Context(), tenant, task.ID, created.ID)
	if err != nil || !run.TurnActive() {
		t.Fatalf("seed active turn: run=%+v err=%v", run, err)
	}

	synctest.Test(t, func(t *testing.T) {
		ended, cleared, endRunCalled, settleCalls := 0, 0, 0, 0
		var settleSawCancel bool
		port := &executiontest.Stub{
			CancelFunc: func(context.Context, execution.Scope, execution.Ref) (execution.Result, error) {
				return execution.Result{Status: execution.StatusCancelled}, nil
			},
			EndFunc: func(ctx context.Context, _ execution.Scope, sid string) error {
				if ctx.Err() != nil || sid != "session" {
					t.Fatalf("unexpected End call: ctxErr=%v sid=%s", ctx.Err(), sid)
				}
				ended++
				return nil
			},
		}
		db := resultStore{
			task:        func(context.Context, string, string) (store.Task, error) { return task, nil },
			read:        func(context.Context, string, string, string) (store.TaskRun, error) { return run, nil },
			markEnding:  func(context.Context, string, string) error { return nil },
			clearEnding: func(context.Context, string) error { cleared++; return nil },
			endRun:      func(context.Context, string, string) error { endRunCalled++; return nil },
			settle: func(ctx context.Context, _, _, _, _ string) (bool, error) {
				settleCalls++
				if ctx.Err() != nil {
					settleSawCancel = true
				}
				return false, execution.ErrUnavailable
			},
		}
		h := New(db, port, port, nil)
		defer h.Close()
		h.resultRetry = executionwait.Policy{Budget: 60 * time.Millisecond, Interval: 10 * time.Millisecond}

		reqCtx, cancelReq := context.WithCancel(t.Context())
		cancelReq() // even if caller disconnects after Cancel, bounded settle runs under WithoutCancel
		_, err := h.Stop(reqCtx, execution.Scope{TenantID: tenant, UserID: owner}, task.ID, run.ID)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Stop err=%v, want DeadlineExceeded", err)
		}
		if settleCalls < 2 || settleSawCancel || ended != 1 || cleared != 1 || endRunCalled != 0 {
			t.Fatalf("settleCalls=%d settleSawCancel=%t ended=%d cleared=%d endRunCalled=%d", settleCalls, settleSawCancel, ended, cleared, endRunCalled)
		}
	})
}

func TestTaskWatchReleasesTaskLockBetweenRetriesAndSkipsSupersededMove(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ref := execution.Ref{SessionID: "session", TurnID: "turn"}
		run := store.TaskRun{ID: "run", TaskID: "task", TenantID: "tenant", StartedBy: "owner"}
		scope := Owner(run)
		port := &executiontest.Stub{
			SubscribeFunc: func(context.Context, execution.Scope, string, uint64) (execution.Stream, error) {
				return nil, execution.ErrUnavailable
			},
			ReadTurnsFunc: func(context.Context, execution.Scope, string, string, int) (execution.TurnPage, error) {
				return execution.TurnPage{Items: []execution.Turn{{Ref: ref, Result: &execution.Result{Status: execution.StatusCompleted}}}}, nil
			},
		}
		var h *Host
		settleAttempt := 0
		moveCalls := 0
		newerStarted := false
		secondAttempt := make(chan struct{})
		releaseSecondAttempt := make(chan struct{})
		db := resultStore{
			settle: func(context.Context, string, string, string, string) (bool, error) {
				settleAttempt++
				if settleAttempt == 1 {
					return false, execution.ErrUnavailable
				}
				close(secondAttempt)
				<-releaseSecondAttempt
				return true, nil
			},
			read: func(context.Context, string, string, string) (store.TaskRun, error) {
				r := run
				r.LastTurnID = ref.TurnID
				r.LastResult = string(execution.StatusCompleted)
				if newerStarted {
					r.LastTurnID = "newer-turn"
				}
				return r, nil
			},
			move: func(context.Context, string, string, string, string) (store.Task, error) {
				moveCalls++
				if moveCalls == 1 {
					return store.Task{}, execution.ErrUnavailable
				}
				return store.Task{}, nil
			},
		}
		h = New(db, port, port, slog.New(slog.NewTextHandler(io.Discard, nil)))
		defer h.Close()
		h.resultRetry = executionwait.Policy{Interval: 100 * time.Millisecond}

		h.watch(scope, run, ref)
		synctest.Wait() // first settle failed; watch is sleeping before attempt 2

		// Task lock must be free while watch sleeps between retry attempts.
		h.lock(run.TaskID)()

		time.Sleep(100 * time.Millisecond)
		<-secondAttempt
		close(releaseSecondAttempt)
		// Wait until attempt 2 finishes SettleTaskTurn + failing TransitionTaskStatus
		// and enters the next retry sleep.
		synctest.Wait()

		// Simulate a newer turn starting while watch sleeps between attempts.
		unlock := h.lock(run.TaskID)
		newerStarted = true
		unlock()

		time.Sleep(100 * time.Millisecond)
		h.workers.Wait()
		if settleAttempt != 2 || moveCalls != 1 {
			t.Fatalf("settleAttempt=%d moveCalls=%d (expected stale move after newer turn to be skipped)", settleAttempt, moveCalls)
		}
	})
}
