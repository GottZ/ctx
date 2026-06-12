-- =============================================================================
-- 054_llmlog_backend_telemetry.sql — Provenance + Trust + Kosten im LLM-Log (F3-P2)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Fixt die Fallback-Blindstelle (host=Primary bei usedFallback) strukturell:
-- backend_name = tatsächlich antwortendes Backend; metadata.chain trägt alle
-- Versuche. cost_usd aus OpenRouter usage.cost (Response-Body, G29); lokale
-- Backends NULL. Egress-Audit = partial Index auf backend_locality='external'
-- — zusammen mit der bestehenden block_ids-Spalte (M025) ist die Egress-Spur
-- ID-genau rekonstruierbar, auch für künftige body-lose Slim-Zeilen (P3/P4).
-- api_key_id (K7/E4): Caller-Attribution — die letzte günstige Migration für
-- das Proxy-Accounting-Fundament; bewusst KEIN FK (Log referenziert, Key-
-- Delete darf Historie nicht anonymisieren — 051-Audit-Linie).
-- =============================================================================

ALTER TABLE context_llm_log
    ADD COLUMN IF NOT EXISTS backend_name         TEXT,
    ADD COLUMN IF NOT EXISTS backend_trust        TEXT,
    ADD COLUMN IF NOT EXISTS backend_locality     TEXT,
    ADD COLUMN IF NOT EXISTS required_sensitivity TEXT,
    ADD COLUMN IF NOT EXISTS attempt              SMALLINT,
    ADD COLUMN IF NOT EXISTS cost_usd             NUMERIC(14,8),
    ADD COLUMN IF NOT EXISTS api_key_id           UUID;

CREATE INDEX IF NOT EXISTS idx_llm_log_backend
    ON context_llm_log (backend_name, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_llm_log_external
    ON context_llm_log (created_at DESC) WHERE backend_locality = 'external';

INSERT INTO _migrations (version, filename, applied_at)
VALUES (54, '054_llmlog_backend_telemetry.sql', now()) ON CONFLICT (version) DO NOTHING;
