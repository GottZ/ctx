-- =============================================================================
-- 160_audit_keys_quota.sql — audit triggers for knowledge keys
-- (context_api_keys) and tenant quotas (context_tenant_quota): whitelist
-- projections, a shared actor context, a per-entity index, one snapshot per
-- existing row
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Until now nobody recorded who minted, re-roled, revoked or deleted an api
-- key, or who changed a tenant's quota. Both tables now write their history
-- into the existing append-only channel context_settings_audit, through AFTER
-- ROW triggers like the settings, secrets, backends and block-type audits: a
-- trigger is atomic with the write and covers every path — API, boot, psql.
--
-- What a row carries. old_value/new_value come ONLY from a whitelist
-- projection, audit_state_api_key(k) and audit_state_tenant_quota(q), never
-- from to_jsonb(row) minus a list of secrets: a blacklist forwards every
-- column a later migration adds, a future secret included, into a table
-- tenant-admins will read (GET /api/audit). Excluded on purpose:
-- context_api_keys.key_hash (the credential), last_used_at (written by every
-- authentication), created_at/updated_at (bookkeeping), id (it is the
-- entity_key); on context_tenant_quota id, created_at, updated_at, the
-- updated_by/updated_by_principal attribution and the writer-less metadata
-- JSONB. A Go test pins the column lists of both tables against these
-- projections (internal/store/audit_classification_test.go): a new column
-- stays red until someone classifies it.
--
-- When a key row is written. INSERT and DELETE always; UPDATE only when one
-- of the ten audited columns is named in the statement AND changes.
-- ctx_auth_by_id sets last_used_at on every authenticated request; a trigger
-- without the OF list would write one audit row per request. The UPDATE
-- action is named after the change: revoke (active true -> false),
-- reactivate (false -> true), update (anything else). The quota trigger
-- fires on every write but records an UPDATE only when the projection
-- changes, so a re-upsert of the same policy and the ON DELETE SET NULL of
-- updated_by after a key hard-delete leave no row.
--
-- Who and how. audit_actor_meta() reads the transaction-local settings the
-- writer stamped: ctx.api_key_id (the acting key; its label, principal,
-- tenant and server-admin flag are copied, so the reader can redact
-- server-admin actors for tenant-admins), ctx.request_id, and ctx.audit_via
-- — the channel, one of api, bootstrap, provision, offboarding (Go
-- constants in internal/store/audit_via.go). Without a stamped via the
-- channel is 'api' when an actor was stamped and 'sql' when none was: a psql
-- edit reads via='sql'. A caller may name a fallback actor (the quota
-- trigger passes the row's updated_by when the statement set it); the
-- fallback fills api_key_id and the actor fields but never turns 'sql' into
-- 'api' — only a stamp does. metadata.tenant_id is the tenant the row
-- belongs to (key: tenant_id; quota: the owner of its scope in
-- context_tenant_scopes, absent for a scope no tenant owns).
--
-- Which rows: one 'snapshot' row per existing key and per existing quota row
-- (whitelist state, metadata {via: 'migration', tenant_id}), written only
-- for an entity without any audit row yet — a second run writes nothing.
-- The backfill runs after CREATE TRIGGER in the same transaction: CREATE
-- TRIGGER takes SHARE ROW EXCLUSIVE, so a concurrent write waits for the
-- commit and is recorded by the trigger, while the backfill sees every row
-- committed before. Live on 2026-11-03: 12 keys, 0 quota rows => 12 rows.
--
-- Cost and locks: no table rewrite. CREATE TRIGGER locks context_api_keys
-- and context_tenant_quota in SHARE ROW EXCLUSIVE until the commit, which
-- also holds back the last_used_at update of every authentication until the
-- backfill is done (live, 12 keys: milliseconds; the W06-6 review measured
-- 0.7 s for the whole file at 1 000 000 audit rows and 2 000 keys); reads
-- are not blocked. lock_timeout 3s fails the boot instead of queueing behind
-- a long transaction, and the next start retries. idx_settings_audit_entity
-- serves the per-entity-type reads of the audit surface (entity_type,
-- created_at DESC); it is built first, before the triggers take their lock.
--
-- Idempotent: CREATE OR REPLACE FUNCTION, DROP TRIGGER IF EXISTS + CREATE,
-- CREATE INDEX IF NOT EXISTS, backfill guarded by NOT EXISTS.
--
-- Way back: DROP TRIGGER trg_api_keys_audit_ins_del, trg_api_keys_audit_upd
-- ON context_api_keys; DROP TRIGGER trg_tenant_quota_audit ON
-- context_tenant_quota; then the five functions and the index. The audit
-- rows stay (append-only history).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- Built before the triggers: it touches only the audit table, so it does not
-- lengthen the lock the triggers take on the two audited tables.
CREATE INDEX IF NOT EXISTS idx_settings_audit_entity
    ON context_settings_audit (entity_type, created_at DESC);

-- Whitelist projection of a key: the ten audited columns, nothing else.
CREATE OR REPLACE FUNCTION audit_state_api_key(k context_api_keys)
    RETURNS JSONB AS $$
    SELECT jsonb_build_object(
        'label', k.label, 'home_scope', k.home_scope,
        'allowed_scopes', to_jsonb(k.allowed_scopes), 'write_scopes', to_jsonb(k.write_scopes),
        'active', k.active, 'is_admin', k.is_admin,
        'tenant_id', k.tenant_id, 'tenant_role', k.tenant_role,
        'confirm_writes', k.confirm_writes, 'principal_id', k.principal_id)
$$ LANGUAGE sql IMMUTABLE;

-- Whitelist projection of a tenant quota: the policy, nothing else.
CREATE OR REPLACE FUNCTION audit_state_tenant_quota(q context_tenant_quota)
    RETURNS JSONB AS $$
    SELECT jsonb_build_object(
        'scope', q.scope,
        'daily_cost_usd', q.daily_cost_usd, 'monthly_cost_usd', q.monthly_cost_usd,
        'daily_calls', q.daily_calls,
        'on_exceed', q.on_exceed, 'enabled', q.enabled)
$$ LANGUAGE sql IMMUTABLE;

-- The actor context of every audit trigger of the OpenAI-API plan (this one,
-- the gateway limits, the model consumers/keys): actor, its label and the
-- metadata keys via, request_id, actor_principal, actor_tenant_id,
-- actor_server_admin (is_admin = server-admin, internal/auth/role.go).
CREATE OR REPLACE FUNCTION audit_actor_meta(p_fallback_actor UUID DEFAULT NULL)
RETURNS TABLE (actor UUID, actor_label TEXT, meta JSONB) AS $$
DECLARE
    v_stamped UUID := NULLIF(current_setting('ctx.api_key_id', true), '')::uuid;
    v_actor   UUID := COALESCE(v_stamped, p_fallback_actor);
    v_via     TEXT := left(NULLIF(current_setting('ctx.audit_via', true), ''), 32);
    v_label TEXT; v_principal UUID; v_tenant UUID; v_admin BOOLEAN;
BEGIN
    IF v_actor IS NOT NULL THEN
        SELECT k.label, k.principal_id, k.tenant_id, k.is_admin
          INTO v_label, v_principal, v_tenant, v_admin
          FROM context_api_keys k WHERE k.id = v_actor;
    END IF;
    RETURN QUERY SELECT v_actor, v_label, jsonb_strip_nulls(jsonb_build_object(
        'via',                COALESCE(v_via, CASE WHEN v_stamped IS NULL THEN 'sql' ELSE 'api' END),
        'request_id',         NULLIF(current_setting('ctx.request_id', true), ''),
        'actor_principal',    v_principal,
        'actor_tenant_id',    v_tenant,
        'actor_server_admin', v_admin));
END;
$$ LANGUAGE plpgsql STABLE;

CREATE OR REPLACE FUNCTION audit_api_keys_write() RETURNS TRIGGER AS $$
DECLARE
    a RECORD;
BEGIN
    SELECT * INTO a FROM audit_actor_meta();
    INSERT INTO context_settings_audit
        (entity_type, entity_key, scope, action, old_value, new_value,
         api_key_id, actor_label, metadata)
    VALUES (
        'api_key',
        COALESCE(NEW.id, OLD.id)::text,
        COALESCE(NEW.home_scope, OLD.home_scope),
        CASE TG_OP WHEN 'INSERT' THEN 'create'
                   WHEN 'DELETE' THEN 'delete'
                   ELSE CASE WHEN OLD.active AND NOT NEW.active THEN 'revoke'
                             WHEN NOT OLD.active AND NEW.active THEN 'reactivate'
                             ELSE 'update' END
        END,
        CASE WHEN TG_OP <> 'INSERT' THEN audit_state_api_key(OLD) END,
        CASE WHEN TG_OP <> 'DELETE' THEN audit_state_api_key(NEW) END,
        a.actor, a.actor_label,
        a.meta || jsonb_strip_nulls(jsonb_build_object(
            'tenant_id', COALESCE(NEW.tenant_id, OLD.tenant_id))));
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_api_keys_audit_ins_del ON context_api_keys;
CREATE TRIGGER trg_api_keys_audit_ins_del
    AFTER INSERT OR DELETE ON context_api_keys
    FOR EACH ROW EXECUTE FUNCTION audit_api_keys_write();

-- UPDATE only on the audited columns, and only when one of them changes:
-- the per-request last_used_at write of ctx_auth_by_id never reaches it.
DROP TRIGGER IF EXISTS trg_api_keys_audit_upd ON context_api_keys;
CREATE TRIGGER trg_api_keys_audit_upd
    AFTER UPDATE OF label, home_scope, allowed_scopes, write_scopes, active, is_admin,
                    tenant_id, tenant_role, confirm_writes, principal_id
    ON context_api_keys FOR EACH ROW
    WHEN ((OLD.label, OLD.home_scope, OLD.allowed_scopes, OLD.write_scopes, OLD.active,
           OLD.is_admin, OLD.tenant_id, OLD.tenant_role, OLD.confirm_writes, OLD.principal_id)
          IS DISTINCT FROM
          (NEW.label, NEW.home_scope, NEW.allowed_scopes, NEW.write_scopes, NEW.active,
           NEW.is_admin, NEW.tenant_id, NEW.tenant_role, NEW.confirm_writes, NEW.principal_id))
    EXECUTE FUNCTION audit_api_keys_write();

CREATE OR REPLACE FUNCTION audit_tenant_quota_write() RETURNS TRIGGER AS $$
DECLARE
    a       RECORD;
    v_old   JSONB;
    v_new   JSONB;
    v_scope TEXT := COALESCE(NEW.scope, OLD.scope);
BEGIN
    IF TG_OP <> 'INSERT' THEN v_old := audit_state_tenant_quota(OLD); END IF;
    IF TG_OP <> 'DELETE' THEN v_new := audit_state_tenant_quota(NEW); END IF;
    IF TG_OP = 'UPDATE' AND v_old IS NOT DISTINCT FROM v_new THEN
        RETURN NULL;
    END IF;
    -- Fallback actor: the updated_by the statement wrote. A DELETE, or an
    -- UPDATE that left updated_by as it was, names no one.
    SELECT * INTO a FROM audit_actor_meta(
        CASE WHEN TG_OP = 'INSERT'
               OR (TG_OP = 'UPDATE' AND NEW.updated_by IS DISTINCT FROM OLD.updated_by)
             THEN NEW.updated_by END);
    INSERT INTO context_settings_audit
        (entity_type, entity_key, scope, action, old_value, new_value,
         api_key_id, actor_label, metadata)
    VALUES (
        'tenant_quota', v_scope, v_scope,
        CASE TG_OP WHEN 'INSERT' THEN 'create' WHEN 'DELETE' THEN 'delete' ELSE 'update' END,
        v_old, v_new,
        a.actor, a.actor_label,
        a.meta || jsonb_strip_nulls(jsonb_build_object(
            'tenant_id', (SELECT ts.tenant_id FROM context_tenant_scopes ts WHERE ts.scope = v_scope))));
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_tenant_quota_audit ON context_tenant_quota;
CREATE TRIGGER trg_tenant_quota_audit
    AFTER INSERT OR UPDATE OR DELETE ON context_tenant_quota
    FOR EACH ROW EXECUTE FUNCTION audit_tenant_quota_write();

-- Backfill: one snapshot per entity without history.
INSERT INTO context_settings_audit
    (entity_type, entity_key, scope, action, old_value, new_value, metadata)
SELECT 'api_key', k.id::text, k.home_scope, 'snapshot', NULL, audit_state_api_key(k),
       jsonb_strip_nulls(jsonb_build_object('via', 'migration', 'tenant_id', k.tenant_id))
  FROM context_api_keys k
 WHERE NOT EXISTS (SELECT 1 FROM context_settings_audit a
                    WHERE a.entity_key = k.id::text AND a.entity_type = 'api_key');

INSERT INTO context_settings_audit
    (entity_type, entity_key, scope, action, old_value, new_value, metadata)
SELECT 'tenant_quota', q.scope, q.scope, 'snapshot', NULL, audit_state_tenant_quota(q),
       jsonb_strip_nulls(jsonb_build_object(
           'via', 'migration',
           'tenant_id', (SELECT ts.tenant_id FROM context_tenant_scopes ts WHERE ts.scope = q.scope)))
  FROM context_tenant_quota q
 WHERE NOT EXISTS (SELECT 1 FROM context_settings_audit a
                    WHERE a.entity_key = q.scope AND a.entity_type = 'tenant_quota');
