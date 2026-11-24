-- =============================================================================
-- 195_blocks_scope_updated_index.sql — browse and recent read their page from
-- an index per scope (issue #237)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Browse (the empty-query path of the compact and the full search: REST
-- /api/search, the MCP search tool, the chat ctx_search, `ctx search`) and
-- recent (REST /api/recent, the MCP recent tool, the chat ctx_recent) list
-- the visible blocks newest first, ORDER BY updated_at DESC, id DESC with a
-- keyset cursor on (updated_at, id). No index delivered that order for a
-- reader's scopes, and the visibility clause `scope = ANY($1) OR id =
-- ANY($grants)` left the planner one way: read every visible row of the
-- scopes, sort them, keep a page. A page cost what the reader's scopes hold.
-- Measured on the production image (rows of about 1.3 kB, 500 scopes
-- interleaved, 1 in 13 archived; random_page_cost 1.1 and 4, custom and
-- generic plan; docs/operations.md, "Migration 195"): at 1 000 000 rows a
-- reader of one scope of 184 000 rows read 157 848-201 391 buffers per page
-- (0.4-1.0 s), a reader of 500 scopes 200 743-204 469 (3.4-4.3 s), of a
-- scope of 920 rows 935-1 009, a category of 5 % of the big scope 8 377; at
-- 5 000 000 rows the reader of one scope 792 750-1 006 463 (1.0-4.8 s) and
-- the reader of 500 scopes 1 006 337-1 022 330.
--
-- This migration adds:
--
--   idx_blocks_scope_updated ON context_blocks (scope, updated_at DESC, id DESC)
--       WHERE NOT is_archived
--   idx_blocks_scope_category_updated ON context_blocks (scope, category, updated_at DESC, id DESC)
--       WHERE NOT is_archived
--
-- and the listings read one range per read scope (store/blocks_list.go): a
-- LATERAL arm per distinct read scope over the first index — over the
-- second when the listing names a category —, each from the cursor on with
-- the page's LIMIT, one arm for the granted blocks outside the read scopes
-- by primary key, and the sort of their rows for the LIMIT above them. The
-- same rows in the same order (TestListWalkSameRowsAsTheStatementsBefore195:
-- over 1 000 pages with and without the indexes). On the same stores, at
-- 1 000 000 and 5 000 000 rows: 13-16 buffers per page for one scope, big or
-- small, first page or deep; 39-47 for three scopes; 98-100 with 25 grants;
-- a category 3-16; 652-756 for 50 scopes and 6 531-7 540 (11-42 ms) for 500
-- — a reader of K scopes reads up to K × LIMIT rows and K descents, whatever
-- its scopes and other tenants hold.
--
-- What keeps the statement before 195: a type list, tags, tags_all and the
-- cluster facet (each can keep a handful of a scope's rows, an arm would
-- walk the scope to find them, and the statement before 195 reads the
-- filter's own index), and every listing while the index its arms read does
-- not stand (below).
--
-- The rejected key without the scope, (updated_at DESC, id DESC): the
-- statements as they were can walk it, and do so for a small reader too,
-- which then reads every tenant's newer rows (measured at 1 000 000 rows: a
-- reader of a scope without rows 3-4 buffers before, up to 223 041 with it).
--
-- HOT. scope, category, updated_at, id and is_archived are named by other
-- indexes of context_blocks already (idx_blocks_workflow_board names scope,
-- updated_at, id and is_archived; idx_context_category the category), so no
-- update that was HOT stops being HOT; an update that writes updated_at was
-- not HOT before either (TestBrowseIndexesTakeNoUpdateOffHOT; measured at
-- 1 000 000 rows: 1 000 of 1 000 dream claims and 1 000 of 2 000 updates of
-- store.UpdateBlock HOT with and without the indexes). The indexes name no
-- embedding column: they are not in the attnum class of the re-embed
-- cutover. Write cost: about 200-600 bytes of WAL per insert beside 14 200-
-- 14 700 (two entries); an update that writes updated_at adds an entry to
-- each, as it did to every other index before. Size: 51.7 MB and 60.3 MB at
-- 1 000 000 rows, 257 MB and 300 MB at 5 000 000; each grew by 20 MB over
-- 200 000 updates of the updated_at of the same 2 000 rows, which a VACUUM
-- leaves for reuse rather than returning.
--
-- WITHOUT THE INDEXES the arms read their scope through idx_context_scope and
-- sort it, which costs more than the statement before 195 for many scopes or
-- a category (measured at 1 000 000 rows: 744 081 against 200 743-204 469
-- buffers for 500 scopes, 18 881 against 8 377 for the category). So the
-- listings read the catalog (store.listIndexReadySQL: valid, ready, exactly
-- the definitions below; a process keeps its reading for a minute) and send
-- the statements before 195 while the index their arms read does not stand.
--
-- INDEX BUILD AND ITS RUNBOOK (docs/development.md, "A migration with a row
-- threshold"). The runner runs each file in ONE transaction
-- (store/migrations.go), where CONCURRENTLY is impossible. CREATE INDEX takes
-- SHARE on context_blocks until the file commits: readers go on, every
-- INSERT, UPDATE and DELETE waits; each build reads the whole heap.
--   * A valid index of a name with exactly its definition, as PostgreSQL
--     deparses it:
--       CREATE INDEX idx_blocks_scope_updated ON public.context_blocks USING btree (scope, updated_at DESC, id DESC) WHERE (NOT is_archived)
--       CREATE INDEX idx_blocks_scope_category_updated ON public.context_blocks USING btree (scope, category, updated_at DESC, id DESC) WHERE (NOT is_archived)
--     is kept; the file builds what is missing.
--   * Below 500 000 rows (ctx_migration_rows, migration 191): the missing
--     ones built inline. Measured with a writer inserting a block every 2 ms
--     (rows of about 1.5 KB of heap, host load 4.6-5.7): both builds, and
--     the writer's longest wait, 0.61-0.86 s at 250 000 rows (390 MB heap),
--     1.29-1.83 s at 500 000 (781 MB); 11 + 14 MB and 22 + 28 MB of index.
--   * Above, or a name taken by anything else (another definition, an
--     INVALID index — an aborted CONCURRENTLY build — or a relation that is
--     not an index): WARNING, nothing built or dropped for that name, and no
--     SHARE lock on context_blocks for it. Build it out of band on the
--     running binary (readers and writers go on). First, what holds the
--     names:
--
--       SELECT c.relname, c.relkind, i.indisvalid, pg_get_indexdef(c.oid)
--         FROM pg_class c
--         JOIN pg_namespace n ON n.oid = c.relnamespace
--         LEFT JOIN pg_index i ON i.indexrelid = c.oid
--        WHERE n.nspname = 'public'
--          AND c.relname IN ('idx_blocks_scope_updated', 'idx_blocks_scope_category_updated');
--
--     No row for a name: build it. An index, valid, of the definition above:
--     keep it. That definition with indisvalid false (an aborted build):
--     DROP INDEX CONCURRENTLY <name>; then build it. Anything else (another
--     definition, a view, a table, a sequence: not ctx's to drop): rename
--     it, then build it. The builds:
--
--       CREATE INDEX CONCURRENTLY idx_blocks_scope_updated
--           ON context_blocks (scope, updated_at DESC, id DESC)
--           WHERE NOT is_archived;
--       CREATE INDEX CONCURRENTLY idx_blocks_scope_category_updated
--           ON context_blocks (scope, category, updated_at DESC, id DESC)
--           WHERE NOT is_archived;
--
--     No IF NOT EXISTS: it would pass over an INVALID index or a foreign
--     object of the name. Until an index stands with its definition the
--     listings that read it send the statements before 195, at their cost,
--     and the schema-contract manifest stays red. Measured at 1 000 000
--     rows: both builds 3.2-3.3 s, a writer's longest wait 3-55 ms.
--
-- Rows, not the heap: the threshold counts live rows, the build reads every
-- heap page. A context_blocks heap far larger than its rows (mass deletes)
-- passes as small and makes the inline build read its size: compact it out
-- of band first, or build the indexes by the runbook before the upgrade.
--
-- `SET LOCAL lock_timeout = '3s'` (house pattern) bounds the acquisition of
-- the SHARE lock, not the build: a transaction that holds a write on
-- context_blocks longer fails the migration with lock_not_available (55P03),
-- the file rolls back with nothing changed and the boot fails loudly; the
-- next boot tries again.
--
-- OLDER BINARY. v5.81.0 (the rollback image; executed with its code on a
-- database at 195) reads with the statements before 195 — the same rows —
-- and pays the indexes' writes. Its migration runner leaves version 195
-- alone. Its schema-contract check finds _migrations:195 without an
-- embedded file (migration_integrity, breaking) and two indexes it does not
-- know (param): in mode warn (the default) it starts and /health reports
-- schema_contract attention; in mode enforce it refuses to start until
-- CTX_CONTRACT_MODE=warn or off is set.
--
-- WAY BACK: only for a name whose check above shows an index of its
-- definition (any validity) — DROP INDEX CONCURRENTLY <name>; for both, then
-- DELETE FROM _migrations WHERE version = 195. The listings of this release
-- send the statements before 195 from their next reading of the catalog on
-- (within a minute).
--
-- Index change only, no table or column: the test.sh T07 counts stay. The
-- schema-contract manifest gains the two indexes and migration_max
-- 194 -> 195. Idempotent, forward-only. Registration by the runner
-- (store.RunMigrations).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

DO $do$
DECLARE
    -- Measured: the build at the threshold and what waits for it
    -- (docs/operations.md, "Migration 195").
    inline_rows CONSTANT BIGINT := 500000;
    -- The two indexes, each as PostgreSQL deparses it and as this file builds
    -- it; the same position in both lists.
    c_defs      CONSTANT TEXT[] := ARRAY[
        'CREATE INDEX idx_blocks_scope_updated ON public.context_blocks USING btree (scope, updated_at DESC, id DESC) WHERE (NOT is_archived)',
        'CREATE INDEX idx_blocks_scope_category_updated ON public.context_blocks USING btree (scope, category, updated_at DESC, id DESC) WHERE (NOT is_archived)'];
    c_builds    CONSTANT TEXT[] := ARRAY[
        'CREATE INDEX idx_blocks_scope_updated ON context_blocks (scope, updated_at DESC, id DESC) WHERE NOT is_archived',
        'CREATE INDEX idx_blocks_scope_category_updated ON context_blocks (scope, category, updated_at DESC, id DESC) WHERE NOT is_archived'];
    c_names     CONSTANT TEXT[] := ARRAY['idx_blocks_scope_updated', 'idx_blocks_scope_category_updated'];
    v_missing   INT[] := '{}';
    v_kind      TEXT;
    v_def       TEXT;
    v_valid     BOOLEAN;
    v_rows      BIGINT;
    i           INT;
BEGIN
    FOR i IN 1 .. array_length(c_names, 1) LOOP
        -- Whatever relation holds the name: CREATE INDEX could not pass a
        -- view, a table or a sequence of this name either.
        SELECT c.relkind::text, pg_get_indexdef(ix.indexrelid), ix.indisvalid INTO v_kind, v_def, v_valid
          FROM pg_class c
          JOIN pg_namespace n ON n.oid = c.relnamespace
          LEFT JOIN pg_index ix ON ix.indexrelid = c.oid
         WHERE n.nspname = 'public' AND c.relname = c_names[i];

        IF NOT FOUND THEN
            v_missing := v_missing || i;
        -- The definition as PostgreSQL deparses it, so a same-named index on
        -- another table, key or predicate is not adopted.
        ELSIF NOT (v_kind = 'i' AND v_valid AND v_def = c_defs[i]) THEN
            RAISE WARNING '% exists but is not the index of migration 195 (valid=%): %. Nothing built or dropped; until it is replaced the listings that read it send their statements before 195, at their cost before 195. Runbook in the header of 195_blocks_scope_updated_index.sql; the schema-contract manifest stays red until then.',
                c_names[i], COALESCE(v_valid, false), COALESCE(v_def, format('a relation of relkind %s, not an index', v_kind));
        END IF;
    END LOOP;

    IF cardinality(v_missing) = 0 THEN
        RETURN;
    END IF;

    v_rows := ctx_migration_rows('public.context_blocks', inline_rows);
    IF v_rows >= inline_rows THEN
        FOREACH i IN ARRAY v_missing LOOP
            RAISE WARNING '% is missing at % rows: build it out of band (CREATE INDEX CONCURRENTLY, runbook in the header of 195_blocks_scope_updated_index.sql). Until then the listings that read it send their statements before 195, at their cost before 195; the schema-contract manifest stays red.',
                c_names[i], v_rows;
        END LOOP;
        RETURN;
    END IF;

    FOREACH i IN ARRAY v_missing LOOP
        EXECUTE c_builds[i];
    END LOOP;
END
$do$;
