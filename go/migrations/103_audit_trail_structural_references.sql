-- =============================================================================
-- 103_audit_trail_structural_references.sql — audit-trail: references link class
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- The daily synthesis report (dream/synthesize_report.go) enumerates the blocks
-- it summarizes (fetchDailyNewBlocks) and — since this wave — persists that
-- fact as deterministic context_structural_links edges (link_class=references,
-- origin=system): report → source block. The write path validates the class
-- against the source type's structural_link_classes allowlist (design/02 §4.1,
-- fail-closed: a type that declares none permits no structural links), and
-- reports classify as audit-trail (metadata.source=dream-synthesis matches the
-- source_prefixes rule). This migration declares the class on the type.
--
-- Deliberate UPDATE of the builtin _global audit-trail row in lockstep with
-- internal/blocktype/builtin.go (085 pattern — the golden gate
-- registry_integration_test.go::TestRegistryGolden_Integration diffs the
-- decoded DB row against the compiled-in builtin set; both move together or it
-- goes red). All other fields stay byte-identical to the 072 seed. Tenant-
-- scoped overrides shadow the _global row via a separate row and are untouched.
-- Idempotent (fixed target values); a second run is a no-op write.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

UPDATE context_block_types
SET config = '{
  "v": 1,
  "retrieval": {"policy": "damped", "damping_factor": 0.3,
                "intent_patterns": ["session","welle","audit","recurrent","handover",
                                    "self-audit","dream v","performance","reset","baseline"]},
  "guard":     {"check": true, "candidate": true},
  "dream":     {"linkable": true},
  "digest":    {"include": true},
  "overview":  {"include": true},
  "parent":    {"mode": "none"},
  "structural_link_classes": ["references"],
  "classify":  {"priority": 20,
                "source_prefixes": ["dream-"],
                "title_patterns": ["session","welle","audit","recurrent","handover",
                                   "self-audit","dream v","performance","reset","baseline"]}
}'::jsonb
WHERE name = 'audit-trail' AND scope = '_global' AND builtin = true;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (103, '103_audit_trail_structural_references.sql', now())
  ON CONFLICT (version) DO NOTHING;
