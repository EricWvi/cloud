package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

// pendingRun creates a pending run of a published one-node workflow, the state the engine starts.
func pendingRun(t *testing.T, s *store.Store, user, tenant, name string) store.WorkflowRun {
	t.Helper()
	project := workflowProject(t, s, user, tenant, "project-"+name)
	w, _ := publishedWorkflow(t, s, tenant, name, "v1")
	input := "第一版输入"
	run, err := s.CreateWorkflowRun(context.Background(), tenant, w.ID, "", store.WorkflowRunInput{ProjectID: project.ID, Input: &input})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func currentNodes(t *testing.T, s *store.Store, runID string) []string {
	t.Helper()
	c, err := s.WorkflowEngineContext(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		CurrentNodes []string `json:"current_nodes"`
	}
	if err := json.Unmarshal(c.Run.State, &state); err != nil {
		t.Fatalf("decode state %s: %v", c.Run.State, err)
	}
	return state.CurrentNodes
}

func keep(raw []byte) ([]byte, error) { return raw, nil }

func TestWorkflowEngineStartIsOneShot(t *testing.T) {
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "engine-start")
	ctx := context.Background()
	run := pendingRun(t, s, user, tenant, "启动")

	c, err := s.WorkflowEngineContext(ctx, run.ID)
	if err != nil || c.Run.ID != run.ID || c.Run.Status != store.WorkflowStatusPending || c.Graph != graphA {
		t.Fatalf("context = %+v %v", c, err)
	}
	if _, err := s.WorkflowEngineContext(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, store.ErrWorkflowRunNotFound) {
		t.Fatalf("missing run context = %v", err)
	}

	// Input stays editable until the run starts, and the fold sees the stored payload.
	edited := "第二版输入"
	if err := s.UpdateWorkflowEngineRunInput(ctx, run.ID, &edited, func(raw []byte) ([]byte, error) {
		return []byte(`{"edited":true}`), nil
	}); err != nil {
		t.Fatal(err)
	}

	entry := store.WorkflowEngineStart{NodeID: "start", NodeType: "start", Input: &edited}
	var seen []byte
	started, err := s.StartWorkflowEngineRun(ctx, run.ID, entry, func(raw []byte) ([]byte, error) {
		seen = raw
		return []byte(`{"prepared":true}`), nil
	})
	if err != nil || !started {
		t.Fatalf("start = %v %v", started, err)
	}
	if !json.Valid(seen) || string(normalize(t, seen)) != `{"edited":true}` {
		t.Fatalf("prepare saw payload %s", seen)
	}
	c, err = s.WorkflowEngineContext(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Run.Status != store.WorkflowStatusRunning || c.Run.StartedAt == nil || c.Run.Input == nil || *c.Run.Input != edited {
		t.Fatalf("started run = %+v", c.Run)
	}
	if string(normalize(t, c.Run.RuntimePayload)) != `{"prepared":true}` {
		t.Fatalf("payload = %s", c.Run.RuntimePayload)
	}
	if got := currentNodes(t, s, run.ID); !slices.Equal(got, []string{"start"}) {
		t.Fatalf("current_nodes = %v", got)
	}
	nodes, err := s.WorkflowEngineNodeRuns(ctx, run.ID)
	if err != nil || len(nodes) != 1 || nodes[0].NodeID != "start" || nodes[0].Status != store.WorkflowStatusRunning || nodes[0].Input == nil || *nodes[0].Input != edited {
		t.Fatalf("entry attempt = %+v %v", nodes, err)
	}

	// A repeated start neither re-prepares nor inserts a second entry, and input is now frozen.
	again, err := s.StartWorkflowEngineRun(ctx, run.ID, entry, func([]byte) ([]byte, error) {
		t.Fatal("prepare ran for a started run")
		return nil, nil
	})
	if err != nil || again {
		t.Fatalf("repeated start = %v %v", again, err)
	}
	if nodes, _ := s.WorkflowEngineNodeRuns(ctx, run.ID); len(nodes) != 1 {
		t.Fatalf("repeated start inserted attempts: %+v", nodes)
	}
	if err := s.UpdateWorkflowEngineRunInput(ctx, run.ID, &edited, keep); !errors.Is(err, store.ErrWorkflowRunState) {
		t.Fatalf("input on running run = %v", err)
	}
}

func normalize(t *testing.T, raw []byte) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	out, _ := json.Marshal(v)
	return out
}

func TestWorkflowEngineCompleteFoldsAndIgnoresLateCallbacks(t *testing.T) {
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "engine-complete")
	ctx := context.Background()
	run := pendingRun(t, s, user, tenant, "完成")
	if _, err := s.StartWorkflowEngineRun(ctx, run.ID, store.WorkflowEngineStart{NodeID: "start", NodeType: "start"}, func([]byte) ([]byte, error) {
		return []byte(`{"folds":0}`), nil
	}); err != nil {
		t.Fatal(err)
	}
	wave, err := s.StartWorkflowEngineNodes(ctx, run.ID, []store.WorkflowEngineStart{
		{NodeID: "agent-a", NodeType: "agent"}, {NodeID: "cond", NodeType: "condition"},
	})
	if err != nil || len(wave) != 2 || wave[0].NodeID != "agent-a" || wave[1].NodeID != "cond" {
		t.Fatalf("wave = %+v %v", wave, err)
	}
	if got := currentNodes(t, s, run.ID); !slices.Equal(got, []string{"start", "agent-a", "cond"}) {
		t.Fatalf("current_nodes after wave = %v", got)
	}

	answer := "A 的回答"
	var folded store.WorkflowNodeRun
	advanced, err := s.CompleteWorkflowEngineNode(ctx, run.ID, wave[0].ID, &answer,
		func(node []byte) ([]byte, error) { return []byte(`{"stop_reason":"end_turn"}`), nil },
		func(raw []byte, node store.WorkflowNodeRun) ([]byte, error) {
			folded = node
			return []byte(`{"folds":1}`), nil
		})
	if err != nil || !advanced || folded.ID != wave[0].ID {
		t.Fatalf("complete = %v %v (fold saw %+v)", advanced, err, folded)
	}
	nodes, err := s.WorkflowEngineNodeRuns(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	agent := nodes[1]
	if agent.Status != store.WorkflowStatusSucceeded || agent.Output == nil || *agent.Output != answer || agent.FinishedAt == nil ||
		string(normalize(t, agent.Payload)) != `{"stop_reason":"end_turn"}` {
		t.Fatalf("completed attempt = %+v", agent)
	}
	if got := currentNodes(t, s, run.ID); !slices.Equal(got, []string{"start", "cond"}) {
		t.Fatalf("current_nodes after completion = %v", got)
	}

	// A duplicate callback neither rewrites the answer nor folds the payload a second time.
	late := "迟到"
	advanced, err = s.CompleteWorkflowEngineNode(ctx, run.ID, wave[0].ID, &late, keep, func([]byte, store.WorkflowNodeRun) ([]byte, error) {
		t.Fatal("fold ran for a finished attempt")
		return nil, nil
	})
	if err != nil || advanced {
		t.Fatalf("duplicate complete = %v %v", advanced, err)
	}
	if missing, err := s.CompleteWorkflowEngineNode(ctx, run.ID, "00000000-0000-0000-0000-000000000000", &late, keep, nil); err != nil || missing {
		t.Fatalf("unknown attempt = %v %v", missing, err)
	}

	// Condition decisions stay scheduler state, never node output.
	decision := "case-1"
	if _, err := s.CompleteWorkflowEngineNode(ctx, run.ID, wave[1].ID, &decision, keep, func(raw []byte, _ store.WorkflowNodeRun) ([]byte, error) {
		return []byte(`{"folds":2}`), nil
	}); err != nil {
		t.Fatal(err)
	}
	c, _ := s.WorkflowEngineContext(ctx, run.ID)
	if string(normalize(t, c.Run.RuntimePayload)) != `{"folds":2}` {
		t.Fatalf("run payload = %s", c.Run.RuntimePayload)
	}
	nodes, _ = s.WorkflowEngineNodeRuns(ctx, run.ID)
	if nodes[2].Status != store.WorkflowStatusSucceeded || nodes[2].Output != nil {
		t.Fatalf("condition attempt = %+v", nodes[2])
	}

	output := `{"answer":"A 的回答"}`
	if err := s.FinishWorkflowEngineRun(ctx, run.ID, &output); err != nil {
		t.Fatal(err)
	}
	c, _ = s.WorkflowEngineContext(ctx, run.ID)
	if c.Run.Status != store.WorkflowStatusSucceeded || c.Run.Output == nil || *c.Run.Output != output || c.Run.FinishedAt == nil {
		t.Fatalf("finished run = %+v", c.Run)
	}
	if got := currentNodes(t, s, run.ID); len(got) != 0 {
		t.Fatalf("current_nodes after finish = %v", got)
	}
	if _, err := s.StartWorkflowEngineNodes(ctx, run.ID, []store.WorkflowEngineStart{{NodeID: "x", NodeType: "agent"}}); !errors.Is(err, store.ErrWorkflowRunState) {
		t.Fatalf("wave on finished run = %v", err)
	}
}

func TestWorkflowEngineFailureFailsTheRunOnce(t *testing.T) {
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "engine-fail")
	ctx := context.Background()
	run := pendingRun(t, s, user, tenant, "失败")
	if _, err := s.StartWorkflowEngineRun(ctx, run.ID, store.WorkflowEngineStart{NodeID: "start", NodeType: "start"}, keep); err != nil {
		t.Fatal(err)
	}
	wave, err := s.StartWorkflowEngineNodes(ctx, run.ID, []store.WorkflowEngineStart{
		{NodeID: "agent-a", NodeType: "agent"}, {NodeID: "agent-b", NodeType: "agent"},
	})
	if err != nil {
		t.Fatal(err)
	}

	raw := "半截回答"
	var attempt int
	advanced, err := s.FailWorkflowEngineNode(ctx, run.ID, wave[0].ID, "第一次失败", &raw, func(n int, node, runPayload []byte) ([]byte, error) {
		attempt = n
		return []byte(`{"error_detail":{"kind":"session"}}`), nil
	})
	if err != nil || !advanced || attempt != 1 {
		t.Fatalf("fail = %v %v attempt %d", advanced, err, attempt)
	}
	c, _ := s.WorkflowEngineContext(ctx, run.ID)
	if c.Run.Status != store.WorkflowStatusFailed || c.Run.Error == nil || *c.Run.Error != "第一次失败" || c.Run.FinishedAt == nil {
		t.Fatalf("failed run = %+v", c.Run)
	}
	if got := currentNodes(t, s, run.ID); !slices.Equal(got, []string{"agent-a"}) {
		t.Fatalf("current_nodes anchor = %v", got)
	}
	nodes, _ := s.WorkflowEngineNodeRuns(ctx, run.ID)
	failed := nodes[1]
	if failed.Status != store.WorkflowStatusFailed || failed.Error == nil || *failed.Error != "第一次失败" || failed.Output == nil || *failed.Output != raw ||
		string(normalize(t, failed.Payload)) != `{"error_detail":{"kind":"session"}}` {
		t.Fatalf("failed attempt = %+v", failed)
	}

	// A late duplicate is ignored; a sibling that fails afterwards records its own row but cannot
	// overwrite the run's terminal error.
	if again, err := s.FailWorkflowEngineNode(ctx, run.ID, wave[0].ID, "重复", nil, func(int, []byte, []byte) ([]byte, error) {
		t.Fatal("detail built for a finished attempt")
		return nil, nil
	}); err != nil || again {
		t.Fatalf("duplicate fail = %v %v", again, err)
	}
	if _, err := s.FailWorkflowEngineNode(ctx, run.ID, wave[1].ID, "兄弟失败", nil, func(int, []byte, []byte) ([]byte, error) {
		return []byte(`{}`), nil
	}); err != nil {
		t.Fatal(err)
	}
	c, _ = s.WorkflowEngineContext(ctx, run.ID)
	if c.Run.Status != store.WorkflowStatusFailed || *c.Run.Error != "第一次失败" {
		t.Fatalf("sibling overwrote the run: %+v", c.Run)
	}
	// Finishing a failed run is a no-op.
	if err := s.FinishWorkflowEngineRun(ctx, run.ID, nil); err != nil {
		t.Fatal(err)
	}
	if c, _ = s.WorkflowEngineContext(ctx, run.ID); c.Run.Status != store.WorkflowStatusFailed {
		t.Fatalf("finish overwrote a failed run: %+v", c.Run)
	}
}

func TestInterruptWorkflowRunsFailsOnlyLiveWork(t *testing.T) {
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "engine-interrupt")
	ctx := context.Background()

	live := pendingRun(t, s, user, tenant, "执行中")
	if _, err := s.StartWorkflowEngineRun(ctx, live.ID, store.WorkflowEngineStart{NodeID: "start", NodeType: "start"}, keep); err != nil {
		t.Fatal(err)
	}
	nodes, _ := s.WorkflowEngineNodeRuns(ctx, live.ID)
	answer := ""
	if _, err := s.CompleteWorkflowEngineNode(ctx, live.ID, nodes[0].ID, &answer, keep, func(raw []byte, _ store.WorkflowNodeRun) ([]byte, error) { return raw, nil }); err != nil {
		t.Fatal(err)
	}
	wave, err := s.StartWorkflowEngineNodes(ctx, live.ID, []store.WorkflowEngineStart{{NodeID: "agent-a", NodeType: "agent"}})
	if err != nil {
		t.Fatal(err)
	}
	untouched := pendingRun(t, s, user, tenant, "未启动")
	done := pendingRun(t, s, user, tenant, "已结束")
	if _, err := s.StartWorkflowEngineRun(ctx, done.ID, store.WorkflowEngineStart{NodeID: "start", NodeType: "start"}, keep); err != nil {
		t.Fatal(err)
	}
	doneNodes, _ := s.WorkflowEngineNodeRuns(ctx, done.ID)
	if _, err := s.CompleteWorkflowEngineNode(ctx, done.ID, doneNodes[0].ID, &answer, keep, func(raw []byte, _ store.WorkflowNodeRun) ([]byte, error) { return raw, nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishWorkflowEngineRun(ctx, done.ID, nil); err != nil {
		t.Fatal(err)
	}

	built := map[int]int{}
	interrupted, err := s.InterruptWorkflowRuns(ctx, "进程重启，运行中断", func(attempt int, node []byte) ([]byte, error) {
		built[attempt]++
		return []byte(`{"error_detail":{"kind":"interrupted_by_restart"}}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(interrupted, []string{live.ID}) || built[1] != 1 || len(built) != 1 {
		t.Fatalf("interrupted = %v, details built %v", interrupted, built)
	}
	c, _ := s.WorkflowEngineContext(ctx, live.ID)
	if c.Run.Status != store.WorkflowStatusFailed || c.Run.Error == nil || *c.Run.Error != "进程重启，运行中断" {
		t.Fatalf("interrupted run = %+v", c.Run)
	}
	nodes, _ = s.WorkflowEngineNodeRuns(ctx, live.ID)
	if nodes[0].Status != store.WorkflowStatusSucceeded || nodes[1].ID != wave[0].ID || nodes[1].Status != store.WorkflowStatusFailed ||
		string(normalize(t, nodes[1].Payload)) != `{"error_detail":{"kind":"interrupted_by_restart"}}` {
		t.Fatalf("interrupted attempts = %+v", nodes)
	}
	for _, id := range []string{untouched.ID, done.ID} {
		before := map[string]string{untouched.ID: store.WorkflowStatusPending, done.ID: store.WorkflowStatusSucceeded}[id]
		if c, _ := s.WorkflowEngineContext(ctx, id); c.Run.Status != before {
			t.Fatalf("run %s changed to %s", id, c.Run.Status)
		}
	}
	// A second boot finds nothing left to interrupt.
	if again, err := s.InterruptWorkflowRuns(ctx, "再次重启", func(int, []byte) ([]byte, error) { return []byte(`{}`), nil }); err != nil || len(again) != 0 {
		t.Fatalf("second interrupt = %v %v", again, err)
	}
}

// A run left running between a completion and its next wave has no active node row: the boot
// sweep must still fail it (and its open Loop scopes), with and without the absorb callback.
// The absorb callback can only exclude runs it was asked about, and a run without rows is never
// asked about, so no callback can keep the set-based guarantee from applying to it.
func TestInterruptWorkflowRunsFailsARunWithoutActiveRows(t *testing.T) {
	for _, mode := range []string{"plain", "absorb-none", "absorb-all"} {
		t.Run(mode, func(t *testing.T) {
			s := storetest.Open(t)
			user, tenant := storetest.Login(t, s, "engine-interrupt-gap-"+mode)
			ctx := context.Background()
			gap := pendingRun(t, s, user, tenant, "间隙"+mode)
			if _, err := s.StartWorkflowEngineRun(ctx, gap.ID, store.WorkflowEngineStart{NodeID: "start", NodeType: "start"}, keep); err != nil {
				t.Fatal(err)
			}
			nodes, err := s.WorkflowEngineNodeRuns(ctx, gap.ID)
			if err != nil {
				t.Fatal(err)
			}
			answer := ""
			if _, err := s.CompleteWorkflowEngineNode(ctx, gap.ID, nodes[0].ID, &answer, keep, func(raw []byte, _ store.WorkflowNodeRun) ([]byte, error) { return raw, nil }); err != nil {
				t.Fatal(err)
			}
			// A second run holds an active Loop row and an open round: it fails through its rows
			// unless the absorb callback keeps it, and its scope must fail with it.
			scoped := pendingRun(t, s, user, tenant, "带轮次"+mode)
			// Absorption only counts when the run has a durable starter to resume under; give
			// the absorbed case one before it starts.
			if mode == "absorb-all" {
				if err := s.SetWorkflowStarter(ctx, tenant, scoped.ID, user); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.StartWorkflowEngineRun(ctx, scoped.ID, store.WorkflowEngineStart{NodeID: "start", NodeType: "start"}, keep); err != nil {
				t.Fatal(err)
			}
			scopedNodes, _ := s.WorkflowEngineNodeRuns(ctx, scoped.ID)
			if _, err := s.CompleteWorkflowEngineNode(ctx, scoped.ID, scopedNodes[0].ID, &answer, keep, func(raw []byte, _ store.WorkflowNodeRun) ([]byte, error) { return raw, nil }); err != nil {
				t.Fatal(err)
			}
			loop, err := s.CreateWorkflowNodeRun(ctx, tenant, scoped.ID, store.WorkflowNodeRunInput{NodeID: "loop-1", NodeType: "loop"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.StartWorkflowLoopRound(ctx, tenant, scoped.ID, loop.ID, 1, []byte(`{"conditionDecisions":{}}`)); err != nil {
				t.Fatal(err)
			}

			detail := func(int, []byte) ([]byte, error) { return []byte(`{}`), nil }
			var interrupted []string
			if mode == "plain" {
				interrupted, err = s.InterruptWorkflowRuns(ctx, "进程重启，运行中断", detail)
			} else {
				absorb := func(string, []store.WorkflowNodeRun) map[string]bool {
					if mode == "absorb-none" {
						return nil
					}
					return map[string]bool{}
				}
				interrupted, err = s.InterruptWorkflowRunsWithIterationRecovery(ctx, "进程重启，运行中断", detail, absorb)
			}
			if err != nil {
				t.Fatal(err)
			}
			// The scoped run is reported through its rows (failed or absorbed), the gap run through
			// the run UPDATE that failed it without any.
			if want := []string{scoped.ID, gap.ID}; !slices.Equal(interrupted, want) {
				t.Fatalf("interrupted = %v, want %v (the run with active rows, then the gap run)", interrupted, want)
			}
			c, err := s.WorkflowEngineContext(ctx, gap.ID)
			if err != nil {
				t.Fatal(err)
			}
			if c.Run.Status != store.WorkflowStatusFailed || c.Run.Error == nil || *c.Run.Error != "进程重启，运行中断" {
				t.Fatalf("run without rows left %s (error %v) after the boot sweep", c.Run.Status, c.Run.Error)
			}
			c, err = s.WorkflowEngineContext(ctx, scoped.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantScoped := store.WorkflowStatusFailed
			if mode == "absorb-all" {
				wantScoped = store.WorkflowStatusRunning
			}
			if c.Run.Status != wantScoped {
				t.Fatalf("run with rows = %s, want %s", c.Run.Status, wantScoped)
			}
			scopes, err := s.WorkflowRunScopes(ctx, tenant, scoped.ID)
			if err != nil || len(scopes) != 1 {
				t.Fatalf("scopes = %+v %v", scopes, err)
			}
			wantScope := store.WorkflowStatusFailed
			if mode == "absorb-all" {
				wantScope = store.WorkflowStatusRunning
			}
			if scopes[0].Status == nil || *scopes[0].Status != wantScope {
				t.Fatalf("scope = %v, want %s", scopes[0].Status, wantScope)
			}
		})
	}
}

// An absorb decision is only honoured for a run with a durable starter: without one, nobody can
// resume the run, so the sweep fails it like every other interrupted run instead of leaving it
// running forever (or making the boot fail on it).
func TestInterruptWorkflowRunsRefusesToAbsorbWithoutAStarter(t *testing.T) {
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "engine-interrupt-nostarter")
	ctx := context.Background()
	run := pendingRun(t, s, user, tenant, "无启动者")
	if _, err := s.StartWorkflowEngineRun(ctx, run.ID, store.WorkflowEngineStart{NodeID: "start", NodeType: "start"}, keep); err != nil {
		t.Fatal(err)
	}
	nodes, _ := s.WorkflowEngineNodeRuns(ctx, run.ID)
	answer := ""
	if _, err := s.CompleteWorkflowEngineNode(ctx, run.ID, nodes[0].ID, &answer, keep, func(raw []byte, _ store.WorkflowNodeRun) ([]byte, error) { return raw, nil }); err != nil {
		t.Fatal(err)
	}
	// A running Iteration owner with a member row: the absorb callback would keep the run.
	if _, err := s.CreateWorkflowNodeRun(ctx, tenant, run.ID, store.WorkflowNodeRunInput{NodeID: "iter", NodeType: "iteration"}); err != nil {
		t.Fatal(err)
	}
	iteration := 0
	member, err := s.CreateWorkflowNodeRun(ctx, tenant, run.ID, store.WorkflowNodeRunInput{NodeID: "worker", NodeType: "agent", Iteration: &iteration})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InterruptWorkflowRunsWithIterationRecovery(ctx, "进程重启，运行中断",
		func(int, []byte) ([]byte, error) { return []byte(`{}`), nil },
		func(string, []store.WorkflowNodeRun) map[string]bool { return map[string]bool{member.ID: true} }); err != nil {
		t.Fatal(err)
	}
	c, err := s.WorkflowEngineContext(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Run.Status != store.WorkflowStatusFailed || c.Run.Error == nil {
		t.Fatalf("run without a starter survived the sweep: %+v", c.Run)
	}
	rows, err := s.WorkflowRunNodes(ctx, tenant, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.NodeID != "iter" && row.NodeID != "worker" {
			continue
		}
		if row.Status != store.WorkflowStatusFailed {
			t.Fatalf("row %s = %s, want failed", row.NodeID, row.Status)
		}
	}
}

func TestWorkflowEngineAbandonClosesOnlyUnstartedAttemptsOfEndedRuns(t *testing.T) {
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "engine-abandon")
	ctx := context.Background()
	run := pendingRun(t, s, user, tenant, "放弃")
	if _, err := s.StartWorkflowEngineRun(ctx, run.ID, store.WorkflowEngineStart{NodeID: "start", NodeType: "start"}, keep); err != nil {
		t.Fatal(err)
	}
	wave, err := s.StartWorkflowEngineNodes(ctx, run.ID, []store.WorkflowEngineStart{
		{NodeID: "agent-a", NodeType: "agent"}, {NodeID: "agent-b", NodeType: "agent"}, {NodeID: "agent-c", NodeType: "agent"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// While the run is running every queued attempt must execute.
	if abandoned, err := s.AbandonWorkflowEngineNode(ctx, run.ID, wave[1].ID); err != nil || abandoned {
		t.Fatalf("abandon in a running run = %v %v", abandoned, err)
	}

	// agent-c already holds a session when agent-a fails the run.
	if _, err := s.BindWorkflowNodeSession(ctx, tenant, run.ID, wave[2].ID, "session-c"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FailWorkflowEngineNode(ctx, run.ID, wave[0].ID, "失败", nil, func(int, []byte, []byte) ([]byte, error) {
		return []byte(`{}`), nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, n := range wave[1:] {
		if abandoned, err := s.AbandonWorkflowEngineNode(ctx, run.ID, n.ID); err != nil || !abandoned {
			t.Fatalf("abandon %s in a failed run = %v %v", n.NodeID, abandoned, err)
		}
	}
	nodes, _ := s.WorkflowEngineNodeRuns(ctx, run.ID)
	status := map[string]string{}
	for _, n := range nodes {
		status[n.NodeID] = n.Status
	}
	want := map[string]string{"start": "running", "agent-a": "failed", "agent-b": "cancelled", "agent-c": "running"}
	for id, s := range want {
		if status[id] != s {
			t.Fatalf("statuses = %v, want %v", status, want)
		}
	}
	if b := nodes[2]; b.FinishedAt == nil {
		t.Fatalf("abandoned attempt has no finish time: %+v", b)
	}
	if got := currentNodes(t, s, run.ID); !slices.Equal(got, []string{"agent-a"}) {
		t.Fatalf("abandon moved the failure anchor: %v", got)
	}
}
