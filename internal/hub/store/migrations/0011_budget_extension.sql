-- Per-investigation budget extensions. When an investigation hits the
-- configured max_steps / max_tokens cap, the loop pauses (status='paused')
-- instead of aborting; operator can extend by another slice or finalize
-- with whatever evidence has been gathered. Both columns are additive on
-- top of the global cap from hub.yaml.

ALTER TABLE investigations ADD COLUMN extra_steps  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE investigations ADD COLUMN extra_tokens INTEGER NOT NULL DEFAULT 0;
