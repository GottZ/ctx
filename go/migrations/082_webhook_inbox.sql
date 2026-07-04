-- =============================================================================
-- 082_webhook_inbox.sql — inbound forge event queue (workflow W13)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- One row = one inbound webhook delivery. The table is a DURABLE, DEBOUNCE-able
-- TRIGGER queue, NOT an authority store: the scheduler inbox arm drains pending
-- rows and fires a forge SyncManager pull per project — the payload is never
-- upserted into a block (design/03-workflow-api-cli.md §5.3: "Events sind Sync-
-- TRIGGER, nie Autoritätsquelle"; the 3-way content hash against the forge IST-
-- state lives in the Achse-02 translator). The audit of the resulting block
-- writes lives in context_write_log, not here — this table is a through-queue.
--
-- Design: design/03 §3.4 (provisional M075, FINAL number 082 per masterplan K1 —
-- 081 project-notify landed at W9, 082 was reserved for W13). Schema/route-shape/
-- security model were fixed at W4 (§3.4/§5.3/§5.6) so no earlier wave rewrites it.
--
-- Redelivery-idempotency (NOT replay-protection, §5.3): UNIQUE(project_id,
-- delivery_id) + INSERT … ON CONFLICT DO NOTHING makes GitHub's own redeliveries
-- (identical X-GitHub-Delivery GUID) collapse to exactly ONE row. The GUID is an
-- UNSIGNED header, so this is no defense against an active replayer — that
-- mitigation is the translator's 3-way hash / updated_at-cursor discard.
--
-- FK ON DELETE CASCADE off context_projects: a project-delete (or the K14 tenant
-- prune, which deletes context_projects rows) drains the inbox for free. The
-- per-project WEBHOOK SECRET is a separate context_secrets row and is drained by
-- store.DeleteProject (project delete) and store.PruneTenant (tenant prune) —
-- neither cascades from this table.
--
-- lock_timeout (R-MIG2): CREATE TABLE / CREATE INDEX on an empty new table take
-- only brief catalog locks. Forward-only, additive: no backfill (new surface).
CREATE TABLE IF NOT EXISTS context_webhook_events (
    id           UUID PRIMARY KEY DEFAULT uuidv7(),
    project_id   UUID NOT NULL REFERENCES context_projects(id) ON DELETE CASCADE,
    delivery_id  TEXT NOT NULL,                    -- X-GitHub-Delivery (GUID, unsigned header)
    event        TEXT NOT NULL,                    -- X-GitHub-Event ('issues','issue_comment',…)
    payload      JSONB NOT NULL,
    received_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at TIMESTAMPTZ,
    status       TEXT NOT NULL DEFAULT 'pending',  -- pending|done|error|skipped
    error        TEXT,
    CONSTRAINT uq_webhook_delivery UNIQUE (project_id, delivery_id)  -- redelivery-idempotent (§5.3)
);

-- Queue predicate: the scheduler inbox arm picks processed_at IS NULL with
-- FOR UPDATE SKIP LOCKED (the embed-backfill pattern). Partial index so the scan
-- never walks the processed history.
CREATE INDEX IF NOT EXISTS idx_webhook_pending
    ON context_webhook_events (received_at) WHERE processed_at IS NULL;

-- Counting window for webhook.rate_limit (§4.4/§5.3): the inbound path is
-- UNAUTHENTICATED (no api_key_id), so the I6 write-throttle mechanic is
-- structurally unusable. count(*) WHERE project_id=$1 AND received_at > now()-'60s'
-- rides this index. Doubles as per-project diagnosis (recent deliveries first).
CREATE INDEX IF NOT EXISTS idx_webhook_project_recent
    ON context_webhook_events (project_id, received_at DESC);

-- Retention-eviction path: the Janitor arm evicts received_at < now()-interval
-- AND processed_at IS NOT NULL (Config webhook.retention, default 14d). This
-- partial index is the exact COMPLEMENT of idx_webhook_pending so the eviction
-- DELETE is index-driven, never a Seq-Scan over 120/min × 14d × N-project rows.
CREATE INDEX IF NOT EXISTS idx_webhook_done
    ON context_webhook_events (received_at) WHERE processed_at IS NOT NULL;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (82, '082_webhook_inbox.sql', now())
  ON CONFLICT (version) DO NOTHING;
