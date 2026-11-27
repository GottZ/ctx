-- =============================================================================
-- 197_model_consumer_record_bodies.sql — the recording switch of a model
-- consumer: record_bodies, off by default, set by a server-admin only; its
-- audit, and ctx_model_auth carrying it to the /v1 door (issue #571)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- What. A consumer with record_bodies = true has every gateway row of its own
-- (chat, a stream included, embeddings, rerank, rows with an error) written to
-- context_llm_log with the request and the answer in full — whatever
-- gateway.body_logging, the alias, the consumer's prompt_sensitivity, the
-- body scan or Ctx-Sensitivity say, without the 64 KiB cut of the body
-- policy, and marked metadata.gw.recorded = true. Such a row can carry
-- credentials in clear text: its bodies are read by a server-admin only, every
-- other reader gets it as a sealed row (internal/llmlog/recorded.go). The
-- switch changes nothing on the request path (required sensitivity, chain,
-- scan, limits, answer) and nothing for any other consumer. Who may set it is
-- the handler's rule (model-consumer-create/-update: switching on is
-- server-admin only, also with self-service, and needs
-- confirm_record_bodies; switching off is open to a tenant-admin); the column
-- is the state.
--
-- (1) context_model_consumers.record_bodies BOOLEAN NOT NULL DEFAULT false.
--     A constant default: no table rewrite, every existing consumer reads
--     false. NOT part of the store's shared consumer columns
--     (store.sqlConsumerCols): repair and migration tests run the store
--     against schemas before 197; the column is read on demand
--     (store.ConsumerRecordBodies) and written only when a write names it.
-- (2) The audit (161 section 7): audit_state_model_consumer gains the key
--     record_bodies, trg_model_consumers_audit_upd fires on it — switching
--     recording on or off is an audited 'update' row like every policy change
--     (internal/store/audit_classification_test.go pins the column audited).
--     From this migration on every model_consumer audit row carries the key;
--     older rows stay as written.
-- (3) ctx_model_auth gains the column record_bodies, LAST (161: callers scan
--     by name, a new column goes last; the sentinel row reads false). The
--     result type changes, so the function is dropped and created in this
--     transaction: an older binary on the same database selects its named
--     columns and does not see the new one; its rows stay unrecorded. Every
--     /v1 request runs ctx_model_auth once at the door and nothing caches a
--     positive answer (165: design/01 E-01-9), so a switch applies from the
--     next request on; a request already running keeps the value it was
--     admitted with — a narrowed policy fires nothing (165).
--
-- Cost and locks. ADD COLUMN with a constant default is a catalog change:
-- ACCESS EXCLUSIVE on context_model_consumers until the commit — milliseconds;
-- during them a /v1 door check (ctx_model_auth reads the table) and every
-- consumer write of another ctx process on the same database wait. CREATE
-- TRIGGER takes SHARE ROW EXCLUSIVE on the same table, inside the same
-- window. DROP/CREATE FUNCTION locks no table; a session that calls
-- ctx_model_auth meanwhile uses the old definition until the commit.
-- lock_timeout 3s fails the boot instead of queueing behind a long
-- transaction; the next start retries. The table holds at most
-- max_model_consumers (64 by default) rows per tenant.
--
-- Idempotent: ADD COLUMN IF NOT EXISTS, CREATE OR REPLACE FUNCTION, DROP
-- TRIGGER IF EXISTS + CREATE, DROP FUNCTION IF EXISTS + CREATE.
--
-- Way back, in this order, one statement each: DROP FUNCTION ctx_model_auth(
-- TEXT, INET, BOOLEAN); the CREATE FUNCTION ctx_model_auth of 161 section 8;
-- the CREATE OR REPLACE FUNCTION audit_state_model_consumer of 161 section 7;
-- DROP TRIGGER trg_model_consumers_audit_upd ON context_model_consumers and
-- the CREATE TRIGGER of 161 section 7; ALTER TABLE context_model_consumers
-- DROP COLUMN record_bodies; DELETE FROM _migrations WHERE version = 197 (so
-- a later upgrade applies it again). The audit rows stay (append-only
-- history). TestMigration197Idempotent runs exactly these statements.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- 1. The switch.
ALTER TABLE context_model_consumers
    ADD COLUMN IF NOT EXISTS record_bodies BOOLEAN NOT NULL DEFAULT false;

-- 2. The audit: the projection and the update trigger of 161 section 7, with
--    record_bodies.
CREATE OR REPLACE FUNCTION audit_state_model_consumer(c context_model_consumers)
    RETURNS JSONB AS $$
    SELECT jsonb_build_object(
        'tenant_id', c.tenant_id, 'label', c.label, 'description', c.description,
        'kind', c.kind, 'tenant_scope', c.tenant_scope, 'all_models', c.all_models,
        'prompt_sensitivity', c.prompt_sensitivity, 'fair_weight', c.fair_weight,
        'admission_lane', c.admission_lane, 'allowed_cidrs', to_jsonb(c.allowed_cidrs),
        'allow_plaintext', c.allow_plaintext, 'active', c.active,
        'peer_instance_id', c.peer_instance_id, 'peer_bound_at', c.peer_bound_at,
        'record_bodies', c.record_bodies)
$$ LANGUAGE sql STABLE;

DROP TRIGGER IF EXISTS trg_model_consumers_audit_upd ON context_model_consumers;
CREATE TRIGGER trg_model_consumers_audit_upd
    AFTER UPDATE OF tenant_id, label, description, kind, tenant_scope, all_models,
                    prompt_sensitivity, fair_weight, admission_lane, allowed_cidrs,
                    allow_plaintext, active, peer_instance_id, peer_bound_at,
                    record_bodies
    ON context_model_consumers FOR EACH ROW
    WHEN ((OLD.tenant_id, OLD.label, OLD.description, OLD.kind, OLD.tenant_scope,
           OLD.all_models, OLD.prompt_sensitivity, OLD.fair_weight, OLD.admission_lane,
           OLD.allowed_cidrs, OLD.allow_plaintext, OLD.active, OLD.peer_instance_id,
           OLD.peer_bound_at, OLD.record_bodies)
          IS DISTINCT FROM
          (NEW.tenant_id, NEW.label, NEW.description, NEW.kind, NEW.tenant_scope,
           NEW.all_models, NEW.prompt_sensitivity, NEW.fair_weight, NEW.admission_lane,
           NEW.allowed_cidrs, NEW.allow_plaintext, NEW.active, NEW.peer_instance_id,
           NEW.peer_bound_at, NEW.record_bodies))
    EXECUTE FUNCTION audit_model_consumers_write();

-- 3. ctx_model_auth of 161 section 8 with record_bodies as the last column.
DROP FUNCTION IF EXISTS ctx_model_auth(TEXT, INET, BOOLEAN);
CREATE FUNCTION ctx_model_auth(p_key_hash TEXT, p_client_ip INET, p_secure BOOLEAN)
RETURNS TABLE (
    model_key_id       UUID,
    consumer_id        UUID,
    tenant_id          UUID,
    tenant_scope       VARCHAR(50),
    kind               TEXT,
    all_models         BOOLEAN,
    prompt_sensitivity TEXT,
    fair_weight        SMALLINT,
    key_expires_at     TIMESTAMPTZ,
    is_valid           BOOLEAN,
    admission_lane     TEXT,
    peer_instance_id   UUID,
    record_bodies      BOOLEAN
    -- New columns go LAST: callers scan by position.
) LANGUAGE plpgsql STABLE AS $$
DECLARE
    v_key    UUID;
    v_cons   UUID;
    v_exp    TIMESTAMPTZ;
    v_tenant UUID;
    v_scope  VARCHAR(50);
    v_kind   TEXT;
    v_all    BOOLEAN;
    v_sens   TEXT;
    v_weight SMALLINT;
    v_lane   TEXT;
    v_peer   UUID;
    v_cidrs  CIDR[];
    v_plain  BOOLEAN;
    v_status TEXT;
    v_record BOOLEAN;
BEGIN
    <<gates>>
    BEGIN
        -- Gate 1: the key exists and lives.
        SELECT k.id, k.consumer_id, k.expires_at INTO v_key, v_cons, v_exp
          FROM context_model_keys k
         WHERE k.key_hash = p_key_hash
           AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at > now());
        IF v_key IS NULL THEN EXIT gates; END IF;

        -- Gate 2: the consumer is active.
        SELECT c.tenant_id, c.tenant_scope, c.kind, c.all_models, c.prompt_sensitivity,
               c.fair_weight, c.admission_lane, c.peer_instance_id, c.allowed_cidrs,
               c.allow_plaintext, c.record_bodies
          INTO v_tenant, v_scope, v_kind, v_all, v_sens, v_weight, v_lane, v_peer,
               v_cidrs, v_plain, v_record
          FROM context_model_consumers c
         WHERE c.id = v_cons AND c.active;
        IF NOT FOUND THEN EXIT gates; END IF;

        -- Gate 3: the tenant is active.
        SELECT t.status INTO v_status FROM context_tenants t WHERE t.id = v_tenant;
        IF v_status IS NULL OR v_status <> 'active' THEN EXIT gates; END IF;

        -- Gate 4: the tenant still owns the consumer's tenant_scope.
        IF v_scope IS NOT NULL AND NOT EXISTS (
               SELECT 1 FROM context_tenant_scopes s
                WHERE s.scope = v_scope AND s.tenant_id = v_tenant) THEN
            EXIT gates;
        END IF;

        -- Gate 5: network allowlist. An unknown client address (NULL) passes
        -- only when there is none.
        IF v_cidrs IS NOT NULL AND NOT COALESCE(p_client_ip <<= ANY (v_cidrs), false) THEN
            EXIT gates;
        END IF;

        -- Gate 6: transport. Without TLS only with allow_plaintext, which the
        -- CHECK binds to private networks.
        IF NOT COALESCE(p_secure, false) AND NOT v_plain THEN
            EXIT gates;
        END IF;

        RETURN QUERY SELECT v_key, v_cons, v_tenant, v_scope, v_kind, v_all, v_sens,
                            v_weight, v_exp, true, v_lane, v_peer, v_record;
        RETURN;
    END gates;

    -- The sentinel: one row, the same whichever gate refused (no oracle).
    RETURN QUERY SELECT NULL::uuid, NULL::uuid, NULL::uuid, NULL::varchar, ''::text, false,
                        'credentials'::text, 1::smallint, NULL::timestamptz, false,
                        'bulk'::text, NULL::uuid, false;
END;
$$;
