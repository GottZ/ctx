-- =============================================================================
-- 085_comment_seed_flip.sql — comment type: INTERIM → §4.1 target (Achse 02, I-E)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Welle I-E (design/02-issue-workflow.md §4.1 / §4.4). Migration 084 seeded the
-- comment type in an INTERIM shape (retrieval=excluded, parent.mode=none) because
-- BOTH mechanisms the §4.1 target needs were unbuilt at I-C time:
--   * the aggregate-to-parent FOLD consumer (Set.AggregateTypes → QueryHandler.
--     foldAggregates) — shipped by Achse-01 T11;
--   * the parent_id WRITE path (store.PutBlockParent, store.InsertCommentBlock) —
--     shipped by Achse-02 I-D.
-- Both are now live (base HEAD T11 + I-D). The decoder's cross-field rule
-- (policy.go: aggregate-to-parent REQUIRES parent.mode != none) and the parent.
-- mode gate both accept the target — the positive probe is
-- blocktype/policy_test.go::TestCommentSeedConfigTarget. So I-E flips the seed to
-- the §4.1 values in lockstep with internal/blocktype/builtin.go (the drift gate
-- registry_integration_test.go::TestRegistryGolden_Integration diffs the decoded
-- DB row against the compiled-in builtin set — both move together or it goes red).
--
-- retrieval=aggregate-to-parent: a comment that ranks in RRF is NOT delivered as
-- itself — it folds onto its parent issue (parent_id), carrying the issue identity
-- + a matched_comment annotation (§4.4). This makes comment a VISIBLE retrieval
-- type (Set.VisibleTypes), unlike the interim excluded shape.
-- parent.mode=required + relationship=comment-of: a comment is never created
-- orphaned (InsertCommentBlock mandates a parent); the fold keeps a parent_id=NULL
-- WARN as the defensive read-side line only.
-- The autonomous-pipeline fields stay OFF (guard.check=false, dream.linkable=
-- false, digest.include=false, overview.include=false) — unchanged from 084.
--
-- This is a deliberate UPDATE of the builtin _global comment row (not ON CONFLICT
-- DO NOTHING like a seed): the flip is a correction of the row 084 planted, and
-- the golden gate requires DB == builtin.go. Tenant-scoped overrides shadow the
-- _global row via a separate row and are untouched. Idempotent (fixed target
-- values); a second run is a no-op write.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

UPDATE context_block_types
SET config = '{
  "v": 1,
  "retrieval": {"policy": "aggregate-to-parent"},
  "guard":     {"check": false, "candidate": false},
  "dream":     {"linkable": false},
  "digest":    {"include": false},
  "overview":  {"include": false},
  "parent":    {"mode": "required", "relationship": "comment-of"},
  "classify":  {}
}'::jsonb
WHERE name = 'comment' AND scope = '_global' AND builtin = true;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (85, '085_comment_seed_flip.sql', now())
  ON CONFLICT (version) DO NOTHING;
