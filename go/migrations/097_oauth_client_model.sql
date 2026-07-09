-- =============================================================================
-- 097_oauth_client_model.sql — OAuth-Client-Modell: Metadaten-Spalten (02-W1)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Server-Welle C1 des OAuth-Stack-Ausbaus (Plan .project/plan-oauth-2026-07-09/,
-- Design 02 §3/§7-W1, Masterplan K1: C1 = Mig 097 — die Nummer war seit S1
-- reserviert, Lücken im Namensraum sind für den Runner regulär, vgl. 066/098).
--
-- Erweitert context_oauth_clients (Mig 023: 7 Spalten + created_by_principal
-- aus 096) um die für OAuth 2.1 nötigen Client-Constraints. Alle Neu-Spalten
-- sind additiv mit verhaltens-neutralen Defaults: der Bestands-Client behält
-- redirect_uris='{}' → die statische S2-Allowlist (oauth.go) bleibt bis zur
-- 03-Verdrahtung (S6/W03-7) die durchsetzende Instanz. Diese Welle ändert
-- KEIN Verhalten — sie liefert Daten, die 02-W2…W4 (Store/CLI, Metadata, DCR)
-- und Achse 03 (Enforcement am /authorize + /token) konsumieren.
--
--   redirect_uris   — exakte Allowlist (OAuth 2.1 §5.4, kein Wildcard/Substring)
--   scopes          — REQUESTABLE ceiling; NIE autoritativ (INV-B: der Client
--                     ist kein Autorisierungs-Gate, die Key-Autorität ist die
--                     harte Decke — client.scopes kann sie nie weiten)
--   grant_types     — erlaubte Grants (MVP: authorization_code, später +refresh)
--   response_types  — nur 'code' (OAuth 2.1, kein implicit)
--   token_endpoint_auth_method — none | client_secret_basic | client_secret_post
--                     (private_key_jwt bewusst NICHT im MVP, kein jwks-Pfad)
--   registration_source — admin | dcr | cimd (Forensik/GC-Selektor für W4b)
--   metadata        — RFC-7591-Low-Prio-Felder (client_uri, logo_uri, contacts,
--                     tos_uri, policy_uri, software_id)
--   updated_at      — Änderungs-Anker (RFC-7592-Vorbereitung, deferred)
--
-- 9. Operation: client_secret_hash bekommt DEFAULT '' — public (none)-Clients
-- aus DCR brauchen kein Secret. Ein leerer Hash ist NIE ein gültiges Secret:
-- die 03-Verdrahtung prüft token_endpoint_auth_method != 'none' VOR jedem
-- Secret-Vergleich (Design 02 §4), und heute liest kein Pfad den Hash (der
-- einzige Prüfer ValidateOAuthClientSecret ist Dead Code, inventory/02 §3).
-- Kein Backfill nötig: alle Bestands-Rows tragen bereits einen Hash-Wert.
--
-- Idempotent (ADD COLUMN IF NOT EXISTS; SET DEFAULT ist von Natur aus
-- idempotent). Forward-only; Spalten droppbar, solange kein DCR-Client
-- existiert. Katalog-leichte DDL; lock_timeout 3s (R-MIG2), SET LOCAL ist
-- tx-scoped (Runner wrappt). test.sh T07: keine neue Tabelle (bleibt 42).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE context_oauth_clients
    ADD COLUMN IF NOT EXISTS redirect_uris  text[]      NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS scopes         text[]      NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS grant_types    text[]      NOT NULL DEFAULT '{authorization_code}',
    ADD COLUMN IF NOT EXISTS response_types text[]      NOT NULL DEFAULT '{code}',
    ADD COLUMN IF NOT EXISTS token_endpoint_auth_method varchar(32) NOT NULL DEFAULT 'none',
    ADD COLUMN IF NOT EXISTS registration_source        varchar(16) NOT NULL DEFAULT 'admin',
    ADD COLUMN IF NOT EXISTS metadata       jsonb       NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS updated_at     timestamptz NOT NULL DEFAULT now();

ALTER TABLE context_oauth_clients
    ALTER COLUMN client_secret_hash SET DEFAULT '';
