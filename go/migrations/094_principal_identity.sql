-- =============================================================================
-- 094_principal_identity.sql — Principal-/Identity-Fundament (OAuth-Achse 01-P1)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Fundament-Welle F1 des OAuth-Stack-Ausbaus (Plan .project/plan-oauth-2026-07-09/,
-- Design 01 §4/§8-P1, Entscheidung E1=B' Big-Bang mit additivem Audit).
--
-- Führt eine echte Principal-Entität ein, auf der die drei Auth-Wege
-- (api_key-Bearer, OAuth-Token, externes Login) konvergieren. DIESE Welle legt
-- NUR das Datenmodell an — ctx_auth() bleibt unangetastet (das ist F2/Mig 095),
-- also ist die Bestands-Authentifizierung nach dieser Migration byte-identisch.
--
--   context_principals          — die Person/Identität (id, display_name, is_active)
--   context_external_identities — externe IdP-Verknüpfung (Achse 04 füllt/verfeinert)
--   context_api_keys.principal_id NOT NULL — jeder Key gehört genau einem Principal
--
-- BACKFILL (grüne Wiese, ~10 Keys): jeder bestehende api_key bekommt einen
-- eigenen frisch angelegten Principal. principal_id wird erst nach dem Backfill
-- auf NOT NULL gesetzt, damit die Migration auf einer befüllten Tabelle läuft.
--
-- HA-SAFE (Design 01 §7): Der Runner (migrations.go) nimmt KEINEN globalen Lock,
-- bevor er den Migrations-Body ausführt — zwei parallel startende ctxd-Instanzen
-- könnten beide die EXISTS-Prüfung passieren und den Backfill doppelt ausführen
-- → doppelte Principals. pg_advisory_xact_lock serialisiert den Body; die zweite
-- Instanz blockt bis zum Commit der ersten und findet dann WHERE principal_id IS
-- NULL leer vor. Der Lock ist transaktions-gebunden (Runner wrappt jede Migration
-- in eine eigene tx) und löst sich beim Commit selbst.
--
-- Alle Schritte idempotent (CREATE TABLE IF NOT EXISTS, ADD COLUMN IF NOT EXISTS,
-- Backfill WHERE principal_id IS NULL, SET NOT NULL ist auf bereits-NOT-NULL ein
-- No-Op, FK-Constraint per duplicate_object-Guard). Forward-only.
--
-- lock_timeout (R-MIG2): ADD COLUMN nullable + CREATE TABLE sind katalog-leichte
-- Operationen; SET NOT NULL scannt die (winzige) Tabelle einmal. SET LOCAL ist
-- transaktions-scoped und selbst-revertierend.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- Serialisiert konkurrierende Migrations-Läufe (HA, Design 01 §7).
SELECT pg_advisory_xact_lock(94094094);

-- --- Principal-Entität --------------------------------------------------------
CREATE TABLE IF NOT EXISTS context_principals (
    id            UUID PRIMARY KEY DEFAULT uuidv7(),
    display_name  VARCHAR,
    primary_email VARCHAR,                         -- aus externem Login, NICHT auth-relevant
    is_active     BOOLEAN NOT NULL DEFAULT true,   -- in ctx_auth enforced (F2/095), kein Deko-Feld
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- --- Externe Identitäten (Achse 04 füllt) ------------------------------------
CREATE TABLE IF NOT EXISTS context_external_identities (
    id           UUID PRIMARY KEY DEFAULT uuidv7(),
    principal_id UUID NOT NULL REFERENCES context_principals(id) ON DELETE CASCADE,
    provider     VARCHAR(50) NOT NULL,   -- 'github' | 'oidc'
    issuer       VARCHAR NOT NULL,       -- allowlisted, validierter Issuer (INV-C), NICHT rohe iss-Claim
    subject      VARCHAR NOT NULL,
    email        VARCHAR,
    verified_at  TIMESTAMPTZ,            -- subject-Reuse-Schutz
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (issuer, subject)
);

-- --- api_keys.principal_id (nullable, dann Backfill, dann NOT NULL) -----------
ALTER TABLE context_api_keys ADD COLUMN IF NOT EXISTS principal_id UUID;

-- Backfill: ein Principal pro noch nicht verknüpftem Key (grüne Wiese, 1:1).
-- Per-Row-Schleife, weil ein Set-INSERT die 1:1-Rückverknüpfung Key↔Principal
-- nicht sauber ausdrücken kann (label ist nicht eindeutig).
DO $$
DECLARE
    k        RECORD;
    v_pid    UUID;
    v_count  INTEGER := 0;
BEGIN
    FOR k IN SELECT id, label FROM context_api_keys WHERE principal_id IS NULL LOOP
        INSERT INTO context_principals (display_name) VALUES (k.label)
            RETURNING id INTO v_pid;
        UPDATE context_api_keys SET principal_id = v_pid WHERE id = k.id;
        v_count := v_count + 1;
    END LOOP;
    RAISE NOTICE '094 Backfill: % Key(s) mit frischem Principal verknüpft.', v_count;
END $$;

-- Jetzt ist die Spalte befüllt → NOT NULL + FK (ON DELETE RESTRICT: keine
-- rechte-erhaltenden Waisen, Principal-Hard-Delete erzwingt vorherige
-- Key-Deaktivierung, Design 01 §4).
ALTER TABLE context_api_keys ALTER COLUMN principal_id SET NOT NULL;

DO $$ BEGIN
    ALTER TABLE context_api_keys
        ADD CONSTRAINT context_api_keys_principal_id_fkey
        FOREIGN KEY (principal_id) REFERENCES context_principals(id) ON DELETE RESTRICT;
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

INSERT INTO _migrations (version, filename, applied_at)
VALUES (94, '094_principal_identity.sql', now()) ON CONFLICT (version) DO NOTHING;
