-- =============================================================================
-- 079_context_projects.sql — project registry + sync run history (workflow W4)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- One row = one project = one repo corpus. scope is the data discriminator
-- (Modell C): every issue/comment block of the project lives in this scope;
-- isolation, RRF, Guard, Dream come for free (vision 019e83df-9666).
-- Design: design/03-workflow-api-cli.md §3.1 (provisional M072, FINAL number 079
-- per masterplan §2 K1); ownership K14 (03-W4 owns the schema; 02-I-F extends it
-- ADDITIVELY in 080 — sync_cursor/forge columns are already the Achse-02 contract).
--
-- INVARIANT: tenant_id = tenant_of(scope). Both columns exist (FK-fast joins),
-- but tenant_id is DERIVED from context_tenant_scopes inside the create-Tx —
-- never taken from the request — and PATCH never changes scope (one source of
-- truth, no inconsistent pairs). The create-Tx assigns the scope to the binding
-- tenant and stamps tenant_id with that same tenant, so the pair is consistent
-- by construction (store.CreateProject).
--
-- identity survives clones/moves: 'github:owner/repo' | 'git-root:<sha>' |
-- 'manual:<slug>' — validated in Go (v2.0.0 line: no CHECK on open sets).
--
-- webhook_secret_ref names a context_secrets row IN THE PROJECT SCOPE with the
-- SERVER-FIXED name 'webhook.github.<project_id>' (§5.3/§5.6). The column is
-- server-managed (set by the W13 webhook-secret lifecycle endpoint, NEVER by
-- PATCH — design/03 §4.2); the plaintext HMAC secret never touches this table.
-- In W4 it is always NULL (no webhook surface yet); the column is fixed now so
-- no later wave rewrites the table.
--
-- lock_timeout (R-MIG2): CREATE TABLE / CREATE INDEX on empty new tables take
-- only brief catalog locks. Forward-only, additive: no backfill (new surface,
-- no existing data), existing scopes/blocks stay byte-identical.
-- Idempotent: CREATE TABLE/INDEX IF NOT EXISTS re-run cleanly; _migrations
-- INSERT ON CONFLICT (version) DO NOTHING.
-- test.sh T07 table count: +2 tables (context_projects, context_project_sync_runs)
-- → 31 → 33; context_blocks columns UNCHANGED (39).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_projects (
    id                 UUID PRIMARY KEY DEFAULT uuidv7(),
    tenant_id          UUID NOT NULL REFERENCES context_tenants(id),
    scope              VARCHAR(50) NOT NULL REFERENCES context_tenant_scopes(scope),
    identity           TEXT NOT NULL,                  -- 'github:owner/repo' | 'git-root:<sha>' | 'manual:<slug>'
    display_name       TEXT NOT NULL DEFAULT '',
    forge              JSONB NOT NULL DEFAULT '{}',    -- {kind:'github', owner, repo, api_base?} — Forge-Abstraktion (Achse 02); api_base-Mutation: §5.7/E6
    webhook_secret_ref TEXT,                           -- server-fixed 'webhook.github.<project_id>' in the PROJECT scope; NULL = no webhook (W13)
    sync_status        TEXT NOT NULL DEFAULT 'idle',   -- idle|running|error — display copy; truth = run-state (§4.4)
    last_sync_at       TIMESTAMPTZ,
    sync_cursor        JSONB NOT NULL DEFAULT '{}',    -- forge-side progress (ETag/updated-since; Achse-02 contract)
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by         UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,
    metadata           JSONB NOT NULL DEFAULT '{}',
    CONSTRAINT uq_projects_scope    UNIQUE (scope),               -- 1 project : 1 scope
    CONSTRAINT uq_projects_identity UNIQUE (tenant_id, identity)  -- Re-Init = idempotency, no duplicate
);
CREATE INDEX IF NOT EXISTS idx_projects_tenant ON context_projects (tenant_id);

-- Sync run history: the counting substrate for project.sync.rate_limit (§4.4 —
-- the I6 mechanic CANNOT carry it: hardcoded 60-s window, keyed per api_key_id,
-- store/blocks.go:277-291; context_access_log has no project dimension). One row
-- per started run; doubles as diagnosis history for `ctx project issues sync
-- --status`. ON DELETE CASCADE off context_projects so a project-delete (or the
-- K14 tenant prune) drains the run history for free — no extra prune step.
CREATE TABLE IF NOT EXISTS context_project_sync_runs (
    id          UUID PRIMARY KEY DEFAULT uuidv7(),
    project_id  UUID NOT NULL REFERENCES context_projects(id) ON DELETE CASCADE,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    status      TEXT NOT NULL DEFAULT 'running',  -- running|done|error|interrupted
    error       TEXT,
    stats       JSONB NOT NULL DEFAULT '{}'       -- Achse-02 contract: fetched/upserted/conflicts
);
CREATE INDEX IF NOT EXISTS idx_sync_runs_project
    ON context_project_sync_runs (project_id, started_at DESC);

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (79, '079_context_projects.sql', now())
  ON CONFLICT (version) DO NOTHING;
