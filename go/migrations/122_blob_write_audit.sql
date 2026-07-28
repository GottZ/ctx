-- =============================================================================
-- 122_blob_write_audit.sql — the audit trail learns to reference a BLOB
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- context_access_log has exactly one reference column, block_id, and it is
-- bound to context_blocks (001_initial.sql). /api/blob/store nevertheless
-- logged the BLOB id through it, so every successful blob write raised 23503 —
-- inside a fire-and-forget goroutine, where the error was logged and dropped.
-- The blob surface has therefore never produced a single audit row, and the
-- write budget that counts those rows could never see a blob write either.
-- This column is the missing dimension (Gap-C0-b / RC-1 W-B2).
--
-- Deliberately WITHOUT a referential constraint, and that is the decision, not
-- an omission:
--
--   1. An audit row states what HAPPENED. A blob deleted afterwards does not
--      unmake the write. ON DELETE SET NULL would erase the very attribution
--      the row exists for; the strict variant would refuse the delete outright
--      and turn the audit trail into a retention lock on user data.
--   2. Every DELETE on context_blobs would grow a per-row referential trigger
--      plus a scan of this table. context_access_log is the highest-volume
--      table in the schema (it grows per REQUEST, target scale 1M+ blocks),
--      so that scan is paid on the delete path forever, for a guarantee the
--      audit semantics do not want in the first place.
--
-- A dangling id is the intended outcome: it says "this blob existed and was
-- written here", which stays true after the delete.
--
-- No DML: the column is born NULL for the whole existing corpus and that is
-- correct — no historical row was ever a blob write (they could not be, see
-- above). Nothing to backfill, nothing to guard, no lock beyond the catalog
-- update. ADD COLUMN without a default is metadata-only since PG 11.
--
-- No index either: the only lookup by this column is the writer's own
-- attribution UPDATE, which addresses the row by primary key. Audit READS go
-- through api_key_id/action/created_at, already covered by 063's composite.
-- An index here would cost every write and serve nobody.
-- =============================================================================

ALTER TABLE context_access_log ADD COLUMN IF NOT EXISTS blob_id UUID;

COMMENT ON COLUMN context_access_log.blob_id IS
    'Blob written by this access (context_blobs.id), unconstrained on purpose: audit rows outlive their blob. Mutually exclusive with block_id by construction of the writer, not by a database rule.';
