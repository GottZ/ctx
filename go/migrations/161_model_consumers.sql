-- =============================================================================
-- 161_model_consumers.sql — external model consumers and their keys
-- (context_model_consumers, context_model_keys), the one model-auth function
-- ctx_model_auth(hash, ip, secure), lifecycle and identity triggers, the
-- whitelist audit of both tables, and the tenant index of the audit read
-- surface
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- What. A model consumer is an external caller of the models ctx serves — an
-- application or another ctx instance (kind 'app' | 'ctx-peer') — with its
-- policy: the tenant it belongs to, pool visibility (tenant_scope, never a
-- corpus right), the release of every visible model alias (all_models; single
-- grants follow in their own migration), the prompt-sensitivity floor, the
-- fairness weight, the ceiling of the external admission lane, a network
-- allowlist, the plaintext permission and the peer binding. A model key
-- (ctxm_...) is a credential of exactly one consumer and carries no policy:
-- rotating a key changes no right. Model consumers are not knowledge keys:
-- no AuthResult, no corpus scope; the only references to context_api_keys
-- are the actor columns created_by_key / revoked_by_key, ON DELETE SET NULL.
-- Nothing in ctx writes these tables yet (admin actions and the /v1 door
-- ship later); until then this migration has no effect outside the database.
--
-- What is never stored: the key itself. A model key lives here as its SHA-256
-- (key_hash, 64 lowercase hex) and its hint (key_hint); the plaintext exists
-- once, in the answer of the mint.
--
-- ctx_model_auth(p_key_hash, p_client_ip, p_secure) is the single check of a
-- presented model key: six gates — key alive (not revoked, not expired;
-- expires_at = now() is expired), consumer active, tenant active, the
-- consumer's tenant_scope still owned by its tenant, network allowlist,
-- transport — and ONE sentinel row for every refusal, so the caller cannot
-- tell which gate refused. STABLE and read-only: it writes no last_used_at
-- (the Go side batches that stamp), so a refused request never writes. The
-- result columns are a contract; a new column goes last (a changed result
-- type needs DROP + CREATE).
--
-- Triggers. Deactivating a consumer revokes its live keys and refuses new
-- ones, psql included: a key insert locks its consumer row FOR SHARE and
-- fails with 23514 on an inactive consumer, so a mint and a deactivation
-- serialize on that row — under READ COMMITTED (every ctx writer) either the
-- deactivation waits and revokes the new key, or the mint waits and fails.
-- Reactivating revives no old key. Known limit: a deactivation under
-- explicit REPEATABLE READ does not revoke a key committed after its
-- snapshot (the key stays live at the inactive consumer and would be valid
-- after a reactivation). tenant_id of a consumer and
-- consumer_id of a key are immutable (SQLSTATE 23514): a key moved to another
-- consumer, or a consumer moved to another tenant, would carry a credential
-- into a foreign policy or tenant. Both tables write their history into
-- context_settings_audit (entity_type model_consumer / model_key) through
-- the whitelist projections audit_state_model_consumer(c) and
-- audit_state_model_key(k) — never to_jsonb(row) minus a list, which would
-- forward every column a later migration adds — with the actor context of
-- audit_actor_meta() (160). metadata adds tenant_id (the filter of the
-- tenant-admin read surface; a key row resolves it through its consumer) and
-- consumer_id. Actions: create, prune (DELETE; only the tenant prune deletes),
-- revoke (key: revoked_at set), deactivate / reactivate (consumer: active
-- flips), update. Never recorded: key_hash, last_used_at (the batched usage
-- stamp — the UPDATE triggers carry a column list, so that flush writes no
-- row), the created_at / updated_at bookkeeping and the actor columns
-- created_by_* / revoked_by_* (the actor travels in api_key_id, actor_label
-- and metadata.actor_*). internal/store/audit_classification_test.go pins
-- every column of both tables as audited or excluded.
--
-- Tenant index of the audit read surface. GET /api/audit filters a
-- tenant-admin's rows on metadata->>'tenant_id'. With idx_settings_audit_entity
-- alone, a tenant without rows of the entity walked the entity's whole index
-- range (1 000 000 rows: 1.1-3.0 s per request, depending on host load).
-- idx_settings_audit_entity_tenant (entity_type, tenant, created_at DESC)
-- serves the tenant shapes directly (same data: 0.3 ms); the reader's SQL is
-- unchanged.
--
-- Cost and locks. No table rewrite (max_model_consumers has a constant
-- default). The index is built first: CREATE INDEX holds SHARE on
-- context_settings_audit until the commit, so every audited write (settings,
-- secrets, backends, keys, quotas) waits for the build, reads do not. Live
-- (about 500 audit rows) that is milliseconds; measured 4.4-9.1 s for
-- 1 100 000 audit rows (82 MB index) on a loaded host. Afterwards ADD COLUMN
-- takes ACCESS EXCLUSIVE on context_tenants and the foreign keys SHARE ROW
-- EXCLUSIVE on context_tenants, context_api_keys and context_principals, each
-- held until the commit — milliseconds after the build, during which
-- knowledge-key authentication of another ctx process on the same database
-- waits.
-- lock_timeout 3s fails the boot instead of queueing behind a long
-- transaction; the next start retries.
--
-- Idempotent: CREATE INDEX / TABLE IF NOT EXISTS, ADD COLUMN IF NOT EXISTS,
-- CREATE OR REPLACE FUNCTION, DROP TRIGGER IF EXISTS + CREATE. No backfill:
-- both tables start empty; every tenant, the default tenant included, gets
-- max_model_consumers = 64.
--
-- Way back, in this order: DROP FUNCTION ctx_model_auth(TEXT, INET, BOOLEAN),
-- audit_state_model_key(context_model_keys) and
-- audit_state_model_consumer(context_model_consumers) (they take the tables'
-- row types); DROP TABLE context_model_keys, context_model_consumers (their
-- triggers go with them); DROP FUNCTION audit_model_keys_write(),
-- audit_model_consumers_write(), model_keys_identity_immutable(),
-- model_consumers_identity_immutable(), model_keys_consumer_active(),
-- model_consumer_deactivate_revokes(), model_cidrs_private(CIDR[]);
-- ALTER TABLE context_tenants DROP COLUMN
-- max_model_consumers; DROP INDEX idx_settings_audit_entity_tenant; DELETE
-- FROM _migrations WHERE version = 161 (so a later upgrade applies it
-- again). The audit rows stay (append-only history).
-- TestMigration161Idempotent runs exactly these statements.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- 0. Tenant index of the audit read surface. First: it is the one step whose
--    duration grows with data, so the locks the steps below take on
--    context_tenants, context_api_keys and context_principals are held for
--    milliseconds, not for the build.
CREATE INDEX IF NOT EXISTS idx_settings_audit_entity_tenant
    ON context_settings_audit (entity_type, (metadata->>'tenant_id'), created_at DESC);

-- 1. Private networks (RFC 1918, RFC 4193 ULA, loopback): the precondition of
--    allow_plaintext. IMMUTABLE so a CHECK may call it, which binds psql
--    writes too. A NULL or empty array and a NULL element are not private.
CREATE OR REPLACE FUNCTION model_cidrs_private(p CIDR[]) RETURNS BOOLEAN
LANGUAGE sql IMMUTABLE AS $$
    SELECT p IS NOT NULL AND cardinality(p) > 0 AND NOT EXISTS (
        SELECT 1 FROM unnest(p) AS c(n)
         WHERE n IS NULL
            OR NOT (n <<= '10.0.0.0/8'::cidr     OR n <<= '172.16.0.0/12'::cidr
                 OR n <<= '192.168.0.0/16'::cidr OR n <<= '127.0.0.0/8'::cidr
                 OR n <<= 'fc00::/7'::cidr       OR n <<= '::1/128'::cidr))
$$;

-- 2. Structural cap per tenant (the max_keys pattern): a storage and UI bound,
--    not the fairness brake. 64 for every tenant, the default tenant
--    included; NULL = unlimited, set only by hand.
ALTER TABLE context_tenants
    ADD COLUMN IF NOT EXISTS max_model_consumers INTEGER DEFAULT 64
        CHECK (max_model_consumers IS NULL OR max_model_consumers >= 0);

-- 3. Consumer = caller identity + policy.
CREATE TABLE IF NOT EXISTS context_model_consumers (
    id                   UUID PRIMARY KEY DEFAULT uuidv7(),
    -- No DEFAULT: a consumer names its tenant. RESTRICT: the tenant prune
    -- removes consumers before the tenant row. Immutable (section 6).
    tenant_id            UUID NOT NULL REFERENCES context_tenants(id) ON DELETE RESTRICT,
    label                VARCHAR(100) NOT NULL,
    description          TEXT NOT NULL DEFAULT '',
    kind                 TEXT NOT NULL DEFAULT 'app' CHECK (kind IN ('app', 'ctx-peer')),
    -- Pool visibility and config-overlay namespace, never a corpus right.
    -- NULL = _global backends only. Gate 4 checks the tenant still owns it.
    tenant_scope         VARCHAR(50) CHECK (tenant_scope IS NULL OR tenant_scope NOT LIKE '\_%'),
    -- Release of every model alias the consumer can see; only an explicit true.
    all_models           BOOLEAN NOT NULL DEFAULT false,
    -- Floor of the prompt sensitivity; the default admits full-trust backends only.
    prompt_sensitivity   TEXT NOT NULL DEFAULT 'credentials'
                         CHECK (prompt_sensitivity IN ('credentials', 'personal', 'internal', 'public')),
    -- Weight in the external fairness.
    fair_weight          SMALLINT NOT NULL DEFAULT 1 CHECK (fair_weight BETWEEN 1 AND 100),
    -- Ceiling of the external lane: urgent may run above ctx's own background
    -- work, bulk at most below it. Default bulk (fail-closed).
    admission_lane       TEXT NOT NULL DEFAULT 'bulk' CHECK (admission_lane IN ('urgent', 'bulk')),
    -- Network allowlist; NULL = any source (gate 5).
    allowed_cidrs        CIDR[],
    -- Transport without TLS allowed — only with private networks only (gate 6).
    allow_plaintext      BOOLEAN NOT NULL DEFAULT false,
    active               BOOLEAN NOT NULL DEFAULT true,
    -- Peer binding of a ctx-peer: the instance it proved on first use;
    -- NULL = not bound yet.
    peer_instance_id     UUID,
    peer_bound_at        TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by_key       UUID REFERENCES context_api_keys(id)   ON DELETE SET NULL,
    created_by_principal UUID REFERENCES context_principals(id) ON DELETE SET NULL,
    CONSTRAINT uq_model_consumers_tenant_label UNIQUE (tenant_id, label),
    CONSTRAINT ck_model_consumers_plaintext_private
        CHECK (NOT allow_plaintext OR model_cidrs_private(allowed_cidrs)),
    CONSTRAINT ck_model_consumers_peer_binding_pair
        CHECK ((peer_instance_id IS NULL) = (peer_bound_at IS NULL)),
    CONSTRAINT ck_model_consumers_peer_binding_kind
        CHECK (peer_instance_id IS NULL OR kind = 'ctx-peer')
);
CREATE INDEX IF NOT EXISTS idx_model_consumers_tenant_active
    ON context_model_consumers (tenant_id) WHERE active;
-- Which consumers belong to instance X (status, the peer's own rotation).
CREATE INDEX IF NOT EXISTS idx_model_consumers_peer_instance
    ON context_model_consumers (peer_instance_id) WHERE peer_instance_id IS NOT NULL;

-- 4. Model key = credential. Exactly one index on key_hash (the UNIQUE).
CREATE TABLE IF NOT EXISTS context_model_keys (
    id                   UUID PRIMARY KEY DEFAULT uuidv7(),
    -- Immutable (section 6). RESTRICT: the tenant prune deletes keys first.
    consumer_id          UUID NOT NULL REFERENCES context_model_consumers(id) ON DELETE RESTRICT,
    key_hash             TEXT NOT NULL CHECK (key_hash ~ '^[0-9a-f]{64}$'),
    key_hint             VARCHAR(16) NOT NULL,
    expires_at           TIMESTAMPTZ,
    revoked_at           TIMESTAMPTZ,
    -- SET NULL: a rotation chain is deletable in one statement (tenant prune).
    rotated_from         UUID REFERENCES context_model_keys(id) ON DELETE SET NULL,
    -- Written by a batched flush from Go, never per request.
    last_used_at         TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by_key       UUID REFERENCES context_api_keys(id)   ON DELETE SET NULL,
    created_by_principal UUID REFERENCES context_principals(id) ON DELETE SET NULL,
    revoked_by_key       UUID REFERENCES context_api_keys(id)   ON DELETE SET NULL,
    revoked_by_principal UUID REFERENCES context_principals(id) ON DELETE SET NULL,
    CONSTRAINT uq_model_keys_hash UNIQUE (key_hash),
    CONSTRAINT ck_model_keys_revoke_order CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);
CREATE INDEX IF NOT EXISTS idx_model_keys_consumer_live
    ON context_model_keys (consumer_id) WHERE revoked_at IS NULL;
-- Key list of a consumer (recently revoked included) and its paginated history.
CREATE INDEX IF NOT EXISTS idx_model_keys_consumer_created
    ON context_model_keys (consumer_id, created_at DESC, id DESC);

-- 5. Deactivating a consumer revokes all its live keys — on every write
--    path (API, boot, psql), so nothing here may fail: a key another transaction committed after this
--    one began is newer than now() (revoked at its created_at instead, which
--    the revoke-order CHECK demands), and a stamped actor key that no longer
--    exists leaves revoked_by_key NULL instead of failing its foreign key.
CREATE OR REPLACE FUNCTION model_consumer_deactivate_revokes() RETURNS TRIGGER AS $$
BEGIN
    UPDATE context_model_keys
       SET revoked_at     = GREATEST(now(), created_at),
           revoked_by_key = (SELECT a.id FROM context_api_keys a
                              WHERE a.id = NULLIF(current_setting('ctx.api_key_id', true), '')::uuid)
     WHERE consumer_id = NEW.id AND revoked_at IS NULL;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_model_consumers_deactivate ON context_model_consumers;
CREATE TRIGGER trg_model_consumers_deactivate
    AFTER UPDATE OF active ON context_model_consumers
    FOR EACH ROW WHEN (OLD.active AND NOT NEW.active)
    EXECUTE FUNCTION model_consumer_deactivate_revokes();

-- A key only for an active consumer. The foreign key alone takes FOR KEY
-- SHARE on the consumer row, which the deactivating UPDATE (FOR NO KEY
-- UPDATE) does not wait for: a mint overlapping a deactivation committed a
-- live key at the inactive consumer. FOR SHARE conflicts with that UPDATE:
-- a mint holding it makes the deactivation wait (whose revoke then sees the
-- committed key), a mint behind a running deactivation waits and reads the
-- committed active = false.
CREATE OR REPLACE FUNCTION model_keys_consumer_active() RETURNS TRIGGER AS $$
DECLARE
    v_active BOOLEAN;
BEGIN
    SELECT c.active INTO v_active
      FROM context_model_consumers c WHERE c.id = NEW.consumer_id
       FOR SHARE;
    IF v_active IS NULL OR NOT v_active THEN
        RAISE EXCEPTION 'context_model_keys: consumer % is not active', NEW.consumer_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_model_keys_consumer_active ON context_model_keys;
CREATE TRIGGER trg_model_keys_consumer_active
    BEFORE INSERT ON context_model_keys
    FOR EACH ROW EXECUTE FUNCTION model_keys_consumer_active();

-- 6. Identity pins: a consumer stays in its tenant, a key with its consumer.
CREATE OR REPLACE FUNCTION model_consumers_identity_immutable() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id THEN
        RAISE EXCEPTION 'context_model_consumers: tenant_id is immutable'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_model_consumers_identity ON context_model_consumers;
CREATE TRIGGER trg_model_consumers_identity
    BEFORE UPDATE OF tenant_id ON context_model_consumers
    FOR EACH ROW EXECUTE FUNCTION model_consumers_identity_immutable();

CREATE OR REPLACE FUNCTION model_keys_identity_immutable() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.consumer_id IS DISTINCT FROM OLD.consumer_id THEN
        RAISE EXCEPTION 'context_model_keys: consumer_id is immutable'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_model_keys_identity ON context_model_keys;
CREATE TRIGGER trg_model_keys_identity
    BEFORE UPDATE OF consumer_id ON context_model_keys
    FOR EACH ROW EXECUTE FUNCTION model_keys_identity_immutable();

-- 7. Audit. Whitelist projections: the audited columns, nothing else. STABLE,
--    not IMMUTABLE: the JSON form of a timestamptz follows the session's
--    TimeZone.
CREATE OR REPLACE FUNCTION audit_state_model_consumer(c context_model_consumers)
    RETURNS JSONB AS $$
    SELECT jsonb_build_object(
        'tenant_id', c.tenant_id, 'label', c.label, 'description', c.description,
        'kind', c.kind, 'tenant_scope', c.tenant_scope, 'all_models', c.all_models,
        'prompt_sensitivity', c.prompt_sensitivity, 'fair_weight', c.fair_weight,
        'admission_lane', c.admission_lane, 'allowed_cidrs', to_jsonb(c.allowed_cidrs),
        'allow_plaintext', c.allow_plaintext, 'active', c.active,
        'peer_instance_id', c.peer_instance_id, 'peer_bound_at', c.peer_bound_at)
$$ LANGUAGE sql STABLE;

CREATE OR REPLACE FUNCTION audit_state_model_key(k context_model_keys)
    RETURNS JSONB AS $$
    SELECT jsonb_build_object(
        'consumer_id', k.consumer_id, 'key_hint', k.key_hint,
        'expires_at', k.expires_at, 'revoked_at', k.revoked_at,
        'rotated_from', k.rotated_from)
$$ LANGUAGE sql STABLE;

-- scope = the consumer's tenant_scope, '_global' when it has none — the
-- namespace the consumer acts in, as home_scope is for a knowledge key.
CREATE OR REPLACE FUNCTION audit_model_consumers_write() RETURNS TRIGGER AS $$
DECLARE
    a RECORD;
BEGIN
    SELECT * INTO a FROM audit_actor_meta();
    INSERT INTO context_settings_audit
        (entity_type, entity_key, scope, action, old_value, new_value,
         api_key_id, actor_label, metadata)
    VALUES (
        'model_consumer',
        COALESCE(NEW.id, OLD.id)::text,
        COALESCE(CASE WHEN TG_OP = 'DELETE' THEN OLD.tenant_scope ELSE NEW.tenant_scope END, '_global'),
        CASE TG_OP WHEN 'INSERT' THEN 'create'
                   WHEN 'DELETE' THEN 'prune'
                   ELSE CASE WHEN OLD.active AND NOT NEW.active THEN 'deactivate'
                             WHEN NOT OLD.active AND NEW.active THEN 'reactivate'
                             ELSE 'update' END
        END,
        CASE WHEN TG_OP <> 'INSERT' THEN audit_state_model_consumer(OLD) END,
        CASE WHEN TG_OP <> 'DELETE' THEN audit_state_model_consumer(NEW) END,
        a.actor, a.actor_label,
        a.meta || jsonb_strip_nulls(jsonb_build_object(
            'tenant_id',   COALESCE(NEW.tenant_id, OLD.tenant_id),
            'consumer_id', COALESCE(NEW.id, OLD.id))));
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- A key row takes tenant_id and scope from its consumer. That row is gone
-- only when one statement deletes keys and consumer together (a writable
-- CTE): the key's prune row then lacks metadata.tenant_id and reads scope
-- '_global'. The tenant prune deletes the keys first, in a statement of
-- their own.
CREATE OR REPLACE FUNCTION audit_model_keys_write() RETURNS TRIGGER AS $$
DECLARE
    a        RECORD;
    v_cons   UUID := COALESCE(NEW.consumer_id, OLD.consumer_id);
    v_tenant UUID;
    v_scope  VARCHAR(50);
BEGIN
    SELECT c.tenant_id, c.tenant_scope INTO v_tenant, v_scope
      FROM context_model_consumers c WHERE c.id = v_cons;
    SELECT * INTO a FROM audit_actor_meta();
    INSERT INTO context_settings_audit
        (entity_type, entity_key, scope, action, old_value, new_value,
         api_key_id, actor_label, metadata)
    VALUES (
        'model_key',
        COALESCE(NEW.id, OLD.id)::text,
        COALESCE(v_scope, '_global'),
        CASE TG_OP WHEN 'INSERT' THEN 'create'
                   WHEN 'DELETE' THEN 'prune'
                   ELSE CASE WHEN OLD.revoked_at IS NULL AND NEW.revoked_at IS NOT NULL THEN 'revoke'
                             ELSE 'update' END
        END,
        CASE WHEN TG_OP <> 'INSERT' THEN audit_state_model_key(OLD) END,
        CASE WHEN TG_OP <> 'DELETE' THEN audit_state_model_key(NEW) END,
        a.actor, a.actor_label,
        a.meta || jsonb_strip_nulls(jsonb_build_object(
            'tenant_id',   v_tenant,
            'consumer_id', v_cons)));
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_model_consumers_audit_ins_del ON context_model_consumers;
CREATE TRIGGER trg_model_consumers_audit_ins_del
    AFTER INSERT OR DELETE ON context_model_consumers
    FOR EACH ROW EXECUTE FUNCTION audit_model_consumers_write();

-- UPDATE only on the audited columns, and only when one of them changes:
-- updated_at and the SET NULL of an actor column never reach it.
DROP TRIGGER IF EXISTS trg_model_consumers_audit_upd ON context_model_consumers;
CREATE TRIGGER trg_model_consumers_audit_upd
    AFTER UPDATE OF tenant_id, label, description, kind, tenant_scope, all_models,
                    prompt_sensitivity, fair_weight, admission_lane, allowed_cidrs,
                    allow_plaintext, active, peer_instance_id, peer_bound_at
    ON context_model_consumers FOR EACH ROW
    WHEN ((OLD.tenant_id, OLD.label, OLD.description, OLD.kind, OLD.tenant_scope,
           OLD.all_models, OLD.prompt_sensitivity, OLD.fair_weight, OLD.admission_lane,
           OLD.allowed_cidrs, OLD.allow_plaintext, OLD.active, OLD.peer_instance_id,
           OLD.peer_bound_at)
          IS DISTINCT FROM
          (NEW.tenant_id, NEW.label, NEW.description, NEW.kind, NEW.tenant_scope,
           NEW.all_models, NEW.prompt_sensitivity, NEW.fair_weight, NEW.admission_lane,
           NEW.allowed_cidrs, NEW.allow_plaintext, NEW.active, NEW.peer_instance_id,
           NEW.peer_bound_at))
    EXECUTE FUNCTION audit_model_consumers_write();

DROP TRIGGER IF EXISTS trg_model_keys_audit_ins_del ON context_model_keys;
CREATE TRIGGER trg_model_keys_audit_ins_del
    AFTER INSERT OR DELETE ON context_model_keys
    FOR EACH ROW EXECUTE FUNCTION audit_model_keys_write();

-- The batched last_used_at flush, the SET NULL of an actor column and the
-- revoked_by_* stamp next to a revocation never reach it on their own. The
-- SET NULL of rotated_from when a predecessor is pruned does: the chain of
-- the surviving key changes, which is an update row.
DROP TRIGGER IF EXISTS trg_model_keys_audit_upd ON context_model_keys;
CREATE TRIGGER trg_model_keys_audit_upd
    AFTER UPDATE OF consumer_id, key_hint, expires_at, revoked_at, rotated_from
    ON context_model_keys FOR EACH ROW
    WHEN ((OLD.consumer_id, OLD.key_hint, OLD.expires_at, OLD.revoked_at, OLD.rotated_from)
          IS DISTINCT FROM
          (NEW.consumer_id, NEW.key_hint, NEW.expires_at, NEW.revoked_at, NEW.rotated_from))
    EXECUTE FUNCTION audit_model_keys_write();

-- 8. The one model-auth function. Gates in order; every refusal leaves the
--    gates block and returns the same sentinel row.
CREATE OR REPLACE FUNCTION ctx_model_auth(p_key_hash TEXT, p_client_ip INET, p_secure BOOLEAN)
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
    peer_instance_id   UUID
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
               c.allow_plaintext
          INTO v_tenant, v_scope, v_kind, v_all, v_sens, v_weight, v_lane, v_peer,
               v_cidrs, v_plain
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
                            v_weight, v_exp, true, v_lane, v_peer;
        RETURN;
    END gates;

    -- The sentinel: one row, the same whichever gate refused (no oracle).
    RETURN QUERY SELECT NULL::uuid, NULL::uuid, NULL::uuid, NULL::varchar, ''::text, false,
                        'credentials'::text, 1::smallint, NULL::timestamptz, false,
                        'bulk'::text, NULL::uuid;
END;
$$;
