-- =============================================================================
-- 167_llm_rejections.sql — context_llm_rejections_hourly: the hourly ledger
-- of every refused model-API request
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- A refused /v1 request writes no context_llm_log row (design/06 I-06-2:
-- nothing before the wire, so a 429 flood cannot amplify itself into the
-- hypertable), and the usage rollup (159) only counts log rows — so without
-- this table a refusal leaves no trace but one throttled log line. This table
-- keeps the count (design/06 §3.4, M-06-4, wave W06-12): one row per hour and
-- dimension (bucket_start, consumer_id, op, model_alias, dispatch_class,
-- target, reason, detail) with the number of refusals and the vain wait of the
-- refusals that queued in L2 (wait_ms_sum over wait_n — B-06-25: under a
-- flood, latency and usage measure only the survivors; this is the other
-- half).
--
-- Dimensions. consumer_id is NULL for a refusal before the model-key door
-- accepted (reason 'auth', instance-wide). op is chat | embed | rerank, ''
-- before the route knew it. model_alias holds only RESOLVED alias names:
-- every refusal before the resolution counts under the fixed sentinel
-- '_unresolved', never under the client's string (design/05 V-06-b), so
-- 10 000 unknown model names make one row. dispatch_class and target are set
-- only for reason 'capacity' (the L2 refusals). reason is the CHECK'd
-- vocabulary of design/06 §4.1; detail names the refusing rule
-- (invalid_key, server_busy, wait_expired, inflight_budget, loop, …).
--
-- Cardinality per hour is at most consumers × details × resolved aliases
-- (+ classes × targets for capacity) — independent of the request rate. The
-- only writer is the rejection flusher of ctxd (llmusage.FlushRejections,
-- every 10 s and at shutdown): one additive upsert statement per flush
-- (n = n + EXCLUDED.n, wait_ms_sum, wait_n). The unique index treats NULL as
-- a value (NULLS NOT DISTINCT, PostgreSQL 15+): without it every instance-wide
-- 'auth' row would add a row per flush instead of a count. fillfactor 70
-- leaves room for HOT updates of the running hour. No foreign key: a consumer
-- pruned with its tenant leaves its counts behind (telemetry, not lifecycle),
-- and no audit trigger (no lifecycle to audit). No backfill: refusals before
-- this migration were never counted.
--
-- Cost and locks: CREATE TABLE and its indexes on a new table — no existing
-- row is read or locked. lock_timeout 3s like every migration since 159.
--
-- Idempotent: IF NOT EXISTS throughout; a second run changes nothing.
--
-- Way back: the flag way back (gateway.enabled false) leaves the table without
-- a writer; then DROP TABLE context_llm_rejections_hourly; DELETE FROM
-- _migrations WHERE version = 167 (so a later upgrade applies it again). An
-- older binary neither reads nor writes the table.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_llm_rejections_hourly (
    bucket_start    TIMESTAMPTZ NOT NULL,
    consumer_id     UUID,
    op              TEXT NOT NULL DEFAULT '',
    model_alias     TEXT NOT NULL DEFAULT '',
    dispatch_class  TEXT NOT NULL DEFAULT '',
    target          TEXT NOT NULL DEFAULT '',
    reason          TEXT NOT NULL,
    detail          TEXT NOT NULL DEFAULT '',
    n               BIGINT NOT NULL DEFAULT 0,
    wait_ms_sum     BIGINT NOT NULL DEFAULT 0,
    wait_n          BIGINT NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_llm_rejections_reason
        CHECK (reason IN ('rate','quota','concurrency','capacity','model','auth','federation'))
) WITH (fillfactor = 70);

CREATE UNIQUE INDEX IF NOT EXISTS uq_llm_rejections_hourly
    ON context_llm_rejections_hourly (bucket_start, consumer_id, op, model_alias,
                                      dispatch_class, target, reason, detail)
    NULLS NOT DISTINCT;
CREATE INDEX IF NOT EXISTS idx_llm_rejections_consumer
    ON context_llm_rejections_hourly (consumer_id, bucket_start DESC)
    WHERE consumer_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_llm_rejections_bucket
    ON context_llm_rejections_hourly (bucket_start DESC);
