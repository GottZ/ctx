-- =============================================================================
-- 063_tenant_quota.sql — per-tenant cost/call budgets + accounting indices (MT, Achse 04)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Multi-Tenant wave T33b (Achse 04-W1, second half). Exact schema vorlage:
-- design/04 §3.2. Quota ENFORCEMENT does not exist today (cost_usd 054:22 +
-- api_key_id 054:23 are logged, never checked) — this lays the policy table
-- (mechanism = code in the enforcement wave 04-W4; policy = this data) plus the
-- accounting / rate-limit access paths the cost-attribution (04-W3) and quota
-- (04-W4) waves read.
--
-- BEHAVIORALLY NEUTRAL / pausable: nothing reads the table yet, and a missing
-- quota row means "no limit" — fail-OPEN by design (a missing row meaning "0"
-- would lock every tenant out the moment the table is created; the fail-CLOSED
-- axis is egress visibility 04-W2, not the cost budget). No CHECK on scope
-- (v2.0.0 line, like 051/053): scope is the free tenant axis; an unknown scope
-- is a new tenant, not a broken gate.
--
-- The NOTIFY trigger rides the same channel as 051/053 (notify_settings_write,
-- 051:91) so a quota write hot-reloads the per-tenant snapshot rather than a
-- hot-path SELECT. Until 065 (Achse 03) carries scope in the NOTIFY payload, a
-- quota write invalidates globally — correct, just less efficient (K-N1); the
-- trigger inherits the scope payload automatically once 065 lands.
--
-- Index Tx trap (db-tenant-surface.md §1, N10): the runner holds each migration
-- in ONE Tx, so CREATE INDEX CONCURRENTLY is forbidden. On context_llm_log
-- (hypertable) the index is built per chunk; on context_access_log (heap, ~91k
-- live rows) a non-concurrent CREATE INDEX is a blocking build on one relation —
-- at large existing volume run it OUT OF BAND (manual CREATE INDEX CONCURRENTLY
-- before the migration deploy; the migration then finds it via IF NOT EXISTS).
-- A tooling seam (N10), not a schema question.
--
-- Idempotent (CREATE TABLE/INDEX IF NOT EXISTS, DROP TRIGGER IF EXISTS + CREATE,
-- _migrations ON CONFLICT). Forward-only, self-registering. 063 closes the
-- 062→064 numbering gap (058-064 now contiguous).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_tenant_quota (
    id                  UUID PRIMARY KEY DEFAULT uuidv7(),
    scope               VARCHAR(50) NOT NULL,        -- the tenant scope (NOT '_global'-capable:
                                                     -- the global default quota lives as a settings key)
    -- Window budgets; NULL = unlimited (fail-OPEN: a missing budget is "no limit").
    daily_cost_usd      NUMERIC(14,8),               -- max external cost / 24h rolling window
    monthly_cost_usd    NUMERIC(14,8),               -- max external cost / 30d rolling window
    daily_calls         INTEGER,                     -- max ATTRIBUTED wire calls / 24h (api_key_id-carried;
                                                     -- does NOT fold background/dream — they carry no api_key_id)
    -- Action on COST-budget exceed: 'block' (Chain returns ErrQuotaExceeded,
    -- external too; local stays) or 'external_off' (only external backends drop
    -- from the chain, local stays). Default 'external_off' = degrade, not lock.
    on_exceed           TEXT NOT NULL DEFAULT 'external_off'
                        CHECK (on_exceed IN ('block','external_off')),
    enabled             BOOLEAN NOT NULL DEFAULT true,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by          UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,
    metadata            JSONB NOT NULL DEFAULT '{}',
    CONSTRAINT uq_tenant_quota_scope UNIQUE (scope)  -- one quota row per tenant
);

-- Hot-reload of the quota policy on the settings NOTIFY channel.
DROP TRIGGER IF EXISTS trg_tenant_quota_notify ON context_tenant_quota;
CREATE TRIGGER trg_tenant_quota_notify
    AFTER INSERT OR UPDATE OR DELETE ON context_tenant_quota
    FOR EACH ROW EXECUTE FUNCTION notify_settings_write();

-- Per-tenant cost-rollup access path: the SUM reads WHERE api_key_id = ANY($keys)
-- AND created_at > now() - window (the keys resolved to a literal UUID list, §6.4).
CREATE INDEX IF NOT EXISTS idx_llm_log_apikey
    ON context_llm_log (api_key_id, created_at DESC) WHERE api_key_id IS NOT NULL;

-- Egress-cost rollup: external + time, cost_usd COVERING (INCLUDE) so the 24h
-- SUM is index-only (no per-row heap fetch). Partial on external keeps it narrow.
CREATE INDEX IF NOT EXISTS idx_llm_log_cost
    ON context_llm_log (created_at DESC) INCLUDE (cost_usd)
    WHERE backend_locality = 'external' AND cost_usd IS NOT NULL;

-- Rate-limit access path: the per-key 60s count (CheckRateLimitByAction,
-- store/blocks.go:277-285) filters api_key_id+action; this composite makes it
-- tenant-selective (today it scans all tenants via the time-only index).
CREATE INDEX IF NOT EXISTS idx_access_log_ratelimit
    ON context_access_log (api_key_id, action, created_at DESC);

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (63, '063_tenant_quota.sql', now())
  ON CONFLICT (version) DO NOTHING;
