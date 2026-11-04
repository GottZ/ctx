-- =============================================================================
-- 164_model_consumer_grants.sql — the single model releases of a model
-- consumer (context_model_consumer_grants: consumer -> model alias id), their
-- identity pin and the whitelist audit of the table
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- What. A grant releases ONE model alias (162) to ONE model consumer (161):
-- the consumer may name that alias on /v1. It binds the alias by its id,
-- never by its name: deleting an alias removes its grants (ON DELETE
-- CASCADE), and an alias created later under the same name — perhaps routed
-- to an external, paid backend — inherits no release (design/01 B28). An
-- alias's name, kind and scope never change (162), so the id is the alias the
-- grant was written for. all_models (161) stays the wildcard beside the
-- grants: true releases every alias the consumer can see; the grants count
-- while it is false. The admin actions write the table (models on
-- model-consumer-create / -update, validated against what the consumer's
-- tenant_scope is offered at that moment) and model-alias-delete refuses
-- while an active consumer holds a grant on the alias (409 model_granted); a
-- psql DELETE of the alias takes the grants with it. The request time checks
-- visibility again, so a grant that a later change of the consumer's scope
-- hides stays in the table and releases nothing. Nothing on /v1 reads the
-- table yet (the door ships later); until then this migration has no effect
-- outside the database. No backfill: releases existed only as all_models,
-- which stays as it is.
--
-- Identity. consumer_id and alias_id never change after the insert (SQLSTATE
-- 23514, constraint model_grants_identity, column = the changed one; psql
-- included): a grant moved to another alias or another consumer would be a
-- release no audit row records. A grant is inserted or deleted, never edited,
-- so the table has no audited UPDATE. created_at and the actor columns stay
-- writable — the ON DELETE SET NULL of created_by_key is an UPDATE.
--
-- Audit. Every insert and delete is written to context_settings_audit
-- (entity_type model_grant, entity_key consumer_id:alias_id, actions grant /
-- ungrant, scope = the consumer's tenant_scope, '_global' when it has none)
-- through the whitelist projection audit_state_model_grant (consumer_id,
-- alias_id) — never to_jsonb(row), which would forward every column a later
-- migration adds — with the actor context of audit_actor_meta() (160).
-- metadata adds tenant_id (through the consumer: the filter of the
-- tenant-admin read surface), consumer_id and alias_id. A grant that leaves
-- with its alias (CASCADE) is an ungrant row attributed to the alias's
-- deleter. A grant that left with its consumer would find the consumer row
-- gone and lack tenant_id and the scope; the tenant prune (the one path that
-- deletes consumers) therefore deletes the grants first, in a statement of
-- its own, as it does the keys. Never recorded: created_at and the actor
-- columns (the actor travels in api_key_id, actor_label and
-- metadata.actor_*). A TRUNCATE fires no row trigger; remove rows with
-- DELETE. internal/store/audit_classification_test.go pins every column.
--
-- Indexes. The primary key (consumer_id, alias_id) serves a consumer's grant
-- list and the one probe per /v1 request; idx_model_grants_alias serves the
-- CASCADE of an alias delete and the 409 count. The actor columns get partial
-- indexes (163's pattern): the ON DELETE SET NULL of a deleted knowledge key
-- or principal looks their rows up.
--
-- Cost and locks. One new, empty table. Its foreign keys take SHARE ROW
-- EXCLUSIVE on context_model_consumers, context_model_aliases,
-- context_api_keys and context_principals until the commit — milliseconds,
-- during which writes to those tables wait (knowledge-key authentication of
-- another ctx process on the same database among them); reads do not.
-- lock_timeout 3s fails the boot instead of queueing; the next start retries.
--
-- Idempotent: CREATE TABLE / INDEX IF NOT EXISTS, CREATE OR REPLACE FUNCTION,
-- DROP TRIGGER IF EXISTS + CREATE.
--
-- Way back, in this order: DROP FUNCTION
-- audit_state_model_grant(context_model_consumer_grants) (it takes the
-- table's row type); DROP TABLE context_model_consumer_grants (no foreign key
-- points at it; its triggers and indexes go with it); DROP FUNCTION
-- audit_model_grants_write(), model_grants_identity_immutable(); DELETE FROM
-- _migrations WHERE version = 164 (so a later upgrade applies it again). An
-- older binary ignores the table. audit_actor_meta() stays (160); the audit
-- rows stay (append-only history). TestMigration164Idempotent runs exactly
-- these statements.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- 1. The grants.
CREATE TABLE IF NOT EXISTS context_model_consumer_grants (
    -- CASCADE: dependent data of the consumer. Only the tenant prune deletes a
    -- consumer (161); RESTRICT would make it know every table of this kind.
    consumer_id          UUID NOT NULL
                         CONSTRAINT fk_model_grants_consumer
                         REFERENCES context_model_consumers (id) ON DELETE CASCADE,
    -- CASCADE: a deleted alias takes its grants; an alias of the same name
    -- created later is not granted (B28). Immutable (section 2).
    alias_id             UUID NOT NULL
                         CONSTRAINT fk_model_grants_alias
                         REFERENCES context_model_aliases (id) ON DELETE CASCADE,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by_key       UUID REFERENCES context_api_keys(id)   ON DELETE SET NULL,
    created_by_principal UUID REFERENCES context_principals(id) ON DELETE SET NULL,
    PRIMARY KEY (consumer_id, alias_id)
);
CREATE INDEX IF NOT EXISTS idx_model_grants_alias
    ON context_model_consumer_grants (alias_id);
CREATE INDEX IF NOT EXISTS idx_model_grants_created_by_key
    ON context_model_consumer_grants (created_by_key) WHERE created_by_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_model_grants_created_by_principal
    ON context_model_consumer_grants (created_by_principal) WHERE created_by_principal IS NOT NULL;

-- 2. Identity pin: consumer_id and alias_id never change. A plain BEFORE
--    UPDATE (no column list), comparing values: the SET NULL of an actor
--    column passes it.
CREATE OR REPLACE FUNCTION model_grants_identity_immutable() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.consumer_id IS DISTINCT FROM OLD.consumer_id THEN
        RAISE EXCEPTION 'context_model_consumer_grants: consumer_id is immutable'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'model_grants_identity',
                  TABLE = 'context_model_consumer_grants', COLUMN = 'consumer_id';
    END IF;
    IF NEW.alias_id IS DISTINCT FROM OLD.alias_id THEN
        RAISE EXCEPTION 'context_model_consumer_grants: alias_id is immutable'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'model_grants_identity',
                  TABLE = 'context_model_consumer_grants', COLUMN = 'alias_id';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_model_grants_identity ON context_model_consumer_grants;
CREATE TRIGGER trg_model_grants_identity
    BEFORE UPDATE ON context_model_consumer_grants
    FOR EACH ROW EXECUTE FUNCTION model_grants_identity_immutable();

-- 3. Audit. Whitelist projection: the audited columns, nothing else. STABLE
--    like 161's projections.
CREATE OR REPLACE FUNCTION audit_state_model_grant(g context_model_consumer_grants)
    RETURNS JSONB AS $$
    SELECT jsonb_build_object('consumer_id', g.consumer_id, 'alias_id', g.alias_id)
$$ LANGUAGE sql STABLE;

-- tenant_id and scope come from the consumer (as for a model key, 161).
CREATE OR REPLACE FUNCTION audit_model_grants_write() RETURNS TRIGGER AS $$
DECLARE
    a        RECORD;
    v_cons   UUID := COALESCE(NEW.consumer_id, OLD.consumer_id);
    v_alias  UUID := COALESCE(NEW.alias_id, OLD.alias_id);
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
        'model_grant',
        v_cons::text || ':' || v_alias::text,
        COALESCE(v_scope, '_global'),
        CASE TG_OP WHEN 'INSERT' THEN 'grant' ELSE 'ungrant' END,
        CASE WHEN TG_OP = 'DELETE' THEN audit_state_model_grant(OLD) END,
        CASE WHEN TG_OP = 'INSERT' THEN audit_state_model_grant(NEW) END,
        a.actor, a.actor_label,
        a.meta || jsonb_strip_nulls(jsonb_build_object(
            'tenant_id',   v_tenant,
            'consumer_id', v_cons,
            'alias_id',    v_alias)));
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- One audit trigger: INSERT and DELETE. No UPDATE trigger — section 2 keeps
-- both audited columns.
DROP TRIGGER IF EXISTS trg_model_grants_audit ON context_model_consumer_grants;
CREATE TRIGGER trg_model_grants_audit
    AFTER INSERT OR DELETE ON context_model_consumer_grants
    FOR EACH ROW EXECUTE FUNCTION audit_model_grants_write();
