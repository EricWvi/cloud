package store_test

import (
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

// inbox reads a user's notifications and fails the test on error.
func inbox(t *testing.T, f *squadFixture, user string) ([]store.Notification, int) {
	t.Helper()
	items, unread, e := f.store.Notifications(f.ctx, f.tenant, user, 0)
	if e != nil {
		t.Fatal(e)
	}
	return items, unread
}

func TestNotifyAssignment(t *testing.T) {
	f := newSquadFixture(t)
	ctx := f.ctx
	owner := f.login(t, "owner", "owner")
	alice := f.login(t, "alice", "member")
	bob := f.login(t, "bob", "member")
	carol := f.login(t, "carol", "member")
	addProject(t, f, owner)

	t.Run("creating an assigned task tells the assignee, not the creator", func(t *testing.T) {
		task := createTask(t, f, owner, store.TaskInput{Title: "建任务即指派", Assignee: &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: alice}})
		items, _ := inbox(t, f, alice)
		if len(items) == 0 || items[0].Kind != store.NotifyAssigned || items[0].TaskID != task.ID ||
			items[0].ActorName == nil || *items[0].ActorName != "owner" || items[0].TaskTitle != "建任务即指派" {
			t.Fatalf("alice = %+v", items)
		}
		mine := createTask(t, f, owner, store.TaskInput{Title: "自己的", Assignee: &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: owner}})
		for _, n := range must(inbox(t, f, owner)) {
			if n.TaskID == mine.ID {
				t.Fatalf("self assignment notified: %+v", n)
			}
		}
	})

	t.Run("reassigning tells the new assignee once", func(t *testing.T) {
		task := createTask(t, f, owner, store.TaskInput{Title: "改派"})
		task, e := f.store.SetTaskAssignee(ctx, f.tenant, task.ID, owner, &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: bob}, task.Version)
		if e != nil {
			t.Fatal(e)
		}
		// Re-sending the same assignee is not a new assignment.
		if task, e = f.store.SetTaskAssignee(ctx, f.tenant, task.ID, owner, &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: bob}, task.Version); e != nil {
			t.Fatal(e)
		}
		if n := countFor(must(inbox(t, f, bob)), task.ID, store.NotifyAssigned); n != 1 {
			t.Fatalf("bob notified %d times", n)
		}
		// Clearing and an Agent assignee tell nobody.
		agentID := f.agent(t, "Agent", "active")
		if task, e = f.store.SetTaskAssignee(ctx, f.tenant, task.ID, owner, &store.TaskAssigneeRef{Type: store.AssigneeAgent, ID: agentID}, task.Version); e != nil {
			t.Fatal(e)
		}
		if _, e = f.store.SetTaskAssignee(ctx, f.tenant, task.ID, owner, nil, task.Version); e != nil {
			t.Fatal(e)
		}
		if n := countFor(must(inbox(t, f, bob)), task.ID, store.NotifyAssigned); n != 1 {
			t.Fatalf("bob notified %d times after agent and clear", n)
		}
	})

	t.Run("a squad assignment tells its current human members except the actor", func(t *testing.T) {
		squad, e := f.store.CreateSquad(ctx, f.tenant, owner, store.SquadInput{Name: "提醒小队"})
		if e != nil {
			t.Fatal(e)
		}
		for _, u := range []string{alice, bob} {
			if squad, e = f.store.AddSquadMember(ctx, storetest.Access(t, f.store, f.tenant, owner), squad.ID, store.SquadMemberInput{Type: "human", UserID: u, Version: squad.Version}); e != nil {
				t.Fatal(e)
			}
		}
		task := createTask(t, f, alice, store.TaskInput{Title: "给小队", Assignee: &store.TaskAssigneeRef{Type: store.AssigneeSquad, ID: squad.ID}})
		if countFor(must(inbox(t, f, bob)), task.ID, store.NotifyAssigned) != 1 {
			t.Fatal("bob was not told about the squad task")
		}
		if countFor(must(inbox(t, f, alice)), task.ID, store.NotifyAssigned) != 0 {
			t.Fatal("alice was told about her own squad assignment")
		}
		// The squad's creator joined it as a maintainer; carol never joined.
		if countFor(must(inbox(t, f, owner)), task.ID, store.NotifyAssigned) != 1 {
			t.Fatal("owner, a member by creating the squad, was not told")
		}
		if items, _ := inbox(t, f, carol); len(items) != 0 {
			t.Fatalf("carol is not in the squad but was told: %+v", items)
		}
	})
}

func TestNotifyMention(t *testing.T) {
	f := newSquadFixture(t)
	ctx := f.ctx
	owner := f.login(t, "owner", "owner")
	alice := f.login(t, "alice", "member")
	addProject(t, f, owner)
	task := createTask(t, f, owner, store.TaskInput{Title: "看评论"})
	agentID := f.agent(t, "Agent", "active")
	in := store.NewTaskComment{TenantID: f.tenant, TaskID: task.ID, AuthorID: owner, CreateKey: "c1", Body: "@alice 帮忙看一下这个报错",
		Mentions: []store.TaskMention{{Type: "user", ID: alice}, {Type: "user", ID: owner}, {Type: "agent", ID: agentID}}}
	comment, e := f.store.CreateTaskComment(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	// A retried request with the same key makes no second notification.
	if _, e := f.store.CreateTaskComment(ctx, in); e != nil {
		t.Fatal(e)
	}
	items, unread := inbox(t, f, alice)
	if len(items) != 1 || unread != 1 || items[0].Kind != store.NotifyMentioned || items[0].CommentID == nil ||
		*items[0].CommentID != comment.ID || items[0].Excerpt != "@alice 帮忙看一下这个报错" {
		t.Fatalf("alice = %+v unread %d", items, unread)
	}
	if items, _ := inbox(t, f, owner); len(items) != 0 {
		t.Fatalf("self mention notified: %+v", items)
	}
}

func TestNotifyReview(t *testing.T) {
	t.Run("a hand move by someone else tells the creator", func(t *testing.T) {
		f := newSquadFixture(t)
		owner := f.login(t, "owner", "owner")
		alice := f.login(t, "alice", "member")
		addProject(t, f, owner)
		task := createTask(t, f, owner, store.TaskInput{Title: "待验收", Assignee: &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: alice}})
		as := storetest.Access(t, f.store, f.tenant, alice)
		task, e := f.store.SetTaskStatus(f.ctx, as, task.ID, store.TaskRunning, task.Version)
		if e != nil {
			t.Fatal(e)
		}
		if countFor(must(inbox(t, f, owner)), task.ID, store.NotifyReview) != 0 {
			t.Fatal("running is not review")
		}
		if _, e = f.store.SetTaskStatus(f.ctx, as, task.ID, store.TaskReview, task.Version); e != nil {
			t.Fatal(e)
		}
		items, _ := inbox(t, f, owner)
		if countFor(items, task.ID, store.NotifyReview) != 1 || items[0].ActorName == nil || *items[0].ActorName != "alice" ||
			items[0].TaskStatus != store.TaskReview {
			t.Fatalf("owner = %+v", items)
		}
	})

	t.Run("the creator's own hand move tells nobody", func(t *testing.T) {
		f := newSquadFixture(t)
		owner := f.login(t, "owner", "owner")
		addProject(t, f, owner)
		task := createTask(t, f, owner, store.TaskInput{Title: "自己验收"})
		as := storetest.Access(t, f.store, f.tenant, owner)
		task, e := f.store.SetTaskStatus(f.ctx, as, task.ID, store.TaskRunning, task.Version)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.store.SetTaskStatus(f.ctx, as, task.ID, store.TaskReview, task.Version); e != nil {
			t.Fatal(e)
		}
		if items, _ := inbox(t, f, owner); len(items) != 0 {
			t.Fatalf("owner = %+v", items)
		}
	})

	t.Run("an Agent run handing the task over tells the creator", func(t *testing.T) {
		f := newSquadFixture(t)
		owner := f.login(t, "owner", "owner")
		addProject(t, f, owner)
		task := createTask(t, f, owner, store.TaskInput{Title: "Agent 做完"})
		if _, e := f.store.TransitionTaskStatus(f.ctx, f.tenant, task.ID, store.TaskPending, store.TaskRunning); e != nil {
			t.Fatal(e)
		}
		if _, e := f.store.TransitionTaskStatus(f.ctx, f.tenant, task.ID, store.TaskRunning, store.TaskReview); e != nil {
			t.Fatal(e)
		}
		items, _ := inbox(t, f, owner)
		if countFor(items, task.ID, store.NotifyReview) != 1 || items[0].ActorID != nil {
			t.Fatalf("owner = %+v", items)
		}
	})

	t.Run("a settled handling workflow tells the creator", func(t *testing.T) {
		f, user, task, wid := reviewWorkflow(t)
		id, _, e := f.store.CreateTaskWorkflowRun(f.ctx, store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "handle", WorkflowID: wid})
		if e != nil {
			t.Fatal(e)
		}
		if _, e := f.store.FinishWorkflowRun(f.ctx, f.tenant, id, "succeeded", nil, nil); e != nil {
			t.Fatal(e)
		}
		if e := f.store.SettleTaskWorkflow(f.ctx, f.tenant, id, "succeeded", ""); e != nil {
			t.Fatal(e)
		}
		if countFor(must(inbox(t, f, user)), task.ID, store.NotifyReview) != 1 {
			t.Fatal("handling workflow settlement did not notify the creator")
		}
	})
}

func TestNotificationsReadAndMembership(t *testing.T) {
	f := newSquadFixture(t)
	ctx := f.ctx
	owner := f.login(t, "owner", "owner")
	alice := f.login(t, "alice", "member")
	addProject(t, f, owner)
	first := createTask(t, f, owner, store.TaskInput{Title: "一", Assignee: &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: alice}})
	createTask(t, f, owner, store.TaskInput{Title: "二", Assignee: &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: alice}})
	items, unread := inbox(t, f, alice)
	if len(items) != 2 || unread != 2 || items[0].TaskTitle != "二" {
		t.Fatalf("inbox = %+v unread %d", items, unread)
	}

	t.Run("read marks only the caller's listed notifications", func(t *testing.T) {
		if n, e := f.store.MarkNotificationsRead(ctx, f.tenant, owner, []string{items[1].ID}, false); e != nil || n != 0 {
			t.Fatalf("owner marking alice's = %d %v", n, e)
		}
		if _, unread := inbox(t, f, alice); unread != 2 {
			t.Fatalf("someone else marked alice's read: unread %d", unread)
		}
		if n, e := f.store.MarkNotificationsRead(ctx, f.tenant, alice, []string{items[1].ID}, false); e != nil || n != 1 {
			t.Fatalf("mark one = %d %v", n, e)
		}
		got, _ := inbox(t, f, alice)
		if got[1].ReadAt == nil || got[0].ReadAt != nil || got[1].TaskID != first.ID {
			t.Fatalf("after mark one = %+v", got)
		}
		if n, e := f.store.MarkNotificationsRead(ctx, f.tenant, alice, nil, true); e != nil || n != 0 {
			t.Fatalf("mark all = %d %v", n, e)
		}
	})

	t.Run("a removed member keeps nothing and receives nothing", func(t *testing.T) {
		if _, e := f.db.Exec(ctx, `DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`, f.tenant, alice); e != nil {
			t.Fatal(e)
		}
		if items, unread := inbox(t, f, alice); len(items) != 0 || unread != 0 {
			t.Fatalf("removed member still sees %+v", items)
		}
		comment := store.NewTaskComment{TenantID: f.tenant, TaskID: first.ID, AuthorID: owner, CreateKey: "late", Body: "还在吗",
			Mentions: []store.TaskMention{{Type: "user", ID: alice}}}
		if _, e := f.store.CreateTaskComment(ctx, comment); e != nil {
			t.Fatalf("a comment mentioning a removed member must still be written: %v", e)
		}
		// Re-joining the team does not bring back old notifications or the one sent while away.
		if _, e := f.db.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'member')`, f.tenant, alice); e != nil {
			t.Fatal(e)
		}
		if items, _ := inbox(t, f, alice); len(items) != 0 {
			t.Fatalf("re-joined member sees %+v", items)
		}
	})
}

func countFor(items []store.Notification, task, kind string) int {
	n := 0
	for _, item := range items {
		if item.TaskID == task && item.Kind == kind {
			n++
		}
	}
	return n
}

func must(items []store.Notification, _ int) []store.Notification { return items }
