-- =============================================================================
-- 065_settings_notify_scope.sql — scope in the settings NOTIFY payload (MT, Achse 03)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Multi-Tenant wave T32 (Achse 03-W6, scope-carried lazy cache invalidation).
-- Builds on 051 (notify_settings_write + the ctx_settings_write channel). Exact
-- spec: design/03-settings-secrets-mt.md §MT3-W6 / §6.3; conflict K-N1.
--
-- The 051 payload carried {entity, key, op}. Per-tenant config snapshots
-- (Achse 06) need to know WHICH scope changed so the NOTIFY listener can drop
-- exactly the affected tenant's cached config generation instead of rebuilding
-- every tenant's generation on every write (the pre-W6 fallback: every write
-- invalidates all — correct, just O(N) lazy rebuilds). This adds the row's
-- scope to the payload, ADDITIVELY: an old listener ignores the extra field, a
-- new one (events/listener.go, T32) routes a tenant scope → InvalidateTenant,
-- a _global / reserved / absent scope → full Reload (base rebuild + cache wipe).
--
-- to_jsonb(COALESCE(NEW, OLD)) already exposes the scope column for ALL four
-- tables that fire this trigger — context_settings (051), context_secrets
-- (051), context_backends (053) and context_tenant_quota (063) each carry a
-- scope column — so v_row->>'scope' resolves on every firing path; the 063
-- quota trigger inherits the scope payload automatically (it EXECUTEs the same
-- function). A hypothetical scope-less table would yield SQL NULL → JSON null →
-- Go "" → the safe _global/full-reload branch (fail-safe over-invalidation).
--
-- CREATE OR REPLACE updates the function body in place; every trigger that
-- EXECUTEs it picks up the new body on its next firing — no trigger re-create
-- needed. Function-only, no table/column change: test.sh table count unchanged
-- (cf. 060). Idempotent (CREATE OR REPLACE + ON CONFLICT DO NOTHING), forward-
-- only, self-registering (M031+ convention). Out-of-order gap (067 already
-- present from T39) is tolerated by the runner's per-version EXISTS check.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE OR REPLACE FUNCTION notify_settings_write() RETURNS TRIGGER AS $$
DECLARE
    v_row JSONB := to_jsonb(COALESCE(NEW, OLD));
BEGIN
    PERFORM pg_notify('ctx_settings_write', json_build_object(
        'entity', TG_TABLE_NAME,
        'key',    COALESCE(v_row->>'key', v_row->>'name'),
        'scope',  v_row->>'scope',
        'op',     TG_OP)::text);
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (65, '065_settings_notify_scope.sql', now())
  ON CONFLICT (version) DO NOTHING;
