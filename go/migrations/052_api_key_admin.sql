-- =============================================================================
-- 052_api_key_admin.sql — admin tier for context_api_keys + ctx_auth update
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Today every valid key of any home_scope can create/delete keys for ALL
-- scopes and flip dream-mode (HandleManage checks only IsValid). The settings/
-- secrets API must not inherit that model: a friend-tenant key setting
-- chat.host or injecting a provider secret is privilege escalation +
-- credential exfiltration. is_admin is the minimal cut; a role/permissions
-- model can layer on additively later (workflow-engine line).
--
-- home_scope VARCHAR(20)->VARCHAR(50): 047 widened context_blocks.scope for
-- multi-tenant scope names ('tenant:crag-research-q1' — its own target
-- examples exceed 20 chars) but left context_api_keys untouched. Without
-- this, no key can CARRY such a scope as home_scope (INSERT: value too
-- long) — per-tenant settings/secrets for those scopes would be unreachable.
-- 052 is the documented DROP+CREATE opportunity for ctx_auth (return type +
-- internal variables enforce the typmod), so the widening happens HERE, not
-- in a later migration that would have to DROP+CREATE ctx_auth again.
-- Widening is metadata-only (047 line). Remaining VARCHAR(20) scope columns
-- elsewhere (blobs, write_log, ingestion_sources, 016, fn signatures
-- 006/011/030) = known follow-up migration outside F2.
--
-- ctx_auth returns a TABLE — adding a column changes the return type, which
-- CREATE OR REPLACE refuses ("cannot change return type"). DROP + CREATE in
-- one tx; migrations run before server start, so there is no live-traffic
-- window within the same process. Old binaries keep working against the new
-- function because auth.go selects named columns, never SELECT *.
--
-- No key is auto-promoted: bootstrap is a documented one-time SQL per key
-- id (host access == full trust anyway; label is NOT unique and would
-- escalate every same-labeled key). See README "Admin bootstrap". The
-- eval/test script keys and the MCP/OAuth token key deliberately stay
-- non-admin (least privilege; the OAuth flow hands the api key itself out
-- as bearer token).
-- =============================================================================

ALTER TABLE context_api_keys
    ADD COLUMN IF NOT EXISTS is_admin BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE context_api_keys
    ALTER COLUMN home_scope TYPE VARCHAR(50);

DROP FUNCTION IF EXISTS ctx_auth(TEXT);

CREATE FUNCTION ctx_auth(p_api_key TEXT)
RETURNS TABLE (
    api_key_id UUID,
    home_scope VARCHAR(50),
    allowed_scopes TEXT[],
    read_scopes TEXT[],
    is_valid BOOLEAN,
    is_admin BOOLEAN
) LANGUAGE plpgsql AS $$
DECLARE
    v_key_hash TEXT;
    v_api_key_id UUID;
    -- VARCHAR(50): PL/pgSQL enforces the typmod on assignment — an internal
    -- VARCHAR(20) variable would re-truncate what the column widening allows.
    v_home_scope VARCHAR(50);
    v_allowed_scopes TEXT[];
    v_is_admin BOOLEAN;
BEGIN
    -- Compute SHA-256 hash of the provided API key
    v_key_hash := encode(digest(p_api_key, 'sha256'), 'hex');

    -- Authenticate: update last_used_at and return key info
    UPDATE context_api_keys
    SET last_used_at = now()
    WHERE context_api_keys.key_hash = v_key_hash
      AND context_api_keys.active = true
    RETURNING
        context_api_keys.id,
        context_api_keys.home_scope,
        context_api_keys.allowed_scopes,
        context_api_keys.is_admin
    INTO v_api_key_id, v_home_scope, v_allowed_scopes, v_is_admin;

    -- Check if we found a valid key
    IF v_api_key_id IS NULL THEN
        -- Invalid key: return sentinel values
        api_key_id     := NULL;
        home_scope     := '__UNAUTHORIZED__';
        allowed_scopes := '{}'::TEXT[];
        read_scopes    := '{}'::TEXT[];
        is_valid       := false;
        is_admin       := false;
        RETURN NEXT;
        RETURN;
    END IF;

    -- Valid key: build read_scopes = [home_scope] || allowed_scopes
    api_key_id     := v_api_key_id;
    home_scope     := v_home_scope;
    allowed_scopes := v_allowed_scopes;
    read_scopes    := ARRAY[v_home_scope::TEXT] || COALESCE(v_allowed_scopes, '{}'::TEXT[]);
    is_valid       := true;
    is_admin       := v_is_admin;
    RETURN NEXT;
    RETURN;
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (52, '052_api_key_admin.sql', now())
  ON CONFLICT (version) DO NOTHING;
