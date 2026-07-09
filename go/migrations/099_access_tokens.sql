-- =============================================================================
-- 099_access_tokens.sql — Universeller opaker Token-Store (03-W03-3 / S3)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Security-Welle S3 des OAuth-Stack-Ausbaus (Plan .project/plan-oauth-2026-07-09/,
-- Design 03 §3, Masterplan K1: S3 = Mig 099; K2: E-03h/E-05-3-Reconcile —
-- DIESE Tabelle ist der EINE Universal-Credential-Store. Achse 05 baut KEINEN
-- zweiten (ihr früheres context_sessions ist verworfen, 05 §3-Kasten); sie
-- legt in Mig 102 nur den Web-Overlay context_web_sessions mit FK hierauf an.
-- Login (05) prägt issued_via='login'-Rows mit audiences=[ctx-mcp, ctx-web]
-- in DIESE Tabelle → E2 Universal-Credential strukturell (ein Token ist
-- zugleich MCP-Bearer).
--
--   token_hash     — SHA-256(opaker Token); Klartext (ctxt_/ctxr_-Präfix +
--                    32B crypto/rand hex) wird NIE gespeichert.
--   token_type     — 'access' | 'refresh' als Row-pro-Token (getrennte TTLs);
--                    Refresh-Minting kommt erst mit S4 (Rotation).
--   api_key_id     — INV-A Einzel-Key-Selektor: ein Token materialisiert
--                    IMMER genau einen Key, nie eine Key-Menge.
--   principal_id   — Person-Anker (Achse 01, seit 094).
--   audiences      — RFC 8707; enthält immer die ctx-MCP-Resource. Der
--                    Mengen-Check am /mcp kommt mit S5 (W03-6).
--   scope          — nie autoritativ (INV-B: Autorisierung = voller Key-Scope
--                    via ctx_auth_by_id; Design 03 §5).
--   refresh_family — Rotations-Familie für Reuse-Detection (S4).
--   parent_id      — rotiert-aus-Kette (S4). ON DELETE SET NULL: der GC darf
--                    Eltern-Rows räumen, ohne Kinder zu blockieren.
--   issued_via     — 'oauth' (/token) | 'login' (Achse 05) — E2-Provenienz.
--
-- Fail-closed-Auflösung (Go, resolveCredential): unbekannter Hash, expired,
-- revoked_at gesetzt, Key inaktiv, Principal inaktiv → alles 401; die
-- Materialisierung läuft über ctx_auth_by_id (095) — EIN Gate-Ort, ein
-- revozierter Key tötet seine Tokens ohne separate Token-Revocation.
--
-- Zweitens: context_oauth_codes.api_key_sealed wird ERSATZLOS gedroppt —
-- die S1-Übergangsspalte (code-versiegelter Klartext-Key für den
-- /token-Passthrough) ist mit den opaken Tokens obsolet; /token braucht nur
-- noch api_key_id. Offene Codes aus der Vor-S3-Ära verlieren ihr Blob, der
-- Flow bleibt intakt (der neue /token-Pfad liest die Spalte nicht mehr).
--
-- ON DELETE CASCADE auf key/principal: gelöschter Key/Principal reißt seine
-- Tokens mit (fail-closed, keine auflösbaren Waisen-Tokens).
--
-- Idempotent (IF [NOT] EXISTS). Forward-only. Katalog-leichte DDL;
-- lock_timeout 3s (R-MIG2), SET LOCAL ist tx-scoped (Runner wrappt).
-- test.sh T07: +1 Tabelle (44→45).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_access_tokens (
    id             UUID PRIMARY KEY DEFAULT uuidv7(),
    token_hash     VARCHAR(64) NOT NULL UNIQUE,
    token_type     VARCHAR(16) NOT NULL CHECK (token_type IN ('access', 'refresh')),
    api_key_id     UUID NOT NULL REFERENCES context_api_keys(id) ON DELETE CASCADE,
    principal_id   UUID NOT NULL REFERENCES context_principals(id) ON DELETE CASCADE,
    client_id      VARCHAR(64),
    audiences      TEXT[] NOT NULL,
    scope          TEXT,
    refresh_family UUID,
    parent_id      UUID REFERENCES context_access_tokens(id) ON DELETE SET NULL,
    issued_via     VARCHAR(16) NOT NULL CHECK (issued_via IN ('oauth', 'login')),
    expires_at     TIMESTAMPTZ NOT NULL,
    revoked_at     TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Revoke-all über den Key (Key-Delete cascaded, Key-Soft-Revoke gated in
-- ctx_auth_by_id; der Index trägt Familien-Revokes + Audits per Key).
CREATE INDEX IF NOT EXISTS idx_access_tokens_api_key ON context_access_tokens (api_key_id);
-- GC-Sweep-Pfad (EvictExpiredOAuthTokens, Scheduler-Janitor-Tick).
CREATE INDEX IF NOT EXISTS idx_access_tokens_expires ON context_access_tokens (expires_at);
-- Familien-Revoke bei Refresh-Reuse (S4): partial auf die Refresh-Rows.
CREATE INDEX IF NOT EXISTS idx_access_tokens_family
    ON context_access_tokens (refresh_family) WHERE token_type = 'refresh';

-- S1-Übergangsspalte raus (098-Kommentar: „W03-3 droppt die Spalte ersatzlos").
ALTER TABLE context_oauth_codes DROP COLUMN IF EXISTS api_key_sealed;

INSERT INTO _migrations (version, filename, applied_at)
VALUES (99, '099_access_tokens.sql', now()) ON CONFLICT (version) DO NOTHING;
