-- When set, the investigator loop skips the operator-approval gate for
-- every tool_call in this investigation — useful for low-risk runs the
-- operator wants to babysit only at the start. Per-investigation so it
-- can be flipped without affecting other in-flight runs.

ALTER TABLE investigations ADD COLUMN auto_approve INTEGER NOT NULL DEFAULT 0;
