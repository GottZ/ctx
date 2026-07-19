-- =============================================================================
-- 107_checkpoint_type_seed.sql — checkpoint block-type seed + evidence repair
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Structural anchor for ID-referenced evidence chains (compaction-checkpoint
-- manifests + transcript source parts). Root cause 2026-07-20 (learnings block
-- 019f7c7a-e6b0-7271-9717-358b460fbd27): the writer sets no type, the blocks
-- ran auto-typed "knowledge" through the DEFAULT guard lane (archive mode,
-- 0.98/0.92) — and consecutive checkpoints of one session are near-duplicates
-- BY CONSTRUCTION (manifest = boilerplate + IDs; parts overlap in the
-- transcript window). The guard auto-archived manifests/parts, and since every
-- read path filters NOT is_archived, ID reference chains broke silently
-- (dangling manifest pointer in an active compaction summary).
--
-- The checkpoint type keeps evidence blocks out of EVERY autonomous pipeline:
--   * retrieval=excluded — resolution runs exclusively over exact block IDs
--     (manifest carries source_block_ids + parent_manifest chain); in
--     retrieval the token-dense transcript parts flood candidate sets and
--     overflow the reranker slot window (1024-token slots, hex-dense prefixes
--     tokenize at ~2.1 bytes/token — the 2026-07-15 exceed_context_size_error
--     incidents).
--   * guard.check=false AND guard.candidate=false — never guard-checked,
--     never a match candidate for other blocks.
--   * dream/digest/overview=false — no links, no topic-map, no clustering.
--   * classify: stable writer title prefix "Compaction source" (priority 30,
--     after system-meta 10 / audit-trail 20). Writers SHOULD still set
--     type=checkpoint explicitly (type_source='manual').
--
-- The config MUST decode byte-equivalently to the compiled-in builtin set in
-- internal/blocktype/builtin.go (checkpoint entry): the golden integration
-- test applies THIS file from migrations.FS and diffs the decoded rows against
-- the builtin set (drift gate, design/01 §4.1 R1). ON CONFLICT DO NOTHING
-- keeps the seed idempotent and never overwrites operator tuning on re-run.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config) VALUES
('checkpoint', '_global', 'Checkpoint', true, false, '{
  "v": 1,
  "retrieval": {"policy": "excluded"},
  "guard":     {"check": false, "candidate": false},
  "dream":     {"linkable": false},
  "digest":    {"include": false},
  "overview":  {"include": false},
  "parent":    {"mode": "none"},
  "classify":  {"priority": 30, "title_patterns": ["compaction source"]}
}'::jsonb)
ON CONFLICT (name, scope) DO NOTHING;

-- ── Data repair (fresh DB: both statements are no-ops) ───────────────────────

-- Retype the existing checkpoint corpus. Only auto-typed rows: a manual type
-- assertion is never overridden (T4 semantics). type_source stays 'auto' —
-- the classify hook would produce the same verdict from the title pattern.
UPDATE context_blocks
   SET type_name = 'checkpoint'
 WHERE category = 'compaction-checkpoints'
   AND type_source = 'auto'
   AND type_name <> 'checkpoint';

-- Un-archive the guard-auto-archived evidence blocks (guard_status =
-- 'archived_dup') so ID reference chains resolve again. Guard metadata
-- (guard_matched_id, guard_similarity, guard_checked_at) stays intact for
-- auditability. Re-archive is impossible on both axes: the sweep predicate
-- selects only guard_checked_at IS NULL (kept set), and checkpoint is no
-- longer in the guard.check type allowlist at all.
--
-- Two guards against the partial unique index (category,title,scope)
-- WHERE NOT is_archived:
--   * rn=1 — among archived title-twins only the NEWEST row (uuidv7 order)
--     returns; an older byte-twin stays archived as a true duplicate.
--   * NOT EXISTS — never un-archive into a slot a live row already occupies.
WITH ranked AS (
  SELECT id,
         row_number() OVER (PARTITION BY category, title, scope ORDER BY id DESC) AS rn
    FROM context_blocks
   WHERE category = 'compaction-checkpoints'
     AND is_archived
     AND guard_status = 'archived_dup'
)
UPDATE context_blocks b
   SET is_archived = false,
       guard_status = 'needs_review',
       updated_at = now()
  FROM ranked r
 WHERE b.id = r.id
   AND r.rn = 1
   AND NOT EXISTS (
     SELECT 1 FROM context_blocks live
      WHERE live.category = b.category
        AND live.title = b.title
        AND live.scope = b.scope
        AND NOT live.is_archived
   );

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (107, '107_checkpoint_type_seed.sql', now())
  ON CONFLICT (version) DO NOTHING;
