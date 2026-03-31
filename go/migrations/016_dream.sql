-- Migration 016: Dream Mode — async cross-reference engine.
-- Adds dream tracking columns and a dedicated links table.

-- Track when Dream last evaluated each block + cooldown.
ALTER TABLE context_blocks
    ADD COLUMN IF NOT EXISTS dream_checked_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS dream_cooldown_until TIMESTAMPTZ;

-- Priority queue index: unchecked blocks first, then lowest quality.
-- Partial index: only non-archived blocks with embeddings.
CREATE INDEX IF NOT EXISTS idx_dream_pending
    ON context_blocks (dream_checked_at ASC NULLS FIRST, quality_score ASC)
    WHERE NOT is_archived AND embedding IS NOT NULL;

-- Dream cross-reference links between blocks.
-- Same-scope rule enforced at application level (Angreifer V5 mitigation).
CREATE TABLE IF NOT EXISTS context_dream_links (
    source_block_id  UUID NOT NULL REFERENCES context_blocks(id),
    target_block_id  UUID NOT NULL REFERENCES context_blocks(id),
    relationship     TEXT NOT NULL CHECK (relationship IN ('topical', 'factual', 'causal', 'supersedes')),
    confidence       REAL NOT NULL DEFAULT 0.5,
    scope            VARCHAR(20) NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata         JSONB DEFAULT '{}',
    PRIMARY KEY (source_block_id, target_block_id),
    CHECK (source_block_id != target_block_id)
);

CREATE INDEX IF NOT EXISTS idx_dream_links_target
    ON context_dream_links(target_block_id);

-- Audit: Dream operations go into existing context_write_log with decision prefix 'dream_*'.
