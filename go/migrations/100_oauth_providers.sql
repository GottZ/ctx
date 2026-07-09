-- =============================================================================
-- 100_oauth_providers.sql — OAuth-Provider-Allowlist (OAuth-Achse 04-W1a)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Consumer-Welle L1 des OAuth-Stack-Ausbaus (Plan .project/plan-oauth-2026-07-09/,
-- Design 04 §3.2/§7-W1a, Masterplan K1: L1 = Mig 100+101; die §3-Platzhalter
-- "Mig 097" sind vom Masterplan über alle Achsen linearisiert — 097 bleibt für
-- C1/02-W1 reserviert, Lücken im Namensraum sind für den Runner regulär).
--
-- Der Allowlist-Träger für externes Login (INV-C): NUR hier registrierte
-- Identity-Provider werden vertraut. expectedIssuer kommt bei der Token-Verify
-- IMMER aus dieser Config-Zeile, NIE aus der rohen iss-Claim des Tokens —
-- Issuer-Spoofing ist damit strukturell blockiert. Anlegen ist admin-only
-- (Achse 04 W4/tierServerAdmin); ein Angreifer kann keinen eigenen IdP
-- registrieren. Reine Schema-Welle: 0 Consumer-Code aktiv, Bestands-Auth
-- bleibt byte-identisch.
--
--   slug          — stabiler Handle in URLs (/auth/login/<slug>) UND im
--                   sealbox-Secret-Namen 'oauth_provider.<slug>.client_secret'.
--                   CHECK-Regex ^[a-z0-9][a-z0-9._-]{0,40}$ hält den slug
--                   ValidSecretName-kompatibel (store/sealbox.go:27 verbietet
--                   ':' — daher Separator '.' im Secret-Namen) und schließt
--                   Pfad-/Header-Injektion über den URL-Teil aus.
--   type          — 'oidc' (Discovery + ID-Token) | 'github' (feste Endpoints,
--                   Userinfo-basiert, kein ID-Token). CHECK, fail-closed.
--   issuer        — OIDC: Discovery-Basis; github: 'https://github.com'.
--                   Der validierte Vertrauensanker für UNIQUE(issuer,subject).
--   client_id     — Client-Kennung beim externen IdP.
--                   client_secret liegt NICHT hier: at-rest verschlüsselt in
--                   context_secrets via sealbox (AES-256-GCM), Name
--                   'oauth_provider.<slug>.client_secret' (E4c). Kein
--                   Klartext-Secret in der Provider-Row, kein zweiter
--                   Krypto-Stack.
--   token_auth    — Token-Endpoint-Auth: 'client_secret_post' | '_basic' |
--                   'none' (public/native Client, RFC 7591/OAuth 2.1 — dann
--                   existiert KEIN Secret, Exchange ist PKCE-only). CHECK.
--   redirect_base — ctx-externe Origin für die redirect_uri; NULL ⇒ Fallback
--                   auf Config. Callback = <base>/auth/callback/<slug>, exakt
--                   so beim Provider registriert.
--   scopes        — angefragte OAuth-Scopes (Default OIDC-Trio).
--   auth_url/token_url/userinfo_url — nur github/non-discovery; OIDC=NULL
--                   (Endpoints kommen aus dem Discovery-Doc, dessen issuer
--                   gegen provider.issuer erzwungen wird — Substitutions-Schutz).
--   id_token_algs — erlaubte Signatur-Algorithmen (parametrisiert den
--                   RS256-Hardlock): kein 'alg:none', keine RS/HS-Confusion.
--   single_tenant_issuer / allowed_claim — INV-C-Härtung (F3): bei einem
--                   Multi-Tenant-Issuer (Azure 'common', Google ohne hd) ist
--                   iss für ALLE Orgs identisch → false erzwingt zur Laufzeit
--                   einen {claim,values[]}-Filter (tid|hd|org); fehlt er, ist
--                   der Provider inaktiv (fail-closed, Go-seitig W4/W6 —
--                   bewusst kein DB-CHECK, damit Admin-Config schrittweise
--                   entstehen darf, ohne dass eine halbe Row je vertraut wird).
--   active        — Kill-Switch pro Provider (Login lädt nur active=true).
--   created_by / created_by_principal — B'-Dual-Attribution (analog 096):
--                   Key-Forensik + Person-Anker, beide additiv-nullable.
--                   created_by ON DELETE SET NULL spiegelt den Bestand
--                   context_oauth_clients.created_by (023); der Principal-FK
--                   ohne Delete-Aktion (NO ACTION) spiegelt 096 — blockt
--                   Principal-Hard-Deletes, solange Config-Zeilen attribuieren.
--
-- Kein tenant_id/scope-Diskriminator: Provider sind wie context_oauth_clients
-- Operator-global (tenant-blind); Tenant/Rolle des eingeloggten Nutzers
-- entscheidet Achse 05, nicht der Provider.
--
-- Idempotent (CREATE TABLE IF NOT EXISTS). Forward-only, rein additiv.
-- Katalog-leichte DDL; lock_timeout 3s (R-MIG2), SET LOCAL ist tx-scoped
-- (Runner wrappt jede Migration in eine eigene Transaktion).
-- test.sh T07: +1 Tabelle (42→43; nach L1 gesamt 44 — T07-Erwartung anpassen).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_oauth_providers (
    id                   UUID PRIMARY KEY DEFAULT uuidv7(),
    slug                 VARCHAR(50) NOT NULL UNIQUE
                             CHECK (slug ~ '^[a-z0-9][a-z0-9._-]{0,40}$'),
    type                 VARCHAR(20) NOT NULL
                             CHECK (type IN ('oidc', 'github')),
    display_name         TEXT NOT NULL,
    issuer               VARCHAR NOT NULL,
    client_id            VARCHAR NOT NULL,
    token_auth           VARCHAR(20) NOT NULL DEFAULT 'client_secret_post'
                             CHECK (token_auth IN ('client_secret_post', 'client_secret_basic', 'none')),
    redirect_base        VARCHAR,
    scopes               TEXT[] NOT NULL DEFAULT '{openid,profile,email}',
    auth_url             VARCHAR,
    token_url            VARCHAR,
    userinfo_url         VARCHAR,
    id_token_algs        TEXT[] NOT NULL DEFAULT '{RS256}',
    single_tenant_issuer BOOLEAN NOT NULL DEFAULT true,
    allowed_claim        JSONB,
    active               BOOLEAN NOT NULL DEFAULT true,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by           UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,
    created_by_principal UUID REFERENCES context_principals(id)
);

INSERT INTO _migrations (version, filename, applied_at)
VALUES (100, '100_oauth_providers.sql', now()) ON CONFLICT (version) DO NOTHING;
