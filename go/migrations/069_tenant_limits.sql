-- =============================================================================
-- 069_tenant_limits.sql — strukturelle per-Tenant-Limits (max_scopes/max_keys)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Self-Service-Onboarding-Achse (BE5/BEQ). Trägt die ZWEI strukturellen
-- Tenant-Limits, gegen die die Self-Service-Anlage von Scopes (scope-create) und
-- Keys (api-key-create) transaktional deckelt (AcquireTenantSlot, BEQ-2). KLAR
-- abgegrenzt von 063_tenant_quota (context_tenant_quota = LLM-KOSTEN-Budget,
-- scope-keyed): das hier sind strukturelle ZÄHL-Limits auf dem Tenant, kein Geld.
--
-- WARUM typisierte Spalten statt metadata-JSONB: ein Security-Control braucht eine
-- DB-validierte Schranke (CHECK >= 0), nicht eine durch jeden anderen metadata-
-- Writer clobberbare, ungeprüfte JSONB-Zelle. Eine additive NULL-fähige Spalte mit
-- Default ist der normale Migrations-Lauf alt→neu (kein BREAKING, kein Backfill).
--
-- DEFAULT-SEMANTIK (fail-closed, S3a): Default 25/50 ist ein KONKRETER Cap — jeder
-- Bestands-Tenant und jeder neue Tenant ist gedeckelt, NIE versehentlich unbegrenzt.
-- NULL = EXPLIZIT unbegrenzt (Operator-Akt). Der System/default-Tenant trägt die
-- Bestands-Keys (private/work/shared) und ist als einziger per Seed auf NULL =
-- unlimited gesetzt. Sane Defaults recherchiert: max_keys=50 (≈ Datadog Org-Limit),
-- max_scopes=25 (Knowledge-Tenant-Bedarf + Exhaustion-Cap). server-admin setzt sie
-- pro Tenant beim tenant-create/-limit-set (BEQ-1). 0 = bewusst "frozen".
--
-- ADD COLUMN ... DEFAULT (PG ≥ 11): konstanter Default = Metadata-only, KEIN Table-
-- Rewrite — Bestand bekommt 25/50 ohne Backfill.
--
-- Index-Tx-Trap (analog 063, db §1): der Runner hält jede Migration in EINER Tx →
-- CREATE INDEX CONCURRENTLY ist verboten. context_tenant_scopes + context_api_keys
-- sind heute klein (Onboarding-Scale), ein nicht-concurrent CREATE INDEX ist ein
-- kurzer Build. Bei echtem 1M-Volumen out-of-band vorab (manuell CREATE INDEX
-- CONCURRENTLY; die Migration findet ihn via IF NOT EXISTS). Tooling-Seam, keine
-- Schema-Frage. Die Indizes tragen die Cap-Counts (count WHERE tenant_id = $1) der
-- Slot-Reservierung, die sonst Seq-Scans über die ganze Child-Tabelle wären.
--
-- Idempotent (ADD COLUMN / CREATE INDEX IF NOT EXISTS, UPDATE auf NULL ist
-- konvergent, _migrations ON CONFLICT). Forward-only, self-registering.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- 1. Strukturelle Zähl-Limits auf dem Tenant. NULL-fähig (NULL = unlimited),
--    Default = konkreter fail-closed Cap, CHECK gegen Negativ-Werte.
ALTER TABLE context_tenants
    ADD COLUMN IF NOT EXISTS max_scopes INTEGER DEFAULT 25
        CHECK (max_scopes IS NULL OR max_scopes >= 0),
    ADD COLUMN IF NOT EXISTS max_keys   INTEGER DEFAULT 50
        CHECK (max_keys   IS NULL OR max_keys   >= 0);

-- 2. System/default-Tenant = unlimited (expliziter Akt; trägt die Bestands-Keys
--    private/work/shared, ist kein Self-Service-Mandant).
UPDATE context_tenants
    SET max_scopes = NULL, max_keys = NULL
    WHERE id = '00000000-0000-0000-0000-0000000d3fa0';

-- 3. Cap-Count-Zugriffspfade: count(*) WHERE tenant_id = $1 unter dem Slot-Lock.
CREATE INDEX IF NOT EXISTS idx_tenant_scopes_tenant
    ON context_tenant_scopes (tenant_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_tenant
    ON context_api_keys (tenant_id);

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (69, '069_tenant_limits.sql', now())
  ON CONFLICT (version) DO NOTHING;
