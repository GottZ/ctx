-- =============================================================================
-- 162_model_aliases.sql — the public model namespace of the OpenAI-compatible
-- API (context_model_aliases), its identity pin, its hot-reload signal and
-- the whitelist audit of the table
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- What. A model alias is the "model" id an external consumer names on /v1: a
-- name (the OpenAI model id), a kind (chat | embedding | rerank), the pool
-- role it routes to (a role, never a backend row: trust, profiles, cooldown
-- and tenant visibility of the pool apply unchanged), the sensitivity floor
-- of its chain (default credentials, the strictest), a policy object of
-- request caps (keys and code ceilings in internal/modelalias, fail-closed
-- parse: an unknown key or a wrong type hides the alias), a description and
-- enabled. scope is '_global' (the operator catalog) or a tenant scope; a
-- tenant alias shadows the _global alias of the same name for that tenant's
-- consumers. Nothing seeds the table: an empty catalog offers no model
-- (fail-closed; which models leave ctx is an admin decision). Nothing in ctx
-- writes it yet — the admin actions and the /v1 door ship later — so this
-- migration has no effect outside the database; ctxd only loads the empty
-- registry.
--
-- Identity. id, name, kind and scope never change after the insert
-- (SQLSTATE 23514, constraint model_aliases_identity, column = the changed
-- one; psql included, an upsert included): the id is what grants bind (they
-- arrive with their own migration, ON DELETE CASCADE) and what the audit
-- history is keyed on; a consumer configuration that names the alias stays
-- readable; a kind cannot turn a chat alias into an embedding endpoint under
-- the same name; an alias moved to another scope would carry the grants of
-- one tenant into another. Renaming is delete + create, so a new alias of an
-- old name inherits no release.
--
-- Hot reload. Every insert, update and delete fires ctx_settings_write
-- through the unchanged notify_settings_write() — payload {entity:
-- 'context_model_aliases', key: name, scope, op}, the shape of
-- context_backends. The listener routes the entity to the model-alias
-- registry and to nothing else (an unknown entity would fall through to the
-- full settings reload).
--
-- Audit. Every change is written to context_settings_audit (entity_type
-- model_alias, entity_key = id, scope = the row's scope, actions create /
-- update / delete) through the whitelist projection audit_state_model_alias
-- — never to_jsonb(row), which would forward every column a later migration
-- adds — with the actor context of audit_actor_meta() (160). metadata adds
-- tenant_id (the tenant owning the scope, from context_tenant_scopes; a
-- _global alias, and an alias of a scope no tenant owns, has none, so only
-- server-admins read its history) and confirm_sensitivity_downgrade: true
-- when the writing transaction set ctx.confirm_downgrade (the admin action
-- that lowers a sensitivity sets it; a malformed value fails the write).
-- Never recorded: id (it is entity_key, and immutable) and the created_at /
-- updated_at bookkeeping; the UPDATE trigger carries the column list of the
-- whitelist, so an update of nothing audited writes no row. A TRUNCATE fires
-- no row trigger — neither audit nor NOTIFY; remove rows with DELETE.
-- internal/store/audit_classification_test.go pins every column.
--
-- Cost and locks. One new, empty table; no existing table is touched, so the
-- migration takes no lock another ctx process could wait for (milliseconds).
-- lock_timeout 3s fails the boot instead of queueing; the next start retries.
--
-- Idempotent: CREATE TABLE / INDEX IF NOT EXISTS, CREATE OR REPLACE FUNCTION,
-- DROP TRIGGER IF EXISTS + CREATE. No foreign key in or out of the table.
--
-- Way back, in this order: DROP FUNCTION
-- audit_state_model_alias(context_model_aliases) (it takes the table's row
-- type); DROP TABLE context_model_aliases (its triggers go with it); DROP
-- FUNCTION audit_model_aliases_write(), model_aliases_identity_immutable();
-- DELETE FROM _migrations WHERE version = 162 (so a later upgrade applies it
-- again). notify_settings_write() and audit_actor_meta() stay: they belong to
-- earlier migrations. The audit rows stay (append-only history).
-- TestMigration162Idempotent runs exactly these statements.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- 1. The namespace.
CREATE TABLE IF NOT EXISTS context_model_aliases (
    id          UUID        PRIMARY KEY DEFAULT uuidv7(),
    -- '_global' or a tenant scope; empty and other reserved ('_'-prefixed)
    -- scopes are refused — the pool reads "" as _global, which would make a
    -- second operator catalog no shadowing rule sees.
    scope       VARCHAR(50) NOT NULL DEFAULT '_global'
                CONSTRAINT ck_model_aliases_scope
                CHECK (scope = '_global' OR (scope <> '' AND scope NOT LIKE '\_%')),
    -- The OpenAI "model" id. Immutable (section 2).
    name        TEXT        NOT NULL
                CONSTRAINT ck_model_aliases_name
                CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$'),
    -- The endpoint family. Immutable (section 2).
    kind        TEXT        NOT NULL
                CONSTRAINT ck_model_aliases_kind
                CHECK (kind IN ('chat', 'embedding', 'rerank')),
    -- A pool role (context_backends.roles); no foreign key, roles are an array.
    role        TEXT        NOT NULL
                CONSTRAINT ck_model_aliases_role
                CHECK (role ~ '^[a-z0-9][a-z0-9:_-]{0,63}$'),
    -- Floor of the chain's sensitivity; raised, never lowered, per request.
    sensitivity TEXT        NOT NULL DEFAULT 'credentials'
                CONSTRAINT ck_model_aliases_sensitivity
                CHECK (sensitivity IN ('public', 'internal', 'personal', 'credentials')),
    -- Request caps; the keys are validated where they are read.
    policy      JSONB       NOT NULL DEFAULT '{}'::jsonb
                CONSTRAINT ck_model_aliases_policy
                CHECK (jsonb_typeof(policy) = 'object'),
    description TEXT        NOT NULL DEFAULT ''
                CONSTRAINT ck_model_aliases_description
                CHECK (length(description) <= 500),
    enabled     BOOLEAN     NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_model_aliases_scope_name UNIQUE (scope, name)
);
CREATE INDEX IF NOT EXISTS idx_model_aliases_scope
    ON context_model_aliases (scope, enabled);

-- 2. Identity pin: id, name, kind and scope never change. A plain BEFORE
--    UPDATE (no column list): it also sees a value another BEFORE trigger set.
CREATE OR REPLACE FUNCTION model_aliases_identity_immutable() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id THEN
        RAISE EXCEPTION 'context_model_aliases: id is immutable'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'model_aliases_identity',
                  TABLE = 'context_model_aliases', COLUMN = 'id';
    END IF;
    IF NEW.name IS DISTINCT FROM OLD.name THEN
        RAISE EXCEPTION 'context_model_aliases: name is immutable'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'model_aliases_identity',
                  TABLE = 'context_model_aliases', COLUMN = 'name';
    END IF;
    IF NEW.kind IS DISTINCT FROM OLD.kind THEN
        RAISE EXCEPTION 'context_model_aliases: kind is immutable'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'model_aliases_identity',
                  TABLE = 'context_model_aliases', COLUMN = 'kind';
    END IF;
    IF NEW.scope IS DISTINCT FROM OLD.scope THEN
        RAISE EXCEPTION 'context_model_aliases: scope is immutable'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'model_aliases_identity',
                  TABLE = 'context_model_aliases', COLUMN = 'scope';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_model_aliases_identity ON context_model_aliases;
CREATE TRIGGER trg_model_aliases_identity
    BEFORE UPDATE ON context_model_aliases
    FOR EACH ROW EXECUTE FUNCTION model_aliases_identity_immutable();

-- 3. Hot reload: the channel and function of context_backends, unchanged.
DROP TRIGGER IF EXISTS trg_model_aliases_notify ON context_model_aliases;
CREATE TRIGGER trg_model_aliases_notify
    AFTER INSERT OR UPDATE OR DELETE ON context_model_aliases
    FOR EACH ROW EXECUTE FUNCTION notify_settings_write();

-- 4. Audit. Whitelist projection: the audited columns, nothing else. STABLE
--    like 161's projections (jsonb_build_object is STABLE).
CREATE OR REPLACE FUNCTION audit_state_model_alias(a context_model_aliases)
    RETURNS JSONB AS $$
    SELECT jsonb_build_object(
        'scope', a.scope, 'name', a.name, 'kind', a.kind, 'role', a.role,
        'sensitivity', a.sensitivity, 'policy', a.policy,
        'description', a.description, 'enabled', a.enabled)
$$ LANGUAGE sql STABLE;

CREATE OR REPLACE FUNCTION audit_model_aliases_write() RETURNS TRIGGER AS $$
DECLARE
    a       RECORD;
    v_scope VARCHAR(50) := CASE WHEN TG_OP = 'DELETE' THEN OLD.scope ELSE NEW.scope END;
BEGIN
    SELECT * INTO a FROM audit_actor_meta();
    INSERT INTO context_settings_audit
        (entity_type, entity_key, scope, action, old_value, new_value,
         api_key_id, actor_label, metadata)
    VALUES (
        'model_alias',
        COALESCE(NEW.id, OLD.id)::text,
        v_scope,
        CASE TG_OP WHEN 'INSERT' THEN 'create' WHEN 'DELETE' THEN 'delete' ELSE 'update' END,
        CASE WHEN TG_OP <> 'INSERT' THEN audit_state_model_alias(OLD) END,
        CASE WHEN TG_OP <> 'DELETE' THEN audit_state_model_alias(NEW) END,
        a.actor, a.actor_label,
        a.meta || jsonb_strip_nulls(jsonb_build_object(
            'tenant_id', (SELECT ts.tenant_id FROM context_tenant_scopes ts WHERE ts.scope = v_scope),
            'confirm_sensitivity_downgrade',
                CASE WHEN COALESCE(NULLIF(current_setting('ctx.confirm_downgrade', true), '')::boolean, false)
                     THEN true END)));
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_model_aliases_audit_ins_del ON context_model_aliases;
CREATE TRIGGER trg_model_aliases_audit_ins_del
    AFTER INSERT OR DELETE ON context_model_aliases
    FOR EACH ROW EXECUTE FUNCTION audit_model_aliases_write();
-- UPDATE only on the audited columns, and only when one of them changes:
-- the updated_at stamp of a write never reaches it on its own.
DROP TRIGGER IF EXISTS trg_model_aliases_audit_upd ON context_model_aliases;
CREATE TRIGGER trg_model_aliases_audit_upd
    AFTER UPDATE OF scope, name, kind, role, sensitivity, policy, description, enabled
    ON context_model_aliases FOR EACH ROW
    WHEN ((OLD.scope, OLD.name, OLD.kind, OLD.role, OLD.sensitivity, OLD.policy,
           OLD.description, OLD.enabled)
          IS DISTINCT FROM
          (NEW.scope, NEW.name, NEW.kind, NEW.role, NEW.sensitivity, NEW.policy,
           NEW.description, NEW.enabled))
    EXECUTE FUNCTION audit_model_aliases_write();
