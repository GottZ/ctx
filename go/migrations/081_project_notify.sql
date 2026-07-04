-- =============================================================================
-- 081_project_notify.sql — project-scoped NOTIFY for the SSE domain-event hub (W9)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Workflow-Achse 03, Welle W9 (design/03-workflow-api-cli.md §4.5/§6.2, §7-W9).
-- Feeds the projectHub → GET /api/project/events per-scope SSE fanout. K1 pins
-- this to migration 081 (the design's provisional "073" is stale; 082=W13).
--
-- ── VEHICLE DECISION (T6-Befund: NOTIFY O(n²) at bulk-Tx) ─────────────────────
-- The design sketch (§6.2) reuses the existing ctx_block_write NOTIFY (004/051,
-- row-level AFTER INSERT/UPDATE, payload {id,op}) and coalesces at the hub. Two
-- reasons this migration builds a DEDICATED channel + dedicated triggers instead
-- of extending the guard/digest trigger:
--
--   1. T6-Befund — Postgres' PreCommit_Notify de-dups the pending-notify list in
--      O(n²) of the notifies queued IN ONE TRANSACTION. The forge Pull-APPLY path
--      commits PER ROW (forge/apply.go: pullCreate/pullUpdate each open their own
--      tx), so a 10k-import is 10k single-notify txs → NO O(n²). But PruneTenant
--      (store/tenant.go) mass-DELETEs context_blocks in 2000-row BATCHES via one
--      pool.Exec each = one implicit tx of 2000 row-notifies → O(2000²) per batch.
--      A naive row-level DELETE notify would light exactly that storm. So DELETE
--      here is STATEMENT-LEVEL with a transition table: ONE trigger fire per
--      statement, coalesced to O(distinct scopes) notifies — the batch storm is
--      structurally impossible.
--   2. Blast radius — extending notify_block_write() would fire the project
--      channel for the ENTIRE knowledge corpus (every block write) and couple the
--      guard/digest listener to the hub. A dedicated channel with a WHEN
--      type-filter fires ONLY for issue/comment rows, and leaves the guard/digest
--      trigger byte-for-byte untouched (zero regression surface).
--
-- INSERT/UPDATE stay ROW-LEVEL: they carry the block id for the id-level frame
-- ({project_id, block_ids, kind, op}, §4.5), and no bulk single-tx issue/comment
-- INSERT/UPDATE path exists today (apply is per-row-tx; API writes are single).
-- If a future wave adds a bulk single-tx issue write, revisit (per-scope
-- statement-level, or per-batch commit) — documented scope boundary (W21).
--
-- ── LISTENER-DISCARD (old binary against the new trigger) ─────────────────────
-- A NOTIFY with no LISTENer is a no-op in Postgres. An old binary does not LISTEN
-- ctx_project_write, so every notify this migration fires is silently discarded —
-- the old binary runs byte-for-byte unchanged (the pausability/rollback
-- invariant, 078 line). The new binary's projectHub is the only consumer.
--
-- ── FRAME PAYLOAD (ids-only, never content — K16) ─────────────────────────────
-- Payload = {id, op, scope, type} (INSERT/UPDATE) or {op:'DELETE', scope, type,
-- bulk:true} (statement-level DELETE). NEVER title/content/body — the hub fans
-- out ids only and the client refetches over the read API, so there is no
-- content-leak path through the stream. NOTIFY payload budget (8000 bytes) is
-- undercut by orders of magnitude (one UUID + three short strings).
--
-- ── LOCKS / IDEMPOTENCY (R-MIG2, 065 pattern) ─────────────────────────────────
-- Function-only + trigger create: brief catalog locks, no table/column change.
-- CREATE OR REPLACE FUNCTION + DROP TRIGGER IF EXISTS + CREATE TRIGGER re-run
-- cleanly; _migrations INSERT ON CONFLICT (version) DO NOTHING. Forward-only, no
-- backfill (no data touched). test.sh T07 table count UNCHANGED (no new table).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- Row-level INSERT/UPDATE: one notify per issue/comment write, carrying the block
-- id (id-level frame). The WHEN clause keeps the whole non-workflow corpus from
-- firing this at all. op='UPDATE' also covers archive-"deletes" (is_archived
-- UPDATE): the client refetches and sees is_archived (§4.5, I17).
CREATE OR REPLACE FUNCTION notify_project_write() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('ctx_project_write', json_build_object(
        'id',    NEW.id,
        'op',    TG_OP,
        'scope', NEW.scope,
        'type',  NEW.type_name)::text);
    RETURN NULL; -- AFTER trigger: return value ignored
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_project_write ON context_blocks;
CREATE TRIGGER trg_project_write
    AFTER INSERT OR UPDATE ON context_blocks
    FOR EACH ROW
    WHEN (NEW.type_name IN ('issue', 'comment'))
    EXECUTE FUNCTION notify_project_write();

-- Statement-level DELETE (physical prune, §3.2): ONE fire per DELETE statement,
-- transition table aggregated to DISTINCT scope — O(distinct scopes) notifies per
-- prune batch instead of O(rows) (T6-Befund defused). bulk:true tells the hub to
-- emit a refetch/removal frame (no ids: a prune drops whole ranges). A prune of a
-- tenant's scopes usually reaches no live subscriber (the tenant's keys are gone
-- → re-auth ends their streams), so this exists mainly to keep the batch from
-- lighting the O(n²) storm, not for a feature.
CREATE OR REPLACE FUNCTION notify_project_delete() RETURNS trigger AS $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN
        SELECT DISTINCT scope, type_name
          FROM oldrows
         WHERE type_name IN ('issue', 'comment')
    LOOP
        PERFORM pg_notify('ctx_project_write', json_build_object(
            'op',    'DELETE',
            'scope', r.scope,
            'type',  r.type_name,
            'bulk',  true)::text);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_project_delete ON context_blocks;
CREATE TRIGGER trg_project_delete
    AFTER DELETE ON context_blocks
    REFERENCING OLD TABLE AS oldrows
    FOR EACH STATEMENT
    EXECUTE FUNCTION notify_project_delete();

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (81, '081_project_notify.sql', now())
  ON CONFLICT (version) DO NOTHING;
