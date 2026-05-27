-- =============================================================================
-- 044_title_text.sql — context_blocks.title VARCHAR(255) → TEXT (v2.0.0)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Trigger: CRAG-Bench S38c uncovered Wikipedia-Titel + Chunk-Suffix
-- Truncation (255-char limit hit for synthetic chunked content). v2.0.0
-- removes the length-Constraint to allow long titles for ingested
-- web/wiki corpora.
--
-- Mechanism: title is referenced by two STORED generated columns
-- (ts_de, ts_en) defined in 001_initial.sql. PostgreSQL refuses
-- ALTER COLUMN ... TYPE on a column used by a generated column, so we
-- drop the dependent columns, alter title, then recreate them
-- (which repopulates the tsvectors from current data) plus their GIN
-- indexes — all inside one transaction so failure leaves the schema
-- intact.
--
-- Cost: ts_de / ts_en are STORED → recreation re-tokenizes every row.
-- This is the only non-metadata-only step in M044-M047. At small/medium
-- corpus sizes (≤100k blocks) it completes in seconds; at 1M+ blocks
-- plan for tens of seconds of write-amplification + index rebuild.
--
-- The two GIN indexes idx_context_ts_de / idx_context_ts_en disappear
-- with the columns and are recreated identically. Other indexes on
-- context_blocks are untouched.
--
-- The uq_context_category_title constraint on (category, title) is
-- unaffected — UNIQUE constraints don't pin a column type beyond
-- equality semantics, which TEXT and VARCHAR share.
--
-- Idempotent via _migrations PK + ON CONFLICT.
-- Reversible: ALTER COLUMN title TYPE VARCHAR(255) (with the same
-- drop-recreate dance for ts_de/ts_en) — fails if any row exceeds
-- 255 chars after roll-forward (pre-rollback filter needed).
-- =============================================================================

BEGIN;

ALTER TABLE context_blocks DROP COLUMN ts_de;
ALTER TABLE context_blocks DROP COLUMN ts_en;

ALTER TABLE context_blocks ALTER COLUMN title TYPE TEXT;

ALTER TABLE context_blocks
    ADD COLUMN ts_de TSVECTOR GENERATED ALWAYS AS (
        to_tsvector('german', coalesce(title::text, '') || ' ' || content)
    ) STORED;

ALTER TABLE context_blocks
    ADD COLUMN ts_en TSVECTOR GENERATED ALWAYS AS (
        to_tsvector('english', coalesce(title::text, '') || ' ' || content)
    ) STORED;

CREATE INDEX IF NOT EXISTS idx_context_ts_de ON context_blocks USING GIN(ts_de);
CREATE INDEX IF NOT EXISTS idx_context_ts_en ON context_blocks USING GIN(ts_en);

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (44, '044_title_text.sql', now())
  ON CONFLICT (version) DO NOTHING;

COMMIT;
