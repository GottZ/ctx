-- =============================================================================
-- 169_chunk_lifecycle_repair.sql — chunk rows that left the 'chunk' lifecycle
-- go back to it, so every chunk slot takes an ingest again (issue #191)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- A chunk row (source_id + chunk_index, written by store.InsertChunk) belongs
-- to the ingest. InsertChunk finds its (source_id, chunk_index) slot only
-- through the arbiter
--   ON CONFLICT (source_id, chunk_index) WHERE NOT is_archived AND lifecycle_state = 'chunk'
-- (uq_source_chunk). Older binaries moved chunk rows out of that lifecycle:
--   - the dream supersedes side-effect retired a chunk target as 'snapshot'
--     with superseded_by set (store.ReconcileSupersedesTargets);
--   - its reverse, and migration 154, handed such a row back as 'knowledge'
--     (which made it a dream pick, so a promotion to 'canonical' followed);
--   - the daily report stamped 'synthesis' on whatever row held its
--     (category, title, scope) — a chunk with that title included.
-- Such a row is outside the arbiter. A changed-content ingest of its slot
-- misses it, tries a fresh insert and fails on uq_context_category_title_scope
-- (23505, "insert chunk failed"); with a changed title the insert lands as a
-- second row of the slot and the old row stays live next to it — and the
-- next ingest that brings the old title back fails on it. The fixed binary
-- no longer moves a chunk out of its lifecycle; this repairs the rows that
-- were moved before.
--
-- Which rows: live rows with a slot (source_id AND chunk_index set) whose
-- lifecycle_state is not 'chunk' ("moved rows"). A row whose source was
-- deleted has source_id NULL (ON DELETE SET NULL), owns no slot and is left
-- alone; archived rows are outside both unique indexes and are left alone
-- too. A slot with a live row outside its source's scope (a manage or MCP
-- update can move any block's scope) is left exactly as it is: the ingest
-- only writes rows in the source's scope (InsertChunk's scope binding), and
-- this repair does not decide between scopes. The second NOTICE counts them.
--
-- What it does, per remaining slot — the slot keeps ONE live row, the one
-- the ingest would be writing into now:
--   (a) archive: every moved row that is not that row. A live 'chunk' row
--       of the slot wins whatever its age (it is the arbiter's row and is not
--       touched — and it can be the older one: migration 156 un-archived
--       chunks into slots whose newer row had already left the lifecycle);
--       otherwise the newest moved row (created_at, then id) does. The
--       losers are older versions of the slot that the ingest would have
--       overwritten in place; they go to is_archived with lifecycle 'chunk',
--       which frees their titles for the slot's live row.
--   (b) restore: the moved rows left are alone in their slot →
--       lifecycle_state = 'chunk', superseded_by = NULL.
-- The supersedes LINKS stay: the query path reads them (dream.SupersedesMap),
-- not the lifecycle or the pointer column.
--
-- Audit: every touched row carries metadata.chunk_lifecycle_repair =
-- {"migration": 169, "lifecycle_before": …, "superseded_by_before": …,
--  "archived": true|false} — the only machine-readable way back:
--   SELECT id, is_archived, metadata->'chunk_lifecycle_repair' FROM context_blocks
--    WHERE metadata ? 'chunk_lifecycle_repair';
-- (A restored live chunk loses the key on its next changed ingest:
-- InsertChunk replaces a chunk's metadata.) updated_at is not bumped, as in
-- 154: neither statement changes content.
--
-- Cost: ONE pass over the rows with a slot collects the moved rows into a
-- temporary table; everything after it works on those few rows and their
-- slots. The pass can use idx_source_id with the index condition
-- `source_id IS NOT NULL` (measured: the planner does at 30 000 chunk rows in
-- 90 000, under 20 ms); where chunk rows dominate the table the planner may scan
-- the table once instead. No statement_timeout: a pass that hit one would
-- roll the boot back on every start.
--
-- Concurrent writers: the boot that runs this is not necessarily the only
-- process on the database — another ctxd on it (a replica not upgraded yet,
-- a rolling deploy) keeps ingesting. InsertChunk's arbiter only sees live
-- 'chunk' rows, so an ingest into the slot of a moved row does not wait for
-- this transaction: a changed title inserts a fresh live chunk. If that
-- commits after the moved rows were collected, the restore in (b) meets it on
-- uq_source_chunk (an insert still open when (b) reaches the row makes (b)
-- wait for it and then meet it the same way). The fresh row is the slot's
-- current version, so (b) archives the moved row instead — per row, in a
-- subtransaction, so the boot never fails on a concurrent ingest. (a) cannot
-- meet one: it only takes rows out of the live set. Not done instead: LOCK
-- TABLE context_blocks IN SHARE ROW EXCLUSIVE MODE, which conflicts with every
-- writer of every other process for the whole migration and, under the 3 s
-- lock_timeout, fails the boot whenever any writer transaction (a guard batch
-- of 100 blocks, a backfill around its embed call) holds its lock longer.
-- The lock_timeout itself stays the house rule: a moved row that another
-- transaction holds locked for more than 3 s fails the boot, which the next
-- start retries.
--
-- Idempotent: after one pass the only moved rows left are those in slots
-- with a row outside the source's scope, and those are skipped again. The
-- file can be run again by hand after an older binary has run against the
-- database — in ONE transaction, or SET LOCAL lock_timeout is ignored:
--   psql -v ON_ERROR_STOP=1 --single-transaction -f 169_chunk_lifecycle_repair.sql
-- Apart from the concurrent ingest above, no statement can hit a unique
-- index: (a) only takes rows out of the live set, and (b) runs on slots that
-- (a) left with exactly one live row, without changing title or scope.
--
-- Tx: the runner (store/migrations.go) wraps every migration in its own
-- transaction, so SET LOCAL and the ON COMMIT DROP tables end with it.
-- Forward-only. No schema object → test.sh table/column counts unchanged.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

DO $$
DECLARE
    v_archived INT;
    v_restored INT := 0;
    v_skipped  INT;
    v_n        INT;
    v_id       UUID;
BEGIN
    -- The one pass: every moved row.
    CREATE TEMP TABLE m169_moved ON COMMIT DROP AS
    SELECT id, source_id, chunk_index
      FROM context_blocks
     WHERE source_id IS NOT NULL
       AND chunk_index IS NOT NULL
       AND NOT is_archived
       AND lifecycle_state <> 'chunk';

    -- The slots to repair: every live row of the slot is in its source's scope.
    CREATE TEMP TABLE m169_slots ON COMMIT DROP AS
    SELECT DISTINCT m.source_id, m.chunk_index
      FROM m169_moved m
      JOIN context_sources s ON s.id = m.source_id
     WHERE NOT EXISTS (
             SELECT 1
               FROM context_blocks x
              WHERE x.source_id = m.source_id
                AND x.chunk_index = m.chunk_index
                AND NOT x.is_archived
                AND x.scope <> s.scope);

    -- (a) archive every moved row its slot does not keep.
    WITH ranked AS (
        SELECT b.id,
               row_number() OVER (PARTITION BY b.source_id, b.chunk_index
                                  ORDER BY (b.lifecycle_state = 'chunk') DESC,
                                           b.created_at DESC NULLS LAST, b.id DESC) AS slot_rank
          FROM context_blocks b
          JOIN m169_slots s ON s.source_id = b.source_id AND s.chunk_index = b.chunk_index
         WHERE NOT b.is_archived
    )
    UPDATE context_blocks b
       SET is_archived     = true,
           lifecycle_state = 'chunk',
           superseded_by   = NULL,
           metadata        = COALESCE(b.metadata, '{}'::jsonb)
                             || jsonb_build_object('chunk_lifecycle_repair',
                                    jsonb_build_object('migration', 169,
                                                       'lifecycle_before', b.lifecycle_state,
                                                       'superseded_by_before', b.superseded_by::text,
                                                       'archived', true))
      FROM ranked r
     WHERE b.id = r.id
       AND r.slot_rank > 1
       -- a live 'chunk' row always ranks first; this keeps it out even so
       AND b.lifecycle_state <> 'chunk';
    GET DIAGNOSTICS v_archived = ROW_COUNT;

    -- (b) restore the moved rows of those slots that are still live: each is
    --     now alone in its slot — unless a concurrent ingest has put a fresh
    --     live chunk into it since the moved rows were collected (see the
    --     header): the restore then meets that row on uq_source_chunk, and the
    --     moved row is archived instead. Row by row, so that one such slot
    --     rolls back only its own restore.
    FOR v_id IN
        SELECT m.id
          FROM m169_moved m
          JOIN m169_slots s ON s.source_id = m.source_id AND s.chunk_index = m.chunk_index
         ORDER BY m.id
    LOOP
        BEGIN
            UPDATE context_blocks b
               SET lifecycle_state = 'chunk',
                   superseded_by   = NULL,
                   metadata        = COALESCE(b.metadata, '{}'::jsonb)
                                     || jsonb_build_object('chunk_lifecycle_repair',
                                            jsonb_build_object('migration', 169,
                                                               'lifecycle_before', b.lifecycle_state,
                                                               'superseded_by_before', b.superseded_by::text,
                                                               'archived', false))
             WHERE b.id = v_id
               AND NOT b.is_archived
               AND b.lifecycle_state <> 'chunk';
            GET DIAGNOSTICS v_n = ROW_COUNT;
            v_restored := v_restored + v_n;
        EXCEPTION WHEN unique_violation THEN
            UPDATE context_blocks b
               SET is_archived     = true,
                   lifecycle_state = 'chunk',
                   superseded_by   = NULL,
                   metadata        = COALESCE(b.metadata, '{}'::jsonb)
                                     || jsonb_build_object('chunk_lifecycle_repair',
                                            jsonb_build_object('migration', 169,
                                                               'lifecycle_before', b.lifecycle_state,
                                                               'superseded_by_before', b.superseded_by::text,
                                                               'archived', true))
             WHERE b.id = v_id
               AND NOT b.is_archived;
            GET DIAGNOSTICS v_n = ROW_COUNT;
            v_archived := v_archived + v_n;
        END;
    END LOOP;

    SELECT count(*) INTO v_skipped
      FROM m169_moved m
     WHERE NOT EXISTS (SELECT 1 FROM m169_slots s
                        WHERE s.source_id = m.source_id AND s.chunk_index = m.chunk_index);

    IF v_archived + v_restored > 0 THEN
        RAISE NOTICE 'migration 169: chunk lifecycle repair — % chunk row(s) returned to their slot, % older row(s) of a slot archived.', v_restored, v_archived;
        RAISE NOTICE 'migration 169: the previous state survives in metadata — SELECT id, is_archived, metadata->''chunk_lifecycle_repair'' FROM context_blocks WHERE metadata ? ''chunk_lifecycle_repair'';';
    END IF;
    IF v_skipped > 0 THEN
        RAISE NOTICE 'migration 169: % row(s) outside the chunk lifecycle left as they are — their slot holds a row outside its source''s scope (docs/operations.md, "Migration 169").', v_skipped;
    END IF;
END $$;
