package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/taskworkflow"
)

const handlingGraph = `{"nodes":[{"id":"start","type":"workflow","data":{"kind":"start","inputVariables":[{"name":"branch","valueType":"string","value":"main"}]}}],"edges":[]}`

type taskWorkflowPort struct {
	db     *store.Store
	user   string
	values map[string]json.RawMessage
}

func (p *taskWorkflowPort) Prepare(_ context.Context, _, user, _ string, _ *string, values map[string]json.RawMessage) error {
	p.user = user
	p.values = values
	return nil
}
func (p *taskWorkflowPort) Start(ctx context.Context, tenant, _, id string) error {
	_, err := p.db.StartWorkflowRun(ctx, tenant, id)
	return err
}
func (p *taskWorkflowPort) Cancel(ctx context.Context, tenant, _, id string) error {
	_, err := p.db.CancelWorkflowRun(ctx, tenant, id)
	return err
}
func (p *taskWorkflowPort) Subscribe(string) (<-chan struct{}, func()) {
	return make(chan struct{}), func() {}
}

func TestTaskWorkflowLifecycle(t *testing.T) {
	f := newSquadFixture(t)
	user := f.login(t, "owner", "owner")
	addProject(t, f, user)
	w, _, err := f.store.CreateWorkflow(f.ctx, f.tenant, "Handling", handlingGraph)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := f.store.PublishWorkflow(f.ctx, f.tenant, w.ID, "v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, status, want string
		recover, fence     bool
	}{
		{name: "success", status: "succeeded", want: "review"},
		{name: "failure", status: "failed", want: "review"},
		{name: "cancelled", status: "cancelled", want: "pending"},
		{name: "interrupted", want: "pending", recover: true},
		{name: "fenced success", status: "succeeded", want: "cancelled", fence: true},
		{name: "fenced restart", want: "cancelled", recover: true, fence: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := createTask(t, f, user, store.TaskInput{Title: "Same title", Assignee: &store.TaskAssigneeRef{Type: "human", ID: user}})
			other := createTask(t, f, user, store.TaskInput{Title: "Same title", Assignee: &store.TaskAssigneeRef{Type: "human", ID: user}})
			port := &taskWorkflowPort{db: f.store}
			h := taskworkflow.New(f.store, port)
			defer h.Close()
			in := store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "click", WorkflowID: w.ID}
			values := map[string]json.RawMessage{"branch": json.RawMessage(`"main"`)}
			id, err := h.Start(f.ctx, in, values)
			if err != nil {
				t.Fatal(err)
			}
			if port.user != user || string(port.values["branch"]) != `"main"` {
				t.Fatalf("caller/values lost: %+v", port)
			}
			running, err := f.store.Task(f.ctx, f.tenant, task.ID)
			if err != nil || running.Status != "running" {
				t.Fatalf("start task=%+v %v", running, err)
			}
			detail, err := f.store.WorkflowRun(f.ctx, f.tenant, id)
			if err != nil || detail.ProjectID != taskProject || detail.SnapshotID != snap.ID {
				t.Fatalf("frozen run=%+v %v", detail, err)
			}
			if got, err := f.store.WorkflowRunTask(f.ctx, f.tenant, id); err != nil || got != task.ID {
				t.Fatalf("owning task=%s %v", got, err)
			}
			if retry, err := h.Start(f.ctx, in, values); err != nil || retry != id {
				t.Fatalf("idempotent=%s %v", retry, err)
			}
			if _, err := f.store.CreateTaskRun(f.ctx, store.NewTaskRun{TenantID: f.tenant, TaskID: task.ID, StartedBy: user, StartKey: "agent", ExecutorID: "test"}); !errors.Is(err, store.ErrRunActive) {
				t.Fatalf("workflow excludes Agent: %v", err)
			}
			second := in
			second.TaskID = other.ID
			otherID, err := h.Start(f.ctx, second, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, pair := range []struct{ task, run string }{{task.ID, id}, {other.ID, otherID}} {
				got, err := h.List(f.ctx, f.tenant, pair.task)
				if err != nil || len(got) != 1 || got[0].WorkflowRunID != pair.run || got[0].StartedBy != user || got[0].Kind != "handle" || got[0].Version != "v1" {
					t.Fatalf("history=%+v %v", got, err)
				}
			}
			if tc.fence {
				if _, err := f.db.Exec(f.ctx, `UPDATE v2_tasks SET status='cancelled',version=version+1 WHERE id=$1`, task.ID); err != nil {
					t.Fatal(err)
				}
			}
			if tc.recover {
				h.Close()
				h = taskworkflow.New(f.store, port)
				defer h.Close()
				if err := h.Recover(f.ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				if tc.status == "cancelled" {
					_, err = f.store.CancelWorkflowRun(f.ctx, f.tenant, id)
				} else {
					_, err = f.store.FinishWorkflowRun(f.ctx, f.tenant, id, tc.status, nil, nil)
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.List(f.ctx, f.tenant, task.ID); err != nil {
					t.Fatal(err)
				}
			}
			got, err := f.store.Task(f.ctx, f.tenant, task.ID)
			if err != nil || got.Status != tc.want {
				t.Fatalf("settled task=%+v want %s: %v", got, tc.want, err)
			}
			list, err := h.List(f.ctx, f.tenant, task.ID)
			if err != nil || (tc.recover && list[0].Status != "interrupted") {
				t.Fatalf("settled history=%+v %v", list, err)
			}
			if retry, err := h.Start(f.ctx, in, values); err != nil || retry != id {
				t.Fatalf("terminal retry=%s %v", retry, err)
			}
			if err := h.Recover(f.ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTaskWorkflowAdmission(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", "owner")
	other := f.login(t, "other", "member")
	addProject(t, f, owner)
	task := createTask(t, f, owner, store.TaskInput{Title: "Task", Assignee: &store.TaskAssigneeRef{Type: "human", ID: owner}})
	w, _, err := f.store.CreateWorkflow(f.ctx, f.tenant, "Handling", handlingGraph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PublishWorkflow(f.ctx, f.tenant, w.ID, "v1", nil); err != nil {
		t.Fatal(err)
	}
	in := store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: other, Key: "same", WorkflowID: w.ID}
	if _, _, err := f.store.CreateTaskWorkflowRun(f.ctx, in); !errors.Is(err, store.ErrTaskNotAssignee) {
		t.Fatalf("non-assignee=%v", err)
	}
	in.UserID = owner
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); <-start; _, _, err := f.store.CreateTaskWorkflowRun(f.ctx, in); errs <- err }()
	go func() {
		defer wg.Done()
		<-start
		_, err := f.store.CreateTaskRun(f.ctx, store.NewTaskRun{TenantID: f.tenant, TaskID: task.ID, StartedBy: owner, StartKey: "agent", ExecutorID: "test", ExpectedVersion: task.Version})
		errs <- err
	}()
	close(start)
	wg.Wait()
	close(errs)
	success, active := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, store.ErrRunActive) {
			active++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || active != 1 {
		t.Fatalf("concurrent admission success=%d active=%d", success, active)
	}
	agentTask := createTask(t, f, owner, store.TaskInput{Title: "Task", Assignee: &store.TaskAssigneeRef{Type: "human", ID: owner}})
	if _, err := f.store.CreateTaskRun(f.ctx, store.NewTaskRun{TenantID: f.tenant, TaskID: agentTask.ID, StartedBy: owner, StartKey: "agent", ExecutorID: "test"}); err != nil {
		t.Fatal(err)
	}
	in.TaskID = agentTask.ID
	if _, _, err := f.store.CreateTaskWorkflowRun(f.ctx, in); !errors.Is(err, store.ErrRunActive) {
		t.Fatalf("Agent excludes workflow: %v", err)
	}
	in.TenantID = "00000000-0000-4000-8000-000000000099"
	if _, _, err := f.store.CreateTaskWorkflowRun(f.ctx, in); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant admission: %v", err)
	}
	if _, err := f.store.TaskWorkflowRuns(f.ctx, in.TenantID, in.TaskID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant history: %v", err)
	}
}

func TestTaskWorkflowKinds(t *testing.T) {
	f := newSquadFixture(t)
	user := f.login(t, "owner", "owner")
	addProject(t, f, user)
	task := createTask(t, f, user, store.TaskInput{Title: "Same title", Assignee: &store.TaskAssigneeRef{Type: "human", ID: user}})
	w, _, err := f.store.CreateWorkflow(f.ctx, f.tenant, "Workflow", handlingGraph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PublishWorkflow(f.ctx, f.tenant, w.ID, "v1", nil); err != nil {
		t.Fatal(err)
	}
	makeRun := func() store.WorkflowRun {
		r, err := f.store.CreateWorkflowRun(f.ctx, f.tenant, w.ID, "", store.WorkflowRunInput{ProjectID: taskProject})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	split := makeRun()
	d, err := f.store.CreateTaskDecomposition(f.ctx, store.NewTaskDecomposition{TaskID: task.ID, TenantID: f.tenant, StartedBy: user, StartKey: "split", WorkflowID: w.ID, WorkflowName: w.Name, WorkflowRunID: split.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.SettleTaskDecomposition(f.ctx, d.ID, []store.Subtask{{Title: "Child"}}, ""); err != nil {
		t.Fatal(err)
	}
	source := makeRun()
	node, err := f.store.CreateWorkflowNodeRun(f.ctx, f.tenant, source.ID, store.WorkflowNodeRunInput{NodeID: "review", NodeType: "form"})
	if err != nil {
		t.Fatal(err)
	}
	results, _ := json.Marshal([]map[string]string{{"taskId": task.ID}})
	if _, err := f.db.Exec(f.ctx, `UPDATE v2_workflow_runs SET started_by=$2 WHERE id=$1`, source.ID, user); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(f.ctx, `INSERT INTO v2_workflow_forms(id,tenant_id,run_id,node_id,reviewer_id,config,findings,report,results) VALUES($1,$2,$3,'review',$4,'{}','[]','Report',$5)`, node.ID, f.tenant, source.ID, user, results); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.CreateTaskWorkflowRun(f.ctx, store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "handle", WorkflowID: w.ID}); err != nil {
		t.Fatal(err)
	}
	list, err := f.store.TaskWorkflowRuns(f.ctx, f.tenant, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	other := createTask(t, f, user, store.TaskInput{Title: task.Title, Assignee: &store.TaskAssigneeRef{Type: "human", ID: user}})
	if got, err := f.store.TaskWorkflowRuns(f.ctx, f.tenant, other.ID); err != nil || len(got) != 0 {
		t.Fatalf("source leaked to same-title task: %+v %v", got, err)
	}
	kinds := map[string]bool{}
	for _, r := range list {
		if r.StartedBy != user || r.StartedByName != "owner" || r.Version != "v1" || r.WorkflowName != "Workflow" {
			t.Fatalf("metadata=%+v", r)
		}
		kinds[r.Kind] = true
	}
	if len(list) != 3 || !kinds["handle"] || !kinds["decomposition"] || !kinds["source"] {
		t.Fatalf("kinds=%+v", list)
	}
	got, err := f.store.Task(f.ctx, f.tenant, task.ID)
	if err != nil || got.Status != "running" {
		t.Fatalf("handling admission did not move task: %+v %v", got, err)
	}
}

func TestTaskWorkflowSnapshotAndStartStates(t *testing.T) {
	f := newSquadFixture(t)
	user := f.login(t, "owner", "owner")
	addProject(t, f, user)
	w, draft, err := f.store.CreateWorkflow(f.ctx, f.tenant, "Handling", handlingGraph)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := f.store.PublishWorkflow(f.ctx, f.tenant, w.ID, "v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := f.store.PublishWorkflow(f.ctx, f.tenant, w.ID, "v2", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, snapshot, want, initial string
		failure                       error
	}{
		{name: "active version", want: v2.ID, initial: "pending"},
		{name: "explicit history", snapshot: v1.ID, want: v1.ID, initial: "review"},
		{name: "already running", want: v2.ID, initial: "running"},
		{name: "draft refused", snapshot: draft.ID, initial: "pending", failure: store.ErrWorkflowDraftSnapshot},
		{name: "closed refused", initial: "cancelled", failure: store.ErrInvalidTransition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := createTask(t, f, user, store.TaskInput{Title: "Task", Assignee: &store.TaskAssigneeRef{Type: "human", ID: user}})
			// Seed the lifecycle boundary independently of the start behavior being tested.
			if _, err := f.db.Exec(f.ctx, `UPDATE v2_tasks SET status=$2 WHERE id=$1`, task.ID, tc.initial); err != nil {
				t.Fatal(err)
			}
			h := taskworkflow.New(f.store, &taskWorkflowPort{db: f.store})
			defer h.Close()
			in := store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "start", WorkflowID: w.ID, SnapshotID: tc.snapshot}
			id, err := h.Start(f.ctx, in, nil)
			if tc.failure != nil {
				if !errors.Is(err, tc.failure) {
					t.Fatalf("start error=%v want %v", err, tc.failure)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			run, err := f.store.WorkflowRun(f.ctx, f.tenant, id)
			if err != nil || run.SnapshotID != tc.want {
				t.Fatalf("snapshot=%+v %v", run, err)
			}
			got, err := f.store.Task(f.ctx, f.tenant, task.ID)
			if err != nil || got.Status != "running" {
				t.Fatalf("start state=%+v %v", got, err)
			}
			in.Key = "another"
			if _, err := h.Start(f.ctx, in, nil); !errors.Is(err, store.ErrRunActive) {
				t.Fatalf("second live handling run=%v", err)
			}
		})
	}
}

func TestDeletedTaskWorkflowReleasesAdmission(t *testing.T) {
	f := newSquadFixture(t)
	user := f.login(t, "owner", "owner")
	addProject(t, f, user)
	task := createTask(t, f, user, store.TaskInput{Title: "Task", Assignee: &store.TaskAssigneeRef{Type: "human", ID: user}})
	w, _, err := f.store.CreateWorkflow(f.ctx, f.tenant, "Handling", handlingGraph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PublishWorkflow(f.ctx, f.tenant, w.ID, "v1", nil); err != nil {
		t.Fatal(err)
	}
	in := store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "original", WorkflowID: w.ID}
	id, _, err := f.store.CreateTaskWorkflowRun(f.ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	// A pending run can be deleted before dispatch. Recovery must release its durable reservation.
	if err := f.store.DeleteWorkflowRun(f.ctx, f.tenant, id); err != nil {
		t.Fatal(err)
	}
	h := taskworkflow.New(f.store, &taskWorkflowPort{db: f.store})
	defer h.Close()
	if err := h.Recover(f.ctx); err != nil {
		t.Fatal(err)
	}
	in.Key = "retry"
	if _, err := h.Start(f.ctx, in, nil); err != nil {
		t.Fatalf("deleted reservation blocked retry: %v", err)
	}
}
