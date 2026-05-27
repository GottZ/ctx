-- =============================================================================
-- 047_scope_varchar50.sql — context_blocks.scope VARCHAR(20)→VARCHAR(50) (v2.0.0)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Trigger: M045 dropped chk_scope, enabling arbitrary scope strings for
-- multi-tenant deployments. VARCHAR(20) is too narrow for compound or
-- prefixed scope names (e.g. 'tenant:crag-research-q1' or
-- 'org-1234567:archive'). Bumping to VARCHAR(50) keeps a sane upper bound
-- without ALTER-ing to TEXT.
--
-- Why VARCHAR(50) not TEXT: scope is indexed (idx_context_scope) and used
-- as ANY-Array element in queries (W47-11 backlog). A bounded length
-- preserves planner statistics quality and prevents accidental abuse
-- (e.g. someone storing entire paragraphs in scope).
--
-- Properties: ALTER COLUMN ... TYPE VARCHAR(50) is metadata-only when
-- widening (no row re-validation). All existing scope ≤20-char values
-- remain valid.
--
-- Idempotent via _migrations PK + ON CONFLICT.
-- Reversible: ALTER COLUMN scope TYPE VARCHAR(20) — fails if any row
-- has scope > 20 chars after roll-forward.
-- =============================================================================

ALTER TABLE context_blocks ALTER COLUMN scope TYPE VARCHAR(50);

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (47, '047_scope_varchar50.sql', now())
  ON CONFLICT (version) DO NOTHING;
