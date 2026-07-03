-- =============================================================================
-- 078_write_scopes.sql — explicit per-key write scopes (Workflow-Achse W3, E4=b)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Adds context_api_keys.write_scopes — an explicit, per-key set of scopes the
-- key may WRITE blocks to, over and above its home_scope (+ shared-when-allowed).
-- Design: design/03-workflow-api-cli.md §3.3 (provisional M074, final number 078
-- per masterplan §2 K1); decision E4=b (DECISIONS.md).
--
-- SHAPE: TEXT[] NOT NULL DEFAULT '{}', identical to allowed_scopes (058/052 line)
-- — same array type, same '_'-reserved namespace, same T22 tenant rules. NOT
-- JSONB: allowed_scopes is TEXT[], and the write gate intersects the two arrays
-- in Go (writableBlockScopes), so keeping one array type avoids a cast layer.
--
-- BACKFILL: none. DEFAULT '{}' makes writableBlockScopes byte-identical for every
-- existing key (home_scope ∪ {shared-when-allowed}); the shared special case is
-- untouched. This is the pausability/rollback invariant: an old binary ignores
-- the column entirely, a new binary with an empty column reproduces v4.2.x exactly.
--
-- INVARIANT write_scopes ⊆ allowed_scopes ∪ {home_scope} is enforced DOUBLE, both
-- in Go (no DB CHECK — v2.0.0 line, open sets are validated in code): (a) at mint/
-- update time (api-key-create / api-key-update reject a write_scope with no read
-- right — a blind-writer), and (b) at the SINGLE eval point writableBlockScopes,
-- whose formula intersects write_scopes with (allowed_scopes ∪ home_scope) so a
-- stale entry left by a later allowed_scopes shrink is neutralised for free — one
-- fail-closed eval point, not N write sites (design §3.3 / §5.1).
--
-- ctx_auth is RETURNS TABLE, so a new return column forces DROP+CREATE (052/060
-- line; OR REPLACE refuses a return-type change). write_scopes is appended AS THE
-- LAST column so every named-column SELECT (auth.go) and AuthResult{} literal stays
-- valid; an old binary that selects the original 8 columns keeps working. The body
-- is byte-for-byte the 060 body plus the one new column — no behavioural change to
-- identity, the tenant status gate, or the positional read_scopes build.
--
-- lock_timeout (R-MIG2): ADD COLUMN with a constant NOT NULL DEFAULT is a
-- catalog-only, non-rewriting change on PG11+ (the default is stored in
-- pg_attribute, no table rewrite); DROP/CREATE FUNCTION takes only brief catalog
-- locks. The runner wraps each migration in its own transaction, so SET LOCAL is
-- transaction-scoped and self-reverting.
-- Idempotent: ADD COLUMN IF NOT EXISTS + DROP FUNCTION IF EXISTS + CREATE re-run
-- cleanly; _migrations INSERT ON CONFLICT (version) DO NOTHING. Forward-only.
-- Function-only body change + one additive column → test.sh table count UNCHANGED
-- (no new table; context_api_keys column count is not pinned by test.sh).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE context_api_keys
    ADD COLUMN IF NOT EXISTS write_scopes TEXT[] NOT NULL DEFAULT '{}';

DROP FUNCTION IF EXISTS ctx_auth(TEXT);

CREATE FUNCTION ctx_auth(p_api_key TEXT)
RETURNS TABLE (
    api_key_id     UUID,
    home_scope     VARCHAR(50),
    allowed_scopes TEXT[],
    read_scopes    TEXT[],
    is_valid       BOOLEAN,
    is_admin       BOOLEAN,
    tenant_id      UUID,
    tenant_role    TEXT,
    write_scopes   TEXT[]    -- NEW (W3, E4b): explicit per-key write scopes; RAW
                             -- column value — the ⊆ intersection is applied in Go
) LANGUAGE plpgsql AS $$
DECLARE
    v_key_hash       TEXT;
    v_api_key_id     UUID;
    v_home_scope     VARCHAR(50);
    v_allowed_scopes TEXT[];
    v_is_admin       BOOLEAN;
    v_tenant_id      UUID;
    v_tenant_role    TEXT;
    v_status         TEXT;
    v_read_scopes    TEXT[];
    v_cand           TEXT[];
    v_s              TEXT;
    v_write_scopes   TEXT[];      -- NEW
BEGIN
    v_key_hash := encode(digest(p_api_key, 'sha256'), 'hex');

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
        context_api_keys.tenant_role,
        context_api_keys.write_scopes
    INTO v_api_key_id, v_home_scope, v_allowed_scopes, v_is_admin, v_tenant_id, v_tenant_role, v_write_scopes;

    -- Key miss: sentinel (unchanged shape; +1 explicit new column).
    IF v_api_key_id IS NULL THEN
        api_key_id     := NULL;
        home_scope     := '__UNAUTHORIZED__';
        allowed_scopes := '{}'::TEXT[];
        read_scopes    := '{}'::TEXT[];
        is_valid       := false;
        is_admin       := false;
        tenant_id      := NULL;
        tenant_role    := '';
        write_scopes   := '{}'::TEXT[];
        RETURN NEXT;
        RETURN;
    END IF;

    -- Tenant status gate (design/01 §5.2), BEFORE the read_scopes build.
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
        write_scopes   := '{}'::TEXT[];
        RETURN NEXT;
        RETURN;
    END IF;

    -- read_scopes POSITIONAL (design/02 §4.1 amendment, Variante A). Unchanged.
    v_read_scopes := ARRAY[v_home_scope::TEXT];
    v_cand := COALESCE(v_allowed_scopes, '{}'::TEXT[])
           || COALESCE((SELECT array_agg(g.granted_scope ORDER BY g.granted_scope)
                          FROM context_tenant_grants g
                         WHERE g.grantee_tenant = v_tenant_id), '{}'::TEXT[]);
    FOREACH v_s IN ARRAY v_cand LOOP
        IF v_s NOT LIKE '\_%' AND NOT (v_s = ANY(v_read_scopes)) THEN
            v_read_scopes := v_read_scopes || v_s;
        END IF;
    END LOOP;

    -- Valid key (+1 new column). write_scopes is returned RAW (COALESCE floor to
    -- '{}' for a NULL column); the ⊆ (allowed ∪ home) intersection is a Go-side
    -- concern (writableBlockScopes), NOT re-implemented here — the DB stays the
    -- record of intent, the gate stays the single fail-closed eval point.
    api_key_id     := v_api_key_id;
    home_scope     := v_home_scope;
    allowed_scopes := v_allowed_scopes;
    read_scopes    := COALESCE(NULLIF(v_read_scopes, '{}'::TEXT[]), ARRAY[v_home_scope]::TEXT[]);
    is_valid       := true;
    is_admin       := v_is_admin;
    tenant_id      := v_tenant_id;
    tenant_role    := v_tenant_role;
    write_scopes   := COALESCE(v_write_scopes, '{}'::TEXT[]);
    RETURN NEXT;
    RETURN;
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (78, '078_write_scopes.sql', now())
  ON CONFLICT (version) DO NOTHING;
