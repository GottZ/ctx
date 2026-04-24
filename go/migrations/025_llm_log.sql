-- Request/response log for every LLM call across pipelines.
-- Purpose: observability, prompt evolution, post-hoc accuracy audits.
-- Partitioned via TimescaleDB hypertable (7-day chunks).
--
-- pipeline values: 'dream-eval', 'dream-temporal', 'ingest-extract',
--                  'query-synthesize', 'query-translate', 'query-temporal', 'rrf-rerank'.

CREATE TABLE context_llm_log (
    id                UUID NOT NULL DEFAULT gen_random_uuid(),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    pipeline          TEXT NOT NULL,
    model             TEXT NOT NULL,
    host              TEXT NOT NULL,
    duration_ms       INTEGER,
    error             TEXT,
    prompt_tokens     INTEGER,
    completion_tokens INTEGER,
    request_system    TEXT,
    request_user      TEXT,
    response_content  TEXT,
    block_ids         UUID[],
    dream_version     SMALLINT,
    metadata          JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (id, created_at)
);

-- TimescaleDB hypertable: 7-day chunks.
-- if_not_exists=true lets the migration replay on a partially-applied DB.
SELECT create_hypertable(
    'context_llm_log',
    'created_at',
    chunk_time_interval => interval '7 days',
    if_not_exists => true
);

CREATE INDEX IF NOT EXISTS idx_llm_log_pipeline
    ON context_llm_log (pipeline, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_llm_log_block_ids
    ON context_llm_log USING GIN (block_ids);

CREATE INDEX IF NOT EXISTS idx_llm_log_error
    ON context_llm_log (created_at DESC)
    WHERE error IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_llm_log_dream_version
    ON context_llm_log (dream_version, created_at DESC)
    WHERE dream_version IS NOT NULL;
