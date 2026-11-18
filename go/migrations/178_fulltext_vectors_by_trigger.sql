-- =============================================================================
-- 178_fulltext_vectors_by_trigger.sql — an update that leaves title and
-- content alone keeps the block's full-text vectors (issue #343, part 1)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- context_blocks.ts_de and ts_en were STORED generated columns over
-- coalesce(title, '') || ' ' || content (migration 044, folded into
-- 113_baseline.sql). On an UPDATE,
-- PostgreSQL skips recomputing a stored generated column whose inputs are not
-- targets of the statement, but only while the table has no BEFORE ROW UPDATE
-- trigger: such a trigger may change any column, so with one present every
-- generated column is recomputed for every updated row (ExecInitStoredGenerated
-- in nodeModifyTable.c). Since migration 174 (trg_block_retype) and 175
-- (trg_block_embed_excluded) context_blocks has two, both column-filtered, and
-- the column filter does not matter: the rule reads only whether such a trigger
-- exists, disabled or not. Every update of a block — archive, metadata, tags,
-- embedding, guard verdict, dream stamp — ran to_tsvector twice over its whole
-- text and wrote both vectors anew. A vector of a wide block lives in TOAST, so
-- each update also wrote a new TOAST value per vector and left the old one dead.
--
-- Measured (testcontainer, production image pgvector-timescaledb:pg18,
-- PostgreSQL 18.6; store of 10 000 blocks, 57 % checkpoint parts of 35 000
-- characters; TestFullTextColumnsMeasure in internal/store):
--   * 400 such blocks archived in one statement: at migration 173 41 ms,
--     2.3 MB WAL, no new TOAST value; at 177 4 264 ms, 49.4 MB WAL, 400 new
--     TOAST values per vector; at 177 with the two BEFORE triggers dropped
--     58 ms and none; with both disabled instead 4 329 ms and 400, as with
--     them; at 178 133 ms, 2.3 MB WAL and none.
--   * 5 700 blocks archived in one statement: 72.3 s, 920 MB WAL, TOAST
--     +223 MB at 177; 1.8 s, 27 MB WAL, TOAST unchanged at 178. One metadata
--     update of one such block: median 11.0 ms at 177, 0.73 ms at 178.
--   * Eight concurrent writers through the store, blocks of 35 000
--     characters, median per call 177 -> 178: insert 138.8 -> 56.9 ms,
--     metadata update 20.9 -> 2.5 ms, content update 113.0 -> 25.2 ms; blocks
--     of 600 characters: insert 2.31 -> 2.32 ms, metadata update 1.71 ->
--     1.83 ms, content update 9.95 -> 7.84 ms.
--   * The price, on one statement: an insert or a text change of a block of
--     600 characters runs a trigger where the generated column was computed
--     inline. Medians of 800 alternating EXPLAIN ANALYZE each: insert 0.489
--     -> 0.514 ms, content update 0.490 -> 0.539 ms. PostgreSQL's own C
--     trigger tsvector_update_trigger (0.540 / 0.590 ms) and a PL/pgSQL body
--     that concatenates once (0.554 / 0.590 ms) were no cheaper.
--
-- Since this migration both columns are ordinary columns kept by one trigger
-- function, context_blocks_fulltext(), which computes them with the
-- expression of the generated columns:
--   trg_block_fulltext       BEFORE INSERT OR UPDATE OF ts_de, ts_en
--                            an insert, and a statement that names a vector
--                            column, get the vectors of the row's text — a
--                            write of its own is replaced, as the generated
--                            column refused it;
--   trg_block_fulltext_text  BEFORE UPDATE OF title, content
--                            WHEN title or content differs from the old row.
-- An update that changes neither keeps both values as they are: the new row
-- version carries the old TOAST pointers, nothing is computed or written. A
-- statement that sends title or content unchanged (UpdateBlock with the same
-- text, an upsert of identical content) now keeps them too; the generated
-- column recomputed them whenever title or content was a target. The values
-- are the same as before for every row: the function's expression is the
-- generated one, and the columns keep the values the generated columns held.
-- Readers (SearchBlocks, ctx_rrf / ctx_rrf_arms, the issue full-text search,
-- the channel probe, the partial GIN indexes of migration 145) are unchanged.
--
-- ALTER COLUMN … DROP EXPRESSION turns a stored generated column into an
-- ordinary one and keeps its data: a catalog change, no table rewrite and no
-- scan (measured: 3.1 ms at 10 000 blocks and 2.2 ms at 50 000, the same
-- relfilenode before and after). It takes ACCESS EXCLUSIVE on context_blocks,
-- readers included, and holds it until the migration commits; the two CREATE
-- TRIGGER run under it. All of it is one transaction (the runner runs each
-- file in one, store/migrations.go): no write between the drop of the
-- expression and the triggers can store a row without vectors.
--
-- `SET LOCAL lock_timeout = '3s'` (house pattern 116/117/126/128/140/145/171/
-- 173): a transaction holding context_blocks longer fails the migration with
-- lock_not_available (55P03), the file rolls back and the boot fails loudly;
-- the next boot tries again.
--
-- The function runs no query, only two expressions, so a backend keeps no
-- plan sized by its first call (the concern migration 177 answers with SET
-- enable_nestloop / jit). It carries SET search_path = pg_catalog: the
-- generated expression bound to_tsvector, the configurations and || when it
-- was created, while PL/pgSQL resolves them at run time; a session whose
-- search_path puts another schema ahead of pg_catalog would otherwise get
-- that schema's 'german'.
--
-- INVARIANT. The generated columns were computed after every BEFORE trigger;
-- trg_block_fulltext_text runs in name order among them and fires only when
-- title or content is a target of the statement. No BEFORE trigger on
-- context_blocks changes title or content today (trg_block_retype writes
-- untrusted_origin, trg_block_embed_excluded embed_excluded and the embedding
-- columns). A trigger that did would have to sort before trg_block_fulltext_text
-- and fire only on statements that name title or content, or the vectors would
-- keep the old text; so would an ALTER COLUMN title/content TYPE … USING that
-- changes the text. The repair below covers both.
--
-- PG_DUMP. pg_dump leaves generated columns out of the data and pg_restore
-- recomputes them; ordinary columns are dumped. A dump taken after this
-- migration carries both vectors as text (measured on the store above: the
-- COPY text of context_blocks gzip-6 59.6 MB without them, 160.8 MB with
-- them), and a restore loads them instead of computing them. pg_restore
-- creates the triggers after the data, so the dumped values are kept.
-- On the measured shape the data of context_blocks in a dump grows by a
-- factor of 2.7; most of it is the vectors of checkpoint blocks, which issue
-- #343 part 2 would take out. While the triggers are off (ALTER TABLE …
-- DISABLE TRIGGER, session_replication_role = replica, pg_restore
-- --disable-triggers) nothing keeps the vectors: a data-only restore of a dump
-- taken BEFORE this migration (it has no vector columns) leaves them NULL, and
-- a text change keeps the old ones. Repair, one full pass:
--     UPDATE context_blocks SET ts_de = NULL
--      WHERE ts_de IS DISTINCT FROM to_tsvector('german', coalesce(title::text, '') || ' ' || content)
--         OR ts_en IS DISTINCT FROM to_tsvector('english', coalesce(title::text, '') || ' ' || content);
-- (trg_block_fulltext recomputes both on every row it names).
--
-- OLDER BINARY. A binary before this migration never writes either column;
-- its inserts and its updates of title or content go through the triggers
-- and get the same vectors as before, and its reads are unchanged. Its
-- schema-contract check reports the function, the two triggers and the
-- dropped expressions as drift, as for every migration after its own.
--
-- REVERT (forward-only tree; nothing needs it for a rollback of the binary):
-- turning the columns back into generated ones is a table rewrite under
-- ACCESS EXCLUSIVE —
--     DROP TRIGGER trg_block_fulltext ON context_blocks;
--     DROP TRIGGER trg_block_fulltext_text ON context_blocks;
--     ALTER TABLE context_blocks DROP COLUMN ts_de, DROP COLUMN ts_en,
--         ADD COLUMN ts_de TSVECTOR GENERATED ALWAYS AS (
--             to_tsvector('german', coalesce(title::text, '') || ' ' || content)) STORED,
--         ADD COLUMN ts_en TSVECTOR GENERATED ALWAYS AS (
--             to_tsvector('english', coalesce(title::text, '') || ' ' || content)) STORED;
--   the two partial GIN indexes of migration 145 rebuilt, and
--     DROP FUNCTION context_blocks_fulltext();
--     DELETE FROM _migrations WHERE version = 178;
--
-- No column or table is added or dropped (test.sh T07 counts stay). The
-- schema-contract manifest records both columns as not generated, the
-- function, the two triggers and migration_max 177 -> 178.
-- Idempotent, forward-only. Registration by the runner (store.RunMigrations).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE OR REPLACE FUNCTION context_blocks_fulltext() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $$
BEGIN
    NEW.ts_de := to_tsvector('german', coalesce(NEW.title::text, '') || ' ' || NEW.content);
    NEW.ts_en := to_tsvector('english', coalesce(NEW.title::text, '') || ' ' || NEW.content);
    RETURN NEW;
END;
$$;

COMMENT ON FUNCTION context_blocks_fulltext() IS
    'trg_block_fulltext / trg_block_fulltext_text (migration 178): computes context_blocks.ts_de and ts_en from title and content on insert, on a change of title or content, and on a write that names a vector column.';

ALTER TABLE context_blocks
    ALTER COLUMN ts_de DROP EXPRESSION IF EXISTS,
    ALTER COLUMN ts_en DROP EXPRESSION IF EXISTS;

CREATE OR REPLACE TRIGGER trg_block_fulltext
    BEFORE INSERT OR UPDATE OF ts_de, ts_en ON context_blocks
    FOR EACH ROW EXECUTE FUNCTION context_blocks_fulltext();

CREATE OR REPLACE TRIGGER trg_block_fulltext_text
    BEFORE UPDATE OF title, content ON context_blocks
    FOR EACH ROW
    WHEN (OLD.title IS DISTINCT FROM NEW.title OR OLD.content IS DISTINCT FROM NEW.content)
    EXECUTE FUNCTION context_blocks_fulltext();
