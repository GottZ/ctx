-- =============================================================================
-- 163_instance_identity.sql — the identity of this ctx instance
-- (context_instance: instance_id + instance_secret) with its whitelist audit,
-- and the indexes on the foreign keys of migration 161
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- What. One row per database: instance_id, a UUIDv4 (gen_random_uuid(); not
-- uuidv7, whose timestamp would tell every peer the age of the instance),
-- and instance_secret, 32 random bytes (gen_random_bytes from pgcrypto, which
-- the baseline creates) — the HMAC key of the hop tokens with which ctx
-- instances recognise their own requests coming back (internal/fedhop). The
-- first apply draws both; every later apply leaves the row as it is. ctxd
-- reads the row at boot (store.LoadInstanceIdentity) and refuses to start
-- without it, or with a secret of another length, logging "federation:
-- instance identity missing — run migrations": without an identity there is
-- no loop protection, and a ctxd without loop protection must neither serve
-- /v1 nor call a peer. Nothing else reads the table yet.
--
-- Per database, not per process. Every ctxd on this database is the same
-- instance (same corpus, same consumers): two processes recognise each
-- other's tokens, so a path A1 -> B -> A2 is a loop. A database restored
-- from another instance's backup carries that instance's identity; the two
-- then take each other's requests for loops and refuse them (fail-closed).
-- The remedy is a rotation (instance-rotate, a later wave; by hand: the
-- UPDATE below, then a restart). No NOTIFY: the identity holds for the life
-- of a process — a rotation takes effect at the next start.
--
-- The secret never leaves this table: no log line, no status field, no
-- audit row — and no error message. PostgreSQL's own NOT NULL and CHECK
-- errors carry the whole failing row in DETAIL, to the client and into the
-- server log; a write that leaves the secret as it is and fails (a NULL id,
-- a false singleton) would carry the live secret there. The row guard
-- (section 2) raises the same SQLSTATEs first, naming the constraint or the
-- column but never a value; the constraints stay as the second door. It
-- also refuses TRUNCATE, which would remove the identity without an audit
-- row (a statement truncation fires no row trigger): DELETE the row.
--
-- Audit. Every change is written to context_settings_audit (entity_type
-- instance, entity_key 'self' — the one row of the table —, scope _global;
-- actions create / rotate (instance_id changed) / update / delete) through
-- the whitelist projection audit_state_instance (instance_id, created_at,
-- rotated_at) with the actor context of audit_actor_meta() (160). Never
-- recorded: instance_secret and the singleton key. metadata carries no
-- tenant_id: the instance belongs to no tenant, and the audit read surface
-- serves entity=instance to server-admins only. INSERT and DELETE are
-- audited because both are identity events: a deleted row is the boot-abort
-- case, and a row inserted by hand (INSERT INTO context_instance DEFAULT
-- VALUES) is a new identity every peer bound to the old one will refuse. The
-- UPDATE trigger carries the column list of the whitelist and compares it:
-- an update of the secret alone writes no row (the secret changes together
-- with the id in a rotation, which writes the rotate row). The creating
-- INSERT below runs before the triggers exist and writes no row — the birth
-- is the row's created_at, and the first change carries it in old_value.
-- internal/store/audit_classification_test.go pins every column.
--
-- Rotation by hand (until instance-rotate ships), then restart ctxd:
--   UPDATE context_instance SET instance_id = gen_random_uuid(),
--          instance_secret = gen_random_bytes(32), rotated_at = now();
--
-- Section 4 indexes the foreign keys of 161 (W01-3 review 2 F-2): the tenant
-- prune deletes model keys, model consumers and knowledge keys, and every
-- deleted row makes the ON DELETE SET NULL / RESTRICT triggers of the
-- referencing columns look up their rows — without an index a sequential
-- scan of the whole table per deleted row (the review measured 2 046 ms for
-- the 7b keys delete at 128 000 keys, 2 019 ms of it in the rotated_from
-- trigger; 10.5 ms with the indexes).
--
-- Cost and locks. One new table of one row; the seven indexes are built on
-- context_model_keys and context_model_consumers, which 161 created and no
-- release served before this one (empty on an upgrade from 160, so the
-- SHARE lock of CREATE INDEX lasts milliseconds). lock_timeout 3s fails the
-- boot instead of queueing; the next start retries.
--
-- Idempotent: CREATE TABLE / INDEX IF NOT EXISTS, the creating INSERT ON
-- CONFLICT DO NOTHING (a skipped insert fires no AFTER row trigger, so no
-- audit row; the row guard passes the default row), CREATE OR
-- REPLACE FUNCTION, DROP TRIGGER IF EXISTS + CREATE.
--
-- Way back, in this order: DROP FUNCTION
-- audit_state_instance(context_instance) (it takes the table's row type);
-- DROP TABLE context_instance (its triggers go with it); DROP FUNCTION
-- audit_instance_write(), instance_row_guard(); DROP INDEX of the seven
-- indexes of section 4;
-- DELETE FROM _migrations WHERE version = 163 (so a later upgrade applies it
-- again — and draws a NEW identity, which every peer bound to the old one
-- refuses). An older binary simply ignores the table: rolling back the
-- binary needs none of this. audit_actor_meta() stays (160); the audit rows
-- stay (append-only history). TestMigration163Idempotent runs exactly these
-- statements.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- 1. The identity. One row: the key is a boolean that can only be true.
CREATE TABLE IF NOT EXISTS context_instance (
    singleton       BOOLEAN     PRIMARY KEY DEFAULT true
                    CONSTRAINT ck_instance_singleton CHECK (singleton),
    -- UUIDv4: no timestamp in the id.
    instance_id     UUID        NOT NULL DEFAULT gen_random_uuid(),
    -- HMAC key of the hop tokens; never leaves this table.
    instance_secret BYTEA       NOT NULL DEFAULT gen_random_bytes(32)
                    CONSTRAINT ck_instance_secret_length CHECK (octet_length(instance_secret) = 32),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Set by a rotation.
    rotated_at      TIMESTAMPTZ
);

-- Drawn on the first apply, untouched by every later one.
INSERT INTO context_instance (singleton) VALUES (true)
    ON CONFLICT (singleton) DO NOTHING;

-- 2. Row guard: a failing write reports its constraint or column, never the
--    row (header); TRUNCATE is refused. Statement and row triggers share the
--    function; a statement trigger has no NEW, so it returns first.
CREATE OR REPLACE FUNCTION instance_row_guard() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'context_instance: TRUNCATE is refused — DELETE the row, which is audited'
            USING ERRCODE = 'restrict_violation', TABLE = 'context_instance';
    END IF;
    IF NEW.singleton IS NULL THEN
        RAISE EXCEPTION 'context_instance: singleton must not be null'
            USING ERRCODE = 'not_null_violation', TABLE = 'context_instance', COLUMN = 'singleton';
    END IF;
    IF NOT NEW.singleton THEN
        RAISE EXCEPTION 'context_instance: singleton must be true'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'ck_instance_singleton',
                  TABLE = 'context_instance', COLUMN = 'singleton';
    END IF;
    IF NEW.instance_id IS NULL THEN
        RAISE EXCEPTION 'context_instance: instance_id must not be null'
            USING ERRCODE = 'not_null_violation', TABLE = 'context_instance', COLUMN = 'instance_id';
    END IF;
    IF NEW.instance_secret IS NULL THEN
        RAISE EXCEPTION 'context_instance: instance_secret must not be null'
            USING ERRCODE = 'not_null_violation', TABLE = 'context_instance', COLUMN = 'instance_secret';
    END IF;
    IF octet_length(NEW.instance_secret) <> 32 THEN
        RAISE EXCEPTION 'context_instance: instance_secret must be 32 bytes'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'ck_instance_secret_length',
                  TABLE = 'context_instance', COLUMN = 'instance_secret';
    END IF;
    IF NEW.created_at IS NULL THEN
        RAISE EXCEPTION 'context_instance: created_at must not be null'
            USING ERRCODE = 'not_null_violation', TABLE = 'context_instance', COLUMN = 'created_at';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_instance_guard ON context_instance;
CREATE TRIGGER trg_instance_guard
    BEFORE INSERT OR UPDATE ON context_instance
    FOR EACH ROW EXECUTE FUNCTION instance_row_guard();
DROP TRIGGER IF EXISTS trg_instance_no_truncate ON context_instance;
CREATE TRIGGER trg_instance_no_truncate
    BEFORE TRUNCATE ON context_instance
    FOR EACH STATEMENT EXECUTE FUNCTION instance_row_guard();

-- 3. Audit. Whitelist projection: the audited columns, nothing else — never
--    the secret. STABLE like the projections of 161/162.
CREATE OR REPLACE FUNCTION audit_state_instance(i context_instance)
    RETURNS JSONB AS $$
    SELECT jsonb_build_object(
        'instance_id', i.instance_id, 'created_at', i.created_at, 'rotated_at', i.rotated_at)
$$ LANGUAGE sql STABLE;

CREATE OR REPLACE FUNCTION audit_instance_write() RETURNS TRIGGER AS $$
DECLARE
    a RECORD;
BEGIN
    SELECT * INTO a FROM audit_actor_meta();
    INSERT INTO context_settings_audit
        (entity_type, entity_key, scope, action, old_value, new_value,
         api_key_id, actor_label, metadata)
    VALUES (
        'instance', 'self', '_global',
        CASE TG_OP WHEN 'INSERT' THEN 'create'
                   WHEN 'DELETE' THEN 'delete'
                   ELSE CASE WHEN NEW.instance_id IS DISTINCT FROM OLD.instance_id
                             THEN 'rotate' ELSE 'update' END
        END,
        CASE WHEN TG_OP <> 'INSERT' THEN audit_state_instance(OLD) END,
        CASE WHEN TG_OP <> 'DELETE' THEN audit_state_instance(NEW) END,
        a.actor, a.actor_label, a.meta);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_instance_audit_ins_del ON context_instance;
CREATE TRIGGER trg_instance_audit_ins_del
    AFTER INSERT OR DELETE ON context_instance
    FOR EACH ROW EXECUTE FUNCTION audit_instance_write();
-- UPDATE only on the audited columns, and only when one of them changes: a
-- write of the secret alone never reaches it.
DROP TRIGGER IF EXISTS trg_instance_audit_upd ON context_instance;
CREATE TRIGGER trg_instance_audit_upd
    AFTER UPDATE OF instance_id, created_at, rotated_at
    ON context_instance FOR EACH ROW
    WHEN ((OLD.instance_id, OLD.created_at, OLD.rotated_at)
          IS DISTINCT FROM
          (NEW.instance_id, NEW.created_at, NEW.rotated_at))
    EXECUTE FUNCTION audit_instance_write();

-- 4. Indexes on the foreign keys of 161 — W01-3 review 2 F-2. One partial
--    index per referencing column that 161 declares as a foreign key without
--    an index (consumer_id and tenant_id have theirs); NULL rows are never
--    looked up, so they stay out of the index. A lookup "col = $1" proves
--    "col IS NOT NULL", so the triggers' plans use them.
CREATE INDEX IF NOT EXISTS idx_model_keys_rotated_from
    ON context_model_keys (rotated_from) WHERE rotated_from IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_model_keys_created_by_key
    ON context_model_keys (created_by_key) WHERE created_by_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_model_keys_revoked_by_key
    ON context_model_keys (revoked_by_key) WHERE revoked_by_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_model_keys_created_by_principal
    ON context_model_keys (created_by_principal) WHERE created_by_principal IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_model_keys_revoked_by_principal
    ON context_model_keys (revoked_by_principal) WHERE revoked_by_principal IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_model_consumers_created_by_key
    ON context_model_consumers (created_by_key) WHERE created_by_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_model_consumers_created_by_principal
    ON context_model_consumers (created_by_principal) WHERE created_by_principal IS NOT NULL;
