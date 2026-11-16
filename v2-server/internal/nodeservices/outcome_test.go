package nodeservices

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// outcomeProbe stands for a caller that wraps the route's provider (the per-turn retry guard):
// it reads Outcome after the provider returns, through a wrapping writer as request metrics adds.
type outcomeProbe struct {
	Provider
	seen chan [2]any
}

func (p outcomeProbe) Complete(w http.ResponseWriter, r *http.Request, m Model) {
	p.Provider.Complete(unwrappingWriter{w}, r, m)
	completed, limit := Outcome(w)
	p.seen <- [2]any{completed, limit}
}

// stall holds an upstream call open until it is cancelled or the test is long over, so a server
// Close never waits on a handler the client side has already given up on.
func stall(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(3 * time.Second):
	}
}

// callRoute posts one Agent model call through the route and returns its status and body; a body
// cut off mid-stream is returned as far as it arrived.
func callRoute(t *testing.T, p Provider) (int, string) {
	t.Helper()
	s := testService(t, p)
	c := serviceClient(t, s, true)
	token, _ := issueToken(t, s, c, "execution")
	req, _ := http.NewRequest("POST", s.URL+"/runtime/openai/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func TestOutcomeReportsLimitsBeforeAndAfterTheHeaderAndOnlyCountsA2xxAsCompleted(t *testing.T) {
	cases := []struct {
		name      string
		upstream  http.HandlerFunc
		idle      time.Duration
		total     time.Duration
		status    int
		completed bool
		limit     string
	}{
		{"idle before the header", func(w http.ResponseWriter, r *http.Request) { stall(r) },
			200 * time.Millisecond, time.Minute, 502, false, CutIdle},
		{"idle after the header", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(flushWriter{w}, "data: {}\n\n")
			stall(r)
		}, 200 * time.Millisecond, time.Minute, 200, false, CutIdle},
		{"total limit on an active stream", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for {
				if _, err := io.WriteString(flushWriter{w}, "data: {}\n\n"); err != nil {
					return
				}
				select {
				case <-r.Context().Done():
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
		}, 300 * time.Millisecond, 600 * time.Millisecond, 200, false, CutMaxDuration},
		{"provider rejects", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) },
			time.Second, time.Minute, 502, false, ""},
		{"normal completion", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}, time.Second, time.Minute, 200, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			upstream := httptest.NewTLSServer(c.upstream)
			defer upstream.Close()
			p, err := NewRemoteProvider(upstream.URL, "backend-key", upstream.Client().Transport)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.SetLimits(c.idle, c.total); err != nil {
				t.Fatal(err)
			}
			probe := outcomeProbe{p, make(chan [2]any, 1)}
			status, body := callRoute(t, probe)
			if status != c.status {
				t.Fatalf("status %d body %q, want %d", status, body, c.status)
			}
			select {
			case got := <-probe.seen:
				if got[0] != c.completed || got[1] != c.limit {
					t.Fatalf("Outcome = %v %q, want %v %q", got[0], got[1], c.completed, c.limit)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("provider never returned")
			}
		})
	}
}

// eofOnCancel is an upstream stream that sends a chunk every `every` (only the first one when
// every is 0) and ends with a clean io.EOF once its request is cancelled, as net/http's transport
// may report a body cut off by cancellation.
type eofOnCancel struct {
	ctx   context.Context
	every time.Duration
	sent  bool
}

func (b *eofOnCancel) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, "data: {}\n\n"), nil
	}
	if b.every > 0 {
		select {
		case <-b.ctx.Done():
			return 0, io.EOF
		case <-time.After(b.every):
			return copy(p, "data: {}\n\n"), nil
		}
	}
	<-b.ctx.Done()
	return 0, io.EOF
}

func (b *eofOnCancel) Close() error { return nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A limit that cancels the upstream call cuts it even when the cancelled body ends with io.EOF.
func TestOutcomeCountsALimitCutAsCutWhenTheCancelledBodyEndsCleanly(t *testing.T) {
	for _, c := range []struct {
		name        string
		every       time.Duration
		idle, total time.Duration
		limit       string
	}{
		{"idle", 0, 100 * time.Millisecond, time.Minute, CutIdle},
		// Chunks 20ms apart keep the 300ms idle limit from firing; only the total limit ends it.
		{"total", 20 * time.Millisecond, 300 * time.Millisecond, 400 * time.Millisecond, CutMaxDuration},
	} {
		t.Run(c.name, func(t *testing.T) {
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
					Body: &eofOnCancel{ctx: r.Context(), every: c.every}, Request: r}, nil
			})
			p, err := NewRemoteProvider("https://upstream.test/v1", "backend-key", transport)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.SetLimits(c.idle, c.total); err != nil {
				t.Fatal(err)
			}
			probe := outcomeProbe{p, make(chan [2]any, 1)}
			status, body := callRoute(t, probe)
			if status != 200 || !strings.Contains(body, "data: {}") {
				t.Fatalf("status %d body %q, want the first chunk under 200", status, body)
			}
			select {
			case got := <-probe.seen:
				if got[0] != false || got[1] != c.limit {
					t.Fatalf("Outcome = %v %q, want false %q: a limit cut was reported as a completed call", got[0], got[1], c.limit)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("provider never returned")
			}
		})
	}
}

// Revoking the grant cancels a running call; the route still aborts it when the cancelled body
// ends with io.EOF, so the Agent never reads a revoked stream as one that ended normally.
func TestRevocationAbortsTheRouteWhenTheCancelledBodyEndsCleanly(t *testing.T) {
	p, err := NewRemoteProvider("https://upstream.test/v1", "backend-key", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
			Body: &eofOnCancel{ctx: r.Context()}, Request: r}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	s := testService(t, p)
	c := serviceClient(t, s, true)
	token, _ := issueToken(t, s, c, "execution")
	req, _ := http.NewRequest("POST", s.URL+"/runtime/openai/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first := make([]byte, 4)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatal(err)
	}
	s.End("execution")
	if data, err := io.ReadAll(resp.Body); err == nil {
		t.Fatalf("revoked stream ended normally with %q", data)
	}
}
