-- =============================================================================
-- 105_structural_class_collision_preflight.sql — dream/structural namespace
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Companion to the reservedGraphClasses guard (graph-structural GA6/GA7,
-- design/02 §4.5): DecodePolicy now REJECTS the five dream relationship names
-- inside structural_link_classes — on the write path AND on every registry
-- (re)load. The guard therefore acts retroactively on already-persisted rows,
-- and the degradation direction is not neutral: a pre-existing colliding BASE
-- row would fail the whole base reload into the builtin fallback (documented
-- FAIL-OPEN — operator visibility narrowings revert), and a colliding TENANT
-- row collapses that tenant's overlay onto base.
--
-- This preflight makes the deploy stop LOUDLY before the binary switch instead
-- of poisoning reloads silently: it RAISEs with row coordinates if
--   (a) any context_block_types config declares a reserved dream class in
--       structural_link_classes, or
--   (b) any context_structural_links row carries a dream class as link_class
--       (raw-SQL legacies: the manage write path validates via the registry,
--       raw SQL does not; 076 deliberately has no CHECK).
-- Read-only checks — idempotent by construction; a clean corpus is a no-op.
-- Scope note (E15): arm (a) reads the current STRING-form entries of
-- structural_link_classes; a future object form ({"class":...}) yields JSON
-- text here that never matches — by then the DecodePolicy guard (the durable
-- enforcement, GA6) must carry the reservation, not this one-shot preflight.
-- The live corpus was sweep-verified collision-free on 2026-07-14 (builtin:
-- references, duplicate-of), so this migration documents + enforces, it does
-- not migrate data.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

DO $$
DECLARE bad RECORD;
BEGIN
  SELECT t.name, t.scope, c.class
    INTO bad
    FROM context_block_types t,
         LATERAL jsonb_array_elements_text(
           COALESCE(t.config->'structural_link_classes', '[]'::jsonb)
         ) AS c(class)
   WHERE c.class IN ('topical','factual','causal','recurrent','supersedes')
   LIMIT 1;
  IF FOUND THEN
    RAISE EXCEPTION USING MESSAGE = format(
      'structural-class collision preflight (105): block type %I (scope %I) declares reserved dream class %I in structural_link_classes — fix the row before deploying the namespace guard (a colliding row would poison every registry reload into the fail-open builtin fallback)',
      bad.name, bad.scope, bad.class);
  END IF;
END $$;

DO $$
DECLARE bad RECORD;
BEGIN
  SELECT l.source_block_id, l.target_block_id, l.link_class, l.scope
    INTO bad
    FROM context_structural_links l
   WHERE l.link_class IN ('topical','factual','causal','recurrent','supersedes')
   LIMIT 1;
  IF FOUND THEN
    RAISE EXCEPTION USING MESSAGE = format(
      'structural-class collision preflight (105): context_structural_links row %s -> %s (scope %I) carries dream class %I as link_class — raw-SQL legacy, delete or reclass the row before deploying (076 has no CHECK; the vocabulary split would route it ambiguously)',
      bad.source_block_id, bad.target_block_id, bad.scope, bad.link_class);
  END IF;
END $$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (105, '105_structural_class_collision_preflight.sql', now())
  ON CONFLICT (version) DO NOTHING;
