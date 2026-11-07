-- =============================================================================
-- 165_model_access_notify.sql — the revocation signal of running model-API
-- requests: NOTIFY on ctx_model_access_write for every write that can end a
-- model key's validity, and ctx_model_auth_live(uuid[]), the bulk liveness
-- check of the keys behind running requests
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- What. ctx_model_auth (161) decides each new /v1 request; nothing caches a
-- positive answer (design/01 E-01-9). A request that is already running was
-- decided before: a revoked or expired key, a deactivated consumer, a
-- suspended or deleted tenant, or a tenant_scope that moved to another tenant
-- ends it through the in-flight registry of the model-key door
-- (internal/handler/model_inflight.go). This migration gives the registry its
-- two database inputs.
--
-- (1) notify_model_access_write() publishes on channel ctx_model_access_write
-- after every write that can change gates 1-4 of ctx_model_auth:
--   - context_model_keys AFTER UPDATE OF revoked_at, expires_at OR DELETE
--   - context_model_consumers AFTER UPDATE OF active OR DELETE
--   - context_tenants AFTER UPDATE OF status OR DELETE
--   - context_tenant_scopes AFTER UPDATE OR DELETE
-- The column lists keep the batched last_used_at flush of the keys (W01-3)
-- and every other column off the wire: an UPDATE fires only when it names a
-- listed column. A tenant update names status always (store.UpdateTenant
-- writes COALESCE(NULLIF($2, ''), status) with every display_name change),
-- so the function drops a tenant UPDATE whose status did not change. Payload (JSON, identity only — never a hash, a hint or a
-- policy value): {table, op, id} plus, per table, consumer_id and expires_at
-- (keys: the new expiry, so the registry re-arms the key's expiry timer
-- without a query — a rotation's grace shortens the predecessor's
-- expires_at), tenant_id and scope (consumers: their tenant and
-- tenant_scope), and for a tenant scope row scope (the OLD scope, the name
-- the consumers carry), tenant_id (the row's tenant after the write) and no
-- id. The registry does not trust the payload for a verdict: it checks the
-- registered keys the payload touches with ctx_model_auth_live and ends the
-- dead ones. A TRUNCATE fires no row trigger; remove rows with DELETE.
--
-- A narrowed policy (models, prompt_sensitivity, tenant_scope, admission
-- lane, fair weight) fires nothing and ends nothing: it applies from the next
-- request (design/01 §4.6 "Grenze"). The grants of 164 have no trigger here
-- for the same reason.
--
-- (2) ctx_model_auth_live(p_key_ids UUID[]) returns the DEAD ids among
-- p_key_ids: gates 1-4 of ctx_model_auth by key id — the key exists, is not
-- revoked and not expired (now()), its consumer is active, the consumer's
-- tenant is active, the tenant still owns the consumer's tenant_scope. A
-- deleted key (the tenant prune) is dead. Gates 5 and 6 (client network,
-- transport) do not apply: a running request's address and transport do not
-- change. One statement for all keys of all running requests of a process,
-- whatever their number: the registry's periodic sweep
-- (gateway.inflight_sweep_interval) and its sweep after a listener reconnect
-- run it once; a NOTIFY runs it over the keys it touches. Parity with
-- ctx_model_auth(hash, NULL, true).is_valid over the fixture matrix of
-- TestModelAuthGateMatrix is a gate (TestModelAuthLiveParity).
--
-- Cost and locks. No table, no column, no index. CREATE TRIGGER takes SHARE
-- ROW EXCLUSIVE on context_model_keys, context_model_consumers,
-- context_tenants and context_tenant_scopes until the commit — milliseconds,
-- during which writes to those tables wait (a tenant write of another ctx
-- process on the same database among them); reads, and with them the
-- authentication of both credential kinds, do not. lock_timeout 3s fails the
-- boot instead of queueing; the next start retries.
--
-- Idempotent: CREATE OR REPLACE FUNCTION, DROP TRIGGER IF EXISTS + CREATE.
--
-- Way back, in this order, one statement each: DROP TRIGGER IF EXISTS
-- trg_model_keys_access_notify ON context_model_keys; the same for
-- trg_model_consumers_access_notify ON context_model_consumers,
-- trg_tenants_access_notify ON context_tenants and
-- trg_tenant_scopes_access_notify ON context_tenant_scopes; DROP FUNCTION
-- notify_model_access_write(); DROP FUNCTION ctx_model_auth_live(UUID[]);
-- DELETE FROM _migrations WHERE version = 165 (so a later upgrade applies it
-- again). An older binary LISTENs on nothing of this and never calls the
-- function. TestMigration165Idempotent runs exactly these statements.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- 1. The signal. One function for the four tables, its payload built per
--    table from named columns (never to_jsonb(row): context_model_keys
--    carries key_hash).
CREATE OR REPLACE FUNCTION notify_model_access_write() RETURNS TRIGGER AS $$
DECLARE
    v_payload JSONB;
BEGIN
    IF TG_TABLE_NAME = 'context_model_keys' THEN
        IF TG_OP = 'DELETE' THEN
            v_payload := jsonb_build_object('id', OLD.id, 'consumer_id', OLD.consumer_id);
        ELSE
            v_payload := jsonb_build_object('id', NEW.id, 'consumer_id', NEW.consumer_id,
                                            'expires_at', NEW.expires_at);
        END IF;
    ELSIF TG_TABLE_NAME = 'context_model_consumers' THEN
        IF TG_OP = 'DELETE' THEN
            v_payload := jsonb_build_object('id', OLD.id, 'tenant_id', OLD.tenant_id,
                                            'scope', OLD.tenant_scope);
        ELSE
            v_payload := jsonb_build_object('id', NEW.id, 'tenant_id', NEW.tenant_id,
                                            'scope', NEW.tenant_scope);
        END IF;
    ELSIF TG_TABLE_NAME = 'context_tenants' THEN
        IF TG_OP = 'UPDATE' AND OLD.status IS NOT DISTINCT FROM NEW.status THEN
            RETURN NULL; -- a display_name change (UpdateTenant names status always)
        END IF;
        v_payload := jsonb_build_object('id', OLD.id);
    ELSE
        -- context_tenant_scopes: the consumers carry the OLD scope name.
        IF TG_OP = 'DELETE' THEN
            v_payload := jsonb_build_object('scope', OLD.scope, 'tenant_id', OLD.tenant_id);
        ELSE
            v_payload := jsonb_build_object('scope', OLD.scope, 'tenant_id', NEW.tenant_id);
        END IF;
    END IF;
    PERFORM pg_notify('ctx_model_access_write',
        (v_payload || jsonb_build_object('table', TG_TABLE_NAME, 'op', TG_OP))::text);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_model_keys_access_notify ON context_model_keys;
CREATE TRIGGER trg_model_keys_access_notify
    AFTER UPDATE OF revoked_at, expires_at OR DELETE ON context_model_keys
    FOR EACH ROW EXECUTE FUNCTION notify_model_access_write();

DROP TRIGGER IF EXISTS trg_model_consumers_access_notify ON context_model_consumers;
CREATE TRIGGER trg_model_consumers_access_notify
    AFTER UPDATE OF active OR DELETE ON context_model_consumers
    FOR EACH ROW EXECUTE FUNCTION notify_model_access_write();

DROP TRIGGER IF EXISTS trg_tenants_access_notify ON context_tenants;
CREATE TRIGGER trg_tenants_access_notify
    AFTER UPDATE OF status OR DELETE ON context_tenants
    FOR EACH ROW EXECUTE FUNCTION notify_model_access_write();

DROP TRIGGER IF EXISTS trg_tenant_scopes_access_notify ON context_tenant_scopes;
CREATE TRIGGER trg_tenant_scopes_access_notify
    AFTER UPDATE OR DELETE ON context_tenant_scopes
    FOR EACH ROW EXECUTE FUNCTION notify_model_access_write();

-- 2. Bulk liveness: the dead ids among p_key_ids (gates 1-4 of
--    ctx_model_auth by key id; a deleted key is dead).
CREATE OR REPLACE FUNCTION ctx_model_auth_live(p_key_ids UUID[]) RETURNS SETOF UUID
LANGUAGE sql STABLE AS $$
    SELECT q.id FROM unnest(p_key_ids) AS q(id)
     WHERE NOT EXISTS (
        SELECT 1
          FROM context_model_keys k
          JOIN context_model_consumers c ON c.id = k.consumer_id AND c.active
          JOIN context_tenants t         ON t.id = c.tenant_id   AND t.status = 'active'
         WHERE k.id = q.id
           AND k.revoked_at IS NULL
           AND (k.expires_at IS NULL OR k.expires_at > now())
           AND (c.tenant_scope IS NULL OR EXISTS (
                 SELECT 1 FROM context_tenant_scopes s
                  WHERE s.scope = c.tenant_scope AND s.tenant_id = c.tenant_id)))
$$;
