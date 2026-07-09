-- =============================================================================
-- 101_sso_states.sql — SSO-State-Store + Identitäts-Verfeinerung (04-W1b)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Consumer-Welle L1 des OAuth-Stack-Ausbaus (Plan .project/plan-oauth-2026-07-09/,
-- Design 04 §3.3+§3.1/§7-W1b, Masterplan K1: L1 = Mig 100+101).
--
-- context_sso_states — serverseitiger, single-use Login-State (Port des
-- serviceportal-Musters sso_pending_states). Bewusst DB-basiert statt
-- in-memory (Kontrast MCP-codeStore): HA-safe by design — Login-Start auf
-- Instanz A, Callback auf Instanz B funktioniert; States überleben Restarts.
--
--   id         — opaker Cookie-Wert = Row-Lookup-Key, steht NIE in einer URL.
--                Doppel-UUID-Prinzip (F1): Row-id (im httpOnly-Cookie) und
--                OAuth-state-Param (in state_data.state) sind ZWEI getrennte
--                UUIDs. Verzehr läuft über die Cookie-Row-id, DANACH wird der
--                URL-state gegen state_data.state verglichen — kollabiert man
--                beide, landet der Consume-Key in IdP-Logs/Referrer/History
--                und der state-Check vergleicht einen Wert mit sich selbst.
--                DEFAULT uuidv7() ist Fallback; der Go-Pfad (W5/W6) erzeugt
--                die id selbst.
--   purpose    — 'login' | 'link' (Cross-Use-Schutz, F1): der Verzehr filtert
--                purpose mit, ein Login-State kann nie einen Account-Link
--                autorisieren und umgekehrt. Kein CHECK — die Werteliste ist
--                Go-seitig (W5) geschlossen, der Consume-Pfad matcht exakt.
--   state_data — {provider_slug, state, pkce_verifier, nonce, return_to}.
--                provider_slug bindet den Callback an den beim Login gewählten
--                Provider (F2, Mix-up-Abwehr): tokenURL/client_id/issuer kommen
--                aus dem versiegelten State, nie aus der Callback-URL.
--   expires_at — TTL; Verzehr ist ein atomarer
--                DELETE … WHERE id=$1 AND purpose=$2 AND expires_at>now()
--                RETURNING state_data — race-/HA-sicherer single-use, ein
--                zweiter Verzehr desselben States bekommt nichts (Replay/CSRF).
--
-- idx_sso_states_expires — GC-Sweep-Pfad für den Cleanup-Worker
-- (CleanupExpiredSSOStates, analog idx_oauth_codes_expires aus 098) —
-- unbegrenztes Wachstum durch abgebrochene Logins wird weggeräumt.
--
-- context_external_identities (aus 094) — additive Verfeinerung (§3.1):
--   last_login_at — Login-Aktivität, NICHT auth-relevant (reine Anzeige/Audit).
--   display_name  — aus dem Provider-Claim, reine Anzeige. Beide Spalten sind
--                   bewusst KEIN Vertrauensanker — der bleibt allein
--                   UNIQUE(issuer,subject) + verified_at (INV-C).
--   idx_external_identities_principal — "welche Identitäten hat Principal X"
--                   (Profil-/Linking-Ansicht, Achse 05/W7) am Ziel-Scale.
--
-- Bestands-Auth bleibt byte-identisch: neue Tabelle + zwei nullable Spalten
-- ohne DEFAULT (kein Table-Rewrite) + zwei Indexe, kein bestehender Auth-Pfad
-- liest eines davon.
--
-- Idempotent (CREATE TABLE/INDEX IF NOT EXISTS, ADD COLUMN IF NOT EXISTS).
-- Forward-only, rein additiv. Katalog-leichte DDL; lock_timeout 3s (R-MIG2),
-- SET LOCAL ist tx-scoped (Runner wrappt jede Migration in eine Transaktion).
-- test.sh T07: +1 Tabelle (43→44; T07-Erwartung 42→44 anpassen, L1 gesamt).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- --- SSO-State (single-use, HA-safe) ------------------------------------------
CREATE TABLE IF NOT EXISTS context_sso_states (
    id         UUID PRIMARY KEY DEFAULT uuidv7(),
    purpose    VARCHAR(10) NOT NULL,
    state_data JSONB NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- GC-Sweep-Pfad (CleanupExpiredSSOStates, Scheduler-Janitor-Tick).
CREATE INDEX IF NOT EXISTS idx_sso_states_expires ON context_sso_states (expires_at);

-- --- context_external_identities: additive Verfeinerung (§3.1) ----------------
ALTER TABLE context_external_identities ADD COLUMN IF NOT EXISTS last_login_at TIMESTAMPTZ;
ALTER TABLE context_external_identities ADD COLUMN IF NOT EXISTS display_name  VARCHAR;

CREATE INDEX IF NOT EXISTS idx_external_identities_principal
    ON context_external_identities (principal_id);

INSERT INTO _migrations (version, filename, applied_at)
VALUES (101, '101_sso_states.sql', now()) ON CONFLICT (version) DO NOTHING;
