-- =============================================================================
-- 064_settings_tenant_index.sql — scope-leading read indexes (MT, Achse 03)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Multi-Tenant wave T27 (Achse 03-W1, settings/secrets tenant resolution).
-- Builds on 051 (context_settings / context_secrets, both with a scope column).
-- Exact schema vorlage: design/03-settings-secrets-mt.md §3.2.
--
-- The per-tenant resolution reads WHERE scope = ANY({tenant, '_global'}) and
-- orders by key/name. The existing uq_settings_key_scope (key, scope) and
-- uq_secrets_name_scope (name, scope) are key/name-LEADING, so they don't serve
-- a scope-first filter. A scope-LEADING index serves that access pattern
-- directly and keeps the per-tenant read at one tenant's row count, not a full
-- table scan as the corpus grows to N tenants (target scale: 1M blocks x N
-- tenants; settings rows stay few PER tenant but the table is shared).
--
-- ADDITIVE, no consumer yet: store.LoadSettingOverridesMulti (this wave) is the
-- only reader and has no call site until the resolution waves (03-W3+). Pure
-- read-path optimization — no structural change (scope is already VARCHAR(50),
-- already in the UNIQUE keys, already mirrored by the audit trigger).
-- Idempotent, forward-only, self-registering (M031+ convention, 057:77-78).
--
-- Tx note (058/059/061 convention): lock_timeout is Tx-scoped (SET LOCAL).
-- CREATE INDEX CONCURRENTLY is forbidden inside the runner's single-Tx-per-file
-- (migrations.go), but context_settings/context_secrets each carry ~1 row today
-- and stay few-per-tenant, so a non-concurrent CREATE INDEX is lock-trivial.
-- Out-of-order gap (062/063 belong to Achse 04, built later) is tolerated by the
-- runner's per-version EXISTS check (cf. 061 header).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE INDEX IF NOT EXISTS idx_settings_scope_key
    ON context_settings (scope, key);

CREATE INDEX IF NOT EXISTS idx_secrets_scope_name
    ON context_secrets (scope, name);

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (64, '064_settings_tenant_index.sql', now())
  ON CONFLICT (version) DO NOTHING;
