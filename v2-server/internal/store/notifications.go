package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Notification kinds: why a task was put in front of the recipient.
const (
	NotifyAssigned  = "assigned"
	NotifyMentioned = "mentioned"
	NotifyReview    = "review"
)

// MaxNotifications bounds one page of the inbox.
const MaxNotifications = 100

// Notification is one in-app notice about a task, as its recipient reads it. Actor is nil when no
// person caused it, such as an Agent run handing the task to 待验收. Excerpt is the start of the
// comment for a mention and empty otherwise.
type Notification struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	TaskID     string     `json:"taskId"`
	TaskTitle  string     `json:"taskTitle"`
	TaskStatus string     `json:"taskStatus"`
	ActorID    *string    `json:"actorId"`
	ActorName  *string    `json:"actorName"`
	CommentID  *string    `json:"commentId"`
	Excerpt    string     `json:"excerpt"`
	CreatedAt  time.Time  `json:"createdAt"`
	ReadAt     *time.Time `json:"readAt"`
}

// notice is one event to tell its recipients about, written in the transaction of the change.
type notice struct {
	tenant, task, kind string
	// actor is the person who caused the event, or empty; they are never told about their own act.
	actor string
	// comment is the mentioning comment, or empty.
	comment string
	// source names the event within the task, so a retried write cannot notify twice.
	source string
	// recipients are the people the event concerns, before the access check.
	recipients []string
}

// notify records one notice; see notifyAll.
func notify(ctx context.Context, q dbTX, n notice, recipients ...string) error {
	n.recipients = recipients
	return notifyAll(ctx, q, n)
}

// notifyAll records every notice of one transaction for each recipient who may see the task now,
// by the same decision as every tenant route: a recipient who is not a current active member gets
// nothing. Identifiers are compared in canonical lower case, since a request may spell a UUID in
// upper case. A transaction that notifies passes all its notices in one call.
//
// The recipients' membership rows are locked FOR KEY SHARE before the access check and held until
// the change commits, so a concurrent removal either committed first, and the recipient is
// skipped, or waits; the notification's foreign key can never fail and roll back the comment or
// status change that caused it. They are locked by one statement in user_id order, the order
// RemoveMember locks owner and member rows in, so the two cannot wait on each other.
func notifyAll(ctx context.Context, q dbTX, notices ...notice) error {
	if len(notices) == 0 {
		return nil
	}
	tenant := notices[0].tenant
	wanted := map[string]bool{}
	for i := range notices {
		n := &notices[i]
		if n.tenant != tenant {
			return fmt.Errorf("notifications for two tenants in one transaction")
		}
		n.actor = strings.ToLower(n.actor)
		keep := []string{}
		seen := map[string]bool{}
		for _, user := range n.recipients {
			user = strings.ToLower(user)
			if user == "" || user == n.actor || seen[user] || !accessUUID.MatchString(user) {
				continue
			}
			seen[user] = true
			keep = append(keep, user)
			wanted[user] = true
		}
		n.recipients = keep
	}
	if len(wanted) == 0 {
		return nil
	}
	users := make([]string, 0, len(wanted))
	for user := range wanted {
		users = append(users, user)
	}
	rows, e := q.Query(ctx, `
		SELECT user_id::text FROM memberships WHERE tenant_id = $1 AND user_id = ANY($2::uuid[])
		ORDER BY user_id FOR KEY SHARE`, tenant, users)
	if e != nil {
		return fmt.Errorf("lock recipient memberships: %w", e)
	}
	held := map[string]bool{}
	for rows.Next() {
		var user string
		if e := rows.Scan(&user); e != nil {
			rows.Close()
			return fmt.Errorf("lock recipient memberships: %w", e)
		}
		held[user] = true
	}
	rows.Close()
	if e := rows.Err(); e != nil {
		return fmt.Errorf("lock recipient memberships: %w", e)
	}
	allowed := map[string]bool{}
	for user := range held {
		if _, e := tenantAccess(ctx, q, tenant, user); errors.Is(e, ErrNotFound) {
			continue
		} else if e != nil {
			return e
		}
		allowed[user] = true
	}
	for _, n := range notices {
		for _, user := range n.recipients {
			if !allowed[user] {
				continue
			}
			if _, e := q.Exec(ctx, `
				INSERT INTO v2_notifications (tenant_id, user_id, kind, task_id, actor_id, comment_id, source)
				VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid, NULLIF($6, '')::uuid, $7)
				ON CONFLICT (user_id, kind, task_id, source) DO NOTHING`,
				n.tenant, user, n.kind, n.task, n.actor, n.comment, n.source); e != nil {
				return fmt.Errorf("record notification: %w", e)
			}
		}
	}
	return nil
}

// assignedNotice is the notice for a new assignment, addressed to whoever it puts the task in
// front of: the person, or every current human member of the squad. An Agent assignee has nobody
// to tell.
func assignedNotice(ctx context.Context, q dbTX, tenant, task, actor string, ref TaskAssigneeRef, version int64) (notice, error) {
	n := notice{tenant: tenant, task: task, kind: NotifyAssigned, actor: actor, source: fmt.Sprintf("assign:%d", version)}
	switch ref.Type {
	case AssigneeHuman:
		n.recipients = []string{ref.ID}
	case AssigneeSquad:
		rows, e := q.Query(ctx, `
			SELECT m.user_id::text FROM v2_squad_members m JOIN v2_squads s ON s.id = m.squad_id
			WHERE s.tenant_id = $1 AND m.squad_id = $2 AND m.member_type = 'human' AND m.ended_at IS NULL`,
			tenant, ref.ID)
		if e != nil {
			return n, fmt.Errorf("list squad members to notify: %w", e)
		}
		for rows.Next() {
			var user string
			if e := rows.Scan(&user); e != nil {
				rows.Close()
				return n, e
			}
			n.recipients = append(n.recipients, user)
		}
		rows.Close()
		if e := rows.Err(); e != nil {
			return n, e
		}
	}
	return n, nil
}

// notifyAssigned records the notice for one new assignment.
func notifyAssigned(ctx context.Context, q dbTX, tenant, task, actor string, ref TaskAssigneeRef, version int64) error {
	n, e := assignedNotice(ctx, q, tenant, task, actor, ref, version)
	if e != nil {
		return e
	}
	return notifyAll(ctx, q, n)
}

// notifyReview tells the task's creator that it reached 待验收 at this version.
func notifyReview(ctx context.Context, q dbTX, tenant, task, creator, actor string, version int64) error {
	return notify(ctx, q, notice{tenant: tenant, task: task, kind: NotifyReview, actor: actor,
		source: fmt.Sprintf("review:%d", version)}, creator)
}

// notifyMentioned tells every person a new comment @-mentions; Agent and workflow targets are not
// people.
func notifyMentioned(ctx context.Context, q dbTX, tenant, task string, c TaskComment) error {
	var recipients []string
	for _, m := range c.Mentions {
		if m.Type == "user" {
			recipients = append(recipients, m.ID)
		}
	}
	return notify(ctx, q, notice{tenant: tenant, task: task, kind: NotifyMentioned, actor: c.AuthorID,
		comment: c.ID, source: "comment:" + c.ID}, recipients...)
}

// NotificationCursor is the position after the last notification of a page: the next page starts
// with older ones.
type NotificationCursor struct {
	CreatedAt time.Time
	ID        string
}

// NotificationPage is one page of the caller's inbox, newest first. Next is nil on the last page;
// Unread counts every unread notification, not only this page's.
type NotificationPage struct {
	Items  []Notification
	Unread int
	Next   *NotificationCursor
}

// Notifications returns one page of the caller's notifications in the tenant, newest first, after
// the cursor when one is given, and how many are unread in all. Every notification can be reached
// by following Next, so an unread count never points at one the inbox cannot show.
func (s *Store) Notifications(ctx context.Context, tenant, user string, after *NotificationCursor, limit int) (NotificationPage, error) {
	if limit < 1 || limit > MaxNotifications {
		limit = MaxNotifications
	}
	var at *time.Time
	var afterID *string
	if after != nil {
		if !accessUUID.MatchString(after.ID) {
			return NotificationPage{}, ErrNotFound
		}
		at, afterID = &after.CreatedAt, &after.ID
	}
	rows, e := s.pool.Query(ctx, `
		SELECT n.id::text, n.kind, n.task_id::text, t.title, t.status, n.actor_id::text, a.display_name,
			n.comment_id::text, COALESCE(left(c.body, 120), ''), n.created_at, n.read_at
		FROM v2_notifications n
		JOIN v2_tasks t ON t.id = n.task_id
		LEFT JOIN users a ON a.id = n.actor_id
		LEFT JOIN v2_task_comments c ON c.id = n.comment_id
		WHERE n.tenant_id = $1 AND n.user_id = $2
			AND ($4::timestamptz IS NULL OR (n.created_at, n.id) < ($4::timestamptz, $5::uuid))
		ORDER BY n.created_at DESC, n.id DESC LIMIT $3`, tenant, user, limit+1, at, afterID)
	if e != nil {
		return NotificationPage{}, fmt.Errorf("list notifications: %w", e)
	}
	defer rows.Close()
	page := NotificationPage{Items: []Notification{}}
	for rows.Next() {
		var n Notification
		if e := rows.Scan(&n.ID, &n.Kind, &n.TaskID, &n.TaskTitle, &n.TaskStatus, &n.ActorID, &n.ActorName,
			&n.CommentID, &n.Excerpt, &n.CreatedAt, &n.ReadAt); e != nil {
			return NotificationPage{}, e
		}
		page.Items = append(page.Items, n)
	}
	if e := rows.Err(); e != nil {
		return NotificationPage{}, e
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.Next = &NotificationCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	page.Unread, e = s.unreadNotifications(ctx, tenant, user)
	return page, e
}

// MarkNotificationsRead marks the caller's listed notifications read, or all of them when all is
// set, and returns how many stay unread. Ids that are not the caller's are ignored.
func (s *Store) MarkNotificationsRead(ctx context.Context, tenant, user string, ids []string, all bool) (int, error) {
	if all {
		ids = nil
	}
	if !all && len(ids) == 0 {
		return s.unreadNotifications(ctx, tenant, user)
	}
	for i, id := range ids {
		if !accessUUID.MatchString(id) {
			return 0, ErrNotFound
		}
		ids[i] = strings.ToLower(id)
	}
	if _, e := s.pool.Exec(ctx, `
		UPDATE v2_notifications SET read_at = now()
		WHERE tenant_id = $1 AND user_id = $2 AND read_at IS NULL AND ($3::boolean OR id = ANY($4::uuid[]))`,
		tenant, user, all, ids); e != nil {
		return 0, fmt.Errorf("mark notifications read: %w", e)
	}
	return s.unreadNotifications(ctx, tenant, user)
}

func (s *Store) unreadNotifications(ctx context.Context, tenant, user string) (int, error) {
	var n int
	if e := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM v2_notifications WHERE tenant_id = $1 AND user_id = $2 AND read_at IS NULL`,
		tenant, user).Scan(&n); e != nil {
		return 0, fmt.Errorf("count unread notifications: %w", e)
	}
	return n, nil
}
