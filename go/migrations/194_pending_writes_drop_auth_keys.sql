-- =============================================================================
-- 194_pending_writes_drop_auth_keys.sql — the staged writes keep no foreign
-- key on context_api_keys and context_principals (issue #277)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
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
--   * a stage reads its key row FOR KEY SHARE and takes the principal from it
--     (store.StagePendingWrite): the row lock the foreign-key check took; a
--     key that is gone stages nothing;
--   * the tenant prune deletes the staged writes of the tenant's keys before
--     the keys, under their row locks (store.PruneTenant): a stage that held
--     a key while the prune waited is deleted with them, one that comes after
--     finds its key gone;
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
-- hypertable first, as every stage does (its statement names
-- context_pending_writes before context_api_keys, and the parser locks in
-- that order), so a stage waits for the file before it holds a lock the file
-- waits for; an authentication never locks context_pending_writes. A LOCK
-- TABLE of the referenced tables first would invert that order against a
-- stage in flight. Measured with two authenticating
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
-- the authentications that arrive queue behind it.
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
-- Constraint change only, no table, column or index: the test.sh T07 counts
-- and the schema-contract manifest's sections stay; migration_max 193 -> 194.
-- Idempotent, forward-only. Registration by the runner (store.RunMigrations).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

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
