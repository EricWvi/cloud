// Package executionwait keeps observers alive through a bounded storage outage.
// It never starts, replays or cancels an execution turn.
package executionwait

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
)

// ErrExhausted distinguishes an exhausted read budget from an execution failure.
// It deliberately carries no database or transport detail.
var ErrExhausted = errors.Join(execution.ErrUnavailable, errors.New("execution result read retry exhausted"))

// Policy bounds one uninterrupted outage, not the duration of a healthy turn.
// Zero values select a one-minute budget, 250ms polls and five-second queries.
type Policy struct {
	Budget       time.Duration
	Interval     time.Duration
	QueryTimeout time.Duration
}

func (p Policy) defaults() Policy {
	if p.Budget <= 0 {
		p.Budget = time.Minute
	}
	if p.Interval <= 0 {
		p.Interval = 250 * time.Millisecond
	}
	if p.QueryTimeout <= 0 {
		p.QueryTimeout = 5 * time.Second
	}
	return p
}

// Transient classifies public execution reads. Hidden resources, invalid cursors
// and other definite refusals must not be retried as database outages.
func Transient(err error) bool {
	return errors.Is(err, execution.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded)
}

// Read retries a read, including an as-yet-unwritten result represented by
// ErrUnavailable. Every query shares the same budget and respects cancellation.
func (p Policy) Read(ctx context.Context, read func(context.Context) error) error {
	p = p.defaults()
	budget, cancel := context.WithTimeout(ctx, p.Budget)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if budget.Err() != nil {
			return ErrExhausted
		}
		query, stop := context.WithTimeout(budget, p.QueryTimeout)
		err := read(query)
		stop()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			return nil
		}
		if budget.Err() != nil {
			return ErrExhausted
		}
		if !Transient(err) {
			return err
		}
		if err := pause(budget, p.Interval); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return ErrExhausted
		}
	}
}

// Store retains an already-decided business outcome until storage recovers or
// its owning host closes. The read decision is never retried here: once its
// budget expires, a late execution result cannot turn that failure into success.
// Permanent domain errors return immediately. Callers supply that classification
// because persistence errors are not the execution port's public errors.
func (p Policy) Store(ctx context.Context, write func(context.Context) error, permanent func(error) bool) error {
	p = p.defaults()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		query, stop := context.WithTimeout(ctx, p.QueryTimeout)
		err := write(query)
		stop()
		if err == nil || permanent(err) || !storageTransient(err) {
			return err
		}
		if err := pause(ctx, p.Interval); err != nil {
			return err
		}
	}
}

func storageTransient(err error) bool {
	if Transient(err) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || pgconn.SafeToRetry(err) {
		return true
	}
	var network net.Error
	if errors.As(err, &network) {
		return true
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return strings.HasPrefix(pg.Code, "08") || strings.HasPrefix(pg.Code, "53") ||
			pg.Code == "57P01" || pg.Code == "57P02" || pg.Code == "57P03" ||
			pg.Code == "40001" || pg.Code == "40P01"
	}
	return false
}

func pause(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
