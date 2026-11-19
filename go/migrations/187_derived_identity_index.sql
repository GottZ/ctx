-- =============================================================================
-- 187_derived_identity_index.sql — the derived-identity claim probe becomes
-- one btree descent (PR #386 follow-up, PR #392)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- store.ProvenanceClaimed asks whether a derivative — live OR archived —
-- holds a (category, title, scope), and store.foreignProvenanceClaim whether
-- one of another arm does:
--
--     SELECT EXISTS (SELECT 1 FROM context_blocks
--                     WHERE metadata ? 'provenance'
--                       AND category = $1 AND title = $2 AND scope = $3 ...)
--
-- It runs on every fresh UpsertBlock insert (the INSERT half of the S3 gate),
-- on the ingest pre-flight, and after every update that moves a block's
-- identity (UpdateBlock, UpdateIssueBlock, #392). No index served that
-- question: the unique index on the identity is partial on live rows, and
-- the planner answered it with an index scan of idx_trgm_title_gist (title =
-- $2 on the trigram GiST) over ALL rows, live and archived. Measured (PR
-- #392; 50 000 live blocks beside 20 000 / 220 000 / 420 000 archived ones, a
-- quarter of them derivatives, custom and generic plan alike): a miss read
-- p50 69 / 197 / 312 and p95 618 / 1 845 / 2 942 shared buffers — the probe
-- grew with the archive.
--
--   idx_context_derived_identity ON context_blocks
--       (category, title COLLATE "C", scope) WHERE metadata ? 'provenance'
--
-- holds only derivatives (the predicate is the probe's own leading term, so
-- the planner proves the implication) and answers the probe with one btree
-- descent, whatever the size of the archive.
--
-- TITLE COLLATE "C", AND THE PROBE CHANGES WITH IT. A plain (category,
-- title, scope) partial index is taken only while its estimate beats the
-- trigram GiST's, whose estimate for title = $2 stays at a few rows whatever
-- the scan reads: measured for #386 without a VACUUM after the load, the
-- planner went back to the GiST at 270 000 and 470 000 rows. The probe of
-- this release therefore compares the title under the C collation
-- (title COLLATE "C" = $2), which the trigram indexes (default collation)
-- cannot serve and this index can: the GiST is no path for it at all.
-- Equality under a deterministic collation is byte equality, and the
-- default collation of a database is deterministic, so both forms
-- answer alike (store tests compare them on titles with non-ASCII
-- characters, case and surrounding blanks). The C-collated form without this
-- index has no title index left and reads every row of the category, so
-- store.ProvenanceClaimed sends it only while this index stands as built
-- here (store.derivedIdentityIndexReadySQL, probed once a minute per process
-- and database) and its form before this release otherwise.
--
-- Write cost: a row enters the index only when it carries a provenance; the
-- predicate is evaluated on every insert and on every update that is not
-- HOT. metadata, category, title and scope are indexed already
-- (idx_context_metadata, uq_context_category_title_scope), so the index takes
-- no update off the HOT path that was on it.
--
-- INDEX BUILD AND ITS RUNBOOK (the 171/182 pattern). The runner runs each file
-- in ONE transaction (store/migrations.go), where CONCURRENTLY is impossible.
-- CREATE INDEX takes SHARE on context_blocks until the file commits: readers
-- go on, every INSERT, UPDATE and DELETE waits; the build reads the whole
-- heap, so its time follows the table, not the index.
--   * A valid index of this name with exactly this definition: no-op.
--   * Below 500 000 rows (pg_class.reltuples; never analysed: a bounded
--     count) and no index of this name: built inline. Measured with a writer
--     on a second connection (docs/operations.md, "Migration 187").
--   * Above, or a name taken by anything else (another definition, or an
--     INVALID index — an aborted CONCURRENTLY build): WARNING, nothing built
--     or dropped, and no lock on context_blocks. Build it out of band on the
--     running binary (readers and writers go on):
--
--       DROP INDEX CONCURRENTLY IF EXISTS idx_context_derived_identity;
--       CREATE INDEX CONCURRENTLY idx_context_derived_identity
--           ON context_blocks (category, title COLLATE "C", scope)
--           WHERE metadata ? 'provenance';
--
--     (the DROP only removes an INVALID leftover or a foreign index of this
--     name — check what it is first). Until the index stands with this
--     definition the probe runs in its form before this release, at its cost
--     before this release, and the schema-contract manifest stays red.
--
-- `SET LOCAL lock_timeout = '3s'` (house pattern): a transaction that holds a
-- write on context_blocks longer fails the migration with lock_not_available
-- (55P03), the file rolls back with nothing changed and the boot fails
-- loudly; the next boot tries again.
--
-- OLDER BINARY. v5.66.0 (the rollback image) sends the probe in its form
-- before this release; the planner may serve it from the trigram GiST as
-- before (the index cannot serve its default-collation title). Its
-- schema-contract check finds _migrations:187 without an embedded file
-- (breaking) and an index it does not know (param): in mode warn (the default) it starts and
-- /health reports schema_contract attention; in mode enforce it refuses to
-- start (every _migrations row without an embedded file is breaking) until
-- CTX_CONTRACT_MODE=warn or off is set.
--
-- WAY BACK: DROP INDEX CONCURRENTLY IF EXISTS idx_context_derived_identity;
-- then DELETE FROM _migrations WHERE version = 187. The probe of this release
-- returns to its form before this release within a minute.
--
-- Index change only, no table or column: the test.sh T07 counts stay. The
-- schema-contract manifest gains the index and migration_max 186 -> 187.
-- Idempotent, forward-only. Registration by the runner (store.RunMigrations).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

DO $do$
DECLARE
    c_def   CONSTANT TEXT := 'CREATE INDEX idx_context_derived_identity ON public.context_blocks USING btree (category, title COLLATE "C", scope) WHERE (metadata ? ''provenance''::text)';
    v_def   TEXT;
    v_valid BOOLEAN;
    v_rows  BIGINT;
BEGIN
    SELECT pg_get_indexdef(c.oid), i.indisvalid INTO v_def, v_valid
      FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
      JOIN pg_index     i ON i.indexrelid = c.oid
     WHERE n.nspname = 'public' AND c.relname = 'idx_context_derived_identity';

    IF FOUND THEN
        -- The definition as PostgreSQL deparses it, so a same-named index on
        -- another table, key or predicate is not adopted.
        IF v_valid AND v_def = c_def THEN
            RETURN;                 -- usable, nothing to do
        END IF;
        RAISE WARNING 'idx_context_derived_identity exists but is not the derived-identity index (valid=%): %. Nothing built or dropped; until it is replaced the provenance claim probe runs in its form before 187, at its cost before 187. Runbook in the header of 187_derived_identity_index.sql; the schema-contract manifest stays red until then.',
            v_valid, v_def;
        RETURN;
    END IF;

    SELECT c.reltuples::BIGINT INTO v_rows
      FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname = 'public' AND c.relname = 'context_blocks';
    IF v_rows IS NULL OR v_rows < 0 THEN
        -- reltuples < 0: never analysed. A bounded count, never a full scan.
        SELECT count(*) INTO v_rows FROM (SELECT 1 FROM context_blocks LIMIT 500000) t;
    END IF;

    IF v_rows >= 500000 THEN
        RAISE WARNING 'idx_context_derived_identity is missing at % rows: build it out of band (CREATE INDEX CONCURRENTLY, runbook in the header of 187_derived_identity_index.sql). Until then the provenance claim probe runs in its form before 187, at its cost before 187; the schema-contract manifest stays red.',
            v_rows;
        RETURN;
    END IF;

    CREATE INDEX idx_context_derived_identity
        ON context_blocks (category, title COLLATE "C", scope)
        WHERE metadata ? 'provenance';
END
$do$;
