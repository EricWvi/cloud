package executionwait

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
)

func TestReadRecoversWithoutReplayingWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		err := (Policy{}).Read(t.Context(), func(ctx context.Context) error {
			calls++
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("query has no deadline")
			}
			if calls < 4 {
				return execution.ErrUnavailable
			}
			return nil
		})
		if err != nil || calls != 4 {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	})
}

func TestReadBudgetBoundsBlockedQueries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		calls := 0
		p := Policy{Budget: time.Second, QueryTimeout: 200 * time.Millisecond, Interval: 10 * time.Millisecond}
		err := p.Read(t.Context(), func(ctx context.Context) error {
			calls++
			<-ctx.Done()
			return ctx.Err()
		})
		if !errors.Is(err, ErrExhausted) || time.Since(start) != time.Second || calls < 2 {
			t.Fatalf("elapsed=%s calls=%d err=%v", time.Since(start), calls, err)
		}
	})
}

func TestReadReturnsDefiniteErrorsImmediately(t *testing.T) {
	for _, want := range []error{execution.ErrNotFound, execution.ErrInvalidRequest, execution.ErrConflict} {
		calls := 0
		err := (Policy{}).Read(t.Context(), func(context.Context) error { calls++; return want })
		if !errors.Is(err, want) || calls != 1 {
			t.Fatalf("want=%v calls=%d err=%v", want, calls, err)
		}
	}
}

func TestReadAndPendingStoreRespectHostCancellation(t *testing.T) {
	for _, storing := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "store"}[storing], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				calls := 0
				op := func(context.Context) error {
					calls++
					cancel()
					return execution.ErrUnavailable
				}
				var err error
				if storing {
					err = (Policy{}).Store(ctx, op)
				} else {
					err = (Policy{}).Read(ctx, op)
				}
				if !errors.Is(err, context.Canceled) || calls != 1 {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
			})
		})
	}
}

func TestStoreRetainsOutcomePastReadBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := Policy{Budget: time.Second, Interval: time.Second}
		calls := 0
		err := p.Store(t.Context(), func(context.Context) error {
			calls++
			if calls < 4 {
				return &pgconn.PgError{Code: "57P01"}
			}
			return nil
		})
		if err != nil || calls != 4 {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	})
}

func TestStoreDoesNotRetryInvalidData(t *testing.T) {
	for _, want := range []error{errors.New("bad payload"), &pgconn.PgError{Code: "23514"}, execution.ErrNotFound} {
		calls := 0
		err := (Policy{}).Store(t.Context(), func(context.Context) error { calls++; return want })
		if !errors.Is(err, want) || calls != 1 {
			t.Fatalf("want=%v calls=%d err=%v", want, calls, err)
		}
	}
}
