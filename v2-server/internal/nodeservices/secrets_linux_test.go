//go:build linux

package nodeservices

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/node"
)

// TestNodeIsHandedNoUpstreamSecret collects everything the services hand to Node for real
// upstream providers of both protocols: the Node configuration, every file it names, and the
// grant issue, renew and revoke responses. No upstream key or address may appear in any of it,
// as written or encoded; Node reaches the model only through the local proxy with its session
// token.
func TestNodeIsHandedNoUpstreamSecret(t *testing.T) {
	const (
		openAIURL   = "https://openai-upstream.invalid/v1"
		openAIKey   = "openai-upstream-key-a5"
		messagesURL = "https://messages-upstream.invalid/v1"
		messagesKey = "messages-upstream-key-a5"
		bearerURL   = "https://bearer-upstream.invalid/api"
		bearerToken = "bearer-upstream-token-a5"
	)
	secrets := secretForms(openAIURL, openAIKey, messagesURL, messagesKey, bearerURL, bearerToken, "openai-upstream.invalid", "messages-upstream.invalid", "bearer-upstream.invalid")
	openAI, err := NewRemoteProvider(openAIURL, openAIKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := NewRemoteMessagesProvider(messagesURL, messagesKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	bearer, err := NewBearerMessagesProvider(bearerURL, bearerToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := testServiceWith(t, map[string]Binding{
		"openai":   {Model: testModel, Protocol: OpenAICompletions, Provider: openAI},
		"messages": {Model: testModel, Protocol: AnthropicMessages, Provider: messages},
		"bearer":   {Model: testModel, Protocol: AnthropicMessages, Provider: bearer},
	})

	root := t.TempDir()
	paths := node.Paths{Root: root, NodeHome: filepath.Join(root, "n"), HostHome: filepath.Join(root, "h"), Repositories: filepath.Join(root, "r"), UserHome: filepath.Join(root, "u"), Config: filepath.Join(root, "config.json"), Socket: filepath.Join(root, "n", "control.sock")}
	config, err := s.NodeConfiguration(paths, node.Options{ControllerID: "controller", ExpectedNodeID: "node", HeartbeatInterval: time.Second, FrameTimeout: time.Second}, "/usr/bin/deno")
	if err != nil {
		t.Fatal(err)
	}
	handed := map[string][]byte{"Node configuration": config}
	// Every regular file the configuration names is read by Node, so it is handed over too.
	var decoded any
	if err := json.Unmarshal(config, &decoded); err != nil {
		t.Fatal(err)
	}
	var named func(any)
	named = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for _, child := range v {
				named(child)
			}
		case []any:
			for _, child := range v {
				named(child)
			}
		case string:
			// System programs such as /usr/bin/git are not written by the services.
			local := strings.HasPrefix(v, root+"/") || strings.HasPrefix(v, s.directory+"/")
			if info, err := os.Stat(v); local && err == nil && info.Mode().IsRegular() {
				data, err := os.ReadFile(v)
				if err != nil {
					t.Fatal(err)
				}
				handed[v] = data
			}
		}
	}
	named(decoded)
	for _, want := range []string{filepath.Join(root, "gitconfig"), s.TLS.CACert, s.TLS.ClientCert, s.TLS.ClientKey} {
		if _, ok := handed[want]; !ok {
			t.Fatalf("configuration does not name %s; handed %d items", want, len(handed))
		}
	}

	client := serviceClient(t, s, true)
	grants := s.URL + "/internal/v1/model-grants"
	for _, binding := range []string{"openai", "messages", "bearer"} {
		execution := "execution-" + binding
		if err := s.Permit(execution, binding); err != nil {
			t.Fatal(err)
		}
		status, data := request(t, client, "POST", grants, "", `{"bindingId":"`+binding+`","executionId":"`+execution+`"}`)
		var issued struct{ GrantID, Token string }
		if err := json.Unmarshal(data, &issued); err != nil || status != 200 || issued.GrantID == "" || issued.Token == "" {
			t.Fatalf("%s grant=%d %s", binding, status, data)
		}
		handed[binding+" grant"] = data
		status, data = request(t, client, "POST", grants+"/"+issued.GrantID+"/renew", "", `{"executionId":"`+execution+`"}`)
		if status != 200 {
			t.Fatalf("%s renew=%d %s", binding, status, data)
		}
		handed[binding+" renewal"] = data
		status, data = request(t, client, "DELETE", grants+"/"+issued.GrantID, "", `{"executionId":"`+execution+`"}`)
		if status != 200 {
			t.Fatalf("%s revoke=%d %s", binding, status, data)
		}
		handed[binding+" revocation"] = data
	}

	for name, data := range handed {
		for _, secret := range secrets {
			if strings.Contains(string(data), secret) {
				t.Errorf("%s handed to Node contains upstream secret %q", name, secret)
			}
		}
	}
}
