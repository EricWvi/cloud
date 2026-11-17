package executionruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// recoveryAgent records what each turn was run with and answers from a script.
type recoveryAgent struct {
	mu      sync.Mutex
	runs    []execution.Session
	prompts []string
	// results, when not empty, answer the next turns in order instead of a completed one.
	results []execution.Result
	ended   []string
	// reattached is the checkout each workspace was reattached to: clone execution and path.
	reattached map[string][2]string
}

func (a *recoveryAgent) Run(ctx context.Context, _ execution.Scope, session execution.Session, turn execution.Turn, emit func(context.Context, execution.Event) error) (execution.Result, error) {
	text, _ := turn.Prompt.Text()
	a.mu.Lock()
	a.runs = append(a.runs, session)
	a.prompts = append(a.prompts, text)
	result := execution.Result{Status: execution.StatusCompleted, Answer: "答复：" + text, AnswerComplete: true, StopReason: "end_turn"}
	var err error
	if len(a.results) > 0 {
		result, a.results = a.results[0], a.results[1:]
		if result.NotStarted {
			err = errors.New(result.FailureCode)
		}
	}
	a.mu.Unlock()
	if err != nil {
		return result, err
	}
	return result, emit(ctx, execution.Event{Kind: execution.EventTurnEnded, Result: &result})
}

func (a *recoveryAgent) End(_ context.Context, sid string) error {
	a.mu.Lock()
	a.ended = append(a.ended, sid)
	a.mu.Unlock()
	return nil
}
func (*recoveryAgent) Rebuild(string) {}
func (a *recoveryAgent) Reattach(workspace, clone, path string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.reattached == nil {
		a.reattached = map[string][2]string{}
	}
	a.reattached[workspace] = [2]string{clone, path}
}
func (*recoveryAgent) Close(context.Context) error { return nil }

func (a *recoveryAgent) last() (execution.Session, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runs[len(a.runs)-1], a.prompts[len(a.prompts)-1]
}

const recoveryGraph = `{"nodes":[{"id":"start","data":{"kind":"start"}},{"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"起草","interactive":true}}}],"edges":[{"source":"start","target":"a"}]}`

// parkedSession starts a held workflow session whose node is parked awaiting input, records its
// checkout at dir and a page history, and restarts the service over the same database.
func parkedSession(t *testing.T, f *runtimeFixture, agent *recoveryAgent, dir string) (execution.Ref, store.WorkflowNodeRun) {
	t.Helper()
	ctx := f.ctx
	if err := f.service.Close(ctx); err != nil {
		t.Fatal(err)
	}
	var err error
	if f.service, err = New(ctx, Config{Store: f.db, Models: f.models, Agent: agent}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.CreateProject(ctx, f.scope, store.Project{ID: "project", Name: "Demo", RepositoryURL: "https://example.com/demo.git", Commit: "fixed", MirrorPath: "/fake/unused"}); err != nil {
		t.Fatal(err)
	}
	w, _, err := f.db.CreateWorkflow(ctx, f.scope.TenantID, "交互恢复", recoveryGraph)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.db.PublishWorkflow(ctx, f.scope.TenantID, w.ID, "v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.db.CreateWorkflowRun(ctx, f.scope.TenantID, w.ID, snapshot.ID, store.WorkflowRunInput{ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.StartWorkflowRun(ctx, f.scope.TenantID, run.ID); err != nil {
		t.Fatal(err)
	}
	node, err := f.db.CreateWorkflowNodeRun(ctx, f.scope.TenantID, run.ID, store.WorkflowNodeRunInput{NodeID: "a", NodeType: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := f.service.Start(ctx, f.scope, execution.StartRequest{Caller: execution.Caller{Kind: execution.CallerWorkflow, ID: run.ID, NodeID: "a", AttemptID: node.ID},
		Target: execution.Target{ProjectID: "project"}, ExecutorID: "opencode:fixture", Prompt: execution.TextPrompt("起草方案"), Lifetime: execution.LifetimeHeld})
	if err != nil {
		t.Fatal(err)
	}
	awaitResult(t, f, ref)
	session, err := f.db.ReadExecutionSession(ctx, f.scope, ref.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.RecordWorkspace(ctx, session.WorkspaceID, store.WorkspaceLocation{Path: dir, BaseCommit: "fixed", CloneExecution: "c-old-incarnation"}); err != nil {
		t.Fatal(err)
	}
	history := &execution.HistorySnapshot{Records: []execution.HistoryRecord{historyRecord(t, 1, chunk("user_message_chunk", "起草方案")), historyRecord(t, 2, chunk("agent_message_chunk", "答复：起草方案")), historyRecord(t, 3, turnEnded())}}
	if _, err := f.db.AppendExecutionEvent(ctx, f.scope, execution.Event{Ref: ref, Kind: execution.EventHistorySnapshot, History: history}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.BindWorkflowNodeSession(ctx, f.scope.TenantID, run.ID, node.ID, ref.SessionID); err != nil {
		t.Fatal(err)
	}
	if moved, err := f.db.MoveWorkflowEngineNode(ctx, run.ID, node.ID, store.WorkflowStatusRunning, store.WorkflowStatusPending,
		func([]byte) ([]byte, error) { return []byte(`{"interactive":{"turns":1}}`), nil }); err != nil || !moved {
		t.Fatalf("park = %v %v", moved, err)
	}
	// The process stops; the next one recovers over the same database.
	if err := f.service.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if f.service, err = New(ctx, Config{Store: f.db, Models: f.models, Agent: agent}); err != nil {
		t.Fatal(err)
	}
	return ref, node
}

func awaitResult(t *testing.T, f *runtimeFixture, ref execution.Ref) execution.Result {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		turns, err := f.db.ReadExecutionTurns(f.ctx, f.scope, ref.SessionID, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, turn := range turns.Items {
			if turn.Ref == ref && turn.Result != nil {
				return *turn.Result
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("turn %+v never ended", ref)
	return execution.Result{}
}

func followUp(t *testing.T, f *runtimeFixture, session string, node store.WorkflowNodeRun, n int, prompt string) (execution.Ref, error) {
	t.Helper()
	return f.service.Append(f.ctx, f.scope, execution.AppendRequest{SessionID: session,
		Caller: execution.Caller{Kind: execution.CallerWorkflow, ID: node.RunID, NodeID: "a", AttemptID: node.ID + "/turn-" + strconv.Itoa(n)}, Prompt: execution.TextPrompt(prompt)})
}

func TestRecoveredSessionStartsItsAgentOnTheRecordedCheckout(t *testing.T) {
	f := runtimeDatabase(t, nil)
	agent := &recoveryAgent{}
	dir := t.TempDir()
	first, node := parkedSession(t, f, agent, dir)

	session, err := f.db.ReadExecutionSession(f.ctx, f.scope, first.SessionID)
	if err != nil || session.EndedAt != nil {
		t.Fatalf("session after restart = %+v %v, want the parked node's session open", session, err)
	}
	ref, err := followUp(t, f, first.SessionID, node, 1, "再改一版")
	if err != nil {
		t.Fatalf("follow-up after restart: %v", err)
	}
	if r := awaitResult(t, f, ref); r.Status != execution.StatusCompleted {
		t.Fatalf("recovered turn = %+v", r)
	}
	run, prompt := agent.last()
	agent.mu.Lock()
	reattached := agent.reattached[run.WorkspaceID]
	agent.mu.Unlock()
	if prompt != "再改一版" || run.Recovery == nil || reattached != [2]string{"c-old-incarnation", dir} {
		t.Fatalf("recovered turn ran with %q %+v on %v, want the typed prompt on the recorded checkout", prompt, run.Recovery, reattached)
	}
	if !strings.Contains(run.Recovery.Handoff, "答复：起草方案") || len(run.Recovery.History) != 3 {
		t.Fatalf("recovery handoff %q history %d, want the recorded conversation", run.Recovery.Handoff, len(run.Recovery.History))
	}
	turns, err := f.db.ReadExecutionTurns(f.ctx, f.scope, first.SessionID, "", 100)
	if err != nil || len(turns.Items) != 2 {
		t.Fatalf("turns = %+v %v", turns, err)
	}
	if text, _ := turns.Items[1].Prompt.Text(); text != "再改一版" {
		t.Fatalf("recorded prompt = %q, want what was typed without the handoff", text)
	}

	// The new Agent holds the conversation now: the next turn continues it as usual.
	ref, err = followUp(t, f, first.SessionID, node, 2, "定稿")
	if err != nil {
		t.Fatal(err)
	}
	awaitResult(t, f, ref)
	if run, _ := agent.last(); run.Recovery != nil {
		t.Fatalf("second turn after the restart recovered again: %+v", run.Recovery)
	}
}

func TestRecoveredSessionWaitsWhenItsAgentCannotStart(t *testing.T) {
	f := runtimeDatabase(t, nil)
	agent := &recoveryAgent{}
	first, node := parkedSession(t, f, agent, t.TempDir())
	agent.mu.Lock()
	agent.results = []execution.Result{{Status: execution.StatusFailed, FailureCode: "agent_prepare_failed", NotStarted: true}}
	agent.mu.Unlock()

	ref, err := followUp(t, f, first.SessionID, node, 1, "再改一版")
	if err != nil {
		t.Fatal(err)
	}
	if r := awaitResult(t, f, ref); r.Status != execution.StatusFailed || r.AgentLost {
		t.Fatalf("unstarted recovery = %+v, want a failed turn that keeps the session", r)
	}
	if session, err := f.db.ReadExecutionSession(f.ctx, f.scope, first.SessionID); err != nil || session.EndedAt != nil {
		t.Fatalf("session = %+v %v, want it still open", session, err)
	}
	ref, err = followUp(t, f, first.SessionID, node, 2, "再试一次")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	awaitResult(t, f, ref)
	if run, _ := agent.last(); run.Recovery == nil {
		t.Fatal("the retry did not recover the Agent")
	}
}

func TestRecoveredSessionEndsWhenItsCheckoutIsGone(t *testing.T) {
	f := runtimeDatabase(t, nil)
	agent := &recoveryAgent{}
	dir := filepath.Join(t.TempDir(), "checkout")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	first, node := parkedSession(t, f, agent, dir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	agent.mu.Lock()
	runs := len(agent.runs)
	agent.mu.Unlock()

	ref, err := followUp(t, f, first.SessionID, node, 1, "再改一版")
	if err != nil {
		t.Fatal(err)
	}
	if r := awaitResult(t, f, ref); r.FailureCode != execution.FailureWorkspaceMissing || !r.AgentLost {
		t.Fatalf("turn without a checkout = %+v", r)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.runs) != runs {
		t.Fatal("an Agent was started without its checkout")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if session, err := f.db.ReadExecutionSession(f.ctx, f.scope, first.SessionID); err == nil && session.EndedAt != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the session stayed open without its checkout")
}

func TestRunOfANodeParkedOnAnEndedSessionKeepsItsCheckout(t *testing.T) {
	f := runtimeDatabase(t, nil)
	agent := &recoveryAgent{}
	dir := t.TempDir()
	first, _ := parkedSession(t, f, agent, dir)
	// The starter stopped the node's turn before the restart: its session ended, and the node
	// waits only to be completed. Its run still owns the directory.
	if err := f.service.End(f.ctx, f.scope, first.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	next := &recoveryAgent{}
	var err error
	if f.service, err = New(f.ctx, Config{Store: f.db, Models: f.models, Agent: next}); err != nil {
		t.Fatal(err)
	}
	session, err := f.db.ReadExecutionSession(f.ctx, f.scope, first.SessionID)
	if err != nil || session.EndedAt == nil {
		t.Fatalf("session = %+v %v, want it still ended", session, err)
	}
	next.mu.Lock()
	defer next.mu.Unlock()
	if got := next.reattached[session.WorkspaceID]; got != [2]string{"c-old-incarnation", dir} {
		t.Fatalf("reattached = %v, want the run's recorded checkout for its later sessions", next.reattached)
	}
}
