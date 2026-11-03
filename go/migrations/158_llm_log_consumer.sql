-- =============================================================================
-- 158_llm_log_consumer.sql — context_llm_log names the /v1 caller:
-- consumer_id, at most one identity per row, a per-consumer index
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Every LLM call ctx makes writes one context_llm_log row. A call made for a
-- knowledge-side caller carries that caller's api_key_id (054); background
-- work carries none. The OpenAI-compatible API (/v1, design/01) serves a
-- credential class of its own: external consumers with ctxm_ model keys,
-- which never authenticate as a knowledge key and so never produce an
-- api_key_id (design/01 §4.7 — a consumer's dispatcher principal has
-- ApiKeyID = "" and ConsumerID != ""). Their rows need an attribution column
-- of their own. It lands ahead of the gateway, so the telemetry foundation is
-- in place before the first /v1 row is written.
--
-- consumer_id UUID, nullable, NO foreign key — the api_key_id rule of 054:
-- the log references the credential, it does not depend on it. Deleting a
-- consumer must neither anonymise nor cascade its history, and the column
-- can exist before the consumer table does.
--
-- Identity contract (design/01 §4.7): a row is attributed to at most one
-- caller, a knowledge key OR a consumer, never both. The CHECK
-- ck_llm_log_one_identity is the database side of that contract (the Go side
-- is llmlog.Entry, whose ConsumerID only the gateway stamp sets). A row with
-- both is refused with 23514 — on the hypertable and on every chunk: ALTER
-- TABLE recurses into the chunks that exist, and a chunk created later
-- inherits the constraint.
--
-- Which rows: none change. Every existing row predates /v1 and is internal,
-- so consumer_id stays NULL and there is no backfill.
--
-- Cost: ADD COLUMN without a default is a catalog change. ADD CONSTRAINT
-- validates every existing row once and CREATE INDEX builds the partial index
-- chunk by chunk — two reads of every chunk's heap, the prompt and response
-- bodies stored inline in the row included; only bodies above the TOAST
-- threshold lie outside the heap and are not read. The partial index holds no
-- entry until the first /v1 row. The runner holds the migration in one
-- transaction, so context_llm_log stays locked (ACCESS EXCLUSIVE) until it
-- commits; it runs at boot before this process writes. lock_timeout bounds
-- the wait for a foreign session's lock: past 3 s the migration fails, the
-- boot with it, and the next start retries. Measured on a synthetic copy of
-- production shape (W06-2 probe, llmlog.TestMigration158CostProbe:
-- 1 000 000 rows, 29 chunks, 976 MB of heap, TimescaleDB 2.31.0): CHECK
-- validation 0.45 s, index build 0.43 s, the whole file through the runner
-- 0.75 s — see docs/operations.md "Migration 158".
--
-- Idempotent: ADD COLUMN / CREATE INDEX IF NOT EXISTS; the constraint is
-- added in a block that swallows duplicate_object, so a second run changes
-- nothing.
--
-- Rollback, only together with a revert of the W06-2 code (Entry.ConsumerID
-- names the column in every insert): DROP INDEX idx_llm_log_consumer;
-- ALTER TABLE context_llm_log DROP CONSTRAINT ck_llm_log_one_identity;
-- ALTER TABLE context_llm_log DROP COLUMN consumer_id. A binary from before
-- 158 needs none of that: it never names the column, its rows keep it NULL.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE context_llm_log
    ADD COLUMN IF NOT EXISTS consumer_id UUID;

DO $$
BEGIN
    ALTER TABLE context_llm_log
        ADD CONSTRAINT ck_llm_log_one_identity
        CHECK (num_nonnulls(api_key_id, consumer_id) <= 1);
EXCEPTION WHEN duplicate_object THEN
    NULL;
END
$$;

CREATE INDEX IF NOT EXISTS idx_llm_log_consumer
    ON context_llm_log (consumer_id, created_at DESC)
    WHERE consumer_id IS NOT NULL;
