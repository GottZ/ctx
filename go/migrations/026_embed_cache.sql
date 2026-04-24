-- Embedding cache: avoid re-computing embeddings for repeated text+model pairs.
-- Primary workload: Dream-Keyword embeddings (high repetition across cycles),
-- plus Query-Embeddings (user repeats queries).
-- Self-evicting via last_access — eviction job runs in the scheduler.
--
-- Key design: (text_hash, model) compound PK. Model change automatically
-- makes old entries unreachable via the lookup path.

CREATE TABLE context_embed_cache (
    text_hash    BYTEA NOT NULL,
    model        TEXT NOT NULL,
    embedding    vector(1024) NOT NULL,
    text_preview TEXT NOT NULL,
    hit_count    INTEGER NOT NULL DEFAULT 1,
    first_seen   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_access  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (text_hash, model)
);

-- Eviction-Scan: find oldest entries when size cap is hit.
CREATE INDEX IF NOT EXISTS idx_embed_cache_last_access
    ON context_embed_cache (last_access);

-- Maintenance insight: which entries are carrying their weight.
CREATE INDEX IF NOT EXISTS idx_embed_cache_hit_count
    ON context_embed_cache (hit_count DESC, last_access DESC);
