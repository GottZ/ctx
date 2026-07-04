-- =============================================================================
-- 080_forge_sync.sql — forge sync-state extension + issue↔block mapping (I-F)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Workflow-engine axis 02, Welle I-F (design/02-issue-workflow.md §3.4/§3.5,
-- §4.5). ADDITIVE extension of the project register (079, W4) — masterplan K14:
-- there is NO separate context_forge_repos table; context_projects (079) IS the
-- forge registration (identity, scope binding, forge JSONB, sync_status/
-- last_sync_at/sync_cursor already shipped in 079). This migration only adds the
-- sync-state columns 079 did not carry, plus the per-entity mapping table.
--
-- ── K14 TRANSLATION of design/02 §3.4 (documented deviation) ──────────────────
-- design/02 §3.4 sketches a standalone `context_forge_repos` (owner/repo/etag_*/
-- since_*/local_seq/…). K14 collapses that onto context_projects. The mapping:
--   context_forge_repos.identity/owner/repo/scope/forge_kind → context_projects
--       (identity + forge JSONB {kind,owner,repo,api_base?}, already in 079)
--   context_forge_repos.token_secret       → context_projects.token_secret (HERE)
--   context_forge_repos.sync_enabled       → context_projects.sync_enabled (HERE)
--   context_forge_repos.push_enabled       → context_projects.push_enabled (HERE)
--   context_forge_repos.last_error         → context_projects.last_error   (HERE)
--   context_forge_repos.backoff_until      → context_projects.backoff_until(HERE)
--   context_forge_repos.etag_*/since_*     → context_projects.sync_cursor JSONB
--       (079 already reserves sync_cursor as "forge-side progress (ETag/updated-
--        since); Achse-02 contract" — NO new columns, the JSONB carries them)
--   context_forge_repos.local_seq          → DEFERRED to I-H (draft #L<n> push
--        numbering; pull-only I-F has no draft-number surface)
-- The mapping table `context_forge_sync` (§3.5) becomes context_project_sync_map,
-- keyed on project_id → context_projects (was repo_id → context_forge_repos).
--
-- ── OWNERSHIP: what I-F writes vs. I-G ────────────────────────────────────────
-- The mapping table's block_id is NOT NULL and references a real block. Block
-- creation + the 3-way hash + mapping-row writes are the Pull-APPLY step, which
-- design/02 §7 assigns to Welle I-G ("Pull-Apply (Blocks/Comments/Status/links)").
-- I-F creates the TABLE (schema) and the client/sync-shell that fetches; it does
-- NOT write mapping rows (no block_id exists without apply). base_hash/conflict/
-- conflict_at/forge_updated_at are the I-G 3-way columns, shipped here forward-
-- only so I-G needs no further migration.
--
-- ── PRUNE (K14) ───────────────────────────────────────────────────────────────
-- context_project_sync_map CASCADEs off BOTH context_projects (project_id) and
-- context_blocks (block_id). PruneTenant drains the block corpus (scope-batched)
-- then context_projects (tenant-keyed) — the mapping rows cascade for free from
-- either side, exactly like context_project_sync_runs (079). No new drain step;
-- the store/tenant.go PruneTenant comment is extended to record this, and the
-- I-F gate proves "PruneTenant ⇒ 0 mapping rows of the tenant".
--
-- ── LOCKS / IDEMPOTENCY (R-MIG2, 069 pattern) ─────────────────────────────────
-- ADD COLUMN IF NOT EXISTS with a constant/NULL DEFAULT is a metadata-only
-- catalog change on PG18 (no table rewrite); CREATE TABLE/INDEX IF NOT EXISTS
-- take brief catalog locks on an empty new table. Forward-only, no backfill
-- (new surface), existing rows/blocks byte-identical. Idempotent on re-run.
-- test.sh T07 table count: +1 table (context_project_sync_map) → 33 → 34;
-- context_blocks columns UNCHANGED (40). context_projects columns +5.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- 1: forge sync-state columns on the project register (K14 additive extension).
--    token_secret  = NAME of a context_secrets row in the PROJECT scope (server-
--                    fixed 'forge.token.<project_id>'); the PAT plaintext NEVER
--                    lives here (sealbox line, 051). NULL = local-only / unauth.
--    sync_enabled  = the periodic loop's iterate predicate (fail-closed: a scope
--                    that lost its tenant is set false by the run, §4.5.5/S13).
--    push_enabled  = fail-closed pull-only until an explicit tenant-admin toggle
--                    (§5.6; the write channel opens in I-H, default false now).
--    last_error / backoff_until = offline-first resilience: a wire/rate-limit
--                    error stamps these and the run backs off exponentially
--                    (cap 1h), local work continues (§4.5.3).
ALTER TABLE context_projects
    ADD COLUMN IF NOT EXISTS token_secret  TEXT,
    ADD COLUMN IF NOT EXISTS sync_enabled  BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS push_enabled  BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS last_error    TEXT,
    ADD COLUMN IF NOT EXISTS backoff_until TIMESTAMPTZ;

-- 2: per-entity issue↔block mapping + 3-way sync state (design/02 §3.5/§3.6).
--    Sync writes NEVER go through UpsertBlock (§3.5): the sync identity is
--    (project, kind, forge_id) → block_id, so forge-side title changes are
--    identity-neutral and the 3-way base_hash lives on the mapping row, not the
--    block. forge_id = GitHub issue number / comment id; 0 = local-only (created
--    in ctx, not yet pushed — I-H). base_hash = sha256 of the canonical
--    projection at last successful sync (§3.6; WRITTEN by I-G apply, never here).
--    forge_updated_at is telemetry/display ONLY — never a direction input (W16).
CREATE TABLE IF NOT EXISTS context_project_sync_map (
    project_id       UUID NOT NULL REFERENCES context_projects(id) ON DELETE CASCADE,
    entity_kind      TEXT NOT NULL,                    -- 'issue' | 'comment'
    forge_id         BIGINT NOT NULL DEFAULT 0,        -- GitHub number/id; 0 = local-only (I-H push)
    block_id         UUID NOT NULL REFERENCES context_blocks(id) ON DELETE CASCADE,
    base_hash        VARCHAR(64) NOT NULL,             -- sha256 canonical projection @ last sync (I-G writes)
    conflict         BOOLEAN NOT NULL DEFAULT false,   -- 3-way divergence (I-G sets)
    conflict_at      TIMESTAMPTZ,
    forge_updated_at TIMESTAMPTZ,                      -- telemetry ONLY, never direction (W16)
    synced_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata         JSONB NOT NULL DEFAULT '{}',
    PRIMARY KEY (project_id, entity_kind, forge_id, block_id)
);

-- One mapping row per block (a block belongs to at most one forge entity).
CREATE UNIQUE INDEX IF NOT EXISTS uq_sync_map_block
    ON context_project_sync_map (block_id);
-- Conflict surface for forge-sync-status/CLI/UI: partial, small (§6.3).
CREATE INDEX IF NOT EXISTS idx_sync_map_conflict
    ON context_project_sync_map (project_id) WHERE conflict;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (80, '080_forge_sync.sql', now())
  ON CONFLICT (version) DO NOTHING;
