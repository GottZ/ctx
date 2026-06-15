-- =============================================================================
-- 059_tenants_hybrid.sql — Tenant owner-register + scope partition map (E1, Modell C)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Multi-Tenant wave T02 (Achse 01, Tenant-Identität). Builds on 058
-- (scope-Generalisierung). User-decided tenant model = C (hybrid, E1):
-- context_tenants is the OWNER/management register (lifecycle, display_name,
-- status, quotas, admin assignment), but the data-bearing tables get NO
-- tenant_id — scope STAYS the data discriminator (like model B), so there is
-- NO constraint-swap and NO tenant_id backfill on the 1M+ tables
-- (context_blocks / dream_links / blobs / sources are NOT touched here; that
-- would be model A). The bridge is a narrow partition map: which scopes belong
-- to which tenant. Only context_api_keys (small table, ~7 rows) gains a slim
-- tenant_id FK (which owner holds the key) plus the tenant_role bootstrap.
--
-- Exact schema vorlage: design/01-tenant-model.md §3.4 (Z.325-362), erweitert
-- um die tenant_role-Spalte (Masterplan T02 + design/05-admin-auth-mt.md §3.1
-- K4: der tenant_role-CHECK gehört in DIESE Achse-01-Migration, die tenant_id
-- einführt — NICHT in eine separate 058 der Achse 05, die wurde gestrichen).
--
-- TENANT-DECISION(tenant-role-domain): owner|admin|member (3-Tier RBAC-Bootstrap)
--   — Alt member|tenant-admin (2-Tier), umentscheidbar via additive CHECK-
--   Erweiterung. owner verwaltet+delegiert (ernennt admins, OE-2), admin
--   verwaltet, member nutzt. Dieser Wertebereich ist BYTE-IDENTISCH mit dem
--   Go-Role-Typ (T20) + dem whoami-role-Wire-Contract (T21) zu halten (§9 K4).
--   RBAC-Pfad: Permission-Checks laufen perspektivisch via auth.Can(ar,Perm,
--   tenant) NICHT über den role-String (L2/T20); roles/permissions/
--   api_key_roles-Tabellen sind eine eigene Welle (L3), in der tenant_role zur
--   Default-Rolle / zum role_id-FK wird. 059 bereitet RBAC nur KOMMENTARISCH
--   vor (dieser Marker + die Spalte) und baut RBAC NICHT.
--
-- TENANT-DECISION(default-tenant-slug): 'default' (E9) — kosmetisch, die feste
--   UUID 00000000-0000-0000-0000-0000000d3fa0 ('...d3fa0' ≈ 'default') trägt die
--   Identität und ist das Join-Ziel für jeden Bestands-scope und -Key.
--   Umentscheidbar (nur Backfill-Konstante), die UUID bleibt.
--
-- lock_timeout (R-MIG2): die ALTER TABLE ... ADD COLUMN auf context_api_keys
-- nimmt kurz ACCESS EXCLUSIVE. Der Runner (internal/store/migrations.go) wickelt
-- JEDE Migration in ihre eigene reale Transaktion, daher ist SET LOCAL hier
-- transaktions-gescopt und selbst-revertierend — es leakt nicht in die Session
-- oder nach 060+. Wir bevorzugen fail-fast (Migration bricht sauber ab, rollt
-- zurück, ist einfach re-runnable) gegenüber unbegrenztem Lock-Warten hinter
-- einem langsamen Reader. ADD COLUMN ... DEFAULT 'member' ist ein
-- Metadaten-Default (kein Rewrite seit PG11), 3s ist großzügig.
SET LOCAL lock_timeout = '3s';
--
-- Idempotent: alle CREATE TABLE IF NOT EXISTS / ADD COLUMN IF NOT EXISTS /
-- ON CONFLICT DO NOTHING (slug bzw. scope bzw. version PK). Der Backfill-UPDATE
-- ist spalten-agnostisch und no-op beim 2. Lauf (kein NULL tenant_id mehr).
-- Reversibel: DROP COLUMN tenant_role/tenant_id + DROP TABLE
-- context_tenant_scopes/context_tenants (forward-only in der Praxis).
-- =============================================================================

-- 1. context_tenants — Owner-/Verwaltungs-Registratur (Lifecycle, Status, Quotas).
CREATE TABLE IF NOT EXISTS context_tenants (
    id           UUID PRIMARY KEY DEFAULT uuidv7(),  -- PG18 nativ (001_initial.sql:58)
    slug         VARCHAR(50) NOT NULL,
    display_name TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active','suspended','offboarding')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata     JSONB NOT NULL DEFAULT '{}',
    CONSTRAINT uq_tenants_slug UNIQUE (slug)
);

-- 2. Default-Tenant: feste UUID trägt die Identität (slug ist kosmetisch).
INSERT INTO context_tenants (id, slug, display_name)
  VALUES ('00000000-0000-0000-0000-0000000d3fa0', 'default', 'Default Tenant')
  ON CONFLICT (slug) DO NOTHING;

-- 3. context_tenant_scopes — scope -> tenant (Partition: EIN scope = EIN Tenant).
--    Gibt der Visibility-Achse die per-Tenant-scope-Auflösung, ohne dass die
--    Daten-Tabellen tenant_id tragen.
CREATE TABLE IF NOT EXISTS context_tenant_scopes (
    scope     VARCHAR(50) PRIMARY KEY,          -- ein scope = ein Tenant
    tenant_id UUID NOT NULL REFERENCES context_tenants(id) ON DELETE CASCADE
);

-- 4. Bestands-scopes {private, work, shared} -> default-Tenant. _global
--    (settings/secrets) bleibt System, gehört keinem Tenant — daher NICHT hier
--    (eine fehlende Zeile = "kein Tenant" = System, fail-closed).
INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES
  ('private', '00000000-0000-0000-0000-0000000d3fa0'),
  ('work',    '00000000-0000-0000-0000-0000000d3fa0'),
  ('shared',  '00000000-0000-0000-0000-0000000d3fa0')
  ON CONFLICT (scope) DO NOTHING;

-- 5. Schlanker FK von api_keys auf den Tenant (welcher Owner besitzt den Key).
--    KEINE FK auf context_blocks etc. — deren Tenant ist via scope ableitbar.
--    DEFAULT = default-Tenant: jeder NEU gemintete Key (CreateApiKey setzt
--    tenant_id heute noch nicht — der +tenantID-Param kommt erst T06) landet
--    im default-Tenant (volle private/work/shared-Sicht) statt NULL — ein NULL-
--    Tenant ergäbe eine degradierte read_scopes-Auflösung. Das ist die KORREKTE
--    single-tenant-Übergangs-Semantik, NICHT fail-closed im MT-Sinn: der default-
--    Tenant ist ein REALER Tenant, nicht die Minimal-Sicht (≠ tenant_role 'member',
--    das echt fail-closed ist). PFLICHT T06 (MT-fail-loud): CreateApiKey setzt
--    tenant_id explizit + SET NOT NULL + DROP DEFAULT — sonst landet ein
--    vergessenes Fremd-Tenant-tenant_id still im default-Tenant (dann fail-OPEN).
--    // TENANT-DECISION(api-key-tenant-default): default-UUID-DEFAULT nur Übergang.
--    ON DELETE RESTRICT: ein nacktes Tenant-DELETE wird BLOCKIERT, solange Keys
--    zeigen (23503). Tenant-Lifecycle (User 2026-06-15): "zu gehen" = status
--    'suspended' (stumm — ctx_auth -> __UNAUTHORIZED__ + Background/Dream stoppt
--    für die scopes des Tenants, keine LLM-Kosten für Nicht-Zahler); DATEN bleiben
--    unangetastet -> voll reaktivierbar (status zurück auf 'active'). Die echte
--    Daten-Löschung (full-prune) ist eine EXPLIZITE, geordnete server-admin-Action
--    (Achse 01 / T05 tenant-prune), NIE ein implizites Cascade/Orphan — RESTRICT
--    erzwingt den geordneten Weg (erst Keys/Daten räumen, dann Tenant).
--    context_tenant_scopes behält CASCADE (schmale Zuordnung, keine Daten).
--    // TENANT-DECISION(tenant-delete-fk): RESTRICT (kontrollierter Super-Admin-Prune).
ALTER TABLE context_api_keys ADD COLUMN IF NOT EXISTS tenant_id UUID
    DEFAULT '00000000-0000-0000-0000-0000000d3fa0' REFERENCES context_tenants(id) ON DELETE RESTRICT;

-- 6. tenant_role — RBAC L1-Bootstrap (E2). DEFAULT 'member' = fail-closed
--    (neuer Key trägt keine Verwaltungs-/Delegations-Macht). Orthogonal zu
--    is_admin (052: server-weit) — tenant_role gilt INNERHALB des Tenants.
--    CHECK-Wertebereich siehe TENANT-DECISION(tenant-role-domain) im Header.
ALTER TABLE context_api_keys ADD COLUMN IF NOT EXISTS tenant_role TEXT NOT NULL DEFAULT 'member'
    CHECK (tenant_role IN ('owner','admin','member'));

-- 7. Backfill: Bestands-Keys -> default-Tenant. tenant_role bleibt DEFAULT
--    'member' (KEIN Auto-Promote analog is_admin-Bootstrap 052:30-35 —
--    fail-closed). Der UPDATE ist no-op beim 2. Lauf (idempotent).
UPDATE context_api_keys SET tenant_id = '00000000-0000-0000-0000-0000000d3fa0' WHERE tenant_id IS NULL;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (59, '059_tenants_hybrid.sql', now())
  ON CONFLICT (version) DO NOTHING;
