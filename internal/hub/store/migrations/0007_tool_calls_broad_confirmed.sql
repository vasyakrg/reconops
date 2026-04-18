-- (review C1) Replace the in-rationale string marker
-- "BROAD-SELECTOR-CONFIRMED" with a typed boolean column. The model could
-- (in principle) emit that literal in its own rationale and slip past the
-- gate; a column cannot be forged from prompt content.
ALTER TABLE tool_calls ADD COLUMN broad_confirmed INTEGER NOT NULL DEFAULT 0;
