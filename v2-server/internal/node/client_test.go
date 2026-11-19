//go:build linux

package node

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func socketDirectory(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "v-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func fixture(t *testing.T, serve func(net.Conn) error) (Options, <-chan error) {
	t.Helper()
	path := filepath.Join(socketDirectory(t), "control.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		done <- serve(conn)
	}()
	return Options{SocketPath: path, ControllerID: "controller", ExpectedNodeID: "node", HeartbeatInterval: 20 * time.Millisecond, FrameTimeout: time.Second, RequiredCapabilities: []string{"agent_session"}}, done
}

func greeting(conn net.Conn, nodeID string) error {
	m, err := readFrame(conn)
	if err != nil {
		return err
	}
	if m.Type != "hello" {
		return errors.New("missing hello")
	}
	payload, _ := json.Marshal(map[string]any{"selected_version": 1, "node": Identity{NodeID: nodeID, IncarnationID: "incarnation"}, "capabilities": []string{"agent_session"}})
	frame, _ := encode(Message{Type: "hello_accepted", Version: 1, Payload: payload})
	return writeAll(conn, frame)
}

func TestClientExplicitAckAndHeartbeats(t *testing.T) {
	sequence := uint64(4)
	permitAck := make(chan struct{})
	opts, done := fixture(t, func(conn net.Conn) error {
		if err := greeting(conn, "node"); err != nil {
			return err
		}
		frame, _ := encode(Message{Type: "thread_event", Version: 1, OperationID: "op", ExecutionID: "exec", Sequence: &sequence, Payload: json.RawMessage(`{"record":{"type":"turnEnded"}}`)})
		if err := writeAll(conn, frame); err != nil {
			return err
		}
		// The only automatic message must be a heartbeat, never an ACK before persistence.
		m, err := readFrame(conn)
		if err != nil {
			return err
		}
		if m.Type != "heartbeat" {
			return errors.New("event was acknowledged before owner committed it")
		}
		close(permitAck)
		for {
			m, err = readFrame(conn)
			if err != nil {
				return err
			}
			if m.Type == "heartbeat" {
				continue
			}
			if m.Type != "event_ack" || m.ExecutionID != "exec" || m.OperationID != "op" || m.Sequence == nil || *m.Sequence != 4 {
				return errors.New("wrong ACK coordinates")
			}
			return nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := Dial(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	m, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-permitAck:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := client.Ack(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDialRejectsWrongNode(t *testing.T) {
	opts, done := fixture(t, func(conn net.Conn) error { return greeting(conn, "another-node") })
	client, err := Dial(context.Background(), opts)
	if err == nil {
		client.Close()
		t.Fatal("accepted another node")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCloseUnblocksReceive(t *testing.T) {
	opts, done := fixture(t, func(conn net.Conn) error {
		if err := greeting(conn, "node"); err != nil {
			return err
		}
		for {
			if _, err := readFrame(conn); err != nil {
				return nil
			}
		}
	})
	client, err := Dial(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() { _, err := client.Receive(context.Background()); readDone <- err }()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("closed receive succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("Receive leaked")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReceiveRejectsChangedIncarnation(t *testing.T) {
	opts, done := fixture(t, func(conn net.Conn) error {
		if err := greeting(conn, "node"); err != nil {
			return err
		}
		frame, _ := encode(Message{Type: "heartbeat", Version: 1, Payload: json.RawMessage(`{"node":{"node_id":"node","incarnation_id":"another"}}`)})
		return writeAll(conn, frame)
	})
	client, err := Dial(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Receive(context.Background()); err == nil {
		t.Fatal("accepted changed incarnation")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type observedRead struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *observedRead) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.once.Do(func() { close(c.started) })
	}
	return n, err
}

func TestCancellationClosesPartialFrame(t *testing.T) {
	opts, done := fixture(t, func(conn net.Conn) error {
		if err := greeting(conn, "node"); err != nil {
			return err
		}
		if err := writeAll(conn, []byte{0, 0}); err != nil {
			return err
		}
		for {
			if _, err := readFrame(conn); err != nil {
				return nil
			}
		}
	})
	// Keep heartbeat out of this deliberately partial inbound frame scenario.
	opts.HeartbeatInterval = time.Second
	opts.FrameTimeout = 2 * time.Second
	client, err := Dial(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// Do not replace the shared connection while heartbeat could use it: use a separate
	// one-shot client with the same wire reader. Wait until it consumes the partial frame,
	// so cancellation cannot race the fixture's write and only test closing an empty stream.
	started := make(chan struct{})
	reader := &Client{conn: &observedRead{Conn: client.conn, started: started}, options: opts, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := reader.read(ctx); result <- err }()
	select {
	case <-started:
	case err := <-result:
		t.Fatalf("read ended before consuming partial frame: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("partial frame was not consumed")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled read leaked")
	}
	if _, err := client.Receive(context.Background()); err == nil {
		t.Fatal("partial stream was reused")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// previewGreeting answers Hello as a Node that grants previews exactly when they were requested,
// unless grant forces an answer, and reports the request it saw.
func previewGreeting(conn net.Conn, grant *bool, requested chan<- bool) error {
	m, err := readFrame(conn)
	if err != nil {
		return err
	}
	var hello struct {
		Previews bool `json:"thread_previews"`
	}
	if m.Type != "hello" || json.Unmarshal(m.Payload, &hello) != nil {
		return errors.New("missing hello")
	}
	requested <- hello.Previews
	granted := hello.Previews
	if grant != nil {
		granted = *grant
	}
	payload, _ := json.Marshal(map[string]any{"selected_version": 1, "node": Identity{NodeID: "node", IncarnationID: "incarnation"}, "capabilities": []string{"agent_session"}, "thread_previews": granted})
	frame, _ := encode(Message{Type: "hello_accepted", Version: 1, Payload: payload})
	return writeAll(conn, frame)
}

func TestDialNegotiatesThreadPreviews(t *testing.T) {
	for _, want := range []bool{false, true} {
		requested := make(chan bool, 1)
		opts, done := fixture(t, func(conn net.Conn) error {
			if err := previewGreeting(conn, nil, requested); err != nil {
				return err
			}
			// A preview Node sends without negotiation must never reach the caller.
			for _, kind := range []string{"thread_preview", "agent_session_ended"} {
				frame, _ := encode(Message{Type: kind, Version: 1, OperationID: "op", ExecutionID: "exec", Payload: json.RawMessage(`{}`)})
				if err := writeAll(conn, frame); err != nil {
					return err
				}
			}
			_, _ = readFrame(conn)
			return nil
		})
		opts.ThreadPreviews = want
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		client, err := Dial(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		if <-requested != want || client.ThreadPreviews() != want {
			t.Fatalf("previews requested/negotiated mismatch for %v", want)
		}
		m, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if wantType := map[bool]string{false: "agent_session_ended", true: "thread_preview"}[want]; m.Type != wantType {
			t.Fatalf("previews=%v received %q", want, m.Type)
		}
		// With previews the session end still follows the preview. Reading it also means the peer
		// has written its last frame, so closing cannot break a write still in flight.
		if want {
			if m, err = client.Receive(ctx); err != nil || m.Type != "agent_session_ended" {
				t.Fatalf("previews=true then received %q, %v; want agent_session_ended", m.Type, err)
			}
		}
		client.Close()
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestDialRejectsUnrequestedThreadPreviews(t *testing.T) {
	granted := true
	requested := make(chan bool, 1)
	opts, done := fixture(t, func(conn net.Conn) error { return previewGreeting(conn, &granted, requested) })
	client, err := Dial(context.Background(), opts)
	if err == nil {
		client.Close()
		t.Fatal("accepted previews the Controller did not ask for")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// turnCancelGreeting answers Hello with the given turn_cancel confirmation and capabilities, and
// reports whether the Hello asked for turn cancellation.
func turnCancelGreeting(conn net.Conn, confirm bool, capabilities []string, requested chan<- bool) error {
	m, err := readFrame(conn)
	if err != nil {
		return err
	}
	var hello struct {
		TurnCancel *bool `json:"turn_cancel"`
	}
	if m.Type != "hello" || json.Unmarshal(m.Payload, &hello) != nil || hello.TurnCancel != nil && !*hello.TurnCancel {
		return errors.New("missing hello, or turn_cancel sent as false instead of omitted")
	}
	requested <- hello.TurnCancel != nil
	answer := map[string]any{"selected_version": 1, "node": Identity{NodeID: "node", IncarnationID: "incarnation"}, "capabilities": capabilities}
	if confirm {
		answer["turn_cancel"] = true
	}
	payload, _ := json.Marshal(answer)
	frame, _ := encode(Message{Type: "hello_accepted", Version: 1, Payload: payload})
	return writeAll(conn, frame)
}

// Turn cancellation is negotiated like previews: the Controller asks, and a Node that supports it
// confirms with both the flag and the capability. An older Node ignores the request and answers
// without either, which keeps the session-ending stop. Any other combination fails the handshake.
func TestDialNegotiatesTurnCancel(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ask, confirm bool
		capabilities []string
		negotiated   bool
		refused      bool
	}{
		{name: "not asked", capabilities: []string{"agent_session"}},
		{name: "older Node", ask: true, capabilities: []string{"agent_session"}},
		{name: "granted", ask: true, confirm: true, capabilities: []string{"agent_session", "turn_cancel"}, negotiated: true},
		{name: "confirmation without capability", ask: true, confirm: true, capabilities: []string{"agent_session"}, refused: true},
		{name: "capability without confirmation", ask: true, capabilities: []string{"agent_session", "turn_cancel"}, refused: true},
		{name: "granted unasked", confirm: true, capabilities: []string{"agent_session", "turn_cancel"}, refused: true},
		{name: "granted without sessions", ask: true, confirm: true, capabilities: []string{"plugin_install", "turn_cancel"}, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requested := make(chan bool, 1)
			opts, done := fixture(t, func(conn net.Conn) error { return turnCancelGreeting(conn, tc.confirm, tc.capabilities, requested) })
			opts.TurnCancel = tc.ask
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			client, err := Dial(ctx, opts)
			if got := <-requested; got != tc.ask {
				t.Fatalf("Hello asked for turn_cancel=%v, want %v", got, tc.ask)
			}
			if tc.refused {
				if err == nil {
					client.Close()
					t.Fatal("accepted an inconsistent turn_cancel negotiation")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if client.Supports("turn_cancel") != tc.negotiated {
					t.Fatalf("negotiated turn_cancel=%v, want %v", client.Supports("turn_cancel"), tc.negotiated)
				}
				client.Close()
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
