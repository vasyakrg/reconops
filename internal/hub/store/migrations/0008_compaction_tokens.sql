-- (review C2) Compaction LLM calls are an internal housekeeping cost; they
-- must not push the investigation past its max_tokens budget — otherwise
-- the very first compaction would immediately abort the conversation.
-- Track them separately so the budget gate can subtract them.
ALTER TABLE investigations ADD COLUMN compaction_tokens INTEGER NOT NULL DEFAULT 0;
