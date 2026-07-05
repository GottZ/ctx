-- =============================================================================
-- 090_confirm_writes_capability.sql — per-key confirm_writes capability (F6-C6 D-W4)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Adds context_api_keys.confirm_writes — a per-key OPT-IN flag that marks the
-- key's MCP store/update writes as stage-then-confirm (the D-W5 wave adds the
-- actual staged path; this migration is gate infrastructure only, no
-- behavioural change ships with it).
--
-- THREAT-MODEL NOTE (DECISIONS.md §Klarstellung D-E1/E2, 2026-07-05): gating
-- LLM-initiated writes is the HARNESS's responsibility (Claude Code /
-- claude.ai tool-approval; the ctx web chat's ConfirmCard). This flag is NOT a
-- security boundary — it is a per-principal distrust tool: the option to force
-- server-side staging onto a harness that has no (trusted) gating layer of its
-- own. Keys of harnesses with their own gating correctly keep it off.
--
-- DEFAULT false = fail-open for every existing key (decision D-E2): MCP
-- automations keep writing directly; nothing breaks on deploy. Opt-in is a
-- per-id UPDATE (same bootstrap convention as is_admin, 052).
--
-- ctx_auth is RETURNS TABLE, so the new return column forces DROP+CREATE (052/
-- 060/078 line; OR REPLACE refuses a return-type change). confirm_writes is
-- appended AS THE LAST column so every named-column SELECT (auth.go) and
-- AuthResult{} literal stays valid; an old binary that selects the original 9
-- columns keeps working (rollback compatibility). The body is byte-for-byte
-- the 078 body plus the one new column — no behavioural change to identity,
-- the tenant status gate, or the positional read_scopes build.
--
-- lock_timeout (R-MIG2): ADD COLUMN with a constant NOT NULL DEFAULT is a
-- catalog-only, non-rewriting change on PG11+; DROP/CREATE FUNCTION takes only
-- brief catalog locks. The runner wraps each migration in its own transaction,
-- so SET LOCAL is transaction-scoped and self-reverting.
-- Idempotent: ADD COLUMN IF NOT EXISTS + DROP FUNCTION IF EXISTS + CREATE
-- re-run cleanly; _migrations INSERT ON CONFLICT (version) DO NOTHING.
-- Forward-only. Function-only body change + one additive column → test.sh
-- table count UNCHANGED.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE context_api_keys
    ADD COLUMN IF NOT EXISTS confirm_writes BOOLEAN NOT NULL DEFAULT false;

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
    write_scopes   TEXT[],
    confirm_writes BOOLEAN   -- NEW (F6-C6 D-W4): per-key stage-then-confirm
                             -- opt-in; RAW column value, evaluated at the
                             -- store handlers only (D-W5)
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
    v_write_scopes   TEXT[];
    v_confirm_writes BOOLEAN;     -- NEW
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
        context_api_keys.write_scopes,
        context_api_keys.confirm_writes
    INTO v_api_key_id, v_home_scope, v_allowed_scopes, v_is_admin, v_tenant_id, v_tenant_role, v_write_scopes, v_confirm_writes;

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
        confirm_writes := false;
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
        confirm_writes := false;
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

    -- Valid key (+1 new column). confirm_writes is returned RAW (COALESCE
    -- floor to false for a NULL column); it is consumed at the store handlers
    -- only (HandleStore / mcpStoreHandler, D-W5) — internal writers (digest,
    -- dream) go through store.UpsertBlock and never see it.
    api_key_id     := v_api_key_id;
    home_scope     := v_home_scope;
    allowed_scopes := v_allowed_scopes;
    read_scopes    := COALESCE(NULLIF(v_read_scopes, '{}'::TEXT[]), ARRAY[v_home_scope]::TEXT[]);
    is_valid       := true;
    is_admin       := v_is_admin;
    tenant_id      := v_tenant_id;
    tenant_role    := v_tenant_role;
    write_scopes   := COALESCE(v_write_scopes, '{}'::TEXT[]);
    confirm_writes := COALESCE(v_confirm_writes, false);
    RETURN NEXT;
    RETURN;
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (90, '090_confirm_writes_capability.sql', now())
  ON CONFLICT (version) DO NOTHING;
