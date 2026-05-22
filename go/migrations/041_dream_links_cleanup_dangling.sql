-- =============================================================================
-- 041_dream_links_cleanup_dangling.sql — Dangling-Link-Cleanup (Welle 45)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Welle 45 Topologie-Audit (Sub-Agent 3, 2026-05-22) hat 2 dangling dream_links
-- aufgedeckt, beide Seiten archiviert seit 2026-05-12:
--   - 019e1b5f (waf-test-02-shell-keyword) → 019e1b60 (waf-test-08-delete-and-update)
--   - 019e1b5f (waf-test-02-shell-keyword) → 019e1b60 (waf-test-09-drop-and-update)
--
-- Ursache: dream.CleanupDanglingLinks (go/internal/dream/dream.go:489) existiert
-- als toter Code — wird von keinem Caller invoked. Welle 45 schließt die Lücke
-- a) hier (einmalig DELETE) und b) im Code (Aufruf in runDailySynthesis, plus
-- Erweiterung auf source_block_id archived).
--
-- Idempotent: NOT EXISTS-Guard prüft auf is_archived. Re-Anwendung löscht
-- nichts wenn alle bereits aufgeräumt sind.
-- =============================================================================

DELETE FROM context_dream_links
WHERE source_block_id IN (SELECT id FROM context_blocks WHERE is_archived)
   OR target_block_id IN (SELECT id FROM context_blocks WHERE is_archived);

-- Validation (post-apply, comment-only):
-- SELECT COUNT(*) FROM context_dream_links dl
--   JOIN context_blocks src ON src.id = dl.source_block_id
--   JOIN context_blocks tgt ON tgt.id = dl.target_block_id
--   WHERE src.is_archived OR tgt.is_archived;
-- Erwartung: 0

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (41, '041_dream_links_cleanup_dangling.sql', now())
  ON CONFLICT (version) DO NOTHING;
