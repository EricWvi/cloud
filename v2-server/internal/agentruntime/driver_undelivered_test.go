//go:build linux

package agentruntime

import (
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/node"
)

// A follow-up the execution service accepted can still be refused by the driver before its
// prompt reaches the Agent. Each such refusal is marked NotDelivered, so the caller does not
// count a turn the Agent never got; AgentLost still says whether the session can go on.
func TestDriverIPCFollowUpRefusedBeforeItsAgentGotItIsNotDelivered(t *testing.T) {
	for name, tc := range map[string]struct {
		// before makes the session refuse its next turn before that turn runs; answer instead
		// answers the turn's submit_user_turn as Node received it.
		before func(t *testing.T, f *fakeNodeIPC, p *projectRuntime, start node.Message)
		answer func(f *fakeNodeIPC, submit node.Message)
		want   execution.Result
		err    error
		// submitted says whether Node received the turn's command (and refused it).
		submitted bool
	}{
		"Node gone before the turn": {
			before: func(t *testing.T, f *fakeNodeIPC, p *projectRuntime, _ node.Message) {
				_ = f.Close()
				receiveIPC(t, p.dead)
			},
			want: execution.Result{Status: execution.StatusFailed, FailureCode: execution.FailureNodeLost, NotDelivered: true},
			err:  execution.ErrNodeLost,
		},
		"session ended on Node while the node waited": {
			before: func(t *testing.T, f *fakeNodeIPC, p *projectRuntime, start node.Message) {
				// Node ends the idle session; no turn is active, so only the session learns it.
				f.terminalAs(start, "idle_timeout", "")
				deadline := time.Now().Add(5 * time.Second)
				for {
					p.driver.mu.Lock()
					a := p.driver.sessions["held"]
					p.driver.mu.Unlock()
					a.mu.Lock()
					ended := a.ended
					a.mu.Unlock()
					if ended {
						return
					}
					if time.Now().After(deadline) {
						t.Fatal("the ended session was never marked ended")
					}
					time.Sleep(time.Millisecond)
				}
			},
			want: execution.Result{Status: execution.StatusFailed, FailureCode: "agent_session_unavailable", NotDelivered: true},
			err:  execution.ErrSessionEnded,
		},
		"Node refuses the command of an ended session": {
			answer: func(f *fakeNodeIPC, submit node.Message) {
				var command struct {
					CommandID string `json:"command_id"`
				}
				_ = json.Unmarshal(submit.Payload, &command)
				f.reply(submit, "session_command_rejected", map[string]string{"command_id": command.CommandID, "reason": "session_ended"})
			},
			want:      execution.Result{Status: execution.StatusFailed, FailureCode: "agent_session_ended", NotDelivered: true},
			submitted: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newIPCHarness(t, nil)
			ctx := ipcContext(t)
			session := h.session("held", "run-workspace")
			done := runWorkflowTurn(h, ctx, session, "first")
			f := receiveIPC(t, h.created)
			start := nextIPCCommand(t, f, "start_agent_session")
			f.finishTurn(t, start, "first", "ready")
			if got := receiveIPC(t, done); got.err != nil || got.result.NotDelivered {
				t.Fatalf("first turn: %+v", got)
			}
			h.driver.mu.Lock()
			p := h.driver.projects["project"]
			h.driver.mu.Unlock()
			sentIPC(f)
			if tc.before != nil {
				tc.before(t, f, p, start)
			}
			second := runWorkflowTurn(h, ctx, session, "second")
			if tc.answer != nil {
				tc.answer(f, nextIPCCommand(t, f, "submit_user_turn"))
			}
			got := receiveIPC(t, second)
			if got.err == nil || tc.err != nil && !errors.Is(got.err, tc.err) {
				t.Fatalf("refused follow-up error = %v, want %v", got.err, tc.err)
			}
			if !reflect.DeepEqual(got.result, tc.want) {
				t.Fatalf("refused follow-up = %+v, want %+v", got.result, tc.want)
			}
			if !tc.submitted && sentCommand(f, "submit_user_turn") {
				t.Fatal("Node received the follow-up it is reported not to have got")
			}
		})
	}
}

// What may have reached the Agent is never reported undelivered: a command whose write failed as
// its Node died, and a turn whose session Node ended while the turn was with it.
func TestDriverIPCFollowUpThatMayHaveArrivedIsNotUndelivered(t *testing.T) {
	t.Run("write failed as the Node died", func(t *testing.T) {
		h := newIPCHarness(t, nil)
		ctx := ipcContext(t)
		session := h.session("held", "run-workspace")
		f, p := startedSession(t, h, ctx, session)
		fail := func(m node.Message) error {
			if m.Type != "submit_user_turn" {
				return nil
			}
			p.deadOnce.Do(func() { close(p.dead) })
			return net.ErrClosed
		}
		f.failSend.Store(&fail)
		got := receiveIPC(t, runWorkflowTurn(h, ctx, session, "second"))
		if got.err == nil || got.result.NotDelivered || got.result.FailureCode != execution.FailureNodeLost {
			t.Fatalf("turn whose write failed = %+v, want it lost with its Node, not undelivered", got)
		}
		_ = f.Close()
	})
	t.Run("session ended while the turn was with Node", func(t *testing.T) {
		h := newIPCHarness(t, nil)
		ctx := ipcContext(t)
		session := h.session("held", "run-workspace")
		f, _ := startedSession(t, h, ctx, session)
		second := runWorkflowTurn(h, ctx, session, "second")
		f.terminalAs(nextIPCCommand(t, f, "submit_user_turn"), "agent_failed", "")
		got := receiveIPC(t, second)
		if !errors.Is(got.err, execution.ErrSessionEnded) || !got.result.AgentLost || got.result.NotDelivered {
			t.Fatalf("turn lost with its session = %+v, want the Agent lost and the turn not undelivered", got)
		}
	})
}
