//go:build linux

package agentruntime

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/node"
	"github.com/wanglongan587/cloud/v2-server/internal/nodeservices"
)

// TestDriverIPCSendsNodeNoUpstreamSecret runs an OpenCode and a Claude Code session on bindings
// backed by real upstream providers, through install, clone, start, a follow-up turn and end.
// No command the driver sends Node may carry an upstream key or address, as written or
// encoded: the session names its binding and frozen model, and Node reaches the upstream only
// through the local proxy.
func TestDriverIPCSendsNodeNoUpstreamSecret(t *testing.T) {
	const (
		openAIURL   = "https://openai-upstream.invalid/v1"
		openAIKey   = "openai-upstream-key-a5"
		messagesURL = "https://messages-upstream.invalid/v1"
		messagesKey = "messages-upstream-key-a5"
	)
	openAI, err := nodeservices.NewRemoteProvider(openAIURL, openAIKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := nodeservices.NewRemoteMessagesProvider(messagesURL, messagesKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	secrets := secretForms(openAIURL, openAIKey, messagesURL, messagesKey, "openai-upstream.invalid", "messages-upstream.invalid")
	var mu sync.Mutex
	var sent []node.Message
	h := newIPCHarnessWith(t, map[string]nodeservices.Binding{
		"fixture":          {Model: ipcModel(), Protocol: nodeservices.OpenAICompletions, Provider: openAI},
		"messages-fixture": {Model: ipcModel(), Protocol: nodeservices.AnthropicMessages, Provider: messages},
	}, func(_ *fakeNodeIPC, m node.Message) bool {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, m)
		return false
	})
	ctx := ipcContext(t)

	opencode := h.session("opencode", "workspace")
	done := startIPCRun(h, ctx, opencode, "first", discardIPCEvent)
	f := receiveIPC(t, h.created)
	start := nextIPCCommand(t, f, "start_agent_session")
	f.finishTurn(t, start, "first", "first answer")
	if got := receiveIPC(t, done); got.err != nil {
		t.Fatalf("OpenCode first turn=%+v", got)
	}
	done = startIPCRun(h, ctx, opencode, "second", discardIPCEvent)
	f.finishTurn(t, nextIPCCommand(t, f, "submit_user_turn"), "second", "second answer")
	if got := receiveIPC(t, done); got.err != nil {
		t.Fatalf("OpenCode second turn=%+v", got)
	}

	end := func(session string) {
		t.Helper()
		ended := make(chan error, 1)
		go func() { ended <- h.driver.End(ctx, session) }()
		f.terminal(nextIPCCommand(t, f, "end_session"))
		if err := receiveIPC(t, ended); err != nil {
			t.Fatal(err)
		}
	}
	// One Workspace runs one Agent session at a time.
	end(opencode.ID)

	claude := h.session("claude", "workspace")
	claude.Executor.AgentID, claude.Executor.PluginVersion, claude.Executor.Model.ID = "official/ora-space.claude", "0.4.5", "messages-fixture"
	done = startIPCRun(h, ctx, claude, "first", discardIPCEvent)
	f.finishTurn(t, nextIPCCommand(t, f, "start_agent_session"), "first", "claude answer")
	if got := receiveIPC(t, done); got.err != nil {
		t.Fatalf("Claude Code turn=%+v", got)
	}

	end(claude.ID)

	mu.Lock()
	defer mu.Unlock()
	kinds := map[string]int{}
	for _, m := range sent {
		kinds[m.Type]++
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range secrets {
			if strings.Contains(string(data), secret) {
				t.Errorf("%s sent to Node contains upstream secret %q", m.Type, secret)
			}
		}
	}
	// The check above means something only if every kind of command reached the recorder.
	for kind, want := range map[string]int{"install_plugins": 2, "clone_repository": 1, "start_agent_session": 2, "submit_user_turn": 1, "end_session": 2} {
		if kinds[kind] < want {
			t.Fatalf("sent %v; want at least %d %s", kinds, want, kind)
		}
	}
}
