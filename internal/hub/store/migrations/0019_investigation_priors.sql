-- Cross-investigation priors attached to a run: the prior done-investigation
-- IDs whose conclusions were injected into this investigation's context at
-- start (auto host-overlap selection + any operator-selected ones). Stored as a
-- JSON id array so the detail page can show the operator which prior
-- investigations were referenced. '' = none attached. Purely informational —
-- the digest itself rides in the investigation's system seed message.

ALTER TABLE investigations ADD COLUMN priors_json TEXT NOT NULL DEFAULT '';
