package store_test

import (
	"errors"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

// Editing and reassigning belong to the task's creator, the workspace owner and whoever currently
// does the work; a finished task is read-only, and a running Agent turn keeps its assignee.
func TestTaskManageNeedsCreatorOwnerOrAssignee(t *testing.T) {
	f := newSquadFixture(t)
	ctx := f.ctx
	owner := f.login(t, "owner", "owner")
	lead := f.login(t, "lead", "member")
	doer := f.login(t, "doer", "member")
	mate := f.login(t, "mate", "member")
	bystander := f.login(t, "bystander", "member")
	addProject(t, f, owner)
	as := func(user string) store.Access { return storetest.Access(t, f.store, f.tenant, user) }
	edit := func(user string, task store.Task) (store.Task, error) {
		return f.store.UpdateTask(ctx, as(user), task.ID, store.TaskInput{Title: task.Title + "（改）", ProjectID: taskProject, Version: task.Version})
	}
	reassign := func(user string, task store.Task, to string) (store.Task, error) {
		return f.store.SetTaskAssignee(ctx, as(user), task.ID, &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: to}, task.Version)
	}

	// A person's task: the creator, the owner and the assignee may edit; nobody else.
	task := createTask(t, f, lead, store.TaskInput{Title: "人的任务", Assignee: &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: doer}})
	if _, e := edit(bystander, task); !errors.Is(e, store.ErrForbidden) {
		t.Fatalf("bystander edit = %v", e)
	}
	if _, e := reassign(bystander, task, bystander); !errors.Is(e, store.ErrForbidden) {
		t.Fatalf("bystander reassign = %v", e)
	}
	var e error
	for _, user := range []string{doer, lead, owner} {
		if task, e = edit(user, task); e != nil {
			t.Fatalf("edit by %s = %v", user, e)
		}
	}
	// The assignee may hand it on, and is then no longer the one doing it.
	if task, e = reassign(doer, task, mate); e != nil || task.Assignee.ID != mate {
		t.Fatalf("assignee reassign = %+v %v", task, e)
	}
	if _, e := edit(doer, task); !errors.Is(e, store.ErrForbidden) {
		t.Fatalf("former assignee edit = %v", e)
	}

	// A squad's task: any current member is doing it.
	squad, e := f.store.CreateSquad(ctx, f.tenant, owner, store.SquadInput{Name: "前端"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.AddSquadMember(ctx, as(owner), squad.ID, store.SquadMemberInput{Type: store.MemberHuman, UserID: mate, Version: squad.Version}); e != nil {
		t.Fatal(e)
	}
	squadTask := createTask(t, f, lead, store.TaskInput{Title: "小队任务", Assignee: &store.TaskAssigneeRef{Type: store.AssigneeSquad, ID: squad.ID}})
	if _, e := edit(bystander, squadTask); !errors.Is(e, store.ErrForbidden) {
		t.Fatalf("non-member edit = %v", e)
	}
	if _, e := edit(mate, squadTask); e != nil {
		t.Fatalf("squad member edit = %v", e)
	}

	// An Agent's task has no person doing it: only its creator and the owner manage it.
	agentTask := createTask(t, f, lead, store.TaskInput{Title: "Agent 任务", Assignee: &store.TaskAssigneeRef{Type: store.AssigneeAgent, ID: f.agent(t, "OpenCode", "active")}})
	if _, e := edit(bystander, agentTask); !errors.Is(e, store.ErrForbidden) {
		t.Fatalf("member edit of an Agent task = %v", e)
	}
	if agentTask, e = edit(lead, agentTask); e != nil {
		t.Fatalf("creator edit of an Agent task = %v", e)
	}

	// While its Agent turn runs, the task keeps its assignee; an edit still goes through.
	run, e := f.store.CreateTaskRun(ctx, store.NewTaskRun{TenantID: f.tenant, TaskID: agentTask.ID, StartedBy: bystander, StartKey: "k", ExecutorID: "test"})
	if e != nil {
		t.Fatal(e)
	}
	if e := f.store.RecordTaskTurn(ctx, run.ID, "session", "turn"); e != nil {
		t.Fatal(e)
	}
	agentTask, _ = f.store.Task(ctx, f.tenant, agentTask.ID)
	if _, e := reassign(lead, agentTask, doer); !errors.Is(e, store.ErrRunActive) {
		t.Fatalf("reassign during a turn = %v", e)
	}
	if _, e := edit(lead, agentTask); e != nil {
		t.Fatalf("edit during a turn = %v", e)
	}

	// A finished task is read-only, even to its creator.
	closed, e := f.store.SetTaskStatus(ctx, as(lead), task.ID, store.TaskCancelled, task.Version)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := edit(lead, closed); !errors.Is(e, store.ErrTaskClosed) {
		t.Fatalf("edit of a finished task = %v", e)
	}
	if _, e := reassign(owner, closed, doer); !errors.Is(e, store.ErrTaskClosed) {
		t.Fatalf("reassign of a finished task = %v", e)
	}
}

// Whoever may confirm a task in 待验收 may instead send it back to 待处理 for rework; the person
// doing it may not, and no other status goes back to 待处理 by hand.
func TestTaskSendBackNeedsCreatorOrOwner(t *testing.T) {
	f := newSquadFixture(t)
	ctx := f.ctx
	owner := f.login(t, "owner", "owner")
	lead := f.login(t, "lead", "member")
	doer := f.login(t, "doer", "member")
	addProject(t, f, owner)
	as := func(user string) store.Access { return storetest.Access(t, f.store, f.tenant, user) }
	inReview := func() store.Task {
		t.Helper()
		task := createTask(t, f, lead, store.TaskInput{Title: "返工", Assignee: &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: doer}})
		for _, status := range []string{store.TaskRunning, store.TaskReview} {
			var e error
			if task, e = f.store.SetTaskStatus(ctx, as(doer), task.ID, status, task.Version); e != nil {
				t.Fatalf("%s: %v", status, e)
			}
		}
		return task
	}

	task := inReview()
	if _, e := f.store.SetTaskStatus(ctx, as(doer), task.ID, store.TaskPending, task.Version); !errors.Is(e, store.ErrForbidden) {
		t.Fatalf("assignee send back = %v", e)
	}
	back, e := f.store.SetTaskStatus(ctx, as(lead), task.ID, store.TaskPending, task.Version)
	if e != nil || back.Status != store.TaskPending {
		t.Fatalf("creator send back = %+v %v", back, e)
	}
	other := inReview()
	if back, e := f.store.SetTaskStatus(ctx, as(owner), other.ID, store.TaskPending, other.Version); e != nil || back.Status != store.TaskPending {
		t.Fatalf("owner send back = %+v %v", back, e)
	}
	// 进行中 still only moves on, or ends.
	running, e := f.store.SetTaskStatus(ctx, as(doer), back.ID, store.TaskRunning, back.Version)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.SetTaskStatus(ctx, as(lead), running.ID, store.TaskPending, running.Version); !errors.Is(e, store.ErrInvalidTransition) {
		t.Fatalf("running → pending = %v", e)
	}
}
