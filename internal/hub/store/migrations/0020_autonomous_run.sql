-- Autonomous-run budget. When the operator arms an autonomous burst, the loop
-- auto-approves probe tool_calls (like auto_approve) but ONLY until the
-- investigation's running totals reach these ABSOLUTE targets, then pauses for
-- operator review. 0 means "not armed" on that axis (so 0/0 = not armed at all).
--
-- These are tracked separately from auto_approve (migration 0012): auto_approve
-- is the unbounded manual toggle; this is a bounded, self-pausing burst. The
-- terminal mark_done review carve-out is preserved in either mode — a confident
-- conclusion always surfaces for the operator unless OPERATOR FINALIZE was given.
ALTER TABLE investigations ADD COLUMN auto_run_until_steps  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE investigations ADD COLUMN auto_run_until_tokens INTEGER NOT NULL DEFAULT 0;
