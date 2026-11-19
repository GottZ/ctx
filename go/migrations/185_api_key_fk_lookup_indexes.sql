-- =============================================================================
-- 185_api_key_fk_lookup_indexes.sql — the foreign keys on context_api_keys
-- whose referencing table grows with use get a serving index (issue #277)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Deleting an API key runs one lookup per foreign key that points at
-- context_api_keys(id), on the referencing table, `WHERE col = $1`: CASCADE
-- deletes the rows found, SET NULL updates them. Four of these keys sit on
-- tables that grow with use rather than with the number of keys, tenants or
-- settings, and had no index whose leading column is the key, so their
-- lookup was a sequential scan of a table without a bound, once per deleted
-- key:
--   context_write_log.api_key_id      ON DELETE SET NULL  (guard and dream
--     audit rows, never removed; no writer of this version sets the column,
--     so every row is NULL and the scan finds nothing to update)
--   context_block_grants.granted_by   ON DELETE SET NULL  (one row per shared
--     block and grantee tenant)
--   context_chat_sessions.created_by  ON DELETE SET NULL  (one row per web
--     chat; webchat.session_retention defaults to 0, kept forever)
--   context_oauth_codes.api_key_id    ON DELETE CASCADE   (authorization
--     codes, swept one hour after expiry)
-- A key is deleted by the tenant prune (store.PruneTenant: `DELETE FROM
-- context_api_keys WHERE tenant_id = $1`, one lookup per key and foreign key)
-- and by hand. Measured (docs/operations.md, "Migration 185"): next to
-- 1 000 000 write-log rows a deleted key read 90 189 buffers of the write log
-- (317-445 ms), with the index 82-193 buffers (1.0-3.5 ms).
--
--   idx_write_log_api_key (api_key_id) WHERE api_key_id IS NOT NULL
--   idx_block_grants_granted_by (granted_by) WHERE granted_by IS NOT NULL
--   idx_chat_sessions_created_by (created_by) WHERE created_by IS NOT NULL
--     partial: the lookup's `col = $1` implies the predicate, and the rows
--     without a key (all write-log rows today, grants and chats whose key is
--     gone) stay out of it.
--   idx_oauth_codes_api_key (api_key_id)
--     api_key_id is NOT NULL, so the plain index is the partial one.
--
-- NOT covered here, deliberately:
--   context_pending_writes.api_key_id ON DELETE CASCADE is a key on a
--     TimescaleDB hypertable: it is copied onto every chunk and a key delete
--     probes each copy, so no index makes it constant (the gate of 172). Its
--     fix is a change of the key itself, not an index.
--   created_by / updated_by / rotated_by of the registries (gateway limits,
--     OAuth clients and providers, projects, secrets, settings, tenant grants
--     and quotas): one row per configured object, bounded by configuration,
--     not by use.
--
-- INDEX BUILD AND ITS RUNBOOK (115/140/171/172 pattern). CREATE INDEX without
-- CONCURRENTLY takes a SHARE lock on the indexed table until this file
-- commits: readers go on, writers wait. ctxd runs its migrations before it
-- starts the scheduler and the router (cmd/ctxd/main.go), so on the booting
-- instance nothing waits but the boot itself; on other instances of the same
-- database:
--   context_write_log      the guard batch (guard.writeAuditLog), the dream
--                          audit rows (dream.WriteLinks, store.DreamLinkResolve)
--                          and every block and key delete (SET NULL);
--   context_block_grants   grant and revoke (store/block_grants.go), every
--                          block delete (CASCADE) and tenant delete;
--   context_chat_sessions  every web chat turn (store/chat.go writes
--                          busy_until, title and updated_at), the webchat
--                          retention janitor and the tenant prune;
--   context_oauth_codes    the OAuth authorize and token steps
--                          (store/oauth_codes.go), its sweep, client deletes.
-- The runner runs each file in ONE transaction (store/migrations.go), where
-- CONCURRENTLY is impossible. Therefore, per index:
--   * A usable index of this name (btree, valid, on its table, exactly this
--     key and predicate) makes this a no-op. An operator can build it
--     beforehand, out of band (readers and writers go on):
--
--       CREATE INDEX CONCURRENTLY idx_write_log_api_key
--           ON context_write_log (api_key_id)
--           WHERE api_key_id IS NOT NULL;
--       CREATE INDEX CONCURRENTLY idx_block_grants_granted_by
--           ON context_block_grants (granted_by)
--           WHERE granted_by IS NOT NULL;
--       CREATE INDEX CONCURRENTLY idx_chat_sessions_created_by
--           ON context_chat_sessions (created_by)
--           WHERE created_by IS NOT NULL;
--       CREATE INDEX CONCURRENTLY idx_oauth_codes_api_key
--           ON context_oauth_codes (api_key_id);
--
--     (abortable; an aborted build leaves an INVALID index behind, which is
--     dropped with DROP INDEX CONCURRENTLY before the next try).
--   * A name taken by anything else (another definition, or an INVALID
--     index): WARNING, nothing built or dropped.
--   * Below the row threshold of its table (pg_class.reltuples; never
--     analysed: a bounded count) the migration builds inline:
--       context_write_log       2 000 000 rows. Only background jobs and
--         deletes write it. Measured with rows of the guard's and dream's
--         width, every api_key_id NULL as live (the partial index holds none
--         of them, the build still reads the whole heap): 86-96 ms at
--         1 000 000 rows (488 MB heap), 178-186 ms at 2 000 000 (976 MB).
--       context_block_grants, context_chat_sessions, context_oauth_codes
--         500 000 rows (the house value of 115, 140, 171, 172 and 182):
--         56-169 ms each at 500 000 rows.
--     The whole file at those sizes took 447-502 ms; a writer of a table
--     waits from the lock on its table to the commit (docs/operations.md,
--     "Migration 185").
--   * Above, WARNING, nothing built. Build it out of band: until then every
--     deleted key scans that table once, as before this migration, and the
--     schema-contract manifest stays red (the deliberate trade of 115, 140,
--     171, 172 and 182): /health reports schema_contract attention in
--     contract mode warn, and a boot in contract mode enforce refuses to
--     start.
-- LOCK ORDER. A key delete (the tenant prune) and a block delete (purge, the
-- tenant prune, the parent_id cascade) on another instance lock these tables
-- too, through their foreign-key triggers, which fire in the byte order of
-- their names (RI_ConstraintTrigger_a_<oid>) — not in the order the keys were
-- created: an oid of fewer digits sorts by its leading digit (live, the write
-- log's 50573 fires after the grants' 2095672). On a store grown through the
-- releases (the reference instance, read from pg_trigger) a key delete
-- reaches context_block_grants,
-- context_oauth_codes, context_write_log, context_chat_sessions in this
-- order, and a block delete context_block_grants before context_write_log.
-- The file takes its locks in the same order, so neither closes a cycle
-- with it there (before, the draft of this file took the write log first,
-- and a block delete holding its grant rows and waiting for the write log
-- deadlocked with it). A store whose trigger names sort otherwise (a fresh
-- chain, which numbers the keys in the order of the folded baseline) can close
-- such a
-- cycle during the builds; PostgreSQL then aborts one of the two after
-- deadlock_timeout (40P01): the delete reports the error and is repeated, or
-- the boot fails and the next one retries. The write log's build is the
-- longest (its writers are background jobs and deletes); the lock on the
-- grants and the OAuth codes is held over it, the chat sessions' lock (every
-- web chat turn) for its own build only.
--
-- `SET LOCAL lock_timeout = '3s'` (house pattern): the 3 s bound acquiring
-- each lock, not the build. A writer holding it longer fails the migration
-- with lock_not_available (55P03), the file rolls back with nothing changed
-- and the boot fails loudly; the next boot tries again. While the migration
-- waits for a lock, the writers of that table queue behind it, for at most
-- those 3 s.
--
-- OLDER BINARY. v5.67.0 (the rollback image) writes and deletes as before;
-- the indexes serve its statements too. Its schema-contract check finds
-- _migrations:185 without an embedded file (breaking) and four indexes it
-- does not know (param): in mode warn (the default) it starts and
-- /health reports schema_contract attention; in mode enforce it refuses to
-- start (every _migrations row without an embedded file is breaking) until
-- CTX_CONTRACT_MODE=warn or off is set.
--
-- WAY BACK (forward-only tree; to undo temporarily):
--     DROP INDEX CONCURRENTLY IF EXISTS idx_write_log_api_key;
--     DROP INDEX CONCURRENTLY IF EXISTS idx_block_grants_granted_by;
--     DROP INDEX CONCURRENTLY IF EXISTS idx_chat_sessions_created_by;
--     DROP INDEX CONCURRENTLY IF EXISTS idx_oauth_codes_api_key;
-- then DELETE FROM _migrations WHERE version = 185. Key deletes stay correct
-- without them, at the cost named above.
--
-- Index change only, no table or column: the test.sh T07 counts stay. The
-- schema-contract manifest gains the four indexes and migration_max
-- 184 -> 185. Idempotent, forward-only. Registration by the runner
-- (store.RunMigrations).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

DO $do$
DECLARE
    v_idx   RECORD;
    v_def   TEXT;
    v_valid BOOLEAN;
    v_rows  BIGINT;
    v_want  TEXT;
BEGIN
    FOR v_idx IN
        SELECT * FROM (VALUES
            (1, 'idx_block_grants_granted_by',  'context_block_grants',  'granted_by', 'granted_by IS NOT NULL',  500000),
            (2, 'idx_oauth_codes_api_key',      'context_oauth_codes',   'api_key_id', NULL,                      500000),
            (3, 'idx_write_log_api_key',        'context_write_log',     'api_key_id', 'api_key_id IS NOT NULL', 2000000),
            (4, 'idx_chat_sessions_created_by', 'context_chat_sessions', 'created_by', 'created_by IS NOT NULL',  500000)
        ) AS t(ord, name, tbl, col, pred, max_rows)
        ORDER BY ord
    LOOP
        -- The definition as PostgreSQL deparses it, so a same-named index
        -- on another table, column or predicate is not adopted.
        v_want := format('CREATE INDEX %s ON public.%s USING btree (%s)', v_idx.name, v_idx.tbl, v_idx.col)
               || CASE WHEN v_idx.pred IS NULL THEN '' ELSE format(' WHERE (%s)', v_idx.pred) END;

        SELECT pg_get_indexdef(c.oid), i.indisvalid INTO v_def, v_valid
          FROM pg_class c
          JOIN pg_namespace n ON n.oid = c.relnamespace
          JOIN pg_index     i ON i.indexrelid = c.oid
         WHERE n.nspname = 'public' AND c.relname = v_idx.name;

        IF FOUND THEN
            IF v_valid AND v_def = v_want THEN
                CONTINUE;               -- usable, nothing to do
            END IF;
            RAISE WARNING '% exists but is not the foreign-key lookup index of %(%) (valid=%): %. Nothing built or dropped; deleting an API key scans % once per key until it is there. Runbook in the header of 185_api_key_fk_lookup_indexes.sql: rename or drop it, then build it CONCURRENTLY; the schema-contract manifest stays red until then.',
                v_idx.name, v_idx.tbl, v_idx.col, v_valid, v_def, v_idx.tbl;
            CONTINUE;
        END IF;

        SELECT c.reltuples::BIGINT INTO v_rows
          FROM pg_class c
          JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = 'public' AND c.relname = v_idx.tbl;
        IF v_rows IS NULL OR v_rows < 0 THEN
            -- reltuples < 0: never analysed. A bounded count, never a full scan.
            EXECUTE format('SELECT count(*) FROM (SELECT 1 FROM %I LIMIT %s) t', v_idx.tbl, v_idx.max_rows) INTO v_rows;
        END IF;

        IF v_rows < v_idx.max_rows THEN
            EXECUTE format('CREATE INDEX %I ON %I (%I)', v_idx.name, v_idx.tbl, v_idx.col)
                 || CASE WHEN v_idx.pred IS NULL THEN '' ELSE ' WHERE ' || v_idx.pred END;
        ELSE
            RAISE WARNING '% is missing at % rows of %: build it out of band (CREATE INDEX CONCURRENTLY, runbook in the header of 185_api_key_fk_lookup_indexes.sql). Deleting an API key scans % once per key until then; the schema-contract manifest stays red.',
                v_idx.name, v_rows, v_idx.tbl, v_idx.tbl;
        END IF;
    END LOOP;
END
$do$;
