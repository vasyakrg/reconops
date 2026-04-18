-- (C1) Assistant messages carry tool_calls separately from the freeform
-- content (rationale). On reassembly we MUST emit OpenAI-shape
-- {role:assistant, content, tool_calls:[...]} so the matching tool message's
-- tool_call_id resolves. Without this column we lost tool_calls on rehydrate
-- and the conversation broke on the second turn.
ALTER TABLE messages ADD COLUMN tool_calls_json TEXT;
