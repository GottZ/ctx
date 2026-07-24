-- 113_embed_failures.sql
-- Achse 04 W04-2: Fehler-Memo pro Block — schließt Vorfall 2026-07-10
-- (Backfill-Endlosschleife an einem Block > Slot-Fenster) strukturell statt
-- operational. Dient dem regulären Backfill (Pfad A/B, migration_id NULL)
-- UND später der Re-Embed-Statemachine (migration_id gesetzt, W04-3+).
--
-- last_error ist NORMALISIERT, nie roher Wire-Error: Fehlerklasse + HTTP-
-- Status + header-bereinigter Auszug <=500 Zeichen. Die Konstruktion
-- passiert im Go-Worker (store.NormalizeEmbedError) — Embed-TEXT-Inhalte
-- dürfen die Zeile strukturell nicht erreichen (Backend-Response-Bodies
-- können Eingabe-Fragmente echoen; sensitivity-gebundene Blöcke bekämen
-- sonst ungeregelte Metadaten-Kopien). Lese-Oberfläche: server-admin-only
-- (W04-7, noch nicht gebaut).
CREATE TABLE IF NOT EXISTS context_embed_failures (
    block_id        UUID NOT NULL REFERENCES context_blocks(id) ON DELETE CASCADE,
    -- FK auf context_embed_migrations kommt in Migration 114 nach (ALTER ADD
    -- CONSTRAINT) — die Tabelle existiert hier noch nicht (W04-3).
    migration_id    UUID,
    attempts        INT NOT NULL DEFAULT 1,
    last_error      TEXT NOT NULL,
    last_class      TEXT NOT NULL, -- 'wire','oversize','sensitivity_ineligible','store',…
    next_attempt_at TIMESTAMPTZ NOT NULL,
    first_seen      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- PK-Ersatz (migration_id ist NULL-fähig, ein nackter PK/UNIQUE(block_id,
-- migration_id) ließe beliebig viele Backfill-Zeilen je Block zu, da NULL
-- nie mit NULL matcht): zwei Partial-Unique-Indexe, einer je Regime.
CREATE UNIQUE INDEX IF NOT EXISTS idx_embed_failures_backfill
    ON context_embed_failures (block_id) WHERE migration_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_embed_failures_migration
    ON context_embed_failures (block_id, migration_id) WHERE migration_id IS NOT NULL;

-- Peek-Träger für Pfad A/B: join-getriebene Backoff-Prüfung im Pending-
-- Prädikat ohne Memo-Tabellen-Scan.
CREATE INDEX IF NOT EXISTS idx_embed_failures_next_attempt
    ON context_embed_failures (next_attempt_at) WHERE migration_id IS NULL;
