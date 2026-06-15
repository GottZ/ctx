-- =============================================================================
-- 060_ctx_auth_tenant.sql — ctx_auth gains tenant identity + status gate +
--                           positional read_scopes (E1, Modell C)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Multi-Tenant wave T03/T15 (Achse 01 identity + Achse 02 visibility, fused into
-- ONE ctx_auth DROP+CREATE per the conflict-phase coordination, design/02 §9.1 /
-- design/05 §9 K1). Builds on 059 (context_api_keys.tenant_id/tenant_role +
-- context_tenants) and 061 (context_tenant_grants). Extends ctx_auth from 6 to 8
-- return columns — the 2 new columns (tenant_id UUID, tenant_role TEXT) are added
-- AT THE END so every named-column SELECT (auth.go:48) and every AuthResult{}
-- literal stays valid; old binaries that select the original 6 keep working.
--
-- DROP+CREATE, not CREATE OR REPLACE: adding a column changes the RETURNS TABLE
-- type, which OR REPLACE refuses ("cannot change return type"). Same mechanic as
-- 052:24-28. Migrations run before server start, so there is no live-traffic
-- window within the process.
--
-- THREE behavioural additions over the 052 body, each verified against the
-- canonical design (NOT the pre-Modell-C design/05 §3.4 sketch, which shows
-- tenant_id VARCHAR(50) + COALESCE(v_tenant_id, v_home_scope) — both Modell-A/B
-- relics that would crash under Modell C, see the valid-branch note):
--
--   1. Identity columns: the UPDATE...RETURNING also pulls tenant_id/tenant_role
--      (059). tenant_id is assigned DIRECTLY in the valid branch — it is already
--      a UUID; COALESCE(v_tenant_id, v_home_scope) would try 'work'::uuid and
--      crash. (design/01 §4.1; the §3.4 COALESCE is Modell-A/B only.)
--
--   2. Tenant status gate (design/01 §5.2), placed BEFORE the read_scopes build:
--      a lean single lookup on the tenant_id we already RETURNING'd (NOT the
--      §5.2 second key_hash JOIN — we hold v_tenant_id). The fail-closed
--      condition is `v_status IS NULL OR v_status <> 'active'`: a NULL
--      v_tenant_id, or a tenant row that vanished, leaves v_status NULL, and a
--      bare `<> 'active'` evaluates NULL (falsy) in plpgsql — fail-OPEN. The
--      explicit `IS NULL OR` arm closes that hole. Both 'suspended' and
--      'offboarding' fail the gate → __UNAUTHORIZED__ sentinel (is_valid=false),
--      reusing the same shape as a key-miss (no tenant-existence oracle, §5.1).
--      This is the differentiator of Modell C over B (B has no status row).
--
--   3. read_scopes built POSITIONALLY (design/02 §4.1 AMENDMENT, Variante A),
--      NOT via array_agg(DISTINCT). DISTINCT inside an aggregate forces an
--      alphabetical sort, which breaks the wire-load-bearing invariant
--      read_scopes[0] = home_scope (e.g. a 'work'-home key resolves {work,shared}
--      today; array_agg(DISTINCT) would flip it to {shared,work}, changing the
--      whoami wire (whoami.go:82) and the chat_sessions snapshot (chat.go:114)).
--      Element [1] is fixed to home_scope, then candidates (allowed, then grants
--      in a stable ORDER BY — that array_agg has an explicit ORDER BY, which is
--      deterministic, NOT a DISTINCT sort) are appended with an order-preserving
--      NOT-present dedup. The '_'-prefix filter (system scopes never visible) and
--      the COALESCE floor [home_scope] (never empty = never full access) are
--      unchanged in semantics from the 052 read_scopes line, only their
--      construction moves from concat to the positional loop.
--
-- MIGRATION ORDERING / plpgsql LATE-BINDING (the one empirically-load-bearing
-- point): the runner applies versions ASC (058,059,060,061; migrations.go:63),
-- so at THIS migration's CREATE time context_tenant_grants (version 61) does NOT
-- exist yet. plpgsql resolves table names in a function body at CALL time, not
-- CREATE time (check_function_bodies validates syntax, not table existence), so
-- CREATE FUNCTION succeeds against the not-yet-existing grants table. No call to
-- ctx_auth happens between 060 and 061 (the server starts only after ALL
-- migrations), so the grants table exists by first call. NO renumbering and NO
-- to_regclass guard are needed; the integration test's full 058→...→061 chain is
-- the empirical proof (if late-binding failed, SetupTestDB would error at 060).
--
-- lock_timeout (R-MIG2): DROP/CREATE FUNCTION takes only brief catalog locks (no
-- hot-table rewrite). The runner (internal/store/migrations.go) wraps EACH
-- migration in its own real transaction, so SET LOCAL is transaction-scoped and
-- self-reverting — it does not leak into the session or into 061+. We set it for
-- consistency with 058/059/061 and fail-fast hygiene (clean abort + re-runnable).
SET LOCAL lock_timeout = '3s';
--
-- Idempotent: DROP FUNCTION IF EXISTS + CREATE re-runs cleanly (second run drops
-- the just-created function and recreates it); _migrations INSERT ON CONFLICT
-- (version) DO NOTHING. Reversible: re-apply 052's ctx_auth body (forward-only in
-- practice). No data migration (function-only) — test.sh table count is UNCHANGED.
-- =============================================================================

DROP FUNCTION IF EXISTS ctx_auth(TEXT);

CREATE FUNCTION ctx_auth(p_api_key TEXT)
RETURNS TABLE (
    api_key_id     UUID,
    home_scope     VARCHAR(50),
    allowed_scopes TEXT[],
    read_scopes    TEXT[],
    is_valid       BOOLEAN,
    is_admin       BOOLEAN,
    tenant_id      UUID,    -- NEW (T03, Modell C): owning tenant; UUID, NOT VARCHAR
    tenant_role    TEXT     -- NEW (T03): owner|admin|member; plain TEXT (named Role type = T20)
) LANGUAGE plpgsql AS $$
DECLARE
    v_key_hash       TEXT;
    v_api_key_id     UUID;
    -- VARCHAR(50): PL/pgSQL enforces the typmod on assignment (052:58-60).
    v_home_scope     VARCHAR(50);
    v_allowed_scopes TEXT[];
    v_is_admin       BOOLEAN;
    v_tenant_id      UUID;        -- NEW
    v_tenant_role    TEXT;        -- NEW (TEXT, not VARCHAR — domain enforced by the 059 CHECK)
    v_status         TEXT;        -- NEW: tenant lifecycle status for the gate
    v_read_scopes    TEXT[];      -- NEW: positional build target (element [1] = home_scope)
    v_cand           TEXT[];      -- NEW: ordered candidate scopes (allowed, then grants)
    v_s              TEXT;        -- NEW: FOREACH cursor over candidates
BEGIN
    -- Compute SHA-256 hash of the provided API key.
    v_key_hash := encode(digest(p_api_key, 'sha256'), 'hex');

    -- Authenticate: update last_used_at and return key info, now including the
    -- Modell-C identity columns (059). last_used_at write-on-read is unchanged
    -- (R-SCALE7, the per-request write storm at N tenants, is a later seam).
    UPDATE context_api_keys
    SET last_used_at = now()
    WHERE context_api_keys.key_hash = v_key_hash
      AND context_api_keys.active = true
    RETURNING
        context_api_keys.id,
        context_api_keys.home_scope,
        context_api_keys.allowed_scopes,
        context_api_keys.is_admin,
        context_api_keys.tenant_id,
        context_api_keys.tenant_role
    INTO v_api_key_id, v_home_scope, v_allowed_scopes, v_is_admin, v_tenant_id, v_tenant_role;

    -- Key miss: sentinel (unchanged shape; +2 explicit new columns).
    -- TENANT-DECISION(sentinel-tenant-role): '' (leer) — Alt 'member',
    --   umentscheidbar weil is_valid=false ohnehin in middleware.go vor jedem
    --   Handler stoppt; '' folgt design/01 §5.2:561 (Sentinel-Konsistenz).
    IF v_api_key_id IS NULL THEN
        api_key_id     := NULL;
        home_scope     := '__UNAUTHORIZED__';
        allowed_scopes := '{}'::TEXT[];
        read_scopes    := '{}'::TEXT[];
        is_valid       := false;
        is_admin       := false;
        tenant_id      := NULL;
        tenant_role    := '';
        RETURN NEXT;
        RETURN;
    END IF;

    -- Tenant status gate (design/01 §5.2), BEFORE the read_scopes build. Lean
    -- single lookup on the tenant_id we already hold. `IS NULL OR <> 'active'`
    -- is the fail-closed condition (a bare `<> 'active'` would be NULL/falsy =
    -- fail-OPEN when v_tenant_id is NULL or the tenant row is gone). suspended
    -- AND offboarding both fail → same __UNAUTHORIZED__ sentinel as a key-miss.
    SELECT status INTO v_status FROM context_tenants WHERE id = v_tenant_id;
    IF v_status IS NULL OR v_status <> 'active' THEN
        api_key_id     := NULL;
        home_scope     := '__UNAUTHORIZED__';
        allowed_scopes := '{}'::TEXT[];
        read_scopes    := '{}'::TEXT[];
        is_valid       := false;
        is_admin       := false;
        tenant_id      := NULL;
        tenant_role    := '';
        RETURN NEXT;
        RETURN;
    END IF;

    -- read_scopes POSITIONAL (design/02 §4.1 amendment, Variante A). Element [1]
    -- is home_scope (the wire-load-bearing read_scopes[0] = home invariant); then
    -- candidates (allowed, then cross-tenant grants for THIS key's tenant) are
    -- appended order-preserving with a NOT-present dedup. array_agg(DISTINCT) is
    -- FORBIDDEN (alphabetical sort breaks the invariant). The grant array_agg
    -- uses an explicit ORDER BY (deterministic, NOT a DISTINCT sort).
    -- TENANT-DECISION(read-scopes-variant): A (FOREACH-Append) — Alt B (unnest
    --   WITH ORDINALITY), umentscheidbar weil äquivalent; A ist am direktesten
    --   les- und im Mutationstest pinbar (design/02 §4.1).
    v_read_scopes := ARRAY[v_home_scope::TEXT];
    v_cand := COALESCE(v_allowed_scopes, '{}'::TEXT[])
           || COALESCE((SELECT array_agg(g.granted_scope ORDER BY g.granted_scope)
                          FROM context_tenant_grants g
                         WHERE g.grantee_tenant = v_tenant_id), '{}'::TEXT[]);
    FOREACH v_s IN ARRAY v_cand LOOP
        -- fail-closed: '_'-prefixed system scopes never enter read_scopes
        -- (backslash-escaped LIKE; '\' is the default LIKE escape char). home_scope
        -- itself is NOT filtered — it is the MINIMAL view even if '_'-prefixed.
        IF v_s NOT LIKE '\_%' AND NOT (v_s = ANY(v_read_scopes)) THEN
            v_read_scopes := v_read_scopes || v_s;
        END IF;
    END LOOP;

    -- Valid key (+2 new columns). tenant_id assigned DIRECTLY (already a UUID;
    -- COALESCE(v_tenant_id, v_home_scope) — the design/05 §3.4 Modell-A/B sketch —
    -- would attempt 'work'::uuid and crash). The read_scopes COALESCE floor keeps
    -- [home_scope] for the degenerate '_'-home case (minimal view, never empty).
    api_key_id     := v_api_key_id;
    home_scope     := v_home_scope;
    allowed_scopes := v_allowed_scopes;
    read_scopes    := COALESCE(NULLIF(v_read_scopes, '{}'::TEXT[]), ARRAY[v_home_scope]::TEXT[]);
    is_valid       := true;
    is_admin       := v_is_admin;
    tenant_id      := v_tenant_id;
    tenant_role    := v_tenant_role;
    RETURN NEXT;
    RETURN;
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (60, '060_ctx_auth_tenant.sql', now())
  ON CONFLICT (version) DO NOTHING;
