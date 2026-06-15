-- =============================================================================
-- 058_scope_generalize.sql — legacy scope generalization on the 4 remaining tables
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Multi-Tenant wave T01 (Achse 01). 045 dropped chk_scope and 047 widened
-- context_blocks.scope to VARCHAR(50); 052 did the same for
-- context_api_keys.home_scope. 052:20-22 names the explicit known follow-up:
-- the remaining VARCHAR(20) scope columns on blobs / write_log /
-- ingestion_sources / 016 (dream_links) plus the two surviving 3-value
-- CHECKs (chk_blob_scope, chk_source_scope). This migration is that follow-up
-- for the TABLE COLUMNS + the 2 CHECKs only — so arbitrary, tenant-prefixed
-- scope names ('tenant:crag-research-q1', 'org-1234567:archive') become
-- storable on every data table, consistent with context_blocks.
--
-- Scope of 058 (deliberately narrow): table scope columns + the 2 CHECKs.
-- NOT touched here: the ctx_rrf / ctx_auth-family function signatures that
-- still declare scope/home_scope VARCHAR(20) in their RETURNS TABLE / DECLARE
-- blocks (006/011/030/033/036/037/038/039, 003/007/018/020). 052:20-22 marks
-- "fn signatures 006/011/030" as a SEPARATE later wave because widening a
-- function return type requires DROP FUNCTION + CREATE (CREATE OR REPLACE
-- refuses a changed return type, see 052's ctx_auth note). 058 is metadata
-- on tables only and intentionally does not reopen those functions.
--
-- No consumer: with no tenant emitting non-legacy scope strings, this is
-- behaviorally identical to today (the widened columns still hold every
-- existing ≤20-char value; the dropped CHECKs were the only thing that would
-- have rejected a new short non-legacy value). Pausable / inert until a later
-- wave actually writes tenant-prefixed scopes.
--
-- Properties: ALTER COLUMN ... TYPE VARCHAR(50) is metadata-only when widening
-- (no table rewrite, no row re-validation; 047 line). All existing scope
-- values (≤20 chars) remain valid. DROP CONSTRAINT removes a row-level CHECK,
-- never rewrites.
--
-- lock_timeout (R-MIG2): a metadata-only typmod widening is NOT a rewrite, but
-- ALTER TABLE still acquires ACCESS EXCLUSIVE briefly. At 1M-row scale a
-- concurrent long-running transaction could park that lock request at the head
-- of the lock queue and stall readers behind it. 047 set no lock_timeout, but
-- the migration runner (internal/store/migrations.go) wraps EACH migration in
-- its own real transaction, so SET LOCAL here is transaction-scoped and
-- self-reverting — it does not leak to the session or to migration 059+. We
-- prefer fail-fast (the migration aborts cleanly, rolls back, and is simply
-- re-runnable) over an unbounded ACCESS EXCLUSIVE wait that could freeze these
-- tables behind a slow reader. Strictly safer than 047, no downside for the
-- empty-grant / no-consumer case here. 3s is generous for a metadata-only DDL.
SET LOCAL lock_timeout = '3s';
--
-- Idempotent: DROP CONSTRAINT IF EXISTS + ALTER TYPE is a no-op when the
-- column already is VARCHAR(50); _migrations PK + ON CONFLICT guards the row.
-- Reversible: ALTER COLUMN scope TYPE VARCHAR(20) + re-ADD the CHECKs —
-- narrowing fails if any row has scope >20 chars or a non-legacy value after
-- roll-forward (expected once tenants exist; this is forward-only in practice).
-- =============================================================================

-- 1. Drop the 2 surviving 3-value scope CHECKs (blocks' chk_scope already gone via 045).
ALTER TABLE context_blobs   DROP CONSTRAINT IF EXISTS chk_blob_scope;
ALTER TABLE context_sources DROP CONSTRAINT IF EXISTS chk_source_scope;

-- 2. Widen the 4 remaining VARCHAR(20) scope columns to VARCHAR(50)
--    (context_blocks 047 + context_api_keys.home_scope 052 are already 50).
ALTER TABLE context_blobs       ALTER COLUMN scope TYPE VARCHAR(50);  -- 001_initial.sql:103
ALTER TABLE context_sources     ALTER COLUMN scope TYPE VARCHAR(50);  -- 012_ingestion_sources.sql:19
ALTER TABLE context_dream_links ALTER COLUMN scope TYPE VARCHAR(50);  -- 016_dream.sql:22
ALTER TABLE context_write_log   ALTER COLUMN scope TYPE VARCHAR(50);  -- 001_initial.sql:130

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (58, '058_scope_generalize.sql', now())
  ON CONFLICT (version) DO NOTHING;
