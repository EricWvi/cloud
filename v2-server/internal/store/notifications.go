package store

import (
	"context"
	"errors"
	"fmt"
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
}

// notify records the notice for each recipient who may see the task now, by the same decision as
// every tenant route: a recipient who is not a current active member gets nothing.
func notify(ctx context.Context, q dbTX, n notice, recipients ...string) error {
	seen := map[string]bool{}
	for _, user := range recipients {
		if user == "" || user == n.actor || seen[user] {
			continue
		}
		seen[user] = true
		if _, e := tenantAccess(ctx, q, n.tenant, user); errors.Is(e, ErrNotFound) {
			continue
		} else if e != nil {
			return e
		}
		if _, e := q.Exec(ctx, `
			INSERT INTO v2_notifications (tenant_id, user_id, kind, task_id, actor_id, comment_id, source)
			VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid, NULLIF($6, '')::uuid, $7)
			ON CONFLICT (user_id, kind, task_id, source) DO NOTHING`,
			n.tenant, user, n.kind, n.task, n.actor, n.comment, n.source); e != nil {
			return fmt.Errorf("record notification: %w", e)
		}
	}
	return nil
}

// notifyAssigned tells whoever a new assignment puts the task in front of: the person, or every
// current human member of the squad. An Agent assignee has nobody to tell.
func notifyAssigned(ctx context.Context, q dbTX, tenant, task, actor string, ref TaskAssigneeRef, version int64) error {
	var recipients []string
	switch ref.Type {
	case AssigneeHuman:
		recipients = []string{ref.ID}
	case AssigneeSquad:
		rows, e := q.Query(ctx, `
			SELECT m.user_id::text FROM v2_squad_members m JOIN v2_squads s ON s.id = m.squad_id
			WHERE s.tenant_id = $1 AND m.squad_id = $2 AND m.member_type = 'human' AND m.ended_at IS NULL`,
			tenant, ref.ID)
		if e != nil {
			return fmt.Errorf("list squad members to notify: %w", e)
		}
		for rows.Next() {
			var user string
			if e := rows.Scan(&user); e != nil {
				rows.Close()
				return e
			}
			recipients = append(recipients, user)
		}
		rows.Close()
		if e := rows.Err(); e != nil {
			return e
		}
	default:
		return nil
	}
	return notify(ctx, q, notice{tenant: tenant, task: task, kind: NotifyAssigned, actor: actor,
		source: fmt.Sprintf("assign:%d", version)}, recipients...)
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

// Notifications returns the caller's newest notifications in the tenant and how many are unread.
func (s *Store) Notifications(ctx context.Context, tenant, user string, limit int) ([]Notification, int, error) {
	if limit < 1 || limit > MaxNotifications {
		limit = MaxNotifications
	}
	rows, e := s.pool.Query(ctx, `
		SELECT n.id::text, n.kind, n.task_id::text, t.title, t.status, n.actor_id::text, a.display_name,
			n.comment_id::text, COALESCE(left(c.body, 120), ''), n.created_at, n.read_at
		FROM v2_notifications n
		JOIN v2_tasks t ON t.id = n.task_id
		LEFT JOIN users a ON a.id = n.actor_id
		LEFT JOIN v2_task_comments c ON c.id = n.comment_id
		WHERE n.tenant_id = $1 AND n.user_id = $2
		ORDER BY n.created_at DESC, n.id DESC LIMIT $3`, tenant, user, limit)
	if e != nil {
		return nil, 0, fmt.Errorf("list notifications: %w", e)
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		if e := rows.Scan(&n.ID, &n.Kind, &n.TaskID, &n.TaskTitle, &n.TaskStatus, &n.ActorID, &n.ActorName,
			&n.CommentID, &n.Excerpt, &n.CreatedAt, &n.ReadAt); e != nil {
			return nil, 0, e
		}
		out = append(out, n)
	}
	if e := rows.Err(); e != nil {
		return nil, 0, e
	}
	unread, e := s.unreadNotifications(ctx, tenant, user)
	return out, unread, e
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
	for _, id := range ids {
		if !accessUUID.MatchString(id) {
			return 0, ErrNotFound
		}
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
