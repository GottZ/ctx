-- =============================================================================
-- 184_access_token_fk_lookup_indexes.sql — the two foreign keys on
-- context_access_tokens get a serving index, so the token sweep costs the
-- rows it deletes and not those rows times the tables (issue #277)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- The OAuth token sweep (store.EvictExpiredOAuthTokens, the janitor bundle:
-- `DELETE FROM context_access_tokens WHERE expires_at < now() - interval
-- '7 days'`) deletes many rows in one statement. Per deleted row PostgreSQL
-- runs one lookup per foreign key that points at context_access_tokens(id):
--   context_access_tokens.parent_id        ON DELETE SET NULL  (self
--     reference: every rotated pair points at the refresh row it replaced,
--     store.RotateRefreshToken)
--   context_web_sessions.access_token_id   ON DELETE CASCADE   (the cookie
--     session's current access token, store.CreateWebSession /
--     RepointWebSession)
-- Neither had an index whose leading column is the key, so each lookup was a
-- sequential scan, and the sweep cost (rows deleted) x (rows of the two
-- tables): it grows with the square of the token volume when the expired
-- share stays the same. Measured (docs/operations.md, "Migration 184"):
-- 26.1 s at 50 000 tokens and 445 s at 200 000 (5 % of the rows due, one web
-- session per login family), 46 ms and 234 ms with both indexes.
--
--   idx_access_tokens_parent (parent_id) WHERE parent_id IS NOT NULL
--     partial: the lookup's `parent_id = $1` implies the predicate, and the
--     family anchors (parent_id NULL, every mint and every lone access
--     token) stay out of it.
--   idx_web_sessions_access_token (access_token_id)
--     access_token_id is NOT NULL, so the plain index is the partial one.
--
-- Not covered: context_access_tokens.principal_id and
-- context_oauth_codes.principal_id (ON DELETE CASCADE) are probed only when a
-- principal row is deleted, which no code path does (the excepted keys of
-- store.TestForeignKeysWithADeleteActionHaveAServingIndex).
--
-- INDEX BUILD AND ITS RUNBOOK (115/140/171/172 pattern). CREATE INDEX without
-- CONCURRENTLY takes a SHARE lock on the indexed table until this file
-- commits: readers go on, writers wait. ctxd runs its migrations before it
-- starts the scheduler and the router (cmd/ctxd/main.go), so on the booting
-- instance nothing waits but the boot itself; on other instances of the same
-- database:
--   context_access_tokens  every token mint and refresh rotation (OAuth
--                          /token, web login and its cookie refresh), family
--                          revokes, logout, the sweep;
--   context_web_sessions   every request with a session cookie
--                          (store.ResolveWebSession stamps last_used_at),
--                          login, cookie refresh, logout, the sweep.
-- The runner runs each file in ONE transaction (store/migrations.go), where
-- CONCURRENTLY is impossible. Therefore, per index:
--   * A usable index of this name (valid, and its definition as PostgreSQL
--     deparses it is exactly
--       CREATE INDEX idx_access_tokens_parent ON public.context_access_tokens USING btree (parent_id) WHERE (parent_id IS NOT NULL)
--       CREATE INDEX idx_web_sessions_access_token ON public.context_web_sessions USING btree (access_token_id)
--     ) makes this a no-op. An operator can build it beforehand, out of
--     band (readers and writers go on). First, per name, what holds it:
--
--       SELECT c.relkind, i.indisvalid, pg_get_indexdef(c.oid)
--         FROM pg_class c
--         JOIN pg_namespace n ON n.oid = c.relnamespace
--         LEFT JOIN pg_index i ON i.indexrelid = c.oid
--        WHERE n.nspname = 'public' AND c.relname = 'idx_access_tokens_parent';
--
--     No row: build it. An index, valid, of the definition above: keep it,
--     nothing to build. That definition with indisvalid false (an aborted
--     CONCURRENTLY build): DROP INDEX CONCURRENTLY idx_access_tokens_parent;
--     then build it. Anything else (another definition, a view, a table, a
--     sequence: not ctx's to drop): rename it (ALTER INDEX / ALTER VIEW ...
--     RENAME TO another name), then build it. The build:
--
--       CREATE INDEX CONCURRENTLY idx_access_tokens_parent
--           ON context_access_tokens (parent_id)
--           WHERE parent_id IS NOT NULL;
--       CREATE INDEX CONCURRENTLY idx_web_sessions_access_token
--           ON context_web_sessions (access_token_id);
--
--     No IF NOT EXISTS: it would pass over an INVALID index or a foreign
--     object of the name. The build is abortable; an aborted one leaves an
--     INVALID index behind (the case above).
--   * A name taken by anything else (another definition, an INVALID index,
--     or a relation that is not an index: a view, a table, a sequence):
--     WARNING, nothing built or dropped, no SHARE lock on that table.
--   * Below 500 000 rows of its table (pg_class.reltuples; never analysed: a
--     bounded count) the migration builds inline. Measured at the threshold
--     (docs/operations.md, "Migration 184"): a writer of each table waited
--     at most the build of its own table plus the builds after it.
--   * Above, WARNING, nothing built. Build it out of band: until then the
--     sweep costs as before this migration, and the schema-contract manifest
--     stays red (the deliberate trade of 115, 140, 171, 172 and 182):
--     /health reports schema_contract attention in contract mode warn, and a
--     boot in contract mode enforce refuses to start.
-- The token table goes first and the session table last: every cookie
-- request writes a session row, so its lock is held for the shortest time.
-- The paths that write both tables write the tokens first, as this file
-- locks them: the sweep (its cascade reaches the sessions from a deleted
-- token) and logout (store.DestroyWebSession: the family revoke, then the
-- session row). The session writers read the tokens only (ACCESS SHARE, and
-- ROW SHARE for the foreign-key check of an insert), which SHARE does not
-- block.
--
-- `SET LOCAL lock_timeout = '3s'` (house pattern) bounds each lock
-- acquisition on its own, neither a build nor the time a lock is held. A
-- writer holding a table longer fails the migration with lock_not_available
-- (55P03), the file rolls back with nothing changed and the boot fails
-- loudly; the next boot tries again. A writer of a table that arrives while
-- the file waits for that table queues behind the request: it waits until
-- the file has the lock (at most 3 s) and then until the file commits or
-- rolls back — the builds of its table and of the table after it, and the
-- wait for each later lock (at most 3 s each: the SHARE lock of the next
-- table and, for a table never analysed, the ACCESS SHARE of its bounded
-- count, which only an ACCESS EXCLUSIVE holder delays). In the lock order
-- above, a writer waits at most:
--   context_access_tokens  3 s + 3 s (+ 3 s and the count of a session
--                          table never analysed) + both builds;
--   context_web_sessions   3 s + its own build.
--
-- OLDER BINARY. v5.67.0 (the rollback image) writes, sweeps and deletes as
-- before; the indexes serve its statements too. Its schema-contract check
-- finds _migrations:184 without an embedded file (breaking) and two indexes
-- it does not know (param): in mode warn (the default) it starts and
-- /health reports schema_contract attention; in mode enforce it refuses to
-- start (every _migrations row without an embedded file is breaking) until
-- CTX_CONTRACT_MODE=warn or off is set.
--
-- WAY BACK (forward-only tree; to undo temporarily). Drop a name only when
-- the check query above shows an index of the definition above (any
-- validity); a same-named object of another kind or definition is one this
-- file left alone with a WARNING, and it stays:
--     DROP INDEX CONCURRENTLY idx_access_tokens_parent;
--     DROP INDEX CONCURRENTLY idx_web_sessions_access_token;
-- then DELETE FROM _migrations WHERE version = 184. The sweep stays correct
-- without them, at the cost named above.
--
-- Index change only, no table or column: the test.sh T07 counts stay. The
-- schema-contract manifest gains the two indexes and migration_max
-- 183 -> 184. Idempotent, forward-only. Registration by the runner
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
            (1, 'idx_access_tokens_parent',      'context_access_tokens', 'parent_id',       'parent_id IS NOT NULL', 500000),
            (2, 'idx_web_sessions_access_token', 'context_web_sessions',  'access_token_id', NULL,                    500000)
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
            RAISE WARNING '% exists but is not the foreign-key lookup index of %(%) (valid=%): %. Nothing built or dropped; the token sweep scans % once per deleted token until it is there. Runbook in the header of 184_access_token_fk_lookup_indexes.sql: rename or drop it, then build it CONCURRENTLY; the schema-contract manifest stays red until then.',
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
            RAISE WARNING '% is missing at % rows of %: build it out of band (CREATE INDEX CONCURRENTLY, runbook in the header of 184_access_token_fk_lookup_indexes.sql). The token sweep scans % once per deleted token until then; the schema-contract manifest stays red.',
                v_idx.name, v_rows, v_idx.tbl, v_idx.tbl;
        END IF;
    END LOOP;
END
$do$;
