-- =====================================================================
-- 042_dream_full_reset.sql — Full Dream Reset (Welle 46)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =====================================================================
-- Welle 46 Konvention-Switch (Sub-Agent 2 Semantic-Audit 2026-05-22):
-- supersedes-Direction-Convention auf englische Sprachsemantik
-- ("A supersedes B" → A is newer/authoritative). Statt einzelnen
-- Records zu swappen wird der gesamte dream-Korpus reset und über
-- alle 510 Blocks neu evaluiert. Saubere Baseline, alle neuen Links
-- per neuer Konvention (enforceSupersedesDirection SWAP-Filter).
--
-- Plus: löst implicit auch
--   - 8 phantom-pending blocks (M041 Vorgänger-Pattern)
--   - 92 Pre-v5 Links (kein dream_version-Mix mehr nach Re-Dream)
--   - 10 isolated blocks (kompletter Re-Run gibt allen erneut Chance)
--   - 4 archived-dangling links (TRUNCATE löscht eh alles)
--
-- Idempotent: TRUNCATE auf leerer Tabelle = no-op,
--             UPDATE auf bereits NULL-Werten = no-op.
-- =====================================================================

TRUNCATE TABLE context_dream_links;

UPDATE context_blocks
   SET dream_checked_at = NULL,
       dream_cooldown_until = NULL,
       dream_keywords = NULL,
       dream_temporal_validated_at = NULL
 WHERE NOT is_archived
   AND embedding IS NOT NULL
   AND (block_type IS NULL OR block_type IN ('knowledge', 'source', 'canonical'))
   AND NOT is_meta;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (42, '042_dream_full_reset.sql', now())
  ON CONFLICT (version) DO NOTHING;
