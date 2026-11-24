-- =============================================================================
-- 194_pending_writes_drop_auth_keys.sql — the dream queue count reads the
-- eligible rows from one index (issue #271), and the staged writes keep no
-- foreign key on context_api_keys and context_principals (issue #277)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- PART 1: idx_blocks_dream_eligible.
--
-- dream.QueueDepth (the dream_queue section of manage stats, the /api/status
-- collector every events.queue_stats_interval) counts the dream-eligible
-- blocks of the caller's read scopes:
--   WHERE NOT is_archived
--     AND lifecycle_state IN ('knowledge', 'canonical', 'synthesis')
--     AND type_name = ANY($2)            -- the dream.linkable types
--     AND scope = ANY($1)                -- the read scopes
-- and no index carried the first two conjuncts: the plans combined
-- idx_blocks_type_name, idx_context_scope(_active) and idx_blocks_sensitivity,
-- or walked idx_context_scope over every row of the scopes. This adds
--
--   idx_blocks_dream_eligible ON context_blocks (scope, type_name)
--       WHERE NOT is_archived AND lifecycle_state IN ('knowledge', 'canonical', 'synthesis')
--
-- which holds exactly the eligible rows. The count still reads embedding IS
-- NULL of every eligible row on the heap; the index removes the others.
-- Measured (docs/operations.md, "Migration 194"; production image, rows of
-- about 1.3 kB, per row of a linkable type three of other types, 1 in 13
-- archived), three read scopes holding three quarters of the rows, the plan a pooled
-- connection runs after five executions, random_page_cost 4: 143 443 ->
-- 72 046 buffers at 1 000 000 rows, 573 741 -> 288 887 at 4 000 000; at
-- random_page_cost 1.1 76 026 -> 72 044 and 304 661 -> 288 885; one scope of
-- 10 000 rows beside them 9 429 -> 2 281 at 4 000 000 (1.1); at 10 000 000
-- rows 1 461 880 -> 721 989 (4) and 781 248 -> 721 987 (1.1). The first five
-- executions on a connection are custom plans, which gain nothing at 4 and
-- at 10 000 000 rows and 1.1 read 762 022 -> 1 524 044 buffers (a plain
-- index scan of this index). Write cost: one entry per insert of an eligible
-- block and per update that is not HOT, +57-90 bytes of WAL per insert
-- (under 1 %); about 6.4 bytes per table row (24 MB at 4 000 000 rows, 61 MB
-- at 10 000 000).
--
-- HOT. scope, type_name, is_archived and lifecycle_state are named by other
-- indexes of context_blocks already (idx_context_scope, idx_blocks_type_name,
-- idx_archived and the partial indexes, idx_block_type), so no update that was
-- HOT stops being HOT: the dream pick's claim and RestampBackoff write
-- dream_cooldown_until alone, which no index names (measured: 2 000 of 2 000
-- claims HOT with and without the index; with a dream column in the key, the
-- rejected variant, 0 of 2 000). The predicate names no embedding column: the
-- index is not in the attnum class of the re-embed cutover.
--
-- COUPLING. The predicate is the lifecycle list of dream.dreamEligibleWhere;
-- a state added there and not here leaves QueueDepth on the plans before 194
-- (correct, at their cost). TestDreamEligibleIndexPredicateIsThePickPredicate
-- holds the two lists equal.
--
-- BUILD AND ITS RUNBOOK (docs/development.md, "A migration with a row
-- threshold"). CREATE INDEX takes SHARE on context_blocks until the file
-- commits: readers go on, every INSERT, UPDATE and DELETE waits; the build
-- reads the whole heap.
--   * A valid index of this name with exactly this definition, as
--     PostgreSQL deparses it:
--       CREATE INDEX idx_blocks_dream_eligible ON public.context_blocks USING btree (scope, type_name) WHERE ((NOT is_archived) AND ((lifecycle_state)::text = ANY ((ARRAY['knowledge'::character varying, 'canonical'::character varying, 'synthesis'::character varying])::text[])))
--     no-op.
--   * Below 500 000 rows (ctx_migration_rows, migration 191) and nothing of
--     this name: built inline. Measured with a writer inserting a block
--     every 2 ms (rows of about 1.5 KB of heap, host load 25-38): the build
--     and the writer's longest wait 0.69-1.07 s at 250 000 rows (390 MB
--     heap), 1.00-1.39 s at 500 000 (781 MB); 2.9 MB of index at 415 700
--     entries (the key deduplicates).
--   * Above, or a name taken by anything else (another definition, an
--     INVALID index — an aborted CONCURRENTLY build — or a relation that is
--     not an index): WARNING, nothing built or dropped, no SHARE lock on
--     context_blocks. Build it out of band (readers and writers go on).
--     First, what holds the name:
--
--       SELECT c.relkind, i.indisvalid, pg_get_indexdef(c.oid)
--         FROM pg_class c
--         JOIN pg_namespace n ON n.oid = c.relnamespace
--         LEFT JOIN pg_index i ON i.indexrelid = c.oid
--        WHERE n.nspname = 'public' AND c.relname = 'idx_blocks_dream_eligible';
--
--     No row: build it. An index, valid, of the definition above: keep it.
--     That definition with indisvalid false: DROP INDEX CONCURRENTLY
--     idx_blocks_dream_eligible; then build it. Anything else: rename it,
--     then build it:
--
--       CREATE INDEX CONCURRENTLY idx_blocks_dream_eligible
--           ON context_blocks (scope, type_name)
--           WHERE NOT is_archived AND lifecycle_state IN ('knowledge', 'canonical', 'synthesis');
--
--     No IF NOT EXISTS: it would pass over an INVALID index or a foreign
--     object of the name. Until it stands QueueDepth counts as before 194
--     and the schema-contract manifest stays red.
-- Rows, not the heap: the threshold counts live rows, the build reads every
-- heap page; compact a heap far larger than its rows first, or build the
-- index by the runbook before the upgrade.
--
-- PART 2: the foreign keys of context_pending_writes.
--
-- context_pending_writes (migration 089) is a TimescaleDB hypertable of 1 h
-- chunks with two foreign keys onto the tables every authentication reads:
--   context_pending_writes_api_key_id_fkey   (api_key_id)   -> context_api_keys    ON DELETE CASCADE
--   context_pending_writes_principal_id_fkey (principal_id) -> context_principals  (096, NO ACTION)
-- TimescaleDB copies both onto every chunk, each copy with its own triggers
-- on the referenced tables. Measured (docs/operations.md, "Migration 194"):
--   * the first stage of an hour creates a chunk and takes SHARE ROW
--     EXCLUSIVE on context_api_keys and context_principals until its
--     transaction commits: every authentication (ctx_auth_by_id writes
--     last_used_at) waits for it, and it for them;
--   * dropping a chunk (EvictPendingWrites, writes.confirm_retention, every
--     minute on every instance) takes ACCESS EXCLUSIVE on both tables: with a
--     transaction that read either of them still open, the drop waits — with
--     no lock_timeout — and every authentication queues behind it (2 s probe:
--     no authentication answered until the reader ended);
--   * a key delete (the tenant prune, once per key) probes every chunk:
--     2.5-9.8 ms per key at 10 chunks, 16-26 at 100, 77-98 at 400, 215-269 at
--     1 000.
-- No index helps (the key is copied per chunk), so the keys go. What they
-- carried moves into the code of this release:
--   * every statement that writes a stage — a new stage, the re-arm of an
--     open one, the consume — reads its key row FOR KEY SHARE before it
--     touches a stage row (store.StagePendingWrite, ConsumePendingWrite): the
--     row lock the foreign-key check took for an insert; a new stage takes
--     the principal from it; a key that is gone stages, re-arms and consumes
--     nothing;
--   * the tenant prune deletes the tenant's keys and then, in a statement of
--     its own, the stages of exactly those keys (store.PruneTenant): the key
--     delete waits for a writer that holds a key and the next statement sees
--     that writer's stage, one that comes after finds its key gone, and a key
--     minted while the prune ran goes with its stages. The re-arm and the
--     consume hold the key for a reason of this table: a DELETE on the
--     hypertable that waited for a concurrent UPDATE of its row deleted
--     nothing once the update committed (executed on PostgreSQL 18.6 with
--     TimescaleDB 2.31.0, on the production image and the CI image; a plain
--     table deletes the updated row, and the cascade of the dropped key did
--     too), so a re-arm or consume beside the prune without the key lock
--     left the stage of a deleted key behind;
--   * every read of a stage selects by the caller's key id (lookup, consume,
--     re-arm), and a deleted key does not authenticate.
-- No other path deletes a key row (a revoke sets active = false; key ids are
-- uuidv7() defaults no code path sets), and no path deletes a principal. A
-- key deleted by hand (psql) leaves its stages unreachable until
-- writes.confirm_retention drops their chunk.
--
-- THE DROP AND ITS RUNBOOK. ALTER TABLE ... DROP CONSTRAINT on the hypertable
-- drops the copy on every chunk and the triggers on the referenced tables:
-- it takes ACCESS EXCLUSIVE on context_pending_writes, every chunk,
-- context_api_keys and context_principals until the file commits, so every
-- authentication of every instance waits for the drop. ALTER TABLE takes the
-- hypertable first, as a single stage and a consume do (their statements
-- name context_pending_writes before context_api_keys, and the parser locks
-- in that order), and as the confirm-retention eviction does; an authentication
-- never locks context_pending_writes. A staged MCP batch locks in the other
-- order: its transaction books the write budget (context_access_log, whose
-- key on context_api_keys reads the key) before it stages. A batch caught
-- between its booking and its stage closes a cycle with the drop, and
-- PostgreSQL ends one side after deadlock_timeout (40P01): executed, the
-- migration was the one aborted after 1.03 s, the boot failed, the next boot
-- ran through; every authentication waited that second. No order of the file
-- fits the single stage, the batch and the eviction at once (taking the
-- referenced tables first would close the cycle with the single stage and
-- with an eviction that still drops a chunk with the keys). Measured with two authenticating
-- connections beside it, the drop and the longest authentication: on a host
-- at load 5, 7 ms at 10 chunks, 14 ms at 25 (the 24 h default of
-- writes.confirm_retention), 104 ms at 100, 442 ms at 400, 2.7 s at 1 000;
-- at load 40, 39-85 ms at 25, 156-247 ms at 100, 555-712 ms at 250,
-- 1.41-1.56 s at 400, 1.65-2.42 s at 499. It holds about 11 locks per chunk
-- (11 016 at 1 000).
--   * No foreign key of context_pending_writes onto either table: no-op.
--   * Fewer than 250 chunks (ten days of hourly stages): dropped inline;
--     at the threshold every authentication and every stage waits for the
--     drop, 0.33-0.76 s at 249 chunks on a host at load 34-50. The drop holds about 11 locks per chunk, so at the threshold
--     about 2 750: below the nominal size of
--     PostgreSQL's default lock table (max_locks_per_transaction 64 x 100
--     connections), and it took 11 016 at 1 000 chunks on a lock table of a
--     nominal 3 200 slots (the table grows into the spare shared memory). A
--     drop that finds no room fails with out_of_shared_memory (53200) and
--     rolls back; then raise max_locks_per_transaction or drop the keys by
--     the runbook.
--   * Otherwise: WARNING, nothing dropped, no lock on the referenced tables.
--     The code of this release is correct either way (the prune's delete and
--     the stage's key read come first; a cascade then finds nothing). Drop
--     them out of band in a quiet window, or after writes.confirm_retention
--     has dropped chunks (each drop takes ACCESS EXCLUSIVE on both tables as
--     long as the keys stand):
--
--       SET lock_timeout = '3s';
--       ALTER TABLE context_pending_writes
--           DROP CONSTRAINT context_pending_writes_api_key_id_fkey,
--           DROP CONSTRAINT context_pending_writes_principal_id_fkey;
--
--     First, what stands:
--
--       SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint
--        WHERE contype = 'f' AND conrelid = 'public.context_pending_writes'::regclass;
--
-- `SET LOCAL lock_timeout = '3s'` (house pattern) bounds each lock
-- acquisition, not the time a lock is held: a transaction that holds one of
-- the tables longer fails the migration with lock_not_available (55P03), the
-- file rolls back with nothing changed and the boot fails loudly; the next
-- boot tries again. While the file waits for a lock (at most 3 s per table),
-- the authentications that arrive queue behind it. Part 1 runs in the same
-- transaction: its SHARE lock on context_blocks is held through these waits,
-- and a run of the file that fails here (55P03, 40P01) rolls the index build
-- back with it — block writers then waited for the build and the wait, and
-- the next boot builds again.
--
-- OLDER BINARY. v5.79.0 (the rollback image), run on a database at 194: its
-- runner leaves version 194 alone; its stage statement reads the key
-- without a lock and, without the key, writes a stage for a key id that has
-- no row (principal NULL) — unreachable, no key of that id authenticates;
-- its tenant prune leaves the tenant's stages to the cascade, which is gone
-- (executed: one stage of the pruned key left). Such stages stay until
-- writes.confirm_retention drops their chunk (with a retention of 0, for
-- good):
--     DELETE FROM context_pending_writes p
--      WHERE NOT EXISTS (SELECT 1 FROM context_api_keys k WHERE k.id = p.api_key_id);
-- removes them. In a rolling deploy the prune of this release deletes the
-- stages of the keys it deletes, not those an older instance's prune left.
-- Its schema-contract check finds
-- _migrations:194 without an embedded file (breaking): in mode warn (the
-- default) it starts and /health reports schema_contract attention; in mode
-- enforce it refuses to start until CTX_CONTRACT_MODE=warn or off is set. The
-- manifest lists no constraints, so the keys' absence is no finding of it.
--
-- WAY BACK (forward-only tree; to undo temporarily). Re-adding the keys
-- validates every chunk and brings the chunk locks back:
--     ALTER TABLE context_pending_writes
--         ADD CONSTRAINT context_pending_writes_api_key_id_fkey
--             FOREIGN KEY (api_key_id) REFERENCES context_api_keys(id) ON DELETE CASCADE,
--         ADD CONSTRAINT context_pending_writes_principal_id_fkey
--             FOREIGN KEY (principal_id) REFERENCES context_principals(id);
-- after removing the stages whose key is gone (the DELETE above), then
-- DELETE FROM _migrations WHERE version = 194.
--
-- WAY BACK, part 1: only when the check query of part 1 shows an index of
-- its definition (any validity) — a same-named object of another kind or
-- definition is one this file left alone with a WARNING, and it stays —
-- DROP INDEX CONCURRENTLY idx_blocks_dream_eligible; QueueDepth counts as
-- before 194. The older binary (v5.79.0) pays the index's writes and reads it
-- with the same statement; its schema-contract check reports the index it
-- does not know (param) beside _migrations:194.
--
-- No table or column: the test.sh T07 counts stay. The schema-contract
-- manifest gains idx_blocks_dream_eligible and migration_max 193 -> 194 (it
-- lists no constraints). Idempotent, forward-only. Registration by the runner
-- (store.RunMigrations).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- Part 1: the dream queue index (issue #271). First, so the ACCESS EXCLUSIVE
-- of part 2 on the tables every authentication reads is taken last and held
-- only for the drop.
DO $do$
DECLARE
    -- Measured: the build at the threshold and what waits for it
    -- (docs/operations.md, "Migration 194").
    inline_rows CONSTANT BIGINT := 500000;
    c_def       CONSTANT TEXT := 'CREATE INDEX idx_blocks_dream_eligible ON public.context_blocks USING btree (scope, type_name) WHERE ((NOT is_archived) AND ((lifecycle_state)::text = ANY ((ARRAY[''knowledge''::character varying, ''canonical''::character varying, ''synthesis''::character varying])::text[])))';
    v_kind      TEXT;
    v_def       TEXT;
    v_valid     BOOLEAN;
    v_rows      BIGINT;
BEGIN
    -- Whatever relation holds the name: CREATE INDEX could not pass a view,
    -- a table or a sequence of this name either.
    SELECT c.relkind::text, pg_get_indexdef(i.indexrelid), i.indisvalid INTO v_kind, v_def, v_valid
      FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
      LEFT JOIN pg_index i ON i.indexrelid = c.oid
     WHERE n.nspname = 'public' AND c.relname = 'idx_blocks_dream_eligible';

    IF FOUND THEN
        -- The definition as PostgreSQL deparses it, so a same-named index on
        -- another table, key or predicate is not adopted.
        IF v_kind = 'i' AND v_valid AND v_def = c_def THEN
            RETURN;                 -- usable, nothing to do
        END IF;
        RAISE WARNING 'idx_blocks_dream_eligible exists but is not the index of migration 194 (valid=%): %. Nothing built or dropped; the dream queue counts stay correct at their cost before 194. Runbook in the header of 194_pending_writes_drop_auth_keys.sql; the schema-contract manifest stays red until then.',
            COALESCE(v_valid, false), COALESCE(v_def, format('a relation of relkind %s, not an index', v_kind));
        RETURN;
    END IF;

    v_rows := ctx_migration_rows('public.context_blocks', inline_rows);
    IF v_rows >= inline_rows THEN
        RAISE WARNING 'idx_blocks_dream_eligible is missing at % rows: build it out of band (CREATE INDEX CONCURRENTLY, runbook in the header of 194_pending_writes_drop_auth_keys.sql). The dream queue counts stay correct at their cost before 194; the schema-contract manifest stays red.',
            v_rows;
        RETURN;
    END IF;

    CREATE INDEX idx_blocks_dream_eligible
        ON context_blocks (scope, type_name)
        WHERE NOT is_archived AND lifecycle_state IN ('knowledge', 'canonical', 'synthesis');
END
$do$;

-- Part 2: the foreign keys of the staged writes (issue #277).
DO $do$
DECLARE
    -- Measured: the drop and the authentications waiting for it per chunk
    -- count (docs/operations.md, "Migration 194").
    inline_chunks    CONSTANT INT := 250;
    v_keys           TEXT[];
    v_chunks         INT;
BEGIN
    SELECT array_agg(c.conname::text ORDER BY c.conname) INTO v_keys
      FROM pg_constraint c
     WHERE c.contype = 'f'
       AND c.conrelid = 'public.context_pending_writes'::regclass
       AND c.confrelid IN ('public.context_api_keys'::regclass, 'public.context_principals'::regclass);
    IF v_keys IS NULL THEN
        RETURN;                 -- dropped already (or out of band)
    END IF;

    SELECT count(*) INTO v_chunks
      FROM timescaledb_information.chunks
     WHERE hypertable_schema = 'public' AND hypertable_name = 'context_pending_writes';
    IF v_chunks >= inline_chunks THEN
        RAISE WARNING 'context_pending_writes keeps its foreign keys % at % chunks: dropping them holds ACCESS EXCLUSIVE on context_api_keys and context_principals, which every authentication reads, for the drop on every chunk. Drop them out of band in a quiet window (runbook in the header of 194_pending_writes_drop_auth_keys.sql); this release is correct with and without them.',
            v_keys, v_chunks;
        RETURN;
    END IF;

    EXECUTE 'ALTER TABLE public.context_pending_writes '
         || (SELECT string_agg(format('DROP CONSTRAINT %I', k), ', ') FROM unnest(v_keys) k);
END
$do$;
