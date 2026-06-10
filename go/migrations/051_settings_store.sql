-- =============================================================================
-- 051_settings_store.sql — runtime settings overrides, audit trail, sealed
-- provider secrets (F2: settings persistence)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- context_settings: DB override layer on top of env/defaults. Precedence at
-- runtime: request body (only keys flagged request_overridable, never
-- sensitive/structural) > context_settings > env > code default. One row per
-- (key, scope); deleting the row reverts to env/default. scope is provisioned
-- now ('_global' sentinel; the underscore prefix is SYSTEM-RESERVED and
-- enforced in Go at api-key-create since 052) so the unique constraint never
-- needs a swap-migration once per-tenant settings arrive (target scale:
-- multi-tenant). F2 code reads scope='_global' exclusively.
--
-- context_settings_audit: append-only history, written by AFTER-ROW TRIGGERS
-- on context_settings AND context_secrets — covers API writes, psql direct
-- edits and break-glass factory reset alike, atomically with the mutation
-- (no Go-side audit INSERT, no crash window between write and audit).
-- entity_type/action carry NO CHECK constraints: values are derived from
-- TG_TABLE_NAME/TG_OP inside the trigger (closed set by construction), and
-- hard value lists in the schema are the migration class M045 abolished
-- (v2.0.0 line: validation is a runtime concern).
-- api_key_id has deliberately NO FK: audit rows reference, they never
-- cascade — a key delete must not anonymize history (ON DELETE SET NULL
-- would). actor_label snapshots the key label at write time for the same
-- reason. Settings values in old/new are never sensitive thanks to the
-- secret_ref reject in the settings API (422, F2-W5); secret rows carry
-- NULL values by construction (see audit_settings_write below).
--
-- context_secrets: AES-256-GCM sealed provider credentials, encrypted in Go
-- (crypto/aes stdlib), master key from env CTX_SECRETS_KEY — NOT pgcrypto:
-- pgp_sym_encrypt would ship the master key through the SQL wire protocol
-- into pg_stat_statements/log_statement paths. AAD binds name+scope, so a
-- ciphertext copied onto another row fails authentication.
-- =============================================================================

CREATE TABLE IF NOT EXISTS context_settings (
    id          UUID PRIMARY KEY DEFAULT uuidv7(),
    key         TEXT NOT NULL,
    -- TODO(multi-tenant): per-tenant settings resolve on this column
    -- (tenant scope overrides on top of '_global'); F2 reads '_global' only.
    scope       VARCHAR(50) NOT NULL DEFAULT '_global',
    value       JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by  UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,
    metadata    JSONB NOT NULL DEFAULT '{}',
    CONSTRAINT uq_settings_key_scope UNIQUE (key, scope)
);

CREATE TABLE IF NOT EXISTS context_settings_audit (
    id          UUID PRIMARY KEY DEFAULT uuidv7(),
    entity_type TEXT NOT NULL,                -- 'setting' | 'secret' (trigger: TG_TABLE_NAME; no CHECK, v2.0.0 line)
    entity_key  TEXT NOT NULL,                -- settings key resp. secret name
    scope       VARCHAR(50) NOT NULL DEFAULT '_global',
    action      TEXT NOT NULL,                -- set|unset|create|rotate|delete (trigger: TG_OP; no CHECK)
    old_value   JSONB,                        -- ALWAYS NULL for entity_type='secret'
    new_value   JSONB,                        -- ALWAYS NULL for entity_type='secret'
    api_key_id  UUID,                         -- deliberately NO FK (must never cascade, see header)
    actor_label TEXT,                         -- label snapshot at write time
    metadata    JSONB NOT NULL DEFAULT '{}',  -- via: api|sql, request_id (when set)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_settings_audit_key
    ON context_settings_audit (entity_key, created_at DESC);

CREATE TABLE IF NOT EXISTS context_secrets (
    id           UUID PRIMARY KEY DEFAULT uuidv7(),
    name         TEXT NOT NULL,               -- format validated in Go (no CHECK, v2.0.0 line)
    -- TODO(multi-tenant): per-tenant secrets resolve on this column as well.
    scope        VARCHAR(50) NOT NULL DEFAULT '_global',
    ciphertext   BYTEA NOT NULL,              -- GCM output incl. auth tag
    nonce        BYTEA NOT NULL,              -- 12 bytes, fresh per encryption
    key_version  INT NOT NULL DEFAULT 1,      -- master-key generation (diagnostics/rotation sweep)
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by   UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,
    rotated_at   TIMESTAMPTZ,
    rotated_by   UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,
    metadata     JSONB NOT NULL DEFAULT '{}',
    CONSTRAINT uq_secrets_name_scope UNIQUE (name, scope)
);

-- Hot-reload channel: covers API writes (redundant, idempotent) AND
-- break-glass/psql SQL edits — on BOTH tables, otherwise a secret rotation
-- would not propagate into the running snapshot (a compromised key would
-- keep working). Modeled on notify_block_write/ctx_block_write (M004 line) —
-- deliberately broader here: also DELETE, via COALESCE(NEW, OLD), for the
-- unset/revocation path. Payload carries ONLY identity+op, NEVER values or
-- ciphertext.
CREATE OR REPLACE FUNCTION notify_settings_write() RETURNS TRIGGER AS $$
DECLARE
    v_row JSONB := to_jsonb(COALESCE(NEW, OLD));
BEGIN
    PERFORM pg_notify('ctx_settings_write', json_build_object(
        'entity', TG_TABLE_NAME,
        'key',    COALESCE(v_row->>'key', v_row->>'name'),
        'op',     TG_OP)::text);
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

-- Audit in the trigger instead of the Go store layer: atomic with the
-- mutation and covers all write paths (API, psql, break-glass.sh
-- reset-settings). Actor comes from SET LOCAL ctx.api_key_id (API path);
-- psql edits => NULL + via='sql'. to_jsonb(NEW/OLD) holds ciphertext only
-- in a LOCAL variable for context_secrets — old/new reach the audit row
-- exclusively for settings (whose values are never sensitive, see the
-- secret_ref reject noted in the header).
CREATE OR REPLACE FUNCTION audit_settings_write() RETURNS TRIGGER AS $$
DECLARE
    v_new   JSONB := to_jsonb(NEW);
    v_old   JSONB := to_jsonb(OLD);
    v_actor UUID  := NULLIF(current_setting('ctx.api_key_id', true), '')::uuid;
    v_label TEXT;
BEGIN
    IF v_actor IS NOT NULL THEN
        SELECT label INTO v_label FROM context_api_keys WHERE id = v_actor;
    END IF;
    INSERT INTO context_settings_audit
        (entity_type, entity_key, scope, action, old_value, new_value,
         api_key_id, actor_label, metadata)
    VALUES (
        CASE TG_TABLE_NAME WHEN 'context_settings' THEN 'setting' ELSE 'secret' END,
        COALESCE(v_new->>'key', v_new->>'name', v_old->>'key', v_old->>'name'),
        COALESCE(v_new->>'scope', v_old->>'scope'),
        CASE WHEN TG_TABLE_NAME = 'context_settings'
             THEN CASE TG_OP WHEN 'DELETE' THEN 'unset' ELSE 'set' END
             ELSE CASE TG_OP WHEN 'INSERT' THEN 'create'
                             WHEN 'UPDATE' THEN 'rotate'
                             ELSE 'delete' END
        END,
        CASE WHEN TG_TABLE_NAME = 'context_settings' THEN v_old->'value' END,
        CASE WHEN TG_TABLE_NAME = 'context_settings' THEN v_new->'value' END,
        v_actor, v_label,
        jsonb_strip_nulls(jsonb_build_object(
            'via',        CASE WHEN v_actor IS NULL THEN 'sql' ELSE 'api' END,
            'request_id', NULLIF(current_setting('ctx.request_id', true), '')))
    );
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_settings_notify ON context_settings;
CREATE TRIGGER trg_settings_notify
    AFTER INSERT OR UPDATE OR DELETE ON context_settings
    FOR EACH ROW EXECUTE FUNCTION notify_settings_write();
DROP TRIGGER IF EXISTS trg_settings_audit ON context_settings;
CREATE TRIGGER trg_settings_audit
    AFTER INSERT OR UPDATE OR DELETE ON context_settings
    FOR EACH ROW EXECUTE FUNCTION audit_settings_write();

DROP TRIGGER IF EXISTS trg_secrets_notify ON context_secrets;
CREATE TRIGGER trg_secrets_notify
    AFTER INSERT OR UPDATE OR DELETE ON context_secrets
    FOR EACH ROW EXECUTE FUNCTION notify_settings_write();
DROP TRIGGER IF EXISTS trg_secrets_audit ON context_secrets;
CREATE TRIGGER trg_secrets_audit
    AFTER INSERT OR UPDATE OR DELETE ON context_secrets
    FOR EACH ROW EXECUTE FUNCTION audit_settings_write();

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (51, '051_settings_store.sql', now())
  ON CONFLICT (version) DO NOTHING;
