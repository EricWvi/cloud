package store_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

// inbox reads a user's notifications and fails the test on error.
func inbox(t *testing.T, f *squadFixture, user string) ([]store.Notification, int) {
	t.Helper()
	page, e := f.store.Notifications(f.ctx, f.tenant, user, nil, 0)
	if e != nil {
		t.Fatal(e)
	}
	return page.Items, page.Unread
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

	t.Run("upper-case UUIDs are the same person", func(t *testing.T) {
		upper := strings.ToUpper(bob)
		task := createTask(t, f, owner, store.TaskInput{Title: "大写改派"})
		task, e := f.store.SetTaskAssignee(ctx, f.tenant, task.ID, owner, &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: bob}, task.Version)
		if e != nil {
			t.Fatal(e)
		}
		// The same person spelled in upper case is not a new assignment.
		if _, e = f.store.SetTaskAssignee(ctx, f.tenant, task.ID, owner, &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: upper}, task.Version); e != nil {
			t.Fatal(e)
		}
		if n := countFor(must(inbox(t, f, bob)), task.ID, store.NotifyAssigned); n != 1 {
			t.Fatalf("bob notified %d times", n)
		}
		// Assigning yourself in upper case is still your own act.
		mine := createTask(t, f, bob, store.TaskInput{Title: "大写自派", Assignee: &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: upper}})
		if n := countFor(must(inbox(t, f, bob)), mine.ID, store.NotifyAssigned); n != 0 {
			t.Fatalf("self assignment in upper case notified %d times", n)
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

func TestNotificationPages(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", "owner")
	alice := f.login(t, "alice", "member")
	addProject(t, f, owner)
	oldest := createTask(t, f, owner, store.TaskInput{Title: "最早", Assignee: &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: alice}})
	for i := 0; i < 4; i++ {
		createTask(t, f, owner, store.TaskInput{Title: "较新", Assignee: &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: alice}})
	}
	// Everything but the oldest is read: the one unread notice lies beyond the first page.
	items, _ := inbox(t, f, alice)
	ids := []string{}
	for _, n := range items[:4] {
		ids = append(ids, n.ID)
	}
	if _, e := f.store.MarkNotificationsRead(f.ctx, f.tenant, alice, ids, false); e != nil {
		t.Fatal(e)
	}
	seen := []store.Notification{}
	var after *store.NotificationCursor
	for pages := 0; ; pages++ {
		page, e := f.store.Notifications(f.ctx, f.tenant, alice, after, 2)
		if e != nil {
			t.Fatal(e)
		}
		if page.Unread != 1 || len(page.Items) > 2 || pages > 3 {
			t.Fatalf("page %d = %+v", pages, page)
		}
		seen = append(seen, page.Items...)
		if page.Next == nil {
			break
		}
		after = page.Next
	}
	if len(seen) != 5 || seen[4].TaskID != oldest.ID || seen[4].ReadAt != nil {
		t.Fatalf("pages reached %+v", seen)
	}
	if _, e := f.store.Notifications(f.ctx, f.tenant, alice, &store.NotificationCursor{CreatedAt: time.Now(), ID: "nope"}, 2); e == nil {
		t.Fatal("a cursor with a malformed id was accepted")
	}
}

// A member removed while a comment that mentions them is being written must not roll the comment
// back: the removal and the notification are ordered by the membership row lock.
func TestNotifyRaceWithRemoval(t *testing.T) {
	f := newSquadFixture(t)
	ctx := f.ctx
	owner := f.login(t, "owner", "owner")
	alice := f.login(t, "alice", "member")
	addProject(t, f, owner)
	task := createTask(t, f, owner, store.TaskInput{Title: "并发移出"})

	removal, e := f.db.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = removal.Rollback(ctx) }()
	if _, e := removal.Exec(ctx, `DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`, f.tenant, alice); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() {
		_, e := f.store.CreateTaskComment(ctx, store.NewTaskComment{TenantID: f.tenant, TaskID: task.ID, AuthorID: owner,
			CreateKey: "race", Body: "看一下", Mentions: []store.TaskMention{{Type: "user", ID: alice}}})
		done <- e
	}()
	// Wait until the comment's transaction is blocked behind the removal, then commit the removal.
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	watcher, e := pgx.Connect(waitCtx, os.Getenv("TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = watcher.Close(ctx) }()
	for {
		var waiting bool
		if e := watcher.QueryRow(waitCtx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND (query LIKE '%memberships%' OR query LIKE '%v2_notifications%'))`).Scan(&waiting); e != nil {
			t.Fatal(e)
		}
		if waiting {
			break
		}
		select {
		case e := <-done:
			t.Fatalf("comment finished before the removal committed: %v", e)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if e := removal.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e := <-done; e != nil {
		t.Fatalf("the comment was rolled back by the concurrent removal: %v", e)
	}
	if _, e := f.store.TaskCommentByKey(ctx, f.tenant, task.ID, owner, "race"); e != nil {
		t.Fatalf("comment missing: %v", e)
	}
	var n int
	if e := f.db.QueryRow(ctx, `SELECT count(*) FROM v2_notifications WHERE user_id = $1`, alice).Scan(&n); e != nil || n != 0 {
		t.Fatalf("removed member notified: %d %v", n, e)
	}
}

// Removing a member through the team's own operation takes their notifications with the
// membership row, and nothing reaches them afterwards.
func TestRemoveMemberDropsNotifications(t *testing.T) {
	f := newSquadFixture(t)
	ctx := f.ctx
	owner := f.login(t, "owner", "owner")
	alice := f.login(t, "alice", "member")
	addProject(t, f, owner)
	task := createTask(t, f, owner, store.TaskInput{Title: "移出前", Assignee: &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: alice}})
	if items, _ := inbox(t, f, alice); len(items) != 1 {
		t.Fatalf("before removal = %+v", items)
	}
	if e := f.store.RemoveMember(ctx, f.tenant, alice); e != nil {
		t.Fatal(e)
	}
	var n int
	if e := f.db.QueryRow(ctx, `SELECT count(*) FROM v2_notifications WHERE user_id = $1`, alice).Scan(&n); e != nil || n != 0 {
		t.Fatalf("notifications left after RemoveMember: %d %v", n, e)
	}
	if _, e := f.store.CreateTaskComment(ctx, store.NewTaskComment{TenantID: f.tenant, TaskID: task.ID, AuthorID: owner,
		CreateKey: "after", Body: "还在吗", Mentions: []store.TaskMention{{Type: "user", ID: alice}}}); e != nil {
		t.Fatalf("comment mentioning a removed member: %v", e)
	}
	if e := f.db.QueryRow(ctx, `SELECT count(*) FROM v2_notifications WHERE user_id = $1`, alice).Scan(&n); e != nil || n != 0 {
		t.Fatalf("removed member notified: %d %v", n, e)
	}
}

// A comment that mentions an owner and a member, written while RemoveMember removes that member.
// Both lock the same two membership rows. The interleaving that inverts the order is set up on
// purpose: RemoveMember holds the lower id and waits for the higher one, and only then does the
// comment start. A comment that locked in mention order (higher first) would deadlock with it,
// and PostgreSQL would roll one of them back; locking in id order, it simply waits.
func TestNotifyManyRacesWithRemoveMember(t *testing.T) {
	f := newSquadFixture(t)
	ctx := f.ctx
	owner := f.login(t, "owner", "owner")
	addProject(t, f, owner)
	task := createTask(t, f, owner, store.TaskInput{Title: "并发移出多人"})
	author := f.login(t, "author", "member")
	member := f.login(t, "leaver", "member")
	lower, higher := owner, member
	if member < owner {
		lower, higher = member, owner
	}

	watcher, e := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = watcher.Close(ctx) }()
	waitBlocked := func(n int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			var blocked int
			if e := watcher.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
				WHERE wait_event_type = 'Lock' AND datname = current_database()`).Scan(&blocked); e != nil {
				t.Fatal(e)
			}
			if blocked >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("expected %d blocked sessions, have %d", n, blocked)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Another transaction shares the higher row, so RemoveMember stops between its two rows.
	hold, e := f.db.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = hold.Rollback(ctx) }()
	if _, e := hold.Exec(ctx, `SELECT 1 FROM memberships WHERE tenant_id = $1 AND user_id = $2 FOR KEY SHARE`, f.tenant, higher); e != nil {
		t.Fatal(e)
	}
	removed := make(chan error, 1)
	go func() { removed <- f.store.RemoveMember(ctx, f.tenant, member) }()
	waitBlocked(1)

	// The comment mentions the higher id first, the opposite of RemoveMember's order.
	commented := make(chan error, 1)
	go func() {
		_, e := f.store.CreateTaskComment(ctx, store.NewTaskComment{TenantID: f.tenant, TaskID: task.ID, AuthorID: author,
			CreateKey: "race", Body: "两位看一下",
			Mentions: []store.TaskMention{{Type: "user", ID: higher}, {Type: "user", ID: lower}}})
		commented <- e
	}()
	waitBlocked(2)
	if e := hold.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e := <-removed; e != nil {
		t.Fatalf("RemoveMember rolled back: %v", e)
	}
	if e := <-commented; e != nil {
		t.Fatalf("the comment rolled back: %v", e)
	}
	var n int
	if e := f.db.QueryRow(ctx, `SELECT count(*) FROM v2_notifications WHERE user_id = $1`, member).Scan(&n); e != nil || n != 0 {
		t.Fatalf("removed member kept %d notifications (%v)", n, e)
	}
	if e := f.db.QueryRow(ctx, `SELECT count(*) FROM v2_notifications WHERE user_id = $1 AND kind = 'mentioned'`, owner).Scan(&n); e != nil || n != 1 {
		t.Fatalf("owner got %d mention notifications (%v)", n, e)
	}
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
