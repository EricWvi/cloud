package workflow

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// resumeGraph is start → a → b → c next to an unrelated start → d, an Iteration `iter` whose
// region is gate → worker followed by `after`, and a Loop `loop` whose body is entry → writer
// followed by `out`.
const resumeGraph = `{"schemaVersion":2,"nodes":[
 {"id":"start","data":{"kind":"start","inputVariables":[{"name":"items","valueType":"array[string]","value":["x"]}]}},
 {"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"c","modelId":"m"},"prompt":"a"}}},
 {"id":"b","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"c","modelId":"m"},"prompt":"b"}}},
 {"id":"c","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"c","modelId":"m"},"prompt":"c"}}},
 {"id":"d","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"c","modelId":"m"},"prompt":"d"}}},
 {"id":"iter","data":{"kind":"iteration","iterationConfig":{"iteratorSelector":["start","items"],"collectSelector":["worker","output"],"errorStrategy":"fail"}}},
 {"id":"gate","parentId":"iter","data":{"kind":"condition","cases":[{"id":"go","logic":"and","conditions":[{"variableSelector":["iter","item"],"operator":"not_empty"}]}]}},
 {"id":"worker","parentId":"iter","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"c","modelId":"m"},"prompt":"w"}}},
 {"id":"after","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"c","modelId":"m"},"prompt":"after"}}},
 {"id":"loop","data":{"kind":"loop","loopConfig":{"maxIterations":2,"variables":[],"until":{"logic":"and","conditions":[{"variableSelector":["writer","output"],"operator":"equals","value":"done"}]},"outputs":[{"name":"result","variableSelector":["writer","output"]}]}}},
 {"id":"entry","data":{"kind":"start","containerId":"loop"}},
 {"id":"writer","data":{"kind":"agent","containerId":"loop","agentConfig":{"executor":{"agentCli":"c","modelId":"m"},"prompt":"w"}}},
 {"id":"out","data":{"kind":"output","outputs":[{"name":"r","variableSelector":["loop","result"]}]}}
],"edges":[
 {"source":"start","target":"a"},{"source":"a","target":"b"},{"source":"b","target":"c"},{"source":"start","target":"d"},
 {"source":"start","target":"iter"},{"source":"iter","target":"gate"},{"source":"gate","sourceHandle":"go","target":"worker"},{"source":"iter","target":"after"},
 {"source":"start","target":"loop"},{"source":"entry","target":"writer"},{"source":"loop","target":"out"}
]}`

func row(id, node, kind, status string) NodeRun {
	return NodeRun{ID: id, NodeID: node, NodeType: kind, Status: status}
}

func TestPlanResumeMatchesDesktopRules(t *testing.T) {
	g, err := ParseGraph(resumeGraph)
	if err != nil {
		t.Fatal(err)
	}
	ok := func(id, node, kind string) NodeRun { return row(id, node, kind, StatusSucceeded) }
	for _, tc := range []struct {
		name string
		st   ResumeState
		want ResumePlan
	}{
		{"succeeded run", ResumeState{Status: StatusSucceeded}, ResumePlan{Refusal: ResumeRefusedStatus}},
		{"running run", ResumeState{Status: StatusRunning, Nodes: []NodeRun{row("1", "b", "agent", StatusFailed)}}, ResumePlan{Refusal: ResumeRefusedStatus}},
		{"pending run", ResumeState{Status: StatusPending}, ResumePlan{Refusal: ResumeRefusedStatus}},
		{
			// Start → A succeeds, B fails, C waits; the unrelated branch D stays.
			"ordinary node and its descendants",
			ResumeState{Status: StatusFailed, Nodes: []NodeRun{ok("s", "start", "start"), ok("1", "a", "agent"), ok("2", "d", "agent"), row("3", "b", "agent", StatusFailed)}},
			ResumePlan{Failed: []ResumeFailedNode{{NodeID: "b", NodeRunID: "3", Status: StatusFailed}}, Clear: []string{"b", "c"}},
		},
		{
			"cancelled node of a cancelled run",
			ResumeState{Status: StatusCancelled, Nodes: []NodeRun{ok("s", "start", "start"), row("1", "a", "agent", StatusCancelled)}},
			ResumePlan{Failed: []ResumeFailedNode{{NodeID: "a", NodeRunID: "1", Status: StatusCancelled}}, Clear: []string{"a", "b", "c"}},
		},
		{
			"a running sibling blocks the resume",
			ResumeState{Status: StatusFailed, Nodes: []NodeRun{row("1", "d", "agent", StatusRunning), row("2", "b", "agent", StatusFailed)}},
			ResumePlan{Refusal: ResumeRefusedRunning, Running: []string{"d"}},
		},
		{
			// A container parked by the terminal run has nothing in flight (region.rs:12-24).
			"a parked container does not block",
			ResumeState{Status: StatusFailed, Nodes: []NodeRun{row("1", "loop", "loop", StatusRunning), row("2", "iter", "iteration", StatusRunning), row("3", "b", "agent", StatusFailed)}},
			ResumePlan{Failed: []ResumeFailedNode{{NodeID: "b", NodeRunID: "3", Status: StatusFailed}}, Clear: []string{"b", "c"}},
		},
		{
			"an Iteration member restarts its container",
			ResumeState{Status: StatusFailed, Nodes: []NodeRun{row("1", "iter", "iteration", StatusFailed), ok("2", "gate", "condition"), row("3", "worker", "agent", StatusFailed)}},
			ResumePlan{Failed: []ResumeFailedNode{{NodeID: "iter", NodeRunID: "1", Status: StatusFailed, Container: "iter"}, {NodeID: "worker", NodeRunID: "3", Status: StatusFailed, Container: "iter"}},
				Containers: []string{"iter"}, Clear: []string{"after", "gate", "iter", "worker"}},
		},
		{
			"a Loop body node restarts its Loop",
			ResumeState{Status: StatusCancelled, Nodes: []NodeRun{row("1", "loop", "loop", StatusRunning), row("2", "writer", "agent", StatusCancelled)}},
			ResumePlan{Failed: []ResumeFailedNode{{NodeID: "writer", NodeRunID: "2", Status: StatusCancelled, Container: "loop"}},
				Containers: []string{"loop"}, Clear: []string{"entry", "loop", "out", "writer"}},
		},
		{
			// Desktop leaves a same-wave swift row running (engine.rs:406-439); nothing completes it.
			"only stuck swift rows block",
			ResumeState{Status: StatusFailed, Nodes: []NodeRun{row("1", "out", "output", StatusRunning), row("2", "b", "agent", StatusFailed)}},
			ResumePlan{Refusal: ResumeRefusedStuck, Running: []string{"out"}},
		},
		{
			"a running Agent beside a stuck swift row is waited for",
			ResumeState{Status: StatusFailed, Nodes: []NodeRun{row("1", "out", "output", StatusRunning), row("2", "d", "agent", StatusRunning), row("3", "b", "agent", StatusFailed)}},
			ResumePlan{Refusal: ResumeRefusedRunning, Running: []string{"out", "d"}},
		},
		{"nothing failed", ResumeState{Status: StatusCancelled, Nodes: []NodeRun{ok("s", "start", "start")}}, ResumePlan{Refusal: ResumeRefusedNothing}},
		{
			"a frozen review form is not rerun",
			ResumeState{Status: StatusFailed, Nodes: []NodeRun{row("1", "b", "agent", StatusFailed)}, FormNodeIDs: []string{"c"}},
			ResumePlan{Refusal: ResumeRefusedForm, Failed: []ResumeFailedNode{{NodeID: "b", NodeRunID: "1", Status: StatusFailed}}, Clear: []string{"b", "c"}},
		},
		{
			"an unrelated review form does not block",
			ResumeState{Status: StatusFailed, Nodes: []NodeRun{row("1", "b", "agent", StatusFailed)}, FormNodeIDs: []string{"d"}},
			ResumePlan{Failed: []ResumeFailedNode{{NodeID: "b", NodeRunID: "1", Status: StatusFailed}}, Clear: []string{"b", "c"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := planResume(g, tc.st)
			want := tc.want
			for _, s := range []*[]string{&want.Running, &want.Containers, &want.Clear} {
				if *s == nil {
					*s = []string{}
				}
			}
			if want.Failed == nil {
				want.Failed = []ResumeFailedNode{}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("plan =\n %+v\nwant\n %+v", got, want)
			}
		})
	}
}

func TestClearResumedPayloadDropsWhatClearedNodesWrote(t *testing.T) {
	raw := []byte(`{"locale":"zh-CN","skillMaterialization":{"bindings":[]},"startNodeId":"start","injectLastFailure":true,
		"variablePool":{"revision":7,"catalog":{"start.input":{"valueType":"string","writer":"start"},"a.output":{"valueType":"string","writer":"a"},
			"worker.output":{"valueType":"string","writer":"worker"},"loop.result":{"valueType":"string","writer":"loop"}},
			"values":{"start.input":"go","a.output":"A","worker.output":"W","loop.result":"R"}},
		"conditionDecisions":{"cond":"yes","keep":"no"},
		"iterationLedger":{"iter":{"0":{"status":"succeeded","item":"x"}},"other":{"0":{"status":"succeeded","item":"y"}}},
		"iterationConditionDecisions":{"gate#0":"go","gate#1":"else","stay#0":"go"}}`)
	out, err := clearResumedPayload(raw, []string{"cond", "gate", "iter", "loop", "worker"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := DecodeRunPayload(out)
	if err != nil {
		t.Fatal(err)
	}
	values, _ := json.Marshal(p.VariablePool.Values)
	if string(values) != `{"a.output":"A","start.input":"go"}` || p.VariablePool.Revision != 8 || len(p.VariablePool.Catalog) != 4 {
		t.Fatalf("pool = %+v", p.VariablePool)
	}
	if !reflect.DeepEqual(p.ConditionDecisions, map[string]string{"keep": "no"}) {
		t.Fatalf("decisions = %v", p.ConditionDecisions)
	}
	if _, kept := p.IterationLedger["iter"]; kept || len(p.IterationLedger) != 1 {
		t.Fatalf("ledger = %v", p.IterationLedger)
	}
	if !reflect.DeepEqual(p.IterationConditionDecisions, map[string]string{"stay#0": "go"}) {
		t.Fatalf("iteration decisions = %v", p.IterationConditionDecisions)
	}
}

// gatedResumeRepo reports when the resume transaction begins.
type gatedResumeRepo struct {
	*memoryRepo
	began chan struct{}
}

func (r gatedResumeRepo) ResumeState(context.Context, string) (ResumeState, error) {
	return ResumeState{Status: r.run.Status}, nil
}

func (r gatedResumeRepo) ResumeRun(context.Context, string, string, func(ResumeState) ([]string, error), func([]byte, []string) ([]byte, error)) error {
	r.began <- struct{}{}
	return nil
}

// N15: the resume runs under the run gate, so it never interleaves with a callback or a
// cancellation's fence of the same run.
func TestResumeFromFailureHoldsTheRunGate(t *testing.T) {
	repo := gatedResumeRepo{memoryRepo: newMemoryRepo(resumeGraph, nil), began: make(chan struct{}, 1)}
	repo.run.Status = StatusFailed
	e := NewEngine(repo, nil, nil, time.Now)
	unlock := e.gate("run")
	done := make(chan error, 1)
	go func() { done <- e.ResumeFromFailure(context.Background(), "run", "actor") }()
	select {
	case <-repo.began:
		t.Fatal("the resume began while another holder had the run gate")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case <-repo.began:
	case <-time.After(2 * time.Second):
		t.Fatal("the resume never began after the gate was released")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// formBehindIteration is Start → Iteration(continue){worker} → review form → createTasks → Output
// "after": an absorbed member failure restarts the Iteration, which clears the form as its
// descendant.
const formBehindIteration = `{"schemaVersion":2,"nodes":[
 {"id":"start","data":{"kind":"start","inputVariables":[{"name":"items","valueType":"array[string]","value":["x","y"]},{"name":"findings","valueType":"array[object]","value":[{"key":"k","title":"t","description":"d"}]}]}},
 {"id":"iter","data":{"kind":"iteration","iterationConfig":{"iteratorSelector":["start","items"],"collectSelector":["worker","output"],"errorStrategy":"continue"}}},
 {"id":"worker","parentId":"iter","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"c","modelId":"m"},"prompt":"w"}}},
 {"id":"review","data":{"kind":"form","formConfig":{"title":"Review","submitLabel":"Submit","reportSelector":["start","input"],"itemsSelector":["start","findings"],"fields":[]}}},
 {"id":"tasks","data":{"kind":"createTasks","createTasksConfig":{"formNodeId":"review"}}},
 {"id":"after","data":{"kind":"output","outputs":[{"name":"tasks","variableSelector":["tasks","structured_output","tasks"]}]}}
],"edges":[{"source":"start","target":"iter"},{"source":"iter","target":"worker"},{"source":"iter","target":"review"},{"source":"review","target":"tasks"},{"source":"tasks","target":"after"}]}`

// L2: the form refusal says what applies to the form behind the restarting container: the form's
// own retry when its precondition holds, a new run when the submitted form's tasks succeeded, and
// the never-submitted text only for a form that was not submitted.
func TestPlanResumeNamesWhatAppliesToAClearedForm(t *testing.T) {
	g, err := ParseGraph(formBehindIteration)
	if err != nil {
		t.Fatal(err)
	}
	base := func(tasks string, extra ...NodeRun) []NodeRun {
		return append([]NodeRun{row("s", "start", "start", StatusSucceeded), row("i", "iter", "iteration", StatusSucceeded),
			row("w1", "worker", "agent", StatusSucceeded), row("w2", "worker", "agent", StatusFailed),
			row("r", "review", "form", StatusSucceeded), row("t", "tasks", "createTasks", tasks)}, extra...)
	}
	for _, tc := range []struct {
		name      string
		st        ResumeState
		want      string
		wantClear []string
	}{
		{"submitted form, failed task creation: the form retry", ResumeState{Status: StatusFailed, Nodes: base(StatusFailed), FormNodeIDs: []string{"review"}, SubmittedFormNodeIDs: []string{"review"}},
			ResumeRefusedFormRetry, []string{"after", "iter", "review", "tasks", "worker"}},
		{"submitted form, tasks created, the Output failed: a new run", ResumeState{Status: StatusFailed, Nodes: base(StatusSucceeded, row("a", "after", "output", StatusFailed)), FormNodeIDs: []string{"review"}, SubmittedFormNodeIDs: []string{"review"}},
			ResumeRefusedFormSubmitted, nil},
		{"submitted form of a cancelled run: a new run", ResumeState{Status: StatusCancelled, Nodes: base(StatusFailed), FormNodeIDs: []string{"review"}, SubmittedFormNodeIDs: []string{"review"}},
			ResumeRefusedFormSubmitted, nil},
		{"unsubmitted form", ResumeState{Status: StatusFailed, Nodes: base(StatusFailed), FormNodeIDs: []string{"review"}},
			ResumeRefusedForm, nil},
		{"task handling run", ResumeState{Status: StatusFailed, Nodes: base(StatusFailed), TaskHandling: true},
			ResumeRefusedTaskHandling, nil},
		{"task decomposition run", ResumeState{Status: StatusCancelled, Nodes: base(StatusFailed), TaskDecomposition: true},
			ResumeRefusedTaskDecomposition, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := planResume(g, tc.st)
			if plan.Refusal != tc.want {
				t.Fatalf("refusal = %q, want %q (plan %+v)", plan.Refusal, tc.want, plan)
			}
			if tc.wantClear != nil && !reflect.DeepEqual(plan.Clear, tc.wantClear) {
				t.Fatalf("clear = %v, want %v", plan.Clear, tc.wantClear)
			}
		})
	}
}

// failingContextRepo fails every run read.
type failingContextRepo struct{ *memoryRepo }

func (failingContextRepo) RunContext(context.Context, string) (RunContext, error) {
	return RunContext{}, context.DeadlineExceeded
}

// X14: a cancellation whose status read fails goes no further, so its retry fence is dropped
// again instead of fencing the run until the next cancel or resume.
func TestFenceRetriesForCancelDropsTheFenceWhenTheStatusReadFails(t *testing.T) {
	e := NewEngine(failingContextRepo{newMemoryRepo(resumeGraph, nil)}, nil, nil, time.Now)
	t.Cleanup(e.Close)
	e.armRetry("run", "attempt", time.Now().Add(time.Hour))
	if fenced, armed := e.HeldRetries("run"); fenced || armed != 1 {
		t.Fatalf("setup: fenced=%v armed=%d", fenced, armed)
	}
	if _, err := e.FenceRetriesForCancel(context.Background(), "run"); err == nil {
		t.Fatal("the status read error was swallowed")
	}
	// The run stays running, so it must keep its wait: nothing would re-arm a cancelled timer.
	if fenced, armed := e.HeldRetries("run"); fenced || armed != 1 {
		t.Fatalf("a failed cancellation left fenced=%v armed=%d, want the run as it was", fenced, armed)
	}
}
