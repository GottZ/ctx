-- Migration 017: Performance indexes for Dream Mode queries at scale.

-- SupersedesMap lookup: filtered by relationship + source_block_id.
CREATE INDEX IF NOT EXISTS idx_dream_links_supersedes
    ON context_dream_links(source_block_id)
    WHERE relationship = 'supersedes';

-- Low-confidence review: sorted scan for dream-review endpoint.
CREATE INDEX IF NOT EXISTS idx_dream_links_confidence
    ON context_dream_links(confidence ASC)
    WHERE confidence < 0.7;
