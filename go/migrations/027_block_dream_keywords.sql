-- LLM-generated keywords per block, persisted so Dream doesn't re-invoke the LLM
-- on every cooldown-recheck. Regenerated when block.updated_at > generated_at.
--
-- Why: deterministic tokeniser extracted code-syntax fragments for code blocks
-- (e.g. "mcp.newserver(&mcp.implementation{name"), which embedded into nonsense
-- regions of the vector space and produced irrelevant RRF candidates.
-- LLM extracts conceptual anchors ("MCP server", "streamable HTTP") that embed
-- semantically and match other blocks on topic rather than syntax.

ALTER TABLE context_blocks
    ADD COLUMN dream_keywords              TEXT[],
    ADD COLUMN dream_keywords_generated_at TIMESTAMPTZ;

-- Picker-friendly: find blocks that need keyword generation before the next pass.
CREATE INDEX IF NOT EXISTS idx_blocks_dream_keywords_pending
    ON context_blocks (id)
    WHERE dream_keywords IS NULL
      AND NOT is_archived;
