//go:build linux

package agentruntime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/node"
	"github.com/wanglongan587/cloud/v2-server/internal/nodeservices"
)

// silentEnd forwards everything to a real Node except end_session, so the Node and its Agent stay
// alive while their stop is never acknowledged: the case Rebuild exists for.
type silentEnd struct {
	nodeClient
	swallowed atomic.Int32
}

func (c *silentEnd) Send(ctx context.Context, m node.Message) error {
	if m.Type == "end_session" {
		c.swallowed.Add(1)
		return nil
	}
	return c.nodeClient.Send(ctx, m)
}

// processesUnder lists live processes whose executable or working directory is inside root.
func processesUnder(t *testing.T, root string) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		for _, link := range []string{"exe", "cwd"} {
			target, err := os.Readlink(filepath.Join("/proc", entry.Name(), link))
			if err == nil && strings.HasPrefix(target, root+string(filepath.Separator)) {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids
}

func TestRealRebuildRetiresUnacknowledgedNodeBeforeNextSession(t *testing.T) {
	assets := os.Getenv("V2_NODE_ASSETS_DIR")
	if assets == "" {
		t.Skip("real Node assets required")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// Node socket paths are short; keep the root on a native path under home, not t.TempDir.
	tmp, err := os.MkdirTemp(home, "vr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput()
		if err != nil {
			t.Fatal(string(out), err)
		}
		return strings.TrimSpace(string(out))
	}
	repo, work := filepath.Join(tmp, "repo.git"), filepath.Join(tmp, "seed")
	git("init", "--bare", repo)
	git("init", work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("Fixture project"), 0600); err != nil {
		t.Fatal(err)
	}
	git("-C", work, "add", "README.md")
	git("-C", work, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-m", "Fixture")
	commit := git("-C", work, "rev-parse", "HEAD")
	ref := "refs/ora/upstreams/4da35252-6789-4760-9041-49b68ce3874f/snapshots/" + commit
	git("-C", work, "push", repo, "HEAD:"+ref)
	project := Project{MirrorPath: repo, Commit: commit, SnapshotRef: ref}
	gitDir := git("--exec-path")
	model := nodeservices.Model{ID: "fixture", Name: "Fixture", ContextWindow: 128000, MaxTokens: 16384}
	d, err := New(Options{Root: filepath.Join(tmp, "n"), AssetsDirectory: assets, GitBackend: filepath.Join(gitDir, "git-http-backend"),
		ResolveProject: func(context.Context, execution.Scope, string) (Project, error) { return project, nil },
		Bindings:       map[string]nodeservices.Binding{"fixture": {Provider: nodeservices.FixtureProvider{}, Protocol: nodeservices.OpenAICompletions, Model: model}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := d.Close(stop); err != nil {
			t.Errorf("driver cleanup: %v", err)
		}
	})
	var first *silentEnd
	d.wrapClient = func(c nodeClient) nodeClient {
		if first != nil {
			return c
		}
		first = &silentEnd{nodeClient: c}
		return first
	}
	scope := execution.Scope{TenantID: "tenant", UserID: "user"}
	session := func(id string) execution.Session {
		return execution.Session{ID: id, WorkspaceID: "workspace-" + id, Target: &execution.Target{ProjectID: "project"},
			Executor: &execution.Executor{ID: "opencode:fixture", AgentID: pluginID, PluginVersion: pluginVersion, Model: execution.Model{ID: "fixture"}}}
	}
	run := func(id string) (execution.Result, node.Identity) {
		t.Helper()
		turn := execution.Turn{Ref: execution.Ref{SessionID: id, TurnID: id + "-turn"}, Caller: execution.Caller{Kind: execution.CallerChat, ID: id, AttemptID: id}, Prompt: execution.TextPrompt("Read the repository README.")}
		result, err := d.Run(ctx, scope, session(id), turn, func(context.Context, execution.Event) error { return nil })
		if err != nil {
			t.Fatalf("run %s: %v (%+v)", id, err, result)
		}
		d.mu.Lock()
		p := d.projects["project"]
		d.mu.Unlock()
		return result, p.ipc().Identity()
	}

	result, old := run("first")
	if result.Status != execution.StatusCompleted {
		t.Fatalf("first turn: %+v", result)
	}
	d.mu.Lock()
	root := d.projects["project"].root
	d.mu.Unlock()
	oldProcesses := processesUnder(t, root)
	if len(oldProcesses) == 0 {
		t.Fatal("no Node processes found under the project root")
	}
	ending, stop := context.WithTimeout(ctx, 2*time.Second)
	err = d.End(ending, "first")
	stop()
	if !errors.Is(err, execution.ErrStopUnconfirmed) || first.swallowed.Load() == 0 {
		t.Fatalf("End without acknowledgement: err=%v swallowed=%d", err, first.swallowed.Load())
	}
	d.Rebuild("first")

	result, replacement := run("next")
	if result.Status != execution.StatusCompleted {
		t.Fatalf("next session on the rebuilt Node: %+v", result)
	}
	if replacement.IncarnationID == old.IncarnationID {
		t.Fatal("next session reused the Node marked for rebuild")
	}
	for _, pid := range oldProcesses {
		if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); err == nil {
			if exe, _ := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe")); strings.HasPrefix(exe, root) {
				t.Fatalf("old Node process %d (%s) survived the rebuild", pid, exe)
			}
		}
	}
	if err := d.End(ctx, "first"); err != nil {
		t.Fatalf("supervisor stop did not settle the stuck session: %v", err)
	}
	if err := d.End(ctx, "next"); err != nil {
		t.Fatalf("rebuilt Node did not acknowledge End: %v", err)
	}
}
