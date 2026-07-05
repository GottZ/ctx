-- =============================================================================
-- 089_pending_writes.sql — F6-C6 write-confirmation staging store (D-W1)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- One row = one STAGED write (MCP/Chat LLM path). The LLM client stages a
-- store/update, the server holds the authoritative payload, and only a confirm
-- call that atomically consumes the row executes the write. REST/CLI stay
-- direct (masterplan D-E1: gating is a per-principal distrust tool for LLM
-- harnesses, not a second layer under human paths).
--
-- TimescaleDB hypertable (masterplan D-E4, decision board 2026-07-05): eviction
-- is chunk-drop (D-W3 ticker), never row-DELETE — no dead-tuple bloat, no
-- Seq-Scan at 1M+ scale. Repo precedent: context_llm_log (025, 7-day chunks).
-- Chunk interval here is 1 HOUR: confirm_ttl is minute-scale (default 10m) and
-- confirm_retention hour-scale (default 24h), so 1h chunks keep the drop
-- granularity well below the retention window (24 chunks per day at rest —
-- trivial catalog load at the measured stage rate of well under 1 write/min,
-- D-W0 2026-07-05: 644 external writes in 30 days, peak minute 7 incl. the
-- non-gated dream pipeline).
--
-- TWO decoupled knobs (masterplan D-E3, fixes D2-C1's double-break):
--   writes.confirm_ttl       — expiry clock; expires_at = now()+ttl at stage
--                              time. ttl=0 ⇒ expires_at IS NULL (stage never
--                              expires; 0-is-off convention like llmlog/
--                              webchat/webhook retention). NOT wired to
--                              eviction.
--   writes.confirm_retention — D-W3 chunk-drop window (created_at-based).
--                              0 = keep forever. Independent of the expiry
--                              clock, so ttl=0 is NOT feature-death and
--                              retention=0 is NOT expiry-death.
--
-- UNIQUE constraint note (hypertable restriction): the draft's partial unique
-- index (api_key_id, payload_hash) WHERE consumed_at IS NULL cannot exist on a
-- hypertable — unique indexes must include the partitioning column, which
-- would defeat the dedup purpose. Stage idempotency is therefore APP-SIDE: a
-- re-arm CTE updates the open row's expiry, inserting only when no open row
-- matched (store.StagePendingWrite). A concurrent duplicate race leaves two
-- open rows with the SAME hash = the SAME server-held payload; consume picks
-- exactly one deterministically (newest) and a double-consume lands in the
-- idempotent upsert — accepted per rejected finding D1-m1.
--
-- FK ON DELETE CASCADE off context_api_keys (hypertable→plain FK is supported;
-- the reverse is not): deleting a key drains its stages — a deleted principal
-- must not be able to confirm anything.
--
-- lock_timeout (R-MIG2): CREATE TABLE / create_hypertable / CREATE INDEX on an
-- empty new table take only brief catalog locks. Forward-only, additive.
SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_pending_writes (
    id           UUID NOT NULL DEFAULT uuidv7(),
    api_key_id   UUID NOT NULL REFERENCES context_api_keys(id) ON DELETE CASCADE,
    scope        TEXT NOT NULL,                    -- write scope bound at stage time (fail-closed)
    op           TEXT NOT NULL,                    -- 'store' | 'update'
    origin       TEXT NOT NULL,                    -- 'mcp' | 'chat' (diagnosis)
    payload      JSONB NOT NULL,                   -- server-held, authoritative (tamper-proof: confirm carries only the hash)
    payload_hash TEXT NOT NULL,                    -- sha256(canonical(payload)) — canonicalization lands in D-W2
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ,                      -- NULL = never expires (confirm_ttl=0)
    consumed_at  TIMESTAMPTZ,                      -- NULL = open; set = consumed exactly once
    PRIMARY KEY (id, created_at)
);

-- if_not_exists=true lets the migration replay on a partially-applied DB (025).
SELECT create_hypertable(
    'context_pending_writes',
    'created_at',
    chunk_time_interval => interval '1 hour',
    if_not_exists => true
);

-- Consume/lookup selector: newest OPEN row per (key, hash). Partial index —
-- consumed history never widens the scan. (Partial non-unique indexes are fine
-- on hypertables; only UNIQUE needs the time column.)
CREATE INDEX IF NOT EXISTS idx_pending_open
    ON context_pending_writes (api_key_id, payload_hash, created_at DESC)
    WHERE consumed_at IS NULL;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (89, '089_pending_writes.sql', now())
  ON CONFLICT (version) DO NOTHING;
