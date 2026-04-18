-- Optional per-investigation host allowlist. NULL / empty JSON means "all
-- registered hosts" (preserves the pre-existing behaviour). When set, the
-- tool handlers reject collect / collect_batch on any host_id outside the
-- list, and list_hosts only surfaces the allowed ones to the model.

ALTER TABLE investigations ADD COLUMN allowed_hosts_json TEXT;
