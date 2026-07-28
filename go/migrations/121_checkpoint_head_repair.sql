-- =============================================================================
-- 121_checkpoint_head_repair.sql — repair of the existing checkpoint-head corpus
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- M120 (H-W13) taught the classify registry the SECOND checkpoint title shape,
-- "Compaction checkpoint head <session>". A classify rule only ever governs
-- FUTURE writes: the heads already on disk are untouched by it. They sit in
-- category='compaction-checkpoints' auto-typed 'knowledge' — which means
-- guard.check=true — and the guard's archive lane (0.98/0.92) has hit a large
-- share of them, because consecutive session heads are near-duplicates BY
-- CONSTRUCTION (manifest = boilerplate + ID list). Archived rows are invisible
-- to every NOT is_archived read path, so the ID chains anchored on those heads
-- dangle exactly the way the 2026-07-20 incident (learnings block
-- 019f7c7a-e6b0-7271-9717-358b460fbd27) described for the "Compaction source"
-- parts. M107 repaired that corpus; this migration repairs the heads.
--
-- Statement order is NOT free: (a) retypes, (b) un-archives, and (b) predicates
-- on the type (a) writes. Swapping them makes (b) a no-op on the whole corpus.
--
-- (b) carries THREE guards, each of which is load-bearing on its own:
--   1. rn=1 over (category,title,scope) — the partial unique index
--      (005_scope_unique.sql: UNIQUE (category,title,scope) WHERE NOT
--      is_archived) permits exactly one live row per slot; un-archiving two
--      archived title twins in one statement raises 23505 and aborts the
--      migration — in prod that is a boot abort.
--   2. NOT EXISTS against a live slot holder — same index, other direction:
--      never un-archive into a slot somebody already occupies.
--   3. type_name='checkpoint' — this one is not about the index, it makes the
--      guard-freedom CONSTRUCTIVE. Only the checkpoint type carries
--      guard.check=false (M107 seed). A head whose type_source='manual' keeps
--      its operator-asserted type (statement (a) never overrides a manual
--      assertion, T4 semantics), stays 'knowledge', stays guard-checked — and
--      un-archiving it would merely hand it back to the archive lane on the
--      next guard run. It stays archived on purpose.
--
-- guard_status='active', not M107's 'needs_review': M107 un-archived rows whose
-- guard freedom was not yet structural, so a human still had to look. Here the
-- verdict is already in — the rows leave this migration typed 'checkpoint',
-- which takes them out of the guard lane permanently. Feeding them into the
-- review queue would add ~100 entries a human can only ever confirm. The audit
-- trail rides on metadata.guard_repair='M121' instead: `SELECT ... WHERE
-- metadata ? 'guard_repair'` reconstructs the touched set at any later date.
-- Guard evidence (guard_matched_id, guard_similarity, guard_checked_at) is left
-- intact for the same reason M107 left it intact.
--
-- NOT rollbackable — this is a data repair, not a schema change; the pre-state
-- is not derivable from the post-state. Deploy runbook, MANDATORY before the
-- live run:
--
--   \copy (SELECT id, type_name, type_source, is_archived, guard_status
--            FROM context_blocks
--           WHERE category='compaction-checkpoints') TO 'm121-before.csv' CSV HEADER
--
-- Idempotent: after the first pass (a) finds no auto-typed non-checkpoint head
-- left and (b) finds no archived_dup head that is not already blocked by its
-- own now-live twin. A second run matches zero rows.
--
-- Tx-Hinweis (Konvention 058/059/061/091/116/119/120): der Runner
-- (store/migrations.go) wickelt jede Migration in eine eigene Transaktion,
-- SET LOCAL ist damit selbst-revertierend.
--
-- Forward-only. Kein Schema-Objekt → test.sh table count UNCHANGED.
-- =============================================================================

SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '60s';   -- die Statement-Dauer, nicht nur der Lock-Erwerb

-- (a) Retype: nur auto-typisierte Zeilen.
UPDATE context_blocks
   SET type_name = 'checkpoint'
 WHERE category = 'compaction-checkpoints'
   AND type_source = 'auto'
   AND type_name <> 'checkpoint'
   AND lower(title) LIKE 'compaction checkpoint head%';

-- (b) Un-Archivierung mit DREI Guards:
--     1. rn=1 über (category,title,scope) — gegen den partiellen Unique-Index
--     2. NOT EXISTS gegen einen lebenden Slot-Inhaber
--     3. type_name='checkpoint' — macht die Guard-Freiheit KONSTRUKTIV
WITH ranked AS (
  SELECT id, row_number() OVER (PARTITION BY category, title, scope ORDER BY id DESC) AS rn
    FROM context_blocks
   WHERE category = 'compaction-checkpoints'
     AND is_archived
     AND guard_status = 'archived_dup'
     AND type_name = 'checkpoint'
     AND lower(title) LIKE 'compaction checkpoint head%'
)
UPDATE context_blocks b
   SET is_archived  = false,
       guard_status = 'active',
       metadata     = b.metadata || jsonb_build_object('guard_repair','M121'),
       updated_at   = now()
  FROM ranked r
 WHERE b.id = r.id AND r.rn = 1
   AND NOT EXISTS (SELECT 1 FROM context_blocks live
                    WHERE live.category = b.category AND live.title = b.title
                      AND live.scope = b.scope AND NOT live.is_archived);
