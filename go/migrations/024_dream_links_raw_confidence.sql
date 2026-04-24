-- Raw LLM confidence as first-class column on dream links.
-- Previously: metadata->>'raw_confidence' (JSON), column `confidence` held weighted value
-- (raw × sourceQuality × targetQuality). This conflated two distinct signals and made all
-- gates operating on `confidence` unreachable for population with quality_score ≈ 0.5.
--
-- New model: `raw_confidence` = LLM self-assessment (what LLM returns), used for gates.
--            `confidence`     = weighted value (downstream ranking signal, not gate).

ALTER TABLE context_dream_links
    ADD COLUMN raw_confidence REAL NOT NULL DEFAULT 0.5
        CHECK (raw_confidence >= 0.0 AND raw_confidence <= 1.0);

-- Gate index: links below raw 0.7 are "noise" — query-side gates filter them out.
CREATE INDEX IF NOT EXISTS idx_dream_links_raw_confidence
    ON context_dream_links (raw_confidence)
    WHERE raw_confidence < 0.7;

-- Pipeline version — lets us filter Pre-Reset (v1) from Post-Reset (v2+) data
-- and evolve gate logic without destroying history. Bumped in Go via dream.Version.
ALTER TABLE context_dream_links
    ADD COLUMN dream_version SMALLINT NOT NULL DEFAULT 1;

CREATE INDEX IF NOT EXISTS idx_dream_links_version
    ON context_dream_links (dream_version);
