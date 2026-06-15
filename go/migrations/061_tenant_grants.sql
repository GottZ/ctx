-- =============================================================================
-- 061_tenant_grants.sql — Cross-tenant READ channel (E1, Modell C)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Multi-Tenant wave T14 (Achse 02-V1, the genuine schema contribution of the
-- visibility axis). Builds on 059 (context_tenants + context_tenant_scopes +
-- context_api_keys.tenant_id/tenant_role). Exact schema vorlage:
-- design/02-scope-visibility.md §V1 (Z.131-141).
--
-- context_tenant_grants is the cross-tenant READ channel: one tenant grants
-- ANOTHER tenant read access to one of its OWN scopes (the "friend-tenant" case,
-- design/02 §3.1/§3.3). ADDITIVE table with NO consumer yet — the read_scopes
-- resolver body in migration 060 (a LATER wave) will UNION it into ctx_auth;
-- 061 only lays the table. No backfill (empty initially: cross-tenant sharing is
-- opt-in, least-privilege).
--
-- SEMANTIK (FK + uq):
--   grantee_tenant FK → context_tenants(id) ON DELETE CASCADE
--     — the tenant RECEIVING the read access. Tenant gone → its held grants go.
--   granted_scope  FK → context_tenant_scopes(scope) ON DELETE CASCADE
--     — the scope being shared. Scope removed at offboarding → grant gone. This
--       FK is the FAIL-CLOSED-BY-CONSTRUCTION guard: a grant can ONLY target a
--       registered, tenant-owned scope. System scopes ('_global', '_'-prefixed)
--       are NEVER in context_tenant_scopes (059 maps only private/work/shared;
--       the '_'-prefix is double-enforced on key creation), so a grant on them
--       is rejected by the FK automatically (23503) — NO dedicated _-prefix
--       CHECK is needed on this table.
--   created_by     FK → context_api_keys(id) ON DELETE SET NULL
--     — provenance: which key issued the grant. Key delete ANONYMIZES the
--       history (NULLs created_by), it NEVER deletes the grant (the share
--       survives the key that minted it).
--   uq_tenant_grant (grantee_tenant, granted_scope)
--     — idempotent double-grant guard: re-granting the same (tenant, scope) pair
--       is a no-op at the schema level (23505), never a duplicate row.
--
-- lock_timeout (R-MIG2): this migration only CREATEs new objects (no ALTER on a
-- hot table), so it takes no long-held lock on existing data. The Runner
-- (internal/store/migrations.go) wraps EACH migration in its own real
-- transaction, so SET LOCAL is transaction-scoped and self-reverting — it does
-- not leak into the session or into 062+. We still set it for consistency with
-- 058/059 and fail-fast hygiene (clean abort + re-runnable) rather than
-- unbounded lock-waiting.
SET LOCAL lock_timeout = '3s';
--
-- Idempotent: CREATE TABLE IF NOT EXISTS / CREATE INDEX IF NOT EXISTS /
-- _migrations INSERT ON CONFLICT (version) DO NOTHING. No backfill to be no-op
-- about (the table starts empty). Reversible: DROP TABLE context_tenant_grants
-- (forward-only in practice).
--
-- Note: 060 (the read_scopes resolver plpgsql function that CONSUMES this table)
-- is a LATER wave and is intentionally absent here. The Runner applies
-- migrations per-version (EXISTS check, migrations.go:69-79), so an out-of-order
-- gap (061 present, 060 missing) is tolerated; 060 arrives later and references
-- this table.
-- =============================================================================

-- context_tenant_grants — cross-tenant read grants (grantee gets read on a scope
-- owned by another tenant). uuidv7() is PG18-native (001_initial.sql:58, also
-- used in 059).
CREATE TABLE IF NOT EXISTS context_tenant_grants (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    grantee_tenant  UUID NOT NULL REFERENCES context_tenants(id)              ON DELETE CASCADE,
    granted_scope   VARCHAR(50) NOT NULL REFERENCES context_tenant_scopes(scope) ON DELETE CASCADE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by      UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,
    metadata        JSONB NOT NULL DEFAULT '{}',
    CONSTRAINT uq_tenant_grant UNIQUE (grantee_tenant, granted_scope)
);

-- Lookup path for the resolver (060): "which scopes is THIS tenant granted?"
CREATE INDEX IF NOT EXISTS idx_tenant_grants_grantee ON context_tenant_grants (grantee_tenant);

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (61, '061_tenant_grants.sql', now())
  ON CONFLICT (version) DO NOTHING;
