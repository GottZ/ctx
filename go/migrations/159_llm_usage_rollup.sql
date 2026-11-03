-- =============================================================================
-- 159_llm_usage_rollup.sql — context_llm_usage_hourly: an hourly rollup of
-- every context_llm_log row, kept by an insert trigger, backfilled once
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- The status page, the tenant spend guard and the coming usage surfaces ask
-- context_llm_log the same questions: calls, errors, tokens, GPU time, cost
-- and latency per hour and per caller. Answered from the raw rows, each of
-- them is a scan that grows with the log — at the target scale (ten and more
-- instances, thousands of requests an hour) millions of rows per question.
-- This table keeps the answers as the rows arrive: one row per hour and
-- dimension (bucket_start, api_key_id, consumer_id, pipeline, backend_name,
-- model, model_alias, dispatch_class, backend_locality, hop) with counters,
-- sums and three fixed log2 histograms — duration, queue wait, time to first
-- token; 16 buckets over a 60 ms base, bucket i (0-based) holding
-- ms <= 60·2^i and the last one everything above 983 040 ms (the bucket
-- contract of design/05 §3.5). Nothing reads it yet; the readers follow.
--
-- Dimensions are normalised the way a reader asks: backend_name falls back
-- to host (as /api/status does), NULL text becomes '', model_alias and hop
-- come from metadata.gw (the /v1 telemetry stamp, absent on every row so
-- far). A row names a knowledge key OR a consumer; should one ever carry
-- both (possible only without the CHECK of 158), the consumer wins, so
-- ck_llm_usage_one_identity never fires. No tenant column: a consumer's
-- tenant is a lookup, not a copy. The unique index treats NULL as a value
-- (NULLS NOT DISTINCT, PostgreSQL 15+) — without it key-less background
-- rows never conflict, and each of them would add a rollup row instead of a
-- count.
--
-- Why a trigger: an upsert in the Go writer would miss every row written in
-- SQL (test seeds, psql), and a TimescaleDB continuous aggregate, as
-- documented, can neither be filled inside the runner's transaction nor
-- refreshed without the job scheduler ctx does not run (not tried here). An
-- AFTER INSERT row trigger counts every row, atomically with its insert. It
-- must never raise — an error in it fails the llmlog INSERT and loses the
-- row with its telemetry — so every metadata read is guarded by
-- jsonb_typeof, numbers are floored and clamped (hop 0..16, ttft_ms
-- 0..9e15), NULLs are coalesced, every text dimension is cut to 100
-- characters, and cost_usd is an unbounded NUMERIC: a bounded one overflows
-- on a sum (NUMERIC(18,8) after 10 001 rows at the llmlog column maximum).
-- Totality comes from the guards, not from swallowing errors — an EXCEPTION
-- block would hide the miscount it caused.
--
-- The cut keeps every entry of the unique index below the B-tree limit of
-- 2 704 bytes, which context_llm_log itself does not impose on model, host,
-- dispatch_class or backend_locality (model comes from the upstream's
-- answer). Worst case after left(x, 100), at most 4 bytes per character:
-- six texts of 400 bytes + 4-byte varlena headers = 2 424, bucket_start 8,
-- api_key_id and consumer_id 2 × 16 = 32 (the CHECK allows only one),
-- hop 2, tuple header with null bitmap 16, alignment padding below 30 —
-- under 2 510 bytes. Uncut, six texts of 128 four-byte characters already
-- make a 3 096-byte entry (54000, measured). One cut for all six, the alias
-- included (100 rather than 128), keeps that sum checkable. Two texts that
-- differ only after the 100th character share a rollup row; today's longest
-- dimension text has 25 characters.
--
-- Histograms are added element by element by hist_add: bigint[] +
-- bigint[] does not exist (42883), and intarray's + would be a union. No
-- update touches a dimension column, so an update stays on its page (HOT)
-- as long as the page has room; fillfactor 50 leaves room on every page, so
-- a row version that has to move lands where the next ones fit (92 % HOT at
-- 50 against 89 % at 100 with eight writers on one row), and autovacuum runs
-- per 5 000 dead tuples instead of per 20 % of the table.
--
-- Which rows: all of them. The backfill at the end aggregates every existing
-- context_llm_log row with the same normalisation and writes its counts over
-- whatever the table holds for those dimensions; a row whose counts already
-- hold is left untouched, so a second run changes nothing. It runs after
-- CREATE TRIGGER in the same transaction: CREATE TRIGGER takes SHARE ROW
-- EXCLUSIVE on context_llm_log, so a concurrent INSERT waits for the commit
-- and is then counted by the trigger, while the backfill sees every row
-- committed before it — each row counts exactly once. At boot the runner
-- applies the migration before this process writes anyway.
--
-- Deletes: the rollup is a ledger of calls. DELETE and TRUNCATE on
-- context_llm_log do not decrement it (production deletes no llmlog rows);
-- the body janitor EvictBodies is an UPDATE and does not reach the trigger.
--
-- Cost: on a synthetic copy of production shape (W06-3 probe,
-- llmlog.TestMigration159BackfillProbe: 1 000 000 rows over 29 chunks,
-- 976 MB of heap, TimescaleDB 2.31.0) the backfill took 8.0 s and the whole
-- file through the runner 7.5 s, writing 77 238 rollup rows (109 MB with
-- indexes); everything else is catalog work of milliseconds. For the live
-- table (352 720 rows on 2026-11-03) that is about 3 s at boot,
-- extrapolated. Meanwhile inserts into context_llm_log wait for the commit
-- (the lock of CREATE TRIGGER); at boot nothing writes yet. Every insert
-- afterwards pays one upsert. With eight writers on one dimension — the
-- worst case, all of them queueing for one row lock — p99 insert latency
-- rose by 0.9–1.5 ms over a baseline without the trigger and 92 % of the
-- updates were HOT (W06-3 load gate, llmlog.TestUsageRollupLoad). See
-- docs/operations.md "Migration 159".
--
-- Idempotent: CREATE ... IF NOT EXISTS, CREATE OR REPLACE FUNCTION, DROP
-- TRIGGER IF EXISTS before CREATE TRIGGER, and the backfill as above.
--
-- Rollback (nothing reads the table before the rollup readers land):
-- DROP TRIGGER trg_llm_usage_rollup ON context_llm_log;
-- DROP TABLE context_llm_usage_hourly; DROP FUNCTION llm_usage_rollup_ins(),
-- hist_add(bigint[], bigint[]), llm_usage_hist_one(int),
-- llm_usage_hist_idx(bigint). A binary from before 159 needs none of that:
-- the trigger keeps counting its rows.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_llm_usage_hourly (
    -- dimensions
    bucket_start      TIMESTAMPTZ NOT NULL,
    api_key_id        UUID,
    consumer_id       UUID,
    source            TEXT GENERATED ALWAYS AS
                          (CASE WHEN consumer_id IS NULL THEN 'internal' ELSE 'gateway' END) STORED,
    pipeline          TEXT NOT NULL,
    backend_name      TEXT NOT NULL DEFAULT '',
    model             TEXT NOT NULL DEFAULT '',
    model_alias       TEXT NOT NULL DEFAULT '',
    dispatch_class    TEXT NOT NULL DEFAULT '',
    backend_locality  TEXT NOT NULL DEFAULT '',
    hop               SMALLINT NOT NULL DEFAULT 0,
    -- counters: calls counts every row, failed ones included
    calls             BIGINT NOT NULL DEFAULT 0,
    errors            BIGINT NOT NULL DEFAULT 0,
    unattributed_ok   BIGINT NOT NULL DEFAULT 0,
    estimated_calls   BIGINT NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT NOT NULL DEFAULT 0,
    completion_tokens BIGINT NOT NULL DEFAULT 0,
    duration_ms_sum   BIGINT NOT NULL DEFAULT 0,
    duration_n        BIGINT NOT NULL DEFAULT 0,
    queue_wait_ms_sum BIGINT NOT NULL DEFAULT 0,
    queue_wait_n      BIGINT NOT NULL DEFAULT 0,
    cost_usd          NUMERIC NOT NULL DEFAULT 0,
    -- log2 histograms, bucket contract of design/05 §3.5
    latency_hist      BIGINT[] NOT NULL DEFAULT array_fill(0::bigint, ARRAY[16]),
    queue_wait_hist   BIGINT[] NOT NULL DEFAULT array_fill(0::bigint, ARRAY[16]),
    ttft_hist         BIGINT[] NOT NULL DEFAULT array_fill(0::bigint, ARRAY[16]),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_llm_usage_one_identity CHECK (api_key_id IS NULL OR consumer_id IS NULL)
) WITH (fillfactor = 50,
        autovacuum_vacuum_scale_factor = 0,
        autovacuum_vacuum_threshold = 5000);

CREATE UNIQUE INDEX IF NOT EXISTS uq_llm_usage_hourly
    ON context_llm_usage_hourly (bucket_start, api_key_id, consumer_id, pipeline, backend_name,
                                 model, model_alias, dispatch_class, backend_locality, hop)
    NULLS NOT DISTINCT;
CREATE INDEX IF NOT EXISTS idx_llm_usage_key
    ON context_llm_usage_hourly (api_key_id, bucket_start DESC)
    WHERE api_key_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_llm_usage_consumer
    ON context_llm_usage_hourly (consumer_id, model_alias, bucket_start DESC)
    WHERE consumer_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_llm_usage_bucket
    ON context_llm_usage_hourly (bucket_start DESC);

-- 1-based array index of the histogram bucket for ms, NULL for NULL: the
-- contract's 1 + LEAST(15, GREATEST(0, ceil(log2(max(ms, 1) / 60)))), counted
-- as the number of bounds 60·2^k + 1 (k = 0..14) at or below ms — the same
-- index for every integer ms, by comparison instead of a numeric log (about
-- 11 µs per call, three calls per row, against 0.15 µs).
CREATE OR REPLACE FUNCTION llm_usage_hist_idx(ms BIGINT) RETURNS INT AS $$
    SELECT 1 + width_bucket(ms, ARRAY[61, 121, 241, 481, 961, 1921, 3841, 7681, 15361, 30721,
                                      61441, 122881, 245761, 491521, 983041]::bigint[])
$$ LANGUAGE sql IMMUTABLE PARALLEL SAFE;

-- A histogram with one count in bucket idx; idx NULL gives all zeros.
CREATE OR REPLACE FUNCTION llm_usage_hist_one(idx INT) RETURNS BIGINT[] AS $$
    SELECT array_agg(CASE WHEN g = idx THEN 1::bigint ELSE 0::bigint END ORDER BY g)
      FROM generate_series(1, 16) g
$$ LANGUAGE sql IMMUTABLE PARALLEL SAFE;

-- Element-wise sum of two 16-bucket histograms: bigint[] + bigint[] does not
-- exist (42883), and intarray — not installed — would make + a union. One
-- ARRAY expression rather than unnest/array_agg, because the planner inlines
-- it into the upsert, and the upsert runs under the row lock of the hour's
-- row: the unnest form stays a function call per histogram and roughly
-- doubled the trigger's p99 cost under eight writers on one row.
CREATE OR REPLACE FUNCTION hist_add(a BIGINT[], b BIGINT[]) RETURNS BIGINT[] AS $$
    SELECT ARRAY[COALESCE(a[1], 0) + COALESCE(b[1], 0), COALESCE(a[2], 0) + COALESCE(b[2], 0),
                 COALESCE(a[3], 0) + COALESCE(b[3], 0), COALESCE(a[4], 0) + COALESCE(b[4], 0),
                 COALESCE(a[5], 0) + COALESCE(b[5], 0), COALESCE(a[6], 0) + COALESCE(b[6], 0),
                 COALESCE(a[7], 0) + COALESCE(b[7], 0), COALESCE(a[8], 0) + COALESCE(b[8], 0),
                 COALESCE(a[9], 0) + COALESCE(b[9], 0), COALESCE(a[10], 0) + COALESCE(b[10], 0),
                 COALESCE(a[11], 0) + COALESCE(b[11], 0), COALESCE(a[12], 0) + COALESCE(b[12], 0),
                 COALESCE(a[13], 0) + COALESCE(b[13], 0), COALESCE(a[14], 0) + COALESCE(b[14], 0),
                 COALESCE(a[15], 0) + COALESCE(b[15], 0), COALESCE(a[16], 0) + COALESCE(b[16], 0)]
$$ LANGUAGE sql IMMUTABLE PARALLEL SAFE;

-- Counts one context_llm_log row into its hour and dimension. It must not
-- raise; the guards are described in the header.
CREATE OR REPLACE FUNCTION llm_usage_rollup_ins() RETURNS TRIGGER AS $$
DECLARE
    v_gw    JSONB    := CASE WHEN jsonb_typeof(NEW.metadata->'gw') = 'object'
                             THEN NEW.metadata->'gw' END;
    v_key   UUID     := CASE WHEN NEW.consumer_id IS NULL THEN NEW.api_key_id END;
    v_alias TEXT     := CASE WHEN jsonb_typeof(v_gw->'alias') = 'string'
                             THEN left(v_gw->>'alias', 100) ELSE '' END;
    v_hop   SMALLINT := CASE WHEN jsonb_typeof(v_gw->'hop') = 'number'
                             THEN LEAST(GREATEST(floor((v_gw->>'hop')::numeric), 0), 16)::smallint
                             ELSE 0 END;
    v_est   INT      := CASE WHEN v_gw->>'usage' = 'estimated' THEN 1 ELSE 0 END;
    v_ttft  BIGINT   := CASE WHEN jsonb_typeof(v_gw->'ttft_ms') = 'number'
                             THEN LEAST(GREATEST(floor((v_gw->>'ttft_ms')::numeric), 0), 9e15)::bigint
                             END;
BEGIN
    INSERT INTO context_llm_usage_hourly AS u
        (bucket_start, api_key_id, consumer_id, pipeline, backend_name, model, model_alias,
         dispatch_class, backend_locality, hop,
         calls, errors, unattributed_ok, estimated_calls, prompt_tokens, completion_tokens,
         duration_ms_sum, duration_n, queue_wait_ms_sum, queue_wait_n, cost_usd,
         latency_hist, queue_wait_hist, ttft_hist)
    VALUES (date_trunc('hour', NEW.created_at, 'UTC'), v_key, NEW.consumer_id,
            left(NEW.pipeline, 100), left(COALESCE(NEW.backend_name, NEW.host, ''), 100),
            left(COALESCE(NEW.model, ''), 100), v_alias,
            left(COALESCE(NEW.dispatch_class, ''), 100),
            left(COALESCE(NEW.backend_locality, ''), 100), v_hop,
            1, (NEW.error IS NOT NULL)::int, (NEW.backend_name IS NULL AND NEW.error IS NULL)::int, v_est,
            COALESCE(NEW.prompt_tokens, 0), COALESCE(NEW.completion_tokens, 0),
            COALESCE(NEW.duration_ms, 0), (NEW.duration_ms IS NOT NULL)::int,
            COALESCE(NEW.queue_wait_ms, 0), (NEW.queue_wait_ms IS NOT NULL)::int,
            COALESCE(NEW.cost_usd, 0),
            llm_usage_hist_one(llm_usage_hist_idx(NEW.duration_ms)),
            llm_usage_hist_one(llm_usage_hist_idx(NEW.queue_wait_ms)),
            llm_usage_hist_one(llm_usage_hist_idx(v_ttft)))
    ON CONFLICT (bucket_start, api_key_id, consumer_id, pipeline, backend_name, model, model_alias,
                 dispatch_class, backend_locality, hop)
    DO UPDATE SET calls             = u.calls + 1,
                  errors            = u.errors + EXCLUDED.errors,
                  unattributed_ok   = u.unattributed_ok + EXCLUDED.unattributed_ok,
                  estimated_calls   = u.estimated_calls + EXCLUDED.estimated_calls,
                  prompt_tokens     = u.prompt_tokens + EXCLUDED.prompt_tokens,
                  completion_tokens = u.completion_tokens + EXCLUDED.completion_tokens,
                  duration_ms_sum   = u.duration_ms_sum + EXCLUDED.duration_ms_sum,
                  duration_n        = u.duration_n + EXCLUDED.duration_n,
                  queue_wait_ms_sum = u.queue_wait_ms_sum + EXCLUDED.queue_wait_ms_sum,
                  queue_wait_n      = u.queue_wait_n + EXCLUDED.queue_wait_n,
                  cost_usd          = u.cost_usd + EXCLUDED.cost_usd,
                  latency_hist      = hist_add(u.latency_hist, EXCLUDED.latency_hist),
                  queue_wait_hist   = hist_add(u.queue_wait_hist, EXCLUDED.queue_wait_hist),
                  ttft_hist         = hist_add(u.ttft_hist, EXCLUDED.ttft_hist),
                  updated_at        = now();
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_llm_usage_rollup ON context_llm_log;
CREATE TRIGGER trg_llm_usage_rollup
    AFTER INSERT ON context_llm_log
    FOR EACH ROW EXECUTE FUNCTION llm_usage_rollup_ins();

-- Backfill: every existing row, normalised exactly as the trigger does it,
-- one GROUP BY. The counts overwrite what the table holds for a dimension;
-- the WHERE leaves a row whose counts already hold untouched (no new row
-- version, updated_at unchanged), so a re-run is a no-op.
INSERT INTO context_llm_usage_hourly AS u
    (bucket_start, api_key_id, consumer_id, pipeline, backend_name, model, model_alias,
     dispatch_class, backend_locality, hop,
     calls, errors, unattributed_ok, estimated_calls, prompt_tokens, completion_tokens,
     duration_ms_sum, duration_n, queue_wait_ms_sum, queue_wait_n, cost_usd,
     latency_hist, queue_wait_hist, ttft_hist)
SELECT s.bucket_start, s.api_key_id, s.consumer_id, s.pipeline, s.backend_name, s.model,
       s.model_alias, s.dispatch_class, s.backend_locality, s.hop,
       count(*),
       count(*) FILTER (WHERE s.error IS NOT NULL),
       count(*) FILTER (WHERE s.raw_backend IS NULL AND s.error IS NULL),
       count(*) FILTER (WHERE s.gw->>'usage' = 'estimated'),
       COALESCE(sum(s.prompt_tokens), 0),
       COALESCE(sum(s.completion_tokens), 0),
       COALESCE(sum(s.duration_ms), 0),
       count(s.duration_ms),
       COALESCE(sum(s.queue_wait_ms), 0),
       count(s.queue_wait_ms),
       COALESCE(sum(s.cost_usd), 0),
       ARRAY[count(*) FILTER (WHERE s.lat_i = 1),  count(*) FILTER (WHERE s.lat_i = 2),
             count(*) FILTER (WHERE s.lat_i = 3),  count(*) FILTER (WHERE s.lat_i = 4),
             count(*) FILTER (WHERE s.lat_i = 5),  count(*) FILTER (WHERE s.lat_i = 6),
             count(*) FILTER (WHERE s.lat_i = 7),  count(*) FILTER (WHERE s.lat_i = 8),
             count(*) FILTER (WHERE s.lat_i = 9),  count(*) FILTER (WHERE s.lat_i = 10),
             count(*) FILTER (WHERE s.lat_i = 11), count(*) FILTER (WHERE s.lat_i = 12),
             count(*) FILTER (WHERE s.lat_i = 13), count(*) FILTER (WHERE s.lat_i = 14),
             count(*) FILTER (WHERE s.lat_i = 15), count(*) FILTER (WHERE s.lat_i = 16)],
       ARRAY[count(*) FILTER (WHERE s.qw_i = 1),  count(*) FILTER (WHERE s.qw_i = 2),
             count(*) FILTER (WHERE s.qw_i = 3),  count(*) FILTER (WHERE s.qw_i = 4),
             count(*) FILTER (WHERE s.qw_i = 5),  count(*) FILTER (WHERE s.qw_i = 6),
             count(*) FILTER (WHERE s.qw_i = 7),  count(*) FILTER (WHERE s.qw_i = 8),
             count(*) FILTER (WHERE s.qw_i = 9),  count(*) FILTER (WHERE s.qw_i = 10),
             count(*) FILTER (WHERE s.qw_i = 11), count(*) FILTER (WHERE s.qw_i = 12),
             count(*) FILTER (WHERE s.qw_i = 13), count(*) FILTER (WHERE s.qw_i = 14),
             count(*) FILTER (WHERE s.qw_i = 15), count(*) FILTER (WHERE s.qw_i = 16)],
       ARRAY[count(*) FILTER (WHERE s.ttft_i = 1),  count(*) FILTER (WHERE s.ttft_i = 2),
             count(*) FILTER (WHERE s.ttft_i = 3),  count(*) FILTER (WHERE s.ttft_i = 4),
             count(*) FILTER (WHERE s.ttft_i = 5),  count(*) FILTER (WHERE s.ttft_i = 6),
             count(*) FILTER (WHERE s.ttft_i = 7),  count(*) FILTER (WHERE s.ttft_i = 8),
             count(*) FILTER (WHERE s.ttft_i = 9),  count(*) FILTER (WHERE s.ttft_i = 10),
             count(*) FILTER (WHERE s.ttft_i = 11), count(*) FILTER (WHERE s.ttft_i = 12),
             count(*) FILTER (WHERE s.ttft_i = 13), count(*) FILTER (WHERE s.ttft_i = 14),
             count(*) FILTER (WHERE s.ttft_i = 15), count(*) FILTER (WHERE s.ttft_i = 16)]
  FROM (SELECT date_trunc('hour', l.created_at, 'UTC') AS bucket_start,
               CASE WHEN l.consumer_id IS NULL THEN l.api_key_id END AS api_key_id,
               l.consumer_id,
               left(l.pipeline, 100) AS pipeline,
               left(COALESCE(l.backend_name, l.host, ''), 100) AS backend_name,
               l.backend_name AS raw_backend,
               left(COALESCE(l.model, ''), 100) AS model,
               CASE WHEN jsonb_typeof(g.gw->'alias') = 'string'
                    THEN left(g.gw->>'alias', 100) ELSE '' END AS model_alias,
               left(COALESCE(l.dispatch_class, ''), 100) AS dispatch_class,
               left(COALESCE(l.backend_locality, ''), 100) AS backend_locality,
               CASE WHEN jsonb_typeof(g.gw->'hop') = 'number'
                    THEN LEAST(GREATEST(floor((g.gw->>'hop')::numeric), 0), 16)::smallint
                    ELSE 0 END AS hop,
               g.gw,
               l.error, l.prompt_tokens, l.completion_tokens, l.duration_ms, l.queue_wait_ms,
               l.cost_usd,
               llm_usage_hist_idx(l.duration_ms) AS lat_i,
               llm_usage_hist_idx(l.queue_wait_ms) AS qw_i,
               llm_usage_hist_idx(
                   CASE WHEN jsonb_typeof(g.gw->'ttft_ms') = 'number'
                        THEN LEAST(GREATEST(floor((g.gw->>'ttft_ms')::numeric), 0), 9e15)::bigint
                        END) AS ttft_i
          FROM context_llm_log l
         CROSS JOIN LATERAL (SELECT CASE WHEN jsonb_typeof(l.metadata->'gw') = 'object'
                                         THEN l.metadata->'gw' END AS gw) g) s
 GROUP BY s.bucket_start, s.api_key_id, s.consumer_id, s.pipeline, s.backend_name, s.model,
          s.model_alias, s.dispatch_class, s.backend_locality, s.hop
ON CONFLICT (bucket_start, api_key_id, consumer_id, pipeline, backend_name, model, model_alias,
             dispatch_class, backend_locality, hop)
DO UPDATE SET calls             = EXCLUDED.calls,
              errors            = EXCLUDED.errors,
              unattributed_ok   = EXCLUDED.unattributed_ok,
              estimated_calls   = EXCLUDED.estimated_calls,
              prompt_tokens     = EXCLUDED.prompt_tokens,
              completion_tokens = EXCLUDED.completion_tokens,
              duration_ms_sum   = EXCLUDED.duration_ms_sum,
              duration_n        = EXCLUDED.duration_n,
              queue_wait_ms_sum = EXCLUDED.queue_wait_ms_sum,
              queue_wait_n      = EXCLUDED.queue_wait_n,
              cost_usd          = EXCLUDED.cost_usd,
              latency_hist      = EXCLUDED.latency_hist,
              queue_wait_hist   = EXCLUDED.queue_wait_hist,
              ttft_hist         = EXCLUDED.ttft_hist,
              updated_at        = now()
        WHERE (u.calls, u.errors, u.unattributed_ok, u.estimated_calls, u.prompt_tokens,
               u.completion_tokens, u.duration_ms_sum, u.duration_n, u.queue_wait_ms_sum,
               u.queue_wait_n, u.cost_usd, u.latency_hist, u.queue_wait_hist, u.ttft_hist)
              IS DISTINCT FROM
              (EXCLUDED.calls, EXCLUDED.errors, EXCLUDED.unattributed_ok, EXCLUDED.estimated_calls,
               EXCLUDED.prompt_tokens, EXCLUDED.completion_tokens, EXCLUDED.duration_ms_sum,
               EXCLUDED.duration_n, EXCLUDED.queue_wait_ms_sum, EXCLUDED.queue_wait_n,
               EXCLUDED.cost_usd, EXCLUDED.latency_hist, EXCLUDED.queue_wait_hist,
               EXCLUDED.ttft_hist);
