package executionruntime

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/executionwait"
)

// Subscribe reads directly from the durable cursor. A short poll avoids a replay/live handoff
// race without another in-memory event log; closing this observer never cancels a worker.
func (s *Service) Subscribe(ctx context.Context, scope execution.Scope, sid string, after uint64) (execution.Stream, error) {
	latest, err := s.db.ReadExecutionCursor(ctx, scope, sid)
	if err != nil {
		if s.observerReadFailed != nil {
			s.observerReadFailed()
		}
		return nil, publicError(err)
	}
	if after > latest {
		return nil, execution.ErrInvalidRequest
	}
	owned, cancel := context.WithCancel(ctx)
	return &stream{s: s, scope: scope, sid: sid, cursor: after, ctx: owned, cancel: cancel}, nil
}

type stream struct {
	s      *Service
	scope  execution.Scope
	sid    string
	cursor uint64
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
	retry  executionwait.Policy
}

func (st *stream) Close() error { st.once.Do(st.cancel); return nil }

// asNextError prefers the query context's own cancellation over the error a cancelled
// call returned. Closing the observer (or the caller cancelling) mid-query surfaces as
// context.Canceled rather than a wrapped database error that publicError would classify
// as execution unavailable.
func (st *stream) asNextError(query context.Context, err error) error {
	if qerr := query.Err(); qerr != nil {
		return qerr
	}
	if st.s.observerReadFailed != nil {
		st.s.observerReadFailed()
	}
	return publicError(err)
}

func (st *stream) Next(ctx context.Context) (execution.Event, error) {
	query, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(st.ctx, cancel)
	defer stop()
	for {
		if err := query.Err(); err != nil {
			return execution.Event{}, err
		}
		var event execution.Event
		var available bool
		err := st.retry.Read(query, func(read context.Context) error {
			var err error
			event, available, err = st.read(read)
			return err
		})
		if err != nil {
			return execution.Event{}, err
		}
		if available {
			st.cursor = event.Sequence
			return event, nil
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-query.Done():
			timer.Stop()
			return execution.Event{}, query.Err()
		case <-timer.C:
		}
	}
}

// read keeps the event/session/terminal-recheck sequence in one retry budget.
// Only Next advances the cursor, after the complete read has succeeded.
func (st *stream) read(ctx context.Context) (execution.Event, bool, error) {
	events, err := st.s.db.ReadExecutionEvents(ctx, st.scope, st.sid, st.cursor, 1)
	if err != nil {
		return execution.Event{}, false, st.asNextError(ctx, err)
	}
	if len(events) > 0 {
		return events[0], true, nil
	}
	session, err := st.s.ReadSession(ctx, st.scope, st.sid)
	if err != nil {
		return execution.Event{}, false, st.asNextError(ctx, err)
	}
	if session.EndedAt == nil {
		return execution.Event{}, false, nil
	}
	// End may commit between the empty event query and this session read. Recheck
	// after observing EndedAt so the terminal event cannot be skipped as EOF.
	events, err = st.s.db.ReadExecutionEvents(ctx, st.scope, st.sid, st.cursor, 1)
	if err != nil {
		return execution.Event{}, false, st.asNextError(ctx, err)
	}
	if len(events) > 0 {
		return events[0], true, nil
	}
	return execution.Event{}, false, io.EOF
}
