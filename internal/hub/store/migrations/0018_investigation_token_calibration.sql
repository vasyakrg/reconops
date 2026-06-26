-- Per-investigation token-estimation calibration (Task 6). The compaction
-- trigger estimates prompt tokens from a bytes/token ratio (default ~4). This
-- column stores an EWMA of the observed ratio (sent prompt bytes ÷ the
-- provider's reported prompt_tokens), clamped to a sane band, so the estimate
-- self-corrects on log-dense JSON. 0 means "not yet calibrated" → the default
-- ratio applies. It only affects the local estimate; budget gates and
-- compaction-token accounting are unchanged.

ALTER TABLE investigations ADD COLUMN token_calibration_ratio REAL NOT NULL DEFAULT 0;
