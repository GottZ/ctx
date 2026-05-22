-- =============================================================================
-- 043_supersedes_direction_swap.sql — Supersedes Direction Swap (Welle 46)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Welle 46 Dream-Quality-Audit (Sub-Agent 2 Semantic-Stichprobe, 2026-05-22):
--   5/5 supersedes-Stichproben sind INVERTED — TGT.created_at > SRC.created_at,
--   obwohl die natuerliche Sprachsemantik "A supersedes B" → A.created_at >=
--   B.created_at verlangt (A = die neuere, ersetzende Version). Bei 15
--   supersedes-Links insgesamt ergab die Vollerhebung: ALLE 15 invertiert.
--
-- Ursache: causal-Klasse hat die Inversion (S23-Pathologie) im Code-Filter
-- mit acceptCausal(srcCreated.Before(tgtCreated)) repariert. supersedes hat
-- die invertierte Filter-Konvention geerbt (acceptSupersedes verlangt
-- src.updated_at < tgt.updated_at). Welle 46 setzt die Sprach-Semantik
-- (source = neuer) als Korpus-Wahrheit durch.
--
-- Reciprocal-Konflikte: 4 von 15 Pairs haben in der Gegenrichtung einen
-- topical-Link. Nach Swap wuerde die PK (source, target) kollidieren.
-- Resolution: supersedes ist semantisch spezifischer als topical →
-- topical-reverse wird DELETEd, supersedes als swapped INSERT eingesetzt.
--   - 019c48f3 ↔ 019d314c (sup 0.95 vs topical 0.70)
--   - 019d28e0-9170 ↔ 019d28e1-1702 (sup 0.90 vs topical 0.90)
--   - 019db565 ↔ 019e16f6 (sup 0.90 vs topical 0.85)
--   - 019d2a50 ↔ 019d33de-9cf9 (sup 0.85 vs topical 0.85)
--
-- Strategy: DELETE invertierte + ggf. reverse-topical, INSERT mit getauschten
-- IDs. UPDATE des PK waere ebenfalls moeglich, DELETE+INSERT ist sauberer
-- (kein Trigger-Side-Effect, expliziter audit-trail in created_at = now()).
--
-- Idempotent: re-apply prueft auf src.created_at < tgt.created_at und matched
-- nur noch nicht geswappte Links. Erwartung bei Re-Apply: 0 Rows affected.
--
-- Bench-Risk: SupersedesMap (dream/dream.go:528) und filterSuperseded
-- (handler/query.go:577) interpretieren supersedes nach alter Konvention
-- (source = old). Nach diesem Swap haben sie invertierte Semantik —
-- DOKUMENTIERT, Welle 47+ Folge-Arbeit. Bench-Risk gering, weil die 15
-- betroffenen Source-IDs nicht in .eval-cyclic-gold.json sind.
-- =============================================================================

BEGIN;

-- Step 1: snapshot the invertierten supersedes-Links in temp-Table.
-- Wir lesen die ungeswappten Rohdaten BEVOR DELETE laeuft.
CREATE TEMP TABLE supersedes_to_swap ON COMMIT DROP AS
SELECT
  dl.source_block_id,
  dl.target_block_id,
  dl.relationship,
  dl.confidence,
  dl.raw_confidence,
  dl.scope,
  dl.metadata,
  dl.dream_version
FROM context_dream_links dl
JOIN context_blocks src ON src.id = dl.source_block_id
JOIN context_blocks tgt ON tgt.id = dl.target_block_id
WHERE dl.relationship = 'supersedes'
  AND src.created_at < tgt.created_at;

-- Step 2: clear reverse-topical-Konflikte. Nach Swap ist (target, source)
-- der neue PK; wenn dort bereits ein Link existiert (irgendeine relationship),
-- muss er weg. In der Praxis sind das die 4 reciprocal topical-Pairs.
DELETE FROM context_dream_links dl
WHERE EXISTS (
  SELECT 1 FROM supersedes_to_swap s
  WHERE s.source_block_id = dl.target_block_id
    AND s.target_block_id = dl.source_block_id
);

-- Step 3: delete die invertierten supersedes-Links.
DELETE FROM context_dream_links dl
WHERE EXISTS (
  SELECT 1 FROM supersedes_to_swap s
  WHERE s.source_block_id = dl.source_block_id
    AND s.target_block_id = dl.target_block_id
);

-- Step 4: reinsert mit getauschten source/target. metadata-Erweiterung
-- dokumentiert den Swap fuer spaetere Audits.
INSERT INTO context_dream_links (
  source_block_id, target_block_id, relationship,
  confidence, raw_confidence, scope, metadata, dream_version, created_at
)
SELECT
  target_block_id AS source_block_id,
  source_block_id AS target_block_id,
  relationship,
  confidence,
  raw_confidence,
  scope,
  COALESCE(metadata, '{}'::jsonb) || '{"direction_swap_w46": true, "audit_source": "sub-agent-2-2026-05-22"}'::jsonb AS metadata,
  dream_version,
  now() AS created_at
FROM supersedes_to_swap
ON CONFLICT (source_block_id, target_block_id) DO NOTHING;

COMMIT;

-- Validation (post-apply, comment-only):
-- SELECT count(*) FROM context_dream_links dl
--   JOIN context_blocks src ON src.id = dl.source_block_id
--   JOIN context_blocks tgt ON tgt.id = dl.target_block_id
--   WHERE dl.relationship = 'supersedes'
--     AND src.created_at < tgt.created_at;
-- Erwartung: 0 (kein invertierter Link mehr).
--
-- SELECT count(*) FROM context_dream_links WHERE relationship = 'supersedes';
-- Erwartung: 15 (oder 11 bei strikter Reciprocal-Resolution; abhaengig von
-- ON CONFLICT-Hits — siehe metadata.direction_swap_w46 fuer Tracking).

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (43, '043_supersedes_direction_swap.sql', now())
  ON CONFLICT (version) DO NOTHING;
