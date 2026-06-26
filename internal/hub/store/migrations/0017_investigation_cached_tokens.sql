-- Prompt-cache effectiveness counter (Task 4). Cache-capable providers
-- (Anthropic via OpenRouter, OpenAI) report how many prompt tokens were served
-- from cache via usage.prompt_tokens_details.cached_tokens. We tally them per
-- investigation so operator diagnostics can show how much of the stable prefix
-- (system prompt + tool schemas) is being re-billed vs. cached. Read-only
-- accounting: it never changes routing or budget gates.

ALTER TABLE investigations ADD COLUMN total_cached_tokens INTEGER NOT NULL DEFAULT 0;
