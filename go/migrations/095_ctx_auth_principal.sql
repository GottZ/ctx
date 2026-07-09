-- =============================================================================
-- 095_ctx_auth_principal.sql — ctx_auth + ctx_auth_by_id Principal-Konvergenz
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Fundament-Welle F2 des OAuth-Stack-Ausbaus (Design 01 §8-P2, K3 Masterplan).
-- Baut auf F1/Mig 094 (context_principals + api_keys.principal_id NOT NULL) auf.
--
-- ZWEI Änderungen an der Auth-Funktion:
--
-- (1) ctx_auth() liefert principal_id als NEUE, LETZTE Spalte (Append-Pattern
--     wie 078/090 — jeder benannte SELECT bleibt gültig, alte Binaries lesen
--     die ersten 10 Spalten weiter) und enforced ein DRITTES fail-closed-Gate:
--     der Principal muss is_active sein. Das ist der Revoke-all-Mechanismus
--     (Design 01 §4): principal.is_active=false sperrt ALLE Keys der Person
--     ohne Key-für-Key-Deaktivierung.
--
-- (2) Der innere Scope-Build wird in ctx_auth_by_id(uuid) AUSFAKTORISIERT (K3):
--     ctx_auth(key_plaintext) löst nur key_hash→api_key_id auf und delegiert per
--     RETURN QUERY an ctx_auth_by_id. Achse 03 (Token-Auflösung am /mcp) und
--     Achse 05 (Browser-Session) konsumieren DIESELBE Funktion — kein Zwilling,
--     kein Gate-Drift. ctx_auth_by_id spiegelt ALLE drei fail-closed-Gates
--     (Key active, Tenant status, Principal is_active) UND den last_used_at-Write.
--     Ein neues Auth-Gate, das je nur in einen der Pfade wanderte, ist damit
--     strukturell ausgeschlossen (Risiko #1 Masterplan).
--
-- BYTE-IDENTITÄT (F2-Gate): für einen gültigen, aktiven Key mit aktivem
-- Principal ist die zurückgegebene Autorisierung (scopes, tenant, admin, …)
-- unverändert gegenüber 090 — nur principal_id kommt hinzu. Der Split
-- verändert das Verhalten nicht: ctx_auth macht statt eines UPDATE-by-hash nun
-- ein SELECT-by-hash + delegierten UPDATE-by-id; ein inaktiver/fehlender Key
-- führt in beiden Fassungen zur Sentinel-Antwort, last_used_at wird nur bei
-- active=true berührt.
--
-- SOFT-REVOKE-NEGATIV-GATE (Design 01 §8-P2, eigenes Gate): ein Key mit
-- active=false resolvt über ctx_auth_by_id zu KEINEM AuthResult — der api_key-
-- Parity-Test fängt das nicht, weil der by-id-Zweig die active-Klausel umgehen
-- könnte; hier ist sie im UPDATE-WHERE fest verankert.
--
-- lock_timeout (R-MIG2): reine Funktions-DDL (DROP/CREATE), nur kurze
-- Katalog-Locks. SET LOCAL ist transaktions-scoped + selbst-revertierend.
-- Idempotent: DROP FUNCTION IF EXISTS + CREATE re-runnen sauber; keine
-- Tabellen-Änderung → test.sh table count UNVERÄNDERT. Forward-only (Revert =
-- vorherigen 090-Body re-applyen).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- ctx_auth ändert die RETURN-Signatur (neue Spalte) → DROP erzwungen
-- (OR REPLACE verweigert eine Return-Type-Änderung). ctx_auth_by_id neu.
DROP FUNCTION IF EXISTS ctx_auth(TEXT);
DROP FUNCTION IF EXISTS ctx_auth_by_id(UUID);

-- --- ctx_auth_by_id: der geteilte innere Scope-Build (K3) --------------------
-- Eingang: eine api_key_id (aus key_hash-Auflösung, aus einem OAuth-Token oder
-- aus einer Browser-Session — INV-A: immer GENAU EIN Key). Führt die drei
-- fail-closed-Gates + last_used_at + read_scopes-Build und liefert die volle
-- AuthResult-Zeile inkl. principal_id.
CREATE FUNCTION ctx_auth_by_id(p_api_key_id UUID)
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
    confirm_writes BOOLEAN,
    principal_id   UUID       -- NEW (F2): Person-Attribution; NOT NULL auf api_keys (094)
) LANGUAGE plpgsql AS $$
DECLARE
    v_api_key_id       UUID;
    v_home_scope       VARCHAR(50);
    v_allowed_scopes   TEXT[];
    v_is_admin         BOOLEAN;
    v_tenant_id        UUID;
    v_tenant_role      TEXT;
    v_status           TEXT;
    v_read_scopes      TEXT[];
    v_cand             TEXT[];
    v_s                TEXT;
    v_write_scopes     TEXT[];
    v_confirm_writes   BOOLEAN;
    v_principal_id     UUID;
    v_principal_active BOOLEAN;
BEGIN
    -- Gate 1: Key existiert UND ist active (soft-revoke-Gate). UPDATE-by-id
    -- berührt last_used_at nur bei active=true — der Soft-Revoke-Negativ-Gate.
    UPDATE context_api_keys
    SET last_used_at = now()
    WHERE context_api_keys.id = p_api_key_id
      AND context_api_keys.active = true
    RETURNING
        context_api_keys.id,
        context_api_keys.home_scope,
        context_api_keys.allowed_scopes,
        context_api_keys.is_admin,
        context_api_keys.tenant_id,
        context_api_keys.tenant_role,
        context_api_keys.write_scopes,
        context_api_keys.confirm_writes,
        context_api_keys.principal_id
    INTO v_api_key_id, v_home_scope, v_allowed_scopes, v_is_admin, v_tenant_id,
         v_tenant_role, v_write_scopes, v_confirm_writes, v_principal_id;

    IF v_api_key_id IS NULL THEN
        api_key_id := NULL; home_scope := '__UNAUTHORIZED__'; allowed_scopes := '{}'::TEXT[];
        read_scopes := '{}'::TEXT[]; is_valid := false; is_admin := false;
        tenant_id := NULL; tenant_role := ''; write_scopes := '{}'::TEXT[];
        confirm_writes := false; principal_id := NULL;
        RETURN NEXT; RETURN;
    END IF;

    -- Gate 2: Tenant status (design/01 §5.2), VOR dem read_scopes-Build.
    SELECT status INTO v_status FROM context_tenants WHERE id = v_tenant_id;
    IF v_status IS NULL OR v_status <> 'active' THEN
        api_key_id := NULL; home_scope := '__UNAUTHORIZED__'; allowed_scopes := '{}'::TEXT[];
        read_scopes := '{}'::TEXT[]; is_valid := false; is_admin := false;
        tenant_id := NULL; tenant_role := ''; write_scopes := '{}'::TEXT[];
        confirm_writes := false; principal_id := NULL;
        RETURN NEXT; RETURN;
    END IF;

    -- Gate 3 (NEW, F2): Principal is_active — der Revoke-all-Mechanismus.
    -- principal_id ist NOT NULL auf api_keys (094), aber der Principal selbst
    -- kann deaktiviert sein → sperrt ALLE seine Keys, key-übergreifend.
    SELECT is_active INTO v_principal_active FROM context_principals WHERE id = v_principal_id;
    IF v_principal_active IS NULL OR v_principal_active = false THEN
        api_key_id := NULL; home_scope := '__UNAUTHORIZED__'; allowed_scopes := '{}'::TEXT[];
        read_scopes := '{}'::TEXT[]; is_valid := false; is_admin := false;
        tenant_id := NULL; tenant_role := ''; write_scopes := '{}'::TEXT[];
        confirm_writes := false; principal_id := NULL;
        RETURN NEXT; RETURN;
    END IF;

    -- read_scopes POSITIONAL (design/02 §4.1 Variante A) — unverändert ggü. 090.
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
    principal_id   := v_principal_id;
    RETURN NEXT;
    RETURN;
END;
$$;

-- --- ctx_auth: key_plaintext → api_key_id, delegiert an ctx_auth_by_id -------
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
    confirm_writes BOOLEAN,
    principal_id   UUID
) LANGUAGE plpgsql AS $$
DECLARE
    v_api_key_id UUID;
BEGIN
    -- Nur Credential→Identität; alle Gates + der last_used_at-Write liegen in
    -- ctx_auth_by_id. Ein key_hash-Miss ergibt v_api_key_id = NULL, worauf
    -- ctx_auth_by_id die Sentinel-Antwort liefert (UPDATE matcht keine Zeile).
    SELECT context_api_keys.id INTO v_api_key_id
      FROM context_api_keys
     WHERE context_api_keys.key_hash = encode(digest(p_api_key, 'sha256'), 'hex');

    RETURN QUERY SELECT * FROM ctx_auth_by_id(v_api_key_id);
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
VALUES (95, '095_ctx_auth_principal.sql', now()) ON CONFLICT (version) DO NOTHING;
