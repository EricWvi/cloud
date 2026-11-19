-- An in-app notification tells one team member that a task needs them: it was assigned to them
-- (directly or through a squad they belong to), a comment @-mentioned them, or a task they created
-- reached 待验收. A row hangs off the recipient's membership, so removing someone from the team
-- removes everything they were told there; re-joining does not bring it back. source makes each
-- event write at most one row per recipient, so a retried write stays idempotent.
CREATE TABLE v2_notifications (
	id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id  uuid NOT NULL,
	user_id    uuid NOT NULL,
	kind       text NOT NULL CHECK (kind IN ('assigned', 'mentioned', 'review')),
	task_id    uuid NOT NULL REFERENCES v2_tasks (id) ON DELETE CASCADE,
	actor_id   uuid REFERENCES users (id),
	comment_id uuid REFERENCES v2_task_comments (id) ON DELETE CASCADE,
	source     text NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	read_at    timestamptz,
	FOREIGN KEY (tenant_id, user_id) REFERENCES memberships (tenant_id, user_id) ON DELETE CASCADE,
	UNIQUE (user_id, kind, task_id, source)
);
CREATE INDEX v2_notifications_inbox ON v2_notifications (tenant_id, user_id, created_at DESC, id DESC);
