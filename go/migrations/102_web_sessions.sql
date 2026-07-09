-- =============================================================================
-- 102_web_sessions.sql — Web-Overlay context_web_sessions (05-W1 / R1)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- RBAC-/Session-Welle R1 des OAuth-Stack-Ausbaus (Plan .project/plan-oauth-2026-07-09/,
-- Design 05 §3, Masterplan K1: R1 = Mig 102; K2: E-05-3 = Variante (a) —
-- eigene Overlay-Tabelle, weil csrf/ua/ip Web-only-Rauschen auf der
-- universellen Token-Tabelle wären).
--
-- Reiner Web-Overlay über dem EINEN Universal-Credential-Store
-- context_access_tokens (099): hält KEINEN Token-Klartext und KEINEN
-- Token-Hash — die Cookie-Bindung läuft über access_token_id auf die von 03
-- gehaltene, SHA-256-gehashte Token-Row. Hier liegen nur Referenzen +
-- Web-only-Daten (CSRF, Forensik).
--
--   principal_id    — Audit/whoami, NIE Autorisierungs-Eingabe (INV-B:
--                     Autorisierung = voller Key-Scope via ctx_auth_by_id).
--   access_token_id — der aktuelle ctxt_-Access-Token dieser Cookie-Session.
--                     INV-A: der Overlay verweist auf genau EINE Token-Row
--                     mit genau EINEM api_key_id (der Selektor lebt auf der
--                     Token-Row, nicht doppelt hier) — kein Feld unioniert
--                     Keys eines Principals.
--   refresh_family  — = context_access_tokens.refresh_family; die
--                     Cookie-Rotation folgt der 03-Lineage (S4): bei Refresh
--                     rotiert 03 die Token-Rows innerhalb der Familie, der
--                     Overlay zeigt access_token_id auf die neue Row um. In
--                     099 ist die Spalte nullable (Access-Rows ohne Refresh-
--                     Lineage existieren dort); HIER NOT NULL — eine
--                     Web-Session existiert nur mit Refresh-Lineage, das
--                     CSRF-Secret hängt an der Familie, nicht am Access-Token.
--   csrf_secret     — per-Session Synchronizer-Token (05 §4.4), server-seitig
--                     gehalten, bleibt über Token-Rotationen stabil.
--   user_agent /
--   client_ip       — Forensik (optional), reine Anzeige/Audit.
--
-- Instant-Revoke läuft NICHT über FK-CASCADE: der reguläre Key-Revoke ist
-- Soft-Delete (context_api_keys.active=false), die Overlay-Row überlebt ihn —
-- der per-Request-Resolver (resolveCredential → ctx_auth_by_id) re-appliziert
-- die active-/Status-Gates und fällt fail-closed auf 401 (05 §4.1). Die
-- ON DELETE CASCADEs hier sind nur Aufräum-Netz für die seltenen Hard-Deletes
-- (Tenant-Offboarding, Tests) — keine auflösbaren Waisen-Overlays.
--
-- KEINE Go-Änderung in dieser Welle; Store-Funktionen kommen mit R2.
--
-- Idempotent (CREATE TABLE/INDEX IF NOT EXISTS). Forward-only, rein additiv.
-- Katalog-leichte DDL; lock_timeout 3s (R-MIG2), SET LOCAL ist tx-scoped
-- (Runner wrappt jede Migration in eine Transaktion).
-- test.sh T07: +1 Tabelle (45→46), col_count context_blocks BLEIBT 40.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_web_sessions (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    principal_id    UUID NOT NULL REFERENCES context_principals(id) ON DELETE CASCADE,
    access_token_id UUID NOT NULL REFERENCES context_access_tokens(id) ON DELETE CASCADE,
    refresh_family  UUID NOT NULL,
    csrf_secret     TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at    TIMESTAMPTZ,
    user_agent      TEXT,
    client_ip       INET
);

-- Rotation folgen / Familie invalidieren (Refresh-Reuse → Familien-Revoke, S4).
CREATE INDEX IF NOT EXISTS idx_web_sessions_family ON context_web_sessions (refresh_family);
-- „meine aktiven Browser-Sessions"-Liste (Profil-Ansicht, 05/W7) am Ziel-Scale.
CREATE INDEX IF NOT EXISTS idx_web_sessions_principal ON context_web_sessions (principal_id);

INSERT INTO _migrations (version, filename, applied_at)
VALUES (102, '102_web_sessions.sql', now()) ON CONFLICT (version) DO NOTHING;
