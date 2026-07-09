-- =============================================================================
-- 098_oauth_codes.sql — Persistenter OAuth-Authorization-Code-Store (03-W03-1)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Security-Welle S1 des OAuth-Stack-Ausbaus (Plan .project/plan-oauth-2026-07-09/,
-- Design 03 §3/§7-W03-1, Masterplan K1: S1 = Mig 098; 097 bleibt fuer C1/02-W1
-- reserviert — Luecken im Namensraum sind fuer den Runner regulaer, vgl. 066).
--
-- Ersetzt die prozess-lokale In-Memory-Code-Map (oauth.go): Codes ueberleben
-- Restarts und sind multi-instanz-faehig (Code auf Instanz A, /token auf B).
-- Single-use wird zum atomaren DELETE ... RETURNING (race-/HA-sicher, zwei
-- parallele /token-Calls bekommen genau eine Row).
--
--   code_hash      — SHA-256(code); der Klartext-Code wird NIE gespeichert.
--   api_key_id     — INV-A Einzel-Key-Selektor (Design 01), statt Klartext-Key.
--   principal_id   — Person-Anker (Achse 01, seit 094).
--   client_id      — nullable bis W03-2/S2 (dort wird client_id mandatory).
--   resource/scope — Schema-komplett per Design 03 §3; Befuellung ab W03-6.
--   api_key_sealed — UEBERGANGSSPALTE (W03-1→W03-3): /token liefert bis zu den
--                    opaken Tokens (W03-3/S3) weiterhin den api_key als
--                    access_token (E2-Bestandsverhalten). Der Klartext liegt
--                    hier NICHT roh, sondern AES-256-GCM-verschluesselt unter
--                    einem AUS DEM CODE abgeleiteten Schluessel — die DB kennt
--                    nur code_hash, nicht den Code: ein DB-Dump/Backup allein
--                    kann den Key nicht rekonstruieren; einloesen kann ihn nur
--                    der Code-Besitzer (der OAuth-Client am /token). W03-3
--                    droppt die Spalte ersatzlos.
--
-- ON DELETE CASCADE auf beiden FKs: ein geloeschter Key/Principal reisst seine
-- offenen Codes mit (fail-closed, keine einloesebaren Waisen-Codes).
--
-- Idempotent (CREATE TABLE/INDEX IF NOT EXISTS). Forward-only. Katalog-leichte
-- DDL; lock_timeout 3s (R-MIG2), SET LOCAL ist tx-scoped (Runner wrappt).
-- test.sh T07: +1 Tabelle (41→42).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_oauth_codes (
    id             UUID PRIMARY KEY DEFAULT uuidv7(),
    code_hash      VARCHAR(64) NOT NULL UNIQUE,
    api_key_id     UUID NOT NULL REFERENCES context_api_keys(id) ON DELETE CASCADE,
    principal_id   UUID NOT NULL REFERENCES context_principals(id) ON DELETE CASCADE,
    client_id      VARCHAR(64),
    redirect_uri   TEXT NOT NULL,
    code_challenge VARCHAR(128) NOT NULL,
    resource       TEXT,
    scope          TEXT,
    api_key_sealed BYTEA,
    expires_at     TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- GC-Sweep-Pfad (EvictExpiredOAuthCodes, Scheduler-Janitor-Tick).
CREATE INDEX IF NOT EXISTS idx_oauth_codes_expires ON context_oauth_codes (expires_at);

INSERT INTO _migrations (version, filename, applied_at)
VALUES (98, '098_oauth_codes.sql', now()) ON CONFLICT (version) DO NOTHING;
