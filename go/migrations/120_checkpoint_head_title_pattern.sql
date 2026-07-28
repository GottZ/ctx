-- =============================================================================
-- 120_checkpoint_head_title_pattern.sql — second checkpoint title pattern
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- M107 seeded exactly ONE classify pattern for the checkpoint type: the
-- transcript-part prefix "Compaction source …". The compaction writer emits a
-- SECOND stable title shape — "Compaction checkpoint head <session>" — for the
-- manifest block that anchors the ID chain. Those heads matched no checkpoint
-- rule, fell through to the default type (knowledge, priority-ordered after
-- system-meta 10 / audit-trail 20) and re-entered every autonomous pipeline
-- M107 exists to keep evidence blocks out of: full-pass retrieval, the guard
-- archive lane (0.98/0.92 — consecutive session heads are near-duplicates BY
-- CONSTRUCTION, the exact shape of the 2026-07-20 dangling-manifest incident),
-- dream links, digest and overview.
--
-- Adding the pattern here is a REGISTRY DATA edit, not a code path: the T4
-- classify rules live in context_block_types.config (classify.title_patterns,
-- rrf.MatchesAny — case-insensitive substring). The compiled-in fallback set in
-- internal/blocktype/builtin.go moves in lockstep with this file or the golden
-- drift gate (TestRegistryGolden_Integration) goes red.
--
-- Operator tuning is never overwritten (M107 doctrine, there expressed as
-- ON CONFLICT DO NOTHING): the value predicate on the WHERE clause restricts
-- the UPDATE to rows still carrying the untouched M107 seed. A row a human
-- edited — any other pattern list — is left exactly as found, and the
-- statement stays idempotent on re-run.
--
-- Scope: the '_global' builtin row only. Tenant-scope checkpoint overrides
-- (T12 overlay) are operator data by definition and out of reach here.
--
-- Tx-Hinweis (Konvention 058/059/061/091/116/119): der Runner
-- (store/migrations.go) wickelt jede Migration in eine eigene Transaktion,
-- SET LOCAL ist damit selbst-revertierend.
--
-- Forward-only. Kein Schema-Objekt → test.sh table count UNCHANGED. Die
-- Daten-Reparatur der bereits fehltypisierten Bestands-Heads ist bewusst NICHT
-- Teil dieser Migration (eigene Welle).
-- =============================================================================

SET LOCAL lock_timeout = '2s';

UPDATE context_block_types
   SET config = jsonb_set(config, '{classify,title_patterns}',
                          '["compaction source","compaction checkpoint"]'::jsonb)
 WHERE name = 'checkpoint' AND scope = '_global' AND builtin
   AND config->'classify'->'title_patterns' = '["compaction source"]'::jsonb;
