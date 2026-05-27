-- =============================================================================
-- 046_write_log_block_title_text.sql — context_write_log.block_title TEXT (v2.0.0)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Trigger: M044 made context_blocks.title TEXT. context_write_log mirrors
-- title via block_title at write-time for audit-trail purposes. With M044
-- VARCHAR(500) becomes inconsistent — a long title would silently truncate
-- in the audit log. v2.0.0 makes both sides TEXT for full fidelity.
--
-- Properties: metadata-only ALTER (no row re-write).
--
-- Idempotent via _migrations PK + ON CONFLICT.
-- Reversible: ALTER COLUMN block_title TYPE VARCHAR(500) — fails if any
-- row exceeds 500 chars after roll-forward.
-- =============================================================================

ALTER TABLE context_write_log ALTER COLUMN block_title TYPE TEXT;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (46, '046_write_log_block_title_text.sql', now())
  ON CONFLICT (version) DO NOTHING;
