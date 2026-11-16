//go:build linux

package workflowhost

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/agentruntime"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/executionruntime"
	"github.com/wanglongan587/cloud/v2-server/internal/nodeservices"
	"github.com/wanglongan587/cloud/v2-server/internal/projectrepos"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

// siblingBarrier is the fixture model of two sibling Agents. Each model request names its node by
// a marker in the prompt. In barrier mode a request waits until both nodes have reached the model,
// so a run that serialised its Agents would never get past the first; in hold mode every request
// waits until the Agent's turn is cancelled.
type siblingBarrier struct {
	mu      sync.Mutex
	reached map[string]bool
	both    chan struct{}
	hold    bool
	failed  chan string
}

func newSiblingBarrier(hold bool) *siblingBarrier {
	return &siblingBarrier{reached: map[string]bool{}, both: make(chan struct{}), hold: hold, failed: make(chan string, 4)}
}

func (b *siblingBarrier) Complete(w http.ResponseWriter, r *http.Request, m nodeservices.Model) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	marker := ""
	for _, name := range []string{"SIBLING-A", "SIBLING-B"} {
		if bytes.Contains(body, []byte(name)) {
			marker = name
		}
	}
	if marker != "" {
		b.mu.Lock()
		if !b.reached[marker] {
			b.reached[marker] = true
			if len(b.reached) == 2 {
				close(b.both)
			}
		}
		b.mu.Unlock()
	}
	if b.hold {
		<-r.Context().Done()
		return
	}
	select {
	case <-b.both:
	case <-time.After(90 * time.Second):
		b.failed <- marker
	case <-r.Context().Done():
		return
	}
	nodeservices.FixtureProvider{}.Complete(w, r, m)
}

const realSiblings = `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"SIBLING-A: inspect README and save the fixture result."}}},
 {"id":"b","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"SIBLING-B: inspect README and save the fixture result."}}}
],"edges":[{"source":"start","target":"a"},{"source":"start","target":"b"}]}`

// TestRealWorkflowSiblingAgentsShareTheCheckout runs two sibling Agent nodes of one run through the
// real workflow host, execution service, Node, OpenCode and PostgreSQL: both reach the model before
// either finishes, each in its own session with its own history in the run's one checkout, and
// cancelling a second run while both execute ends both sessions.
func TestRealWorkflowSiblingAgentsShareTheCheckout(t *testing.T) {
	assets := os.Getenv("V2_NODE_ASSETS_DIR")
	if assets == "" {
		t.Skip("real Node assets required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, "vp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	db := storetest.Open(t)
	user, tenant := storetest.Login(t, db, "workflow-siblings")
	scope := execution.Scope{TenantID: tenant, UserID: user}
	repositories, err := projectrepos.New(db, projectrepos.Options{Root: filepath.Join(root, "r")})
	if err != nil {
		t.Fatal(err)
	}
	project, err := repositories.SeedDemo(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	models, err := executionruntime.ModelsFromEnvironment(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	models = executionruntime.WithFixtureModels(models)
	var provider struct {
		sync.Mutex
		current *siblingBarrier
	}
	model := models["fixture"]
	model.Binding.Provider = providerFunc(func(w http.ResponseWriter, r *http.Request, m nodeservices.Model) {
		provider.Lock()
		b := provider.current
		provider.Unlock()
		b.Complete(w, r, m)
	})
	models["fixture"] = model
	use := func(b *siblingBarrier) {
		provider.Lock()
		provider.current = b
		provider.Unlock()
	}
	gitPath, err := exec.CommandContext(ctx, "git", "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	driver, err := agentruntime.New(agentruntime.Options{
		Root: filepath.Join(root, "n"), AssetsDirectory: assets,
		GitBackend: filepath.Join(strings.TrimSpace(string(gitPath)), "git-http-backend"),
		Bindings:   map[string]nodeservices.Binding{"fixture": model.Binding},
		ResolveProject: func(c context.Context, s execution.Scope, id string) (agentruntime.Project, error) {
			p, e := db.GetProject(c, s, id)
			return agentruntime.Project{MirrorPath: p.MirrorPath, Commit: p.Commit, SnapshotRef: p.SnapshotRef}, e
		},
		RecordWorkspace: func(c context.Context, id string, at agentruntime.Checkout) error {
			return db.RecordWorkspace(c, id, store.WorkspaceLocation{Path: at.Path, BaseCommit: at.BaseCommit})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := driver.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	service, err := executionruntime.New(ctx, executionruntime.Config{Store: db, Models: models, Agent: driver})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	host, err := NewHost(Config{Store: db, Execution: service, Catalog: service})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)
	f := &fixture{s: db, host: host, scope: server.WorkflowRunScope{TenantID: tenant, UserID: user}, project: project.ID}
	awaitRun := func(rid string, done func(store.WorkflowRun, []store.WorkflowNodeRun) bool) (store.WorkflowRun, []store.WorkflowNodeRun) {
		t.Helper()
		events, unsubscribe := host.Hub().Subscribe(rid)
		defer unsubscribe()
		for {
			run, err := db.WorkflowRun(ctx, tenant, rid)
			if err != nil {
				t.Fatal(err)
			}
			nodes, err := db.WorkflowEngineNodeRuns(ctx, rid)
			if err != nil {
				t.Fatal(err)
			}
			if done(run, nodes) {
				return run, nodes
			}
			select {
			case <-events:
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
				t.Fatalf("run %s: %v; last %+v", rid, ctx.Err(), run)
			}
		}
	}

	// Both siblings reach the model before either finishes, then each completes on its own.
	barrier := newSiblingBarrier(false)
	use(barrier)
	rid := f.run(t, realSiblings, nil)
	started := time.Now()
	if _, err := host.StartRun(ctx, f.scope, rid); err != nil {
		t.Fatal(err)
	}
	select {
	case <-barrier.both:
		t.Logf("both siblings reached the model %s after the start", time.Since(started).Round(time.Millisecond))
	case marker := <-barrier.failed:
		t.Fatalf("%s waited at the model while its sibling never arrived: the run serialised its Agents", marker)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	run, nodes := awaitRun(rid, func(r store.WorkflowRun, _ []store.WorkflowNodeRun) bool { return terminal(r, nil) })
	if run.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("run = %+v nodes = %+v", run, nodes)
	}
	sessions := map[string]execution.Session{}
	for _, id := range []string{"a", "b"} {
		n := node(nodes, id)
		if n.SessionID == nil || n.Output == nil || !strings.Contains(*n.Output, nodeservices.FixtureAnswer) {
			t.Fatalf("node %s = %+v", id, n)
		}
		s, err := service.ReadSession(ctx, scope, *n.SessionID)
		if err != nil || s.EndedAt == nil {
			t.Fatalf("session of %s = %+v, %v; want ended after its turn", id, s, err)
		}
		sessions[id] = s
		turns, err := service.ReadTurns(ctx, scope, s.ID, "", 100)
		if err != nil || len(turns.Items) != 1 || turns.Items[0].Caller.NodeID != id || turns.Items[0].Result == nil {
			t.Fatalf("history of %s = %+v, %v; want its own single turn", id, turns, err)
		}
		text, _ := turns.Items[0].Prompt.Text()
		other := map[string]string{"a": "SIBLING-B", "b": "SIBLING-A"}[id]
		if strings.Contains(text, other) {
			t.Fatalf("history of %s carries its sibling's prompt", id)
		}
	}
	if sessions["a"].ID == sessions["b"].ID || sessions["a"].WorkspaceID != sessions["b"].WorkspaceID {
		t.Fatalf("siblings: sessions %s / %s, workspaces %s / %s; want two sessions in one checkout",
			sessions["a"].ID, sessions["b"].ID, sessions["a"].WorkspaceID, sessions["b"].WorkspaceID)
	}

	// Cancelling a run while both siblings execute ends both sessions.
	held := newSiblingBarrier(true)
	use(held)
	rid = f.run(t, realSiblings, nil)
	if _, err := host.StartRun(ctx, f.scope, rid); err != nil {
		t.Fatal(err)
	}
	select {
	case <-held.both:
	case <-ctx.Done():
		t.Fatal("the siblings of the second run did not both reach the model")
	}
	awaitRun(rid, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").SessionID != nil && node(n, "b").SessionID != nil
	})
	run, err = host.CancelRun(ctx, f.scope, rid)
	if err != nil || run.Status != store.WorkflowStatusCancelled {
		t.Fatalf("cancel = %+v, %v", run, err)
	}
	_, nodes = awaitRun(rid, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return host.state(rid) == nil })
	for _, id := range []string{"a", "b"} {
		n := node(nodes, id)
		s, err := service.ReadSession(ctx, scope, *n.SessionID)
		if err != nil || s.EndedAt == nil || n.Status != store.WorkflowStatusCancelled {
			t.Fatalf("after the cancel, %s = %s with session %+v, %v; want cancelled and ended", id, n.Status, s, err)
		}
	}
}

type providerFunc func(http.ResponseWriter, *http.Request, nodeservices.Model)

func (p providerFunc) Complete(w http.ResponseWriter, r *http.Request, m nodeservices.Model) {
	p(w, r, m)
}
