-- =============================================================================
-- 045_drop_chk_scope.sql — DROP chk_scope constraint (v2.0.0)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Trigger: CRAG-Bench B5 finding — the hard-coded scope-Liste
-- (private|work|shared) prevents adding new scopes (e.g. 'crag') without
-- a schema migration. v2.0.0 makes scope-validation a runtime concern
-- (API-Key allowed_scopes + home_scope already gate access).
--
-- Properties: ALTER TABLE ... DROP CONSTRAINT is metadata-only (no row
-- re-validation). idx_context_scope and the underlying column remain.
--
-- Breaking-Change-Note (per v2-roadmap.md): clients that relied on
-- "rejected at INSERT" for invalid scope strings must validate
-- themselves. Existing rows with private/work/shared remain valid.
--
-- Idempotent: DROP CONSTRAINT IF EXISTS — safe on re-apply.
-- Reversible: ADD CONSTRAINT chk_scope CHECK (scope IN (...)) — fails
-- if any row contains a scope outside the original whitelist.
--
-- Note: chk_blob_scope on context_blobs is NOT touched here (different
-- migration if/when blobs gain multi-tenant scope variants).
-- =============================================================================

ALTER TABLE context_blocks DROP CONSTRAINT IF EXISTS chk_scope;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (45, '045_drop_chk_scope.sql', now())
  ON CONFLICT (version) DO NOTHING;
