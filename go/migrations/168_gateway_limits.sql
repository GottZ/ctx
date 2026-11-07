-- =============================================================================
-- 168_gateway_limits.sql — context_gateway_limits: the meter's policy as data
-- (per consumer, per tenant, per instance, tenant envelope)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Until this migration every model-API consumer metered under the gateway.*
-- settings (waves W06-10 … W06-14): one RPM/TPM/L1/day/month/cost floor per
-- op for all. This table makes the limits data (design/06 §3.5, M-06-5, wave
-- W06-15; masterplan §3.0): one row per (subject, tier, op) whose NULL
-- columns say nothing — the dimension inherits from the next level.
--
-- Subjects (subject_kind):
--   consumer        consumer_id set — one consumer's limits. Anchored at the
--                   consumer, never at a key: a rotation or a grace window
--                   neither loses nor doubles them (B-06-19).
--   tenant          tenant_id set — the default of every consumer of the
--                   tenant; a consumer created later inherits it (B-06-19).
--   instance        neither id — the default of every consumer of the
--                   instance. It lies ABOVE the gateway.* settings, which
--                   stay the floor of the chain (they are not replaced).
--   tenant_envelope tenant_id set — the aggregate of all consumers of the
--                   tenant (day/month tokens, day requests, month cost); it
--                   replaces gateway.tenant_envelope_factor × base.
-- Resolution (internal/gatewaylimits, Snapshot.Resolve): consumer → tenant
-- → instance → settings per dimension; per level the server-tier row binds
-- and a tenant-tier row can only narrow (effective = min).
--
-- Tiers: tier is the writer, derived from the caller and never from the
-- request body. server rows are written by a server-admin, free in both
-- directions. tenant rows are written by a tenant-admin for its own tenant
-- and consumers, numerically at most the ceiling of the levels above (the
-- manage action refuses a widening with 400 and the field, B-06-5); the
-- egress permission of a tenant row can only be withdrawn — ck_gl_tenant_narrow
-- holds that in the database too (B-06-20). instance and tenant_envelope rows
-- are server tier only.
--
-- No 0 sentinel (B-06-20): every numeric column is > 0 or NULL (= inherits).
-- The cost budget and the permission to reach billable targets are separate
-- columns: allow_external (reporting cost) and allow_unmetered_external
-- (targets that report none, B-06-29).
--
-- Foreign keys. consumer_id → context_model_consumers ON DELETE CASCADE: the
-- only deleter of a consumer is the tenant prune (01 I-K6), which deletes the
-- limits rows explicitly before the consumers in its stamped transaction, so
-- their audit rows keep tenant_id and via='offboarding'; CASCADE is the
-- backstop. tenant_id → context_tenants ON DELETE CASCADE likewise. The actor
-- columns are the 01 §9.5 census exception (actor-only FK to
-- context_api_keys, ON DELETE SET NULL).
--
-- Hot reload: the ctx_settings_write NOTIFY (entity context_gateway_limits)
-- reloads the registry of ctxd on psql edits; manage writes reload it
-- synchronously before they answer. Audit: trg_gateway_limits_audit, entity
-- gateway_limits, whitelist audit_state_gateway_limit, entity_key = the
-- consumer id (consumer rows, design/05 V-06-e), the tenant id (tenant and
-- envelope rows) or 'instance'; metadata.tenant_id through the consumer or
-- the row.
--
-- Section 2 is the federation contract with cluster C3 (design/04 §3 M-F3):
-- a peer binding exists only at a kind='ctx-peer' consumer. Its columns and
-- CHECKs landed with 161 (ck_model_consumers_peer_binding_pair,
-- ck_model_consumers_peer_binding_kind, idx_model_consumers_peer_instance);
-- this migration asserts them instead of re-creating them.
--
-- Cost and locks: CREATE TABLE and its indexes on a new table; the triggers
-- attach to the new table only. No existing row is read or locked.
-- lock_timeout 3s like every migration since 159. No backfill: an empty
-- table is the settings floor for everyone — the behaviour before.
--
-- Idempotent: IF NOT EXISTS, CREATE OR REPLACE, DROP TRIGGER IF EXISTS +
-- CREATE; a second run changes nothing (TestM168Idempotent).
--
-- Way back: delete the rows through gateway-limits-delete (or psql; the
-- registry then falls back to the settings), then DROP TABLE
-- context_gateway_limits; DROP FUNCTION audit_gateway_limits_write(),
-- audit_state_gateway_limit(context_gateway_limits); DELETE FROM _migrations
-- WHERE version = 168. An older binary neither reads nor writes the table.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- 1. The table.
CREATE TABLE IF NOT EXISTS context_gateway_limits (
    id                       UUID PRIMARY KEY DEFAULT uuidv7(),
    subject_kind             TEXT NOT NULL
        CONSTRAINT ck_gl_subject_kind CHECK (subject_kind IN ('consumer', 'tenant', 'instance', 'tenant_envelope')),
    consumer_id              UUID REFERENCES context_model_consumers(id) ON DELETE CASCADE,
    tenant_id                UUID REFERENCES context_tenants(id) ON DELETE CASCADE,
    tier                     TEXT NOT NULL
        CONSTRAINT ck_gl_tier_value CHECK (tier IN ('server', 'tenant')),
    op                       TEXT NOT NULL
        CONSTRAINT ck_gl_op CHECK (op IN ('chat', 'embed', 'rerank')),
    rpm                      INTEGER  CONSTRAINT ck_gl_rpm             CHECK (rpm > 0),
    tpm                      INTEGER  CONSTRAINT ck_gl_tpm             CHECK (tpm > 0),
    max_concurrency          SMALLINT CONSTRAINT ck_gl_max_concurrency CHECK (max_concurrency > 0),
    daily_tokens             BIGINT   CONSTRAINT ck_gl_daily_tokens    CHECK (daily_tokens > 0),
    monthly_tokens           BIGINT   CONSTRAINT ck_gl_monthly_tokens  CHECK (monthly_tokens > 0),
    daily_requests           INTEGER  CONSTRAINT ck_gl_daily_requests  CHECK (daily_requests > 0),
    monthly_cost_usd         NUMERIC(14, 8) CONSTRAINT ck_gl_monthly_cost_usd CHECK (monthly_cost_usd > 0),
    allow_external           BOOLEAN,
    allow_unmetered_external BOOLEAN,
    updated_by               UUID REFERENCES context_api_keys(id)   ON DELETE SET NULL,
    updated_by_principal     UUID REFERENCES context_principals(id) ON DELETE SET NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The subject's ids: exactly the one id its kind names.
    CONSTRAINT ck_gl_subject CHECK (
        CASE subject_kind
            WHEN 'consumer' THEN consumer_id IS NOT NULL AND tenant_id IS NULL
            WHEN 'instance' THEN consumer_id IS NULL AND tenant_id IS NULL
            ELSE consumer_id IS NULL AND tenant_id IS NOT NULL
        END),
    -- Instance defaults and tenant envelopes are written by the server-admin only.
    CONSTRAINT ck_gl_tier CHECK (tier = 'server' OR subject_kind IN ('consumer', 'tenant')),
    -- Narrowing by structure: a tenant row can only withdraw the egress permission.
    CONSTRAINT ck_gl_tenant_narrow CHECK (tier = 'server'
        OR (allow_external IS NOT TRUE AND allow_unmetered_external IS NULL)),
    -- The envelope is an aggregate: no rates, no concurrency, no unmetered permission.
    CONSTRAINT ck_gl_envelope CHECK (subject_kind <> 'tenant_envelope'
        OR (rpm IS NULL AND tpm IS NULL AND max_concurrency IS NULL AND allow_unmetered_external IS NULL))
);

-- One row per (subject, tier, op); NULL ids are a value here.
CREATE UNIQUE INDEX IF NOT EXISTS uq_gateway_limits
    ON context_gateway_limits (subject_kind, consumer_id, tenant_id, tier, op) NULLS NOT DISTINCT;
-- A consumer's rows (resolution, the consumer's audit trail, the prune).
CREATE INDEX IF NOT EXISTS idx_gateway_limits_consumer
    ON context_gateway_limits (consumer_id) WHERE consumer_id IS NOT NULL;
-- A tenant's rows (resolution, the prune).
CREATE INDEX IF NOT EXISTS idx_gateway_limits_tenant
    ON context_gateway_limits (tenant_id) WHERE tenant_id IS NOT NULL;

-- 2. Federation contract (cluster C3, design/04 §3 M-F3): a peer binding
--    exists only at a ctx-peer consumer. Landed with 161 — asserted here.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'ck_model_consumers_peer_binding_kind'
                     AND conrelid = 'context_model_consumers'::regclass)
       OR NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'ck_model_consumers_peer_binding_pair'
                     AND conrelid = 'context_model_consumers'::regclass) THEN
        RAISE EXCEPTION '168: the peer-binding CHECKs of 161 (M-F3) are missing on context_model_consumers';
    END IF;
END $$;

-- 3. Hot reload: the settings NOTIFY channel, entity context_gateway_limits.
DROP TRIGGER IF EXISTS trg_gateway_limits_notify ON context_gateway_limits;
CREATE TRIGGER trg_gateway_limits_notify
    AFTER INSERT OR UPDATE OR DELETE ON context_gateway_limits
    FOR EACH ROW EXECUTE FUNCTION notify_settings_write();

-- 4. Audit. Whitelist projection: the policy columns, nothing else (the actor
--    goes to the audit row's own columns). STABLE like every audit_state_*.
CREATE OR REPLACE FUNCTION audit_state_gateway_limit(l context_gateway_limits)
    RETURNS JSONB AS $$
    SELECT jsonb_build_object(
        'subject_kind', l.subject_kind, 'consumer_id', l.consumer_id, 'tenant_id', l.tenant_id,
        'tier', l.tier, 'op', l.op,
        'rpm', l.rpm, 'tpm', l.tpm, 'max_concurrency', l.max_concurrency,
        'daily_tokens', l.daily_tokens, 'monthly_tokens', l.monthly_tokens,
        'daily_requests', l.daily_requests, 'monthly_cost_usd', l.monthly_cost_usd,
        'allow_external', l.allow_external, 'allow_unmetered_external', l.allow_unmetered_external)
$$ LANGUAGE sql STABLE;

CREATE OR REPLACE FUNCTION audit_gateway_limits_write() RETURNS TRIGGER AS $$
DECLARE
    a          RECORD;
    v_old      JSONB;
    v_new      JSONB;
    v_consumer UUID := COALESCE(NEW.consumer_id, OLD.consumer_id);
    v_tenant   UUID := COALESCE(NEW.tenant_id, OLD.tenant_id);
BEGIN
    IF TG_OP <> 'INSERT' THEN v_old := audit_state_gateway_limit(OLD); END IF;
    IF TG_OP <> 'DELETE' THEN v_new := audit_state_gateway_limit(NEW); END IF;
    IF TG_OP = 'UPDATE' AND v_old IS NOT DISTINCT FROM v_new THEN
        RETURN NULL;
    END IF;
    IF v_consumer IS NOT NULL THEN
        SELECT c.tenant_id INTO v_tenant FROM context_model_consumers c WHERE c.id = v_consumer;
    END IF;
    -- Fallback actor: the updated_by the statement wrote (like tenant_quota).
    SELECT * INTO a FROM audit_actor_meta(
        CASE WHEN TG_OP = 'INSERT'
               OR (TG_OP = 'UPDATE' AND NEW.updated_by IS DISTINCT FROM OLD.updated_by)
             THEN NEW.updated_by END);
    INSERT INTO context_settings_audit
        (entity_type, entity_key, scope, action, old_value, new_value,
         api_key_id, actor_label, metadata)
    VALUES (
        'gateway_limits',
        COALESCE(v_consumer::text, COALESCE(NEW.tenant_id, OLD.tenant_id)::text, 'instance'),
        '_global',
        CASE TG_OP WHEN 'INSERT' THEN 'create' WHEN 'DELETE' THEN 'delete' ELSE 'update' END,
        v_old, v_new,
        a.actor, a.actor_label,
        a.meta || jsonb_strip_nulls(jsonb_build_object(
            'tenant_id',   v_tenant,
            'consumer_id', v_consumer)));
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_gateway_limits_audit ON context_gateway_limits;
CREATE TRIGGER trg_gateway_limits_audit
    AFTER INSERT OR UPDATE OR DELETE ON context_gateway_limits
    FOR EACH ROW EXECUTE FUNCTION audit_gateway_limits_write();
