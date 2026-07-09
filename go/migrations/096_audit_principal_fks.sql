-- =============================================================================
-- 096_audit_principal_fks.sql — Additive Principal-Audit-FKs (OAuth-Achse 01-P3)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Fundament-Welle F3 des OAuth-Stack-Ausbaus (Plan .project/plan-oauth-2026-07-09/,
-- Design 01 §4/§8-P3, Entscheidung E1=B' Big-Bang mit ADDITIVEM Audit).
--
-- Die 12 Audit-FKs auf context_api_keys (11 Tabellen; context_secrets trägt 2)
-- BEHALTEN ihre api_key_id-/*_by-Spalte (Key-/Client-Forensik) und bekommen
-- ADDITIV eine Principal-Spalte (Person-Attribution). Kein Ersetzen — das ist
-- der B'-Kipp-Punkt aus DECISIONS.md (Forensik-Verlust bei naivem Ersetzen).
--
-- Namenskonvention: `api_key_id` → `principal_id`; `X_by` → `X_by_principal`.
-- Alle neuen Spalten UUID NULL, FK → context_principals OHNE Delete-Aktion
-- (NO ACTION) — exakt gespiegelt am Bestand: alle 12 api_key-Audit-FKs sind
-- nullable + NO ACTION (einzige Ausnahme pending_writes.api_key_id CASCADE
-- NOT NULL; die neue Spalte bleibt trotzdem nullable+NO ACTION, weil der
-- Bestand vor 096 kein Principal trägt und der Anker additiv ist).
--
-- BESTAND UNVERÄNDERT (F3-Gate): kein Backfill der Alt-Zeilen. Die historische
-- Person-Attribution bleibt jederzeit deterministisch ableitbar über den
-- erhaltenen api_key-Anker (JOIN context_api_keys k → k.principal_id, seit 094
-- NOT NULL 1:1); der NO-ACTION-FK blockt Key-Deletes solange Audit-Zeilen
-- referenzieren — kein Datenverlust-Fenster. Neue Zeilen tragen beide Anker
-- (die Go-Schreibpfade leiten die Principal-Spalte per Subquery vom acting
-- Key ab — key→principal ist funktional abhängig, ein Drift "falscher
-- Principal zum Key" ist by construction ausgeschlossen, INV-A).
--
-- context_write_log bekommt die Spalte NUR im Schema: seine beiden Writer
-- (guard, dream) sind interne Pfade ohne acting Key (api_key_id bleibt dort
-- NULL) — Vollständigkeit fürs Datenmodell + künftige key-getragene Writer.
--
-- Index: idx_access_log_principal spiegelt idx_access_log_api_key (Person-
-- Attribution-Query am Ziel-Scale); die übrigen Tabellen haben auch für die
-- api_key-Spalte keinen Index → keiner für die Principal-Spalte.
--
-- Idempotent (ADD COLUMN IF NOT EXISTS, FK per duplicate_object-Guard,
-- CREATE INDEX IF NOT EXISTS). Forward-only. Katalog-leichte DDL (nullable
-- Spalten ohne DEFAULT = kein Table-Rewrite); lock_timeout 3s (R-MIG2),
-- SET LOCAL ist transaktions-scoped (Runner wrappt jede Migration in eine tx).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- --- Spalten (12) -------------------------------------------------------------
ALTER TABLE context_access_log     ADD COLUMN IF NOT EXISTS principal_id         UUID;
ALTER TABLE context_write_log      ADD COLUMN IF NOT EXISTS principal_id         UUID;
ALTER TABLE context_pending_writes ADD COLUMN IF NOT EXISTS principal_id         UUID;
ALTER TABLE context_block_grants   ADD COLUMN IF NOT EXISTS granted_by_principal UUID;
ALTER TABLE context_chat_sessions  ADD COLUMN IF NOT EXISTS created_by_principal UUID;
ALTER TABLE context_oauth_clients  ADD COLUMN IF NOT EXISTS created_by_principal UUID;
ALTER TABLE context_projects       ADD COLUMN IF NOT EXISTS created_by_principal UUID;
ALTER TABLE context_secrets        ADD COLUMN IF NOT EXISTS created_by_principal UUID;
ALTER TABLE context_secrets        ADD COLUMN IF NOT EXISTS rotated_by_principal UUID;
ALTER TABLE context_settings       ADD COLUMN IF NOT EXISTS updated_by_principal UUID;
ALTER TABLE context_tenant_grants  ADD COLUMN IF NOT EXISTS created_by_principal UUID;
ALTER TABLE context_tenant_quota   ADD COLUMN IF NOT EXISTS updated_by_principal UUID;

-- --- FKs (12, NO ACTION wie der Bestand) --------------------------------------
DO $$
DECLARE
    spec RECORD;
BEGIN
    FOR spec IN
        SELECT * FROM (VALUES
            ('context_access_log',     'principal_id'),
            ('context_write_log',      'principal_id'),
            ('context_pending_writes', 'principal_id'),
            ('context_block_grants',   'granted_by_principal'),
            ('context_chat_sessions',  'created_by_principal'),
            ('context_oauth_clients',  'created_by_principal'),
            ('context_projects',       'created_by_principal'),
            ('context_secrets',        'created_by_principal'),
            ('context_secrets',        'rotated_by_principal'),
            ('context_settings',       'updated_by_principal'),
            ('context_tenant_grants',  'created_by_principal'),
            ('context_tenant_quota',   'updated_by_principal')
        ) AS t(tbl, col)
    LOOP
        BEGIN
            EXECUTE format(
                'ALTER TABLE %I ADD CONSTRAINT %I FOREIGN KEY (%I) REFERENCES context_principals(id)',
                spec.tbl, spec.tbl || '_' || spec.col || '_fkey', spec.col
            );
        EXCEPTION WHEN duplicate_object THEN NULL;
        END;
    END LOOP;
END $$;

-- --- Index (Spiegel von idx_access_log_api_key) --------------------------------
CREATE INDEX IF NOT EXISTS idx_access_log_principal ON context_access_log (principal_id);

INSERT INTO _migrations (version, filename, applied_at)
VALUES (96, '096_audit_principal_fks.sql', now()) ON CONFLICT (version) DO NOTHING;
