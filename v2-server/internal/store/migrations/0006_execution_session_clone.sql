-- The Node clone execution of the checkout a held workflow session's conversation ran in, bound
-- when the session first survives a restart (RecoverExecutions) from its workspace's record. A
-- workflow run's sessions share one workspace, whose record follows the run's latest clone: when
-- Node no longer knows a checkout and a later node of the run clones again, a recovered session
-- bound to the earlier clone has lost its checkout and must not continue on the new one. Empty
-- means unbound: the session never survived a restart, or its checkout had no clone execution.
ALTER TABLE v2_execution_sessions ADD COLUMN clone_execution text NOT NULL DEFAULT '';
