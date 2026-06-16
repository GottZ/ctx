-- =============================================================================
-- 062_backend_pool_tenant.sql — context_backends scope dimension (MT, Achse 04)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Multi-Tenant wave T33a (Achse 04-W1, per-tenant backend pool). Builds on 053
-- (context_backends + audit trigger). Exact schema vorlage: design/04 §3.1.
--
-- context_backends gains a scope dimension: '_global' = shared server backend
-- (today's 5 rows), '<tenant>' = tenant-private (a friend-tenant's own key).
-- '_global' is consistent with the context_settings/secrets convention (051:42):
-- the '_' prefix is system-reserved (firstReservedScope, api_keys.go:44), so no
-- tenant home_scope can ever collide with it.
--
-- BEHAVIORALLY NEUTRAL: the 5 live backends become '_global' through the
-- DEFAULT (PG18 metadata-only ADD COLUMN, no per-row UPDATE), and Chain() does
-- not yet filter on scope (04-W2). The read path (loadBackendsSQL/scanBackend +
-- Backend.Scope) loads it additively; the write path stays scope-default until
-- per-tenant-admin (04-W5). Forward-compatible with the old binary: a SELECT
-- with a fixed column list ignores the new column, so 062 may run before deploy.
--
-- UNIQUE swap (Befund C, db-tenant-surface.md §8): uq_backends_name (name) →
-- uq_backends_scope_name (scope, name), so two tenants can each own a backend
-- 'openrouter' without colliding with the shared '_global' one. Collision-free
-- on the 5 live rows: all become '_global', and the old UNIQUE(name) and the new
-- UNIQUE(scope,name) are coincident on that set.
--
-- Idempotent: ADD COLUMN / DROP CONSTRAINT / CREATE INDEX are IF [NOT] EXISTS;
-- the unguarded ADD CONSTRAINT is safe because the runner skips an already-
-- applied version (per-version EXISTS check) and a failed file rolls back whole
-- (single Tx) — a re-run drops uq_backends_name then adds the new one once.
-- Out-of-order gap (063 quota is a later sub-wave; 064 settings already exists
-- from Achse 03) is tolerated by the runner. Forward-only, self-registering.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE context_backends
    ADD COLUMN IF NOT EXISTS scope VARCHAR(50) NOT NULL DEFAULT '_global';

-- Backfill is already done by the DEFAULT; explicit for clarity / re-run.
UPDATE context_backends SET scope = '_global' WHERE scope IS NULL;

-- UNIQUE swap: name → (scope, name).
ALTER TABLE context_backends DROP CONSTRAINT IF EXISTS uq_backends_name;
ALTER TABLE context_backends
    ADD CONSTRAINT uq_backends_scope_name UNIQUE (scope, name);

-- Per-tenant browse path (loadBackendsSQL WHERE scope = ANY($1) in 04-W2); scope
-- leads, enabled included because Chain filters enabled.
CREATE INDEX IF NOT EXISTS idx_backends_scope
    ON context_backends (scope, enabled);

-- Audit-trigger adjustment: audit_backends_write (053:71-116) hardcodes
-- scope='_global' in the audit row (053:99). Record the backend's own scope
-- instead — the exact pattern audit_settings_write uses (051:126) — so a
-- tenant-private backend mutation is tenant-attributed automatically. CREATE OR
-- REPLACE (no DROP, no signature change); the existing trigger picks up the new
-- body by name.
CREATE OR REPLACE FUNCTION audit_backends_write() RETURNS TRIGGER AS $$
DECLARE
    v_new   JSONB := to_jsonb(NEW);
    v_old   JSONB := to_jsonb(OLD);
    v_actor UUID  := NULLIF(current_setting('ctx.api_key_id', true), '')::uuid;
    v_label TEXT;
BEGIN
    IF v_new ? 'extra_headers' THEN
        SELECT jsonb_set(v_new, '{extra_headers}',
                         COALESCE(jsonb_object_agg(k, '"[redacted]"'::jsonb), '{}'::jsonb))
          INTO v_new
          FROM jsonb_object_keys(v_new->'extra_headers') AS k;
    END IF;
    IF v_old ? 'extra_headers' THEN
        SELECT jsonb_set(v_old, '{extra_headers}',
                         COALESCE(jsonb_object_agg(k, '"[redacted]"'::jsonb), '{}'::jsonb))
          INTO v_old
          FROM jsonb_object_keys(v_old->'extra_headers') AS k;
    END IF;
    IF v_actor IS NOT NULL THEN
        SELECT label INTO v_label FROM context_api_keys WHERE id = v_actor;
    END IF;
    INSERT INTO context_settings_audit
        (entity_type, entity_key, scope, action, old_value, new_value,
         api_key_id, actor_label, metadata)
    VALUES (
        'backend',
        COALESCE(v_new->>'name', v_old->>'name'),
        COALESCE(v_new->>'scope', v_old->>'scope'),
        CASE TG_OP WHEN 'INSERT' THEN 'create'
                   WHEN 'UPDATE' THEN 'update'
                   ELSE 'delete' END,
        v_old, v_new,
        v_actor, v_label,
        jsonb_strip_nulls(jsonb_build_object(
            'via',        CASE WHEN v_actor IS NULL THEN 'sql' ELSE 'api' END,
            'request_id', NULLIF(current_setting('ctx.request_id', true), '')))
    );
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (62, '062_backend_pool_tenant.sql', now())
  ON CONFLICT (version) DO NOTHING;
