//go:build linux

package workflowhost

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

const realInteractiveChain = `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"Inspect README and save the fixture result.","interactive":true}}},
 {"id":"b","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"Review: {{#a.output#}}"}}}
],"edges":[{"source":"start","target":"a"},{"source":"a","target":"b"}]}`

// realStack is the server's workflow stack over a real Node: what one v2-server process runs.
type realStack struct {
	driver  *agentruntime.Driver
	service *executionruntime.Service
	host    *Host
}

// close shuts the stack down in the order the server does: requests, then execution, then Node.
func (s *realStack) close(t *testing.T) {
	t.Helper()
	s.host.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.service.Close(ctx); err != nil {
		t.Errorf("close execution: %v", err)
	}
	if err := s.driver.Close(ctx); err != nil {
		t.Errorf("close Node: %v", err)
	}
}

// realInteractive drives an interactive node through the real workflow host, execution service,
// Node, OpenCode and PostgreSQL in a fresh data directory.
type realInteractive struct {
	t       *testing.T
	ctx     context.Context
	db      *store.Store
	root    string
	scope   execution.Scope
	f       *fixture
	current *realStack
	start   func() *realStack

	mu sync.Mutex
	// handoffs counts the model requests that carried the recorded conversation.
	handoffs int
}

func newRealInteractive(t *testing.T, ctx context.Context) *realInteractive {
	t.Helper()
	assets := os.Getenv("V2_NODE_ASSETS_DIR")
	if assets == "" {
		t.Skip("real Node assets required")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// Node socket paths are short; keep the root on a native path under home, not t.TempDir.
	root, err := os.MkdirTemp(home, "vi-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	db := storetest.Open(t)
	user, tenant := storetest.Login(t, db, "workflow-restart")
	r := &realInteractive{t: t, ctx: ctx, db: db, root: root, scope: execution.Scope{TenantID: tenant, UserID: user}}
	repositories, err := projectrepos.New(db, projectrepos.Options{Root: filepath.Join(root, "r")})
	if err != nil {
		t.Fatal(err)
	}
	project, err := repositories.SeedDemo(ctx, r.scope)
	if err != nil {
		t.Fatal(err)
	}
	models, err := executionruntime.ModelsFromEnvironment(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	models = executionruntime.WithFixtureModels(models)
	model := models["fixture"]
	model.Binding.Provider = providerFunc(func(w http.ResponseWriter, req *http.Request, m nodeservices.Model) {
		body, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte("ora_previous_conversation")) {
			r.mu.Lock()
			r.handoffs++
			r.mu.Unlock()
		}
		nodeservices.FixtureProvider{}.Complete(w, req, m)
	})
	models["fixture"] = model
	gitPath, err := exec.CommandContext(ctx, "git", "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	r.start = func() *realStack {
		t.Helper()
		driver, err := agentruntime.New(agentruntime.Options{
			Root: filepath.Join(root, "n"), AssetsDirectory: assets,
			GitBackend: filepath.Join(strings.TrimSpace(string(gitPath)), "git-http-backend"),
			Bindings:   map[string]nodeservices.Binding{"fixture": model.Binding},
			ResolveProject: func(c context.Context, s execution.Scope, id string) (agentruntime.Project, error) {
				p, e := db.GetProject(c, s, id)
				return agentruntime.Project{MirrorPath: p.MirrorPath, Commit: p.Commit, SnapshotRef: p.SnapshotRef}, e
			},
			RecordWorkspace: func(c context.Context, id string, at agentruntime.Checkout) error {
				return db.RecordWorkspace(c, id, store.WorkspaceLocation{Path: at.Path, BaseCommit: at.BaseCommit, CloneExecution: at.CloneExecution})
			},
			ForgetCheckout: db.ForgetCloneExecution,
		})
		if err != nil {
			t.Fatal(err)
		}
		service, err := executionruntime.New(ctx, executionruntime.Config{Store: db, Models: models, Agent: driver})
		if err != nil {
			t.Fatal(err)
		}
		host, err := NewHost(Config{Store: db, Execution: service, Catalog: service})
		if err != nil {
			t.Fatal(err)
		}
		if err := host.Interrupt(ctx); err != nil {
			t.Fatal(err)
		}
		return &realStack{driver: driver, service: service, host: host}
	}
	r.current = r.start()
	t.Cleanup(func() { r.current.close(t) })
	r.f = &fixture{s: db, host: r.current.host, scope: server.WorkflowRunScope{TenantID: tenant, UserID: user}, project: project.ID}
	return r
}

func (r *realInteractive) awaitRun(rid string, done func(store.WorkflowRun, []store.WorkflowNodeRun) bool) (store.WorkflowRun, []store.WorkflowNodeRun) {
	r.t.Helper()
	for {
		run, err := r.db.WorkflowRun(r.ctx, r.scope.TenantID, rid)
		if err != nil {
			r.t.Fatal(err)
		}
		nodes, err := r.db.WorkflowEngineNodeRuns(r.ctx, rid)
		if err != nil {
			r.t.Fatal(err)
		}
		if done(run, nodes) {
			return run, nodes
		}
		select {
		case <-time.After(200 * time.Millisecond):
		case <-r.ctx.Done():
			r.t.Fatalf("run %s: %v; last %+v %+v", rid, r.ctx.Err(), run, nodes)
		}
	}
}

// turnsOf waits for A parked after n settled turns.
func (r *realInteractive) turnsOf(n int) func(store.WorkflowRun, []store.WorkflowNodeRun) bool {
	return func(_ store.WorkflowRun, rows []store.WorkflowNodeRun) bool {
		row := node(rows, "a")
		return row.Status == store.WorkflowStatusPending && interactivePayload(r.t, row)["turns"] == float64(n)
	}
}

func (r *realInteractive) handoffsSent() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.handoffs
}

// parked starts the chain and waits for A's first turn, then marks A's checkout: the tool's
// result file is removed and an uncommitted edit is added, so a later turn proves where it ran.
func (r *realInteractive) parked() (rid, session string, before execution.Session, checkout store.WorkspaceLocation, firstHistory []execution.HistoryRecord) {
	r.t.Helper()
	rid = r.f.run(r.t, realInteractiveChain, nil)
	if _, err := r.current.host.StartRun(r.ctx, r.f.scope, rid); err != nil {
		r.t.Fatal(err)
	}
	_, nodes := r.awaitRun(rid, r.turnsOf(1))
	session = *node(nodes, "a").SessionID
	before, err := r.current.service.ReadSession(r.ctx, r.scope, session)
	if err != nil {
		r.t.Fatal(err)
	}
	checkout, err = r.db.ExecutionWorkspace(r.ctx, before.WorkspaceID)
	if err != nil || checkout.CloneExecution == "" {
		r.t.Fatalf("checkout = %+v %v, want its clone execution recorded", checkout, err)
	}
	if err := os.Remove(filepath.Join(checkout.Path, "ora-v2-result.txt")); err != nil {
		r.t.Fatalf("the first turn did not run its tool in the checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkout.Path, "edit-before-restart.txt"), []byte("uncommitted"), 0o600); err != nil {
		r.t.Fatal(err)
	}
	firstHistory, err = r.db.LatestHistory(r.ctx, session)
	if err != nil || len(firstHistory) == 0 {
		r.t.Fatalf("history before the Agent was lost = %d %v", len(firstHistory), err)
	}
	return rid, session, before, checkout, firstHistory
}

// recovered checks A after a turn its new Agent ran: it worked in the original checkout, was
// sent the recorded conversation, and the page's history is one conversation without the
// handoff, the earlier records first.
func (r *realInteractive) recovered(rid, session string, checkout store.WorkspaceLocation, firstHistory []execution.HistoryRecord, turns int) {
	r.t.Helper()
	_, nodes := r.awaitRun(rid, r.turnsOf(turns))
	if p := interactivePayload(r.t, node(nodes, "a")); p["session_ended"] != false || !strings.Contains(p["answer"].(string), nodeservices.FixtureAnswer) {
		r.t.Fatalf("summary after the recovered turn = %v", p)
	}
	if _, err := os.Stat(filepath.Join(checkout.Path, "ora-v2-result.txt")); err != nil {
		r.t.Fatalf("the recovered Agent did not work in the original checkout: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(checkout.Path, "edit-before-restart.txt")); err != nil || string(data) != "uncommitted" {
		r.t.Fatalf("the uncommitted edit was lost: %q %v", data, err)
	}
	if r.handoffsSent() == 0 {
		r.t.Fatal("the recovered Agent was never sent the recorded conversation")
	}
	history, err := r.db.LatestHistory(r.ctx, session)
	if err != nil {
		r.t.Fatal(err)
	}
	seen := map[uint32]bool{}
	users := 0
	for i, record := range history {
		if seen[record.Sequence] {
			r.t.Fatalf("history position %d repeats", record.Sequence)
		}
		seen[record.Sequence] = true
		if i < len(firstHistory) && !bytes.Equal(record.Data, firstHistory[i].Data) {
			r.t.Fatalf("record %d changed after the Agent was lost", i)
		}
		if bytes.Contains(record.Data, []byte("ora_previous_conversation")) {
			r.t.Fatalf("the handoff reached the page: %s", record.Data)
		}
		var line struct {
			Update struct {
				Kind string `json:"sessionUpdate"`
			} `json:"update"`
		}
		if json.Unmarshal(record.Data, &line) == nil && line.Update.Kind == "user_message_chunk" {
			users++
		}
	}
	if len(history) <= len(firstHistory) || users < 2 {
		r.t.Fatalf("history has %d records (%d before) and %d user messages, want the earlier and the recovered turns", len(history), len(firstHistory), users)
	}
}

// completes completes A and waits for the run: B must run in the same checkout, where its tool
// writes the fixture result again.
func (r *realInteractive) completes(rid string, before execution.Session, checkout store.WorkspaceLocation) {
	r.t.Helper()
	// B's tool writes the fixture result again; a file left by A would prove nothing.
	if err := os.Remove(filepath.Join(checkout.Path, "ora-v2-result.txt")); err != nil && !os.IsNotExist(err) {
		r.t.Fatal(err)
	}
	if _, err := r.current.host.CompleteNode(r.ctx, r.f.scope, rid, "a", confirm); err != nil {
		r.t.Fatalf("complete after the Agent was lost: %v", err)
	}
	run, nodes := r.awaitRun(rid, func(run store.WorkflowRun, _ []store.WorkflowNodeRun) bool { return terminal(run, nil) })
	if run.Status != store.WorkflowStatusSucceeded {
		r.t.Fatalf("run = %+v nodes = %+v", run, nodes)
	}
	successor, err := r.current.service.ReadSession(r.ctx, r.scope, *node(nodes, "b").SessionID)
	if err != nil || successor.WorkspaceID != before.WorkspaceID {
		r.t.Fatalf("successor session = %+v %v, want it in the run's checkout", successor, err)
	}
	if _, err := os.Stat(filepath.Join(checkout.Path, "ora-v2-result.txt")); err != nil {
		r.t.Fatalf("the successor did not run in the original checkout: %v", err)
	}
}

// TestRealInteractiveWaitSurvivesARestart parks an interactive node on a real Node and OpenCode,
// restarts the whole stack over the same database and data directory, and continues the node: the
// follow-up runs a new Agent in the original checkout with the recorded conversation, the page's
// history stays one conversation without the handoff, and completing the node releases its
// successor in the same checkout.
func TestRealInteractiveWaitSurvivesARestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	r := newRealInteractive(t, ctx)
	rid, session, before, checkout, firstHistory := r.parked()

	// The server restarts: a new process, a new Node incarnation, the same database and data.
	r.current.close(t)
	r.current = r.start()
	r.f.host = r.current.host
	run, nodes := r.awaitRun(rid, awaitingInput("a"))
	if run.Status != store.WorkflowRunAwaitingInput || *node(nodes, "a").SessionID != session {
		t.Fatalf("after the restart run %s node %+v, want A awaiting input on its session", run.Status, node(nodes, "a"))
	}
	if _, err := r.current.host.SendNodeTurn(ctx, r.f.scope, rid, "a", "Inspect README and save the fixture result again."); err != nil {
		t.Fatalf("follow-up after the restart: %v", err)
	}
	r.recovered(rid, session, checkout, firstHistory, 2)
	r.completes(rid, before, checkout)
}

// TestRealSuccessorAfterARestartRunsInTheRunsCheckout completes a parked node right after a
// restart, without another message: its successor still runs in the run's original checkout,
// uncommitted edits included, not in a fresh clone.
func TestRealSuccessorAfterARestartRunsInTheRunsCheckout(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	r := newRealInteractive(t, ctx)
	rid, _, before, checkout, _ := r.parked()
	r.current.close(t)
	r.current = r.start()
	r.f.host = r.current.host
	r.awaitRun(rid, awaitingInput("a"))
	r.completes(rid, before, checkout)
}

// TestRealInteractiveNodeOutlivesItsNode kills the project's Node while an interactive node
// waits, without restarting the server: the node keeps waiting, the next message finds the Node
// gone before reaching an Agent and runs at once on a new Agent in the original checkout, as after
// a restart, and the successor still runs in that checkout on the new Node.
func TestRealInteractiveNodeOutlivesItsNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	r := newRealInteractive(t, ctx)
	rid, session, before, checkout, firstHistory := r.parked()

	killed := 0
	for _, pid := range nodeProcesses(t, r.root) {
		if syscall.Kill(pid, syscall.SIGKILL) == nil {
			killed++
		}
	}
	if killed == 0 {
		t.Fatal("no Node process to kill")
	}
	// The server notices the death when Node's control socket closes, which follows the process
	// being reaped; wait for that, so the message below finds the Node already gone.
	for len(nodeProcesses(t, r.root)) != 0 {
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("the killed Node never exited")
		}
	}
	time.Sleep(time.Second)
	if _, err := r.current.host.SendNodeTurn(ctx, r.f.scope, rid, "a", "Inspect README and save the fixture result again."); err != nil {
		t.Fatalf("message after the Node died: %v", err)
	}
	_, nodes := r.awaitRun(rid, r.turnsOf(2))
	if p := interactivePayload(t, node(nodes, "a")); p["last_stop_reason"] != "end_turn" {
		page, _ := r.current.service.ReadTurns(ctx, r.scope, session, "", 100)
		for _, turn := range page.Items {
			if turn.Result != nil {
				t.Logf("turn %s: %+v", turn.Caller.AttemptID, *turn.Result)
			}
		}
		t.Fatalf("after the Node died the message ended as %v, want it run at once by a new Agent", p)
	}
	r.recovered(rid, session, checkout, firstHistory, 2)
	r.completes(rid, before, checkout)
}

// TestRealUnknownCheckoutAfterARestartFailsTheNodeAndIsForgotten loses the Node's ledger while
// an interactive node waits, keeping the checkout's directory, and restarts the server: Node no
// longer knows the recorded clone execution, so the follow-up fails the node as a checkout that is
// gone, the record is cleared, and resuming the run clones again, its successor included.
func TestRealUnknownCheckoutAfterARestartFailsTheNodeAndIsForgotten(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	r := newRealInteractive(t, ctx)
	rid, session, before, checkout, _ := r.parked()
	r.current.close(t)
	ledgers, err := filepath.Glob(filepath.Join(r.root, "n", "*", "w", "n"))
	if err != nil || len(ledgers) != 1 {
		t.Fatalf("Node homes = %v %v, want the project's one", ledgers, err)
	}
	if err := os.RemoveAll(ledgers[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(checkout.Path); err != nil {
		t.Fatalf("the checkout's directory must stay: %v", err)
	}
	r.current = r.start()
	r.f.host = r.current.host
	r.awaitRun(rid, awaitingInput("a"))
	if _, err := r.current.host.SendNodeTurn(ctx, r.f.scope, rid, "a", "Inspect README and save the fixture result again."); err != nil {
		t.Fatalf("follow-up after the restart: %v", err)
	}
	run, nodes := r.awaitRun(rid, terminal)
	if run.Status != store.WorkflowStatusFailed || errorKind(t, node(nodes, "a")) != "interactive_session" {
		t.Fatalf("run %s node %+v, want A failed as interactive_session", run.Status, node(nodes, "a"))
	}
	if ended, err := r.current.service.ReadSession(ctx, r.scope, session); err != nil || ended.EndedAt == nil {
		t.Fatalf("A's session = %+v %v, want it ended", ended, err)
	}
	if at, err := r.db.ExecutionWorkspace(ctx, before.WorkspaceID); err != nil || at.CloneExecution != "" {
		t.Fatalf("workspace record = %+v %v, want its unknown clone execution cleared", at, err)
	}

	if _, err := r.current.host.ResumeRun(ctx, r.f.scope, rid); err != nil {
		t.Fatalf("resume: %v", err)
	}
	_, nodes = r.awaitRun(rid, func(_ store.WorkflowRun, rows []store.WorkflowNodeRun) bool {
		row := lastAttempt(rows, "a")
		return row.Status == store.WorkflowStatusPending && row.SessionID != nil && *row.SessionID != session && interactivePayload(t, row)["turns"] == float64(1)
	})
	resumed, err := r.current.service.ReadSession(ctx, r.scope, *lastAttempt(nodes, "a").SessionID)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := r.db.ExecutionWorkspace(ctx, resumed.WorkspaceID)
	if err != nil || fresh.CloneExecution == "" || fresh.CloneExecution == checkout.CloneExecution || fresh.Path == checkout.Path {
		t.Fatalf("resumed checkout = %+v %v, want a fresh clone (was %+v)", fresh, err, checkout)
	}
	if _, err := r.current.host.CompleteNode(ctx, r.f.scope, rid, "a", confirm); err != nil {
		t.Fatalf("complete the resumed node: %v", err)
	}
	run, nodes = r.awaitRun(rid, terminal)
	if run.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("run after the resume = %+v nodes = %+v", run, nodes)
	}
}

// lastAttempt is the node's latest attempt row.
func lastAttempt(nodes []store.WorkflowNodeRun, id string) store.WorkflowNodeRun {
	var last store.WorkflowNodeRun
	for _, n := range nodes {
		if n.NodeID == id {
			last = n
		}
	}
	return last
}

// nodeProcesses lists the project Node processes under root: Node runs from its copy "nb" there
// (the reaper, "rb", is left to clean up after it, as when Node crashes).
func nodeProcesses(t *testing.T, root string) []int {
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
		if exe, err := os.Readlink(filepath.Join("/proc", entry.Name(), "exe")); err == nil && strings.HasPrefix(exe, root+string(filepath.Separator)) && filepath.Base(exe) == "nb" {
			pids = append(pids, pid)
		}
	}
	return pids
}

// realTwoParked parks A and B side by side on the run's one checkout; C follows A and retries.
const realTwoParked = `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"Inspect README and save the fixture result.","interactive":true}}},
 {"id":"b","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"Inspect README and save the fixture result.","interactive":true}}},
 {"id":"c","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"Review: {{#a.output#}}","retry":{"enabled":true,"maxRetries":2,"initialDelaySeconds":0}}}}
],"edges":[{"source":"start","target":"a"},{"source":"start","target":"b"},{"source":"a","target":"c"}]}`

// TestRealSecondParkedNodeDoesNotContinueOnAFreshClone parks two nodes on one checkout, loses the
// Node's ledger and restarts. Completing A sends its successor to the unknown checkout, which is
// forgotten, and the successor's retry clones again. B's conversation ran in the forgotten clone:
// its follow-up fails B as interactive_session instead of continuing on the fresh clone without
// B's edits, which stay in the old directory.
func TestRealSecondParkedNodeDoesNotContinueOnAFreshClone(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	r := newRealInteractive(t, ctx)
	rid := r.f.run(t, realTwoParked, nil)
	if _, err := r.current.host.StartRun(ctx, r.f.scope, rid); err != nil {
		t.Fatal(err)
	}
	_, nodes := r.awaitRun(rid, func(_ store.WorkflowRun, rows []store.WorkflowNodeRun) bool {
		for _, id := range []string{"a", "b"} {
			if row := node(rows, id); row.Status != store.WorkflowStatusPending || interactivePayload(t, row)["turns"] != float64(1) {
				return false
			}
		}
		return true
	})
	b, err := r.current.service.ReadSession(ctx, r.scope, *node(nodes, "b").SessionID)
	if err != nil {
		t.Fatal(err)
	}
	old, err := r.db.ExecutionWorkspace(ctx, b.WorkspaceID)
	if err != nil || old.CloneExecution == "" {
		t.Fatalf("checkout = %+v %v", old, err)
	}
	if err := os.WriteFile(filepath.Join(old.Path, "b-edit.txt"), []byte("uncommitted"), 0o600); err != nil {
		t.Fatal(err)
	}

	r.current.close(t)
	ledgers, err := filepath.Glob(filepath.Join(r.root, "n", "*", "w", "n"))
	if err != nil || len(ledgers) != 1 {
		t.Fatalf("Node homes = %v %v, want the project's one", ledgers, err)
	}
	if err := os.RemoveAll(ledgers[0]); err != nil {
		t.Fatal(err)
	}
	r.current = r.start()
	r.f.host = r.current.host
	r.awaitRun(rid, awaitingInput("b"))

	if _, err := r.current.host.CompleteNode(ctx, r.f.scope, rid, "a", confirm); err != nil {
		t.Fatalf("complete A: %v", err)
	}
	r.awaitRun(rid, func(_ store.WorkflowRun, rows []store.WorkflowNodeRun) bool {
		return lastAttempt(rows, "c").Status == store.WorkflowStatusSucceeded
	})
	fresh, err := r.db.ExecutionWorkspace(ctx, b.WorkspaceID)
	if err != nil || fresh.CloneExecution == "" || fresh.CloneExecution == old.CloneExecution {
		t.Fatalf("workspace after C = %+v %v, want C's fresh clone recorded (was %+v)", fresh, err, old)
	}

	if _, err := r.current.host.SendNodeTurn(ctx, r.f.scope, rid, "b", "Inspect README and save the fixture result again."); err != nil {
		t.Fatalf("follow-up on B: %v", err)
	}
	run, nodes := r.awaitRun(rid, func(run store.WorkflowRun, rows []store.WorkflowNodeRun) bool {
		return terminal(run, rows) || interactivePayload(t, node(rows, "b"))["turns"] == float64(2)
	})
	if run.Status != store.WorkflowStatusFailed || errorKind(t, node(nodes, "b")) != "interactive_session" {
		t.Fatalf("run %s B %s, want B failed as interactive_session rather than continued on the fresh clone", run.Status, node(nodes, "b").Payload)
	}
	if _, err := os.Stat(filepath.Join(old.Path, "b-edit.txt")); err != nil {
		t.Fatalf("B's edit must stay in the old directory: %v", err)
	}
}
