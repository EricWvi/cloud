-- A workspace reclaim is the record that a deleted conversation's checkout is to be removed. It
-- is written in the same transaction that deletes the conversation, because that deletion cuts
-- the only link from the conversation to its session and checkout: the reclaim keeps the
-- session, the checkout's identity (workspace id, path, clone execution) and the person's mode,
-- so a failed or interrupted removal is finished from this row after a restart instead of
-- guessing which directories have no owner. mode never changes once recorded: reclaim removes
-- only a checkout whose work the deliveries hold, discard removes it regardless.
--
-- status: pending (waiting for the session's delivery work, or not yet judged), deleting (judged
-- and being removed: from here on only the identity of the path is checked again, so a removal
-- interrupted halfway is finished, never kept), reclaimed and kept (final; reason says why it was
-- kept, or why a reclaimed one was removed by an operator). attempts, last_error and
-- last_attempt_at record failed attempts for operators; delivery_id, delivery_commit and
-- base_commit record the delivery a reclaim relied on, from which an operator restores the work.
-- v2_workspaces stays: deliveries and restart recovery look checkouts up by workspace id.
CREATE TABLE v2_workspace_reclaims (
	workspace_id    text PRIMARY KEY CHECK (workspace_id <> ''),
	tenant_id       uuid NOT NULL,
	project_id      text NOT NULL CHECK (project_id <> ''),
	session_id      text NOT NULL CHECK (session_id <> ''),
	conversation_id uuid NOT NULL,
	path            text NOT NULL CHECK (path <> ''),
	clone_execution text NOT NULL,
	mode            text NOT NULL CHECK (mode IN ('reclaim', 'discard')),
	status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'deleting', 'reclaimed', 'kept')),
	reason          text NOT NULL DEFAULT '',
	attempts        integer NOT NULL DEFAULT 0,
	last_error      text NOT NULL DEFAULT '',
	last_attempt_at timestamptz,
	delivery_id     uuid,
	delivery_commit text,
	base_commit     text,
	removed_bytes   bigint,
	removed_files   bigint,
	requested_at    timestamptz NOT NULL DEFAULT now(),
	finished_at     timestamptz,
	CHECK ((status IN ('reclaimed', 'kept')) = (finished_at IS NOT NULL)),
	CHECK ((status = 'reclaimed') = (removed_bytes IS NOT NULL AND removed_files IS NOT NULL)),
	CHECK (status <> 'kept' OR reason <> '')
);
CREATE INDEX v2_workspace_reclaims_open ON v2_workspace_reclaims (requested_at) WHERE status IN ('pending', 'deleting');
