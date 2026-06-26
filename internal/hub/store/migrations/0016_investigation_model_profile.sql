-- Per-investigation model routing override (Task 14). Empty string means
-- "auto" — the model router picks a profile per operation by role
-- (plan_next_step→primary, compact_memory→summarizer, etc.). A non-empty value
-- pins every operation in this investigation to the named profile (e.g. force
-- the primary tool-capable model, or a cheaper one).

ALTER TABLE investigations ADD COLUMN model_profile TEXT NOT NULL DEFAULT '';
