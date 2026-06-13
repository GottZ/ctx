-- =============================================================================
-- 056_chat_sessions.sql — Web-Chat Sessions + Messages (F6-C2/G35)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Persistente Chat-Sessions (Design 06 §3.1/§3.10): in-memory stürbe bei jedem
-- Wave-Deploy, und ctxd bleibt bewusst stateless-restartbar.
--
-- Zwei Sichtbarkeits-Achsen, BEWUSST getrennt (06 §3.1):
--   * Ownership = scope (home_scope des anlegenden Keys). Liste + DELETE sind
--     home_scope-weit — Key-Rotation zerstört keine Sessions, created_by bleibt
--     nur als Audit-Pointer (SET-NULL wie access_log).
--   * read_scopes = Snapshot der ReadScopes des anlegenden Keys. Tool-Results
--     können Cross-Scope-Content tragen (private liest hth, live existent) und
--     werden ungekürzt persistiert; Detail-Lesen + Fortsetzen erfordern daher
--     read_scopes ⊆ caller.ReadScopes (sonst 404) — gegen den Schatten-Korpus-
--     Kanal (least-privilege-Agent-Keys der Workflow-Engine-Linie).
--
-- max_sensitivity = High-Water-Mark des Session-Contents, MONOTON steigend
-- (06 §2.3, F3 §2.2): ein credentials-Tool-Result aus Turn 1 steckt in jedem
-- Folge-Turn-Prompt — das Backend-Gate misst über die HWM, nicht über
-- "aktuelle" Inhalte. 'public' ist nur der Anlage-Zustand; AppendMessage hebt
-- die HWM in DERSELBEN TX mit JEDER Message auf max(bisher, msg.sensitivity).
--
-- CHECKs bewusst hart (wie block_role/block_sensitivity, anders als das
-- generalisierte scope): die Stufe geht in einen numerischen Rang-Vergleich
-- (backends.sensRank) — ein unbekannter Wert wäre kein neuer Tenant-Wert,
-- sondern ein kaputtes Gate. message.sensitivity DEFAULT 'credentials' ist
-- fail-closed (06 §2.3 required-Berechnung).
--
-- Busy-Mechanik statt Turn-langem Row-Lock (06 §3.1): ein Turn dauert
-- LLM-Latenz (GPU ~47s, CPU bis 900s) — eine TX so lange offen zu halten
-- sättigte den pgxpool (MaxConns=20) und hielte den xmin-Horizont gegen
-- Autovacuum. busy_until = kurze CAS-TX; abgelaufen = frei (Crash heilt sich).
-- =============================================================================

CREATE TABLE IF NOT EXISTS context_chat_sessions (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    scope           VARCHAR(50) NOT NULL,
    read_scopes     TEXT[] NOT NULL,                 -- Snapshot ar.ReadScopes bei Anlage
    created_by      UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,
    title           VARCHAR(200) NOT NULL DEFAULT 'New chat',
    max_sensitivity TEXT NOT NULL DEFAULT 'public'
                    CHECK (max_sensitivity IN ('credentials','personal','internal','public')),
    busy_until      TIMESTAMPTZ,                     -- aktiver Turn (NULL/abgelaufen = frei)
    metadata        JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_chat_sessions_scope
    ON context_chat_sessions (scope, updated_at DESC) WHERE archived_at IS NULL;

CREATE TABLE IF NOT EXISTS context_chat_messages (
    id            UUID PRIMARY KEY DEFAULT uuidv7(),
    session_id    UUID NOT NULL REFERENCES context_chat_sessions(id) ON DELETE CASCADE,
    seq           INTEGER NOT NULL,
    role          VARCHAR(20) NOT NULL
                  CHECK (role IN ('user','assistant','tool')),
    content       TEXT NOT NULL DEFAULT '',
    sensitivity   TEXT NOT NULL DEFAULT 'credentials'   -- fail-closed; §2.3 required-Berechnung
                  CHECK (sensitivity IN ('credentials','personal','internal','public')),
    tool_calls    JSONB,            -- assistant: [{id,name,arguments}]
    tool_call_id  VARCHAR(64),      -- tool: Korrelation
    tool_name     VARCHAR(64),      -- tool: ctx_query|ctx_search|ctx_get|ctx_recent
    backend       VARCHAR(100),     -- F3-Backend-Name, der den Call bediente
    model         VARCHAR(200),
    prompt_tokens     INTEGER,
    completion_tokens INTEGER,
    duration_ms   INTEGER,
    metadata      JSONB NOT NULL DEFAULT '{}',  -- {canceled:true, iteration:N, truncated:…}
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (session_id, seq)
);
-- KEIN expliziter Index auf (session_id, seq): der UNIQUE-Constraint legt genau
-- diesen B-Tree bereits an und bedient ORDER BY seq-Reads — ein zweiter wäre
-- reine doppelte Write-Last pro INSERT.

INSERT INTO _migrations (version, filename, applied_at)
VALUES (56, '056_chat_sessions.sql', now()) ON CONFLICT (version) DO NOTHING;
