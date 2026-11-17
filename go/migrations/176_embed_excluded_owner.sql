-- =============================================================================
-- 176_embed_excluded_owner.sql — the never-embedded answer is read from the
-- registry of the tenant that owns the block's scope (issue #213)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Migration 175 marks a block of a type that is never embedded
-- (context_blocks.embed_excluded) so it leaves the embed-backfill pick
-- indexes. Its rule, ctx_block_type_never_embedded(name), reads the name's
-- _global row and the rows of every other scope: a type that exists only in
-- tenant scopes, excluded there, was never marked. The header of 175 says such
-- blocks were "filtered by the pick row by row as before this migration";
-- that is not what happened. The pick's predicate was the same function, it
-- answered false, and these blocks were embedded on every pick path (the
-- query path with the arm on and off, a grantee's query, the scheduler bound
-- and unbound), before 175 and after it, although no reader ranks such a
-- vector: the owner's readers resolve the name to `excluded`, every other
-- reader without a row of the name does not know it (an unknown type is
-- invisible). Issue #213.
--
-- THE RULE. ctx_block_type_never_embedded(name, scope): the block of name in
-- scope is never embedded when the name has at least one registry row in R
-- and every row in R carries retrieval.policy `excluded`, where
--
--   R = the _global row, the row of the block's own scope, and the rows of
--       every scope of the tenant that owns the block's scope
--       (context_tenant_scopes)
--
-- the set trg_block_retype (migration 174) reads for the untrusted record.
-- The readers whose registry the owning tenant controls (its keys resolve
-- types by their home scope, which is one of its scopes) are in R; _global
-- stands for every other reader that has no row of the name. Rows of ANOTHER
-- tenant are not read: a tenant's registry change never decides about the
-- blocks of another tenant, and a tenant cannot learn from its own blocks
-- whether another tenant holds a row of a name. Two consequences, stated in
-- docs/multi-tenancy.md:
--   * a reader of another tenant that may read the scope (a scope grant, a
--     block grant, the shared layer) and holds its OWN visible row of the
--     same name ranks the owner's blocks of an excluded type by full text
--     only, no longer by vector;
--   * an overlay that lifts a _global exclusion (written past the narrowing
--     gate, or a tenant row that predates its _global base) keeps the type
--     embeddable for its own tenant's blocks only, no longer for every
--     tenant. A tenant row that predates its base and is later narrowed to
--     `excluded` therefore marks its own tenant's blocks only (175's stated
--     cross-tenant limit is gone).
-- A scope without a context_tenant_scopes row has R = the _global row and its
-- own row. Without a tenant row the name has no row in R and the block stays
-- embeddable, as before. A block that a tenant writes into a scope it does
-- not own (the shared layer) is answered by the registry of the scope's owner.
--
-- The one-argument function of 175 stays as it is: a binary before this
-- migration calls it from its pick predicate and its reconcile, and keeps
-- working against this schema until every process runs this binary (see
-- ROLLING DEPLOY). This binary does not call it.
--
-- GROUPS. The answer is the same for every scope of one tenant (R is), so
-- the reconcile queue, the new table context_embed_excluded_queue, keys on
-- (name, grp) with
--   grp = 't:' || tenant_id   for a scope in context_tenant_scopes,
--         's:' || scope       for a scope without a tenant,
--         '*'                 for a change of a _global row (every group).
-- ctx_embed_excluded_group(scope) computes it.
--
-- WHO QUEUES. The marker is still written only by trg_block_embed_excluded,
-- now BEFORE INSERT OR UPDATE OF type_name, scope, embed_excluded,
-- is_archived: an insert, a re-type, a scope move and an unarchive set it
-- from the two-argument rule. Three AFTER triggers queue the groups whose
-- answer may have turned:
--   * trg_block_types_embed_excluded (context_block_types, replaced): a row
--     of a tenant or tenantless scope that appears, disappears or is or was
--     `excluded` queues (name, its group); a _global row that is or was
--     `excluded` queues (name, '*'), and so does a _global row that appears or
--     disappears while the name has a row in another scope. A change between
--     two policies other than `excluded` queues nothing: no group's answer
--     can turn by it.
--   * trg_tenant_scopes_embed_excluded (context_tenant_scopes, new): a scope
--     that is assigned to a tenant, leaves it or moves between tenants queues
--     (name, group) for every name with a row in a scope of the tenants
--     involved or in the scope itself, for the old and the new group of the
--     scope.
--   * nothing else: a grant changes no answer.
-- The scheduler's reconcile (store.ReconcileEmbedExcluded) reads the answer
-- of every scope of the entry once; it marks the pending and the stale
-- blocks of the name in the scopes that answer true (a stale one drops its
-- vector), scope by scope, and clears the marked blocks of the group whose
-- own answer is false only when a scope answers false; in batches, outside
-- the registry write; a group never reaches a block outside its scopes.
--
-- LOCKS. SET LOCAL lock_timeout = '3s' (house pattern). Every statement of
-- this file is a catalog change on its tables or reads context_block_types:
--   * context_blocks: CREATE OR REPLACE TRIGGER trg_block_embed_excluded
--     takes SHARE ROW EXCLUSIVE, which waits for and blocks writers of
--     context_blocks (readers go on), held to the commit of this file. No
--     statement reads or writes a row of context_blocks: no heap scan, no
--     index build, no row update.
--   * context_block_types: CREATE OR REPLACE TRIGGER, SHARE ROW EXCLUSIVE.
--   * context_tenant_scopes: CREATE OR REPLACE TRIGGER, SHARE ROW EXCLUSIVE.
--   * context_embed_excluded_queue: created; the rows of the old queue
--     (context_embed_excluded_reconcile, a handful) move over in one INSERT
--     and one DELETE.
-- A transaction holding one of these tables longer than 3 s fails the
-- migration with 55P03, the file rolls back, the next boot tries again.
-- At run time the triggers order themselves on transaction-level advisory
-- locks, hashtext('ctx:embed_excluded:' || name) as in 175 and
-- hashtext('ctx:embed_excluded:grp:' || grp) for a group:
--   * a block write whose answer is true takes the name's lock shared, then
--     its group's lock shared, reads the group again (and takes the new
--     group's lock too while the scope moved), and decides again on a fresh
--     snapshot; a block it marks that holds a stale vector drops it;
--   * a registry change that queues takes the name's lock exclusive; for a
--     row of a tenant or tenantless scope it then reads the row's group and
--     takes that group's lock shared, and reads the group once more (a
--     scope assignment that committed meanwhile is seen);
--   * a scope assignment takes the locks of the groups involved exclusive,
--     in sorted order, before it reads the registry.
-- So a block written on the old registry or the old assignment commits
-- before the change does (the reconcile, which starts after it, sees it),
-- and a block written after waits and decides on the new state; a registry
-- row written while its scope is assigned is ordered against the assignment
-- on the group's lock, so one of the two queues the name for the new group.
-- The limits of 175 hold as they were: a block whose answer becomes true
-- while its transaction stays open keeps false (a cost: the pick's predicate
-- filters it), and a transaction that takes locks of two names, or of two
-- groups, in an order another transaction reverses can be ended with 40P01
-- (one name per registry transaction; a block transaction that writes blocks
-- of two tenants while one of their scopes is assigned). The tenant
-- bootstrap (handler bootstrapTenant) assigns the scope and mints the key in
-- one transaction and writes no type, so it takes group locks only.
--
-- ROLLING DEPLOY. A binary before this migration runs against this schema
-- until it is replaced: its pick states the one-argument predicate and NOT
-- embed_excluded (it embeds what 175 embedded, and leaves the blocks this
-- rule marks), its type API and block writes go through the new triggers,
-- and its reconcile reads context_embed_excluded_reconcile, which nothing
-- writes any more. The queue is a new table for that reason: on the old
-- table, the old reconcile would take an entry of this rule, decide it by
-- the one-argument answer (false for a tenant-own type), find nothing to
-- clear and delete it, and the blocks the entry stands for would stay
-- unmarked. Pinned by TestEmbedExcludedRollingOldBinary.
--
-- SEED. Every name that has a row outside _global is queued for '*': the
-- reconcile marks the blocks this rule marks and 175 did not (a tenant-own
-- excluded type; a _global-excluded type whose only lifting overlay is in
-- another tenant). Blocks that hold a vector keep it and keep false, as in
-- 175 (the reconcile marks blocks without a vector only). Rows of the old
-- queue move over with grp '*'. No row of context_blocks is updated here.
--
-- REVERT (forward-only tree; to undo temporarily): the triggers of 175 can be
-- restored by re-running the trigger part of 175_embed_excluded.sql; the
-- markers then follow the 175 rule again after each block's next trigger
-- event or reconcile.
--
-- One table (context_embed_excluded_queue): the test.sh T07 counts move to
-- 70 tables and stay at 45 context_blocks columns. The schema-contract
-- manifest gains the table, the three functions, the trigger on
-- context_tenant_scopes, the changed functions and trigger column list, and
-- migration_max 175 -> 176.
-- Idempotent, forward-only. Registration by the runner (store.RunMigrations).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_embed_excluded_queue (
    name      TEXT        NOT NULL,
    grp       TEXT        NOT NULL,
    queued_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (name, grp)
);

COMMENT ON TABLE context_embed_excluded_queue IS
    'Type names and groups whose never-embedded answer may have changed; the scheduler sets the embed_excluded marker of their live blocks in the scopes of the group and removes the row (migration 176).';

COMMENT ON COLUMN context_embed_excluded_queue.grp IS
    't:<tenant_id> for the scopes of a tenant, s:<scope> for a scope without a tenant, * for every scope; see ctx_embed_excluded_group (migration 176).';

-- The rows the triggers of 175 queued move over, for every group; the old
-- table stays, empty, for a binary before this migration (ROLLING DEPLOY).
INSERT INTO context_embed_excluded_queue (name, grp, queued_at)
SELECT name, '*', queued_at FROM context_embed_excluded_reconcile
ON CONFLICT (name, grp) DO NOTHING;
DELETE FROM context_embed_excluded_reconcile;

COMMENT ON TABLE context_embed_excluded_reconcile IS
    'The reconcile queue of migration 175, superseded by context_embed_excluded_queue (migration 176): nothing writes it any more; it stays for a binary before 176 that reads it during a rolling deploy.';

CREATE OR REPLACE FUNCTION ctx_embed_excluded_group(p_scope TEXT) RETURNS TEXT
LANGUAGE sql STABLE PARALLEL SAFE AS $$
    SELECT CASE WHEN p_scope = '_global' THEN '*'
                ELSE COALESCE((SELECT 't:' || ts.tenant_id::text
                                 FROM context_tenant_scopes ts WHERE ts.scope = p_scope),
                              's:' || p_scope) END
$$;

COMMENT ON FUNCTION ctx_embed_excluded_group(TEXT) IS
    'The reconcile group of a scope: t:<tenant_id> for a scope of a tenant, s:<scope> without one, * for _global (migration 176).';

CREATE OR REPLACE FUNCTION ctx_block_type_never_embedded(p_name TEXT, p_scope TEXT) RETURNS BOOLEAN
LANGUAGE sql STABLE PARALLEL SAFE AS $$
    SELECT EXISTS (SELECT 1 FROM context_block_types t
                    WHERE t.name = p_name
                      AND (t.scope IN ('_global', p_scope)
                           OR t.scope IN (SELECT o.scope FROM context_tenant_scopes b
                                            JOIN context_tenant_scopes o ON o.tenant_id = b.tenant_id
                                           WHERE b.scope = p_scope)))
       AND NOT EXISTS (SELECT 1 FROM context_block_types t
                        WHERE t.name = p_name
                          AND (t.scope IN ('_global', p_scope)
                               OR t.scope IN (SELECT o.scope FROM context_tenant_scopes b
                                                JOIN context_tenant_scopes o ON o.tenant_id = b.tenant_id
                                               WHERE b.scope = p_scope))
                          AND t.config->'retrieval'->>'policy' IS DISTINCT FROM 'excluded')
$$;

COMMENT ON FUNCTION ctx_block_type_never_embedded(TEXT, TEXT) IS
    'true when the type name has a registry row in _global, in the scope or in a scope of the tenant that owns the scope, and every such row carries retrieval.policy excluded: no reader that the owner''s registry or _global decides for ranks a vector of it (migration 176, issue #213).';

CREATE OR REPLACE FUNCTION context_blocks_embed_excluded() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_grp   TEXT;
    v_again TEXT;
    v_turns INT := 0;
BEGIN
    IF TG_OP = 'UPDATE'
       AND NEW.type_name IS NOT DISTINCT FROM OLD.type_name
       AND NEW.scope IS NOT DISTINCT FROM OLD.scope
       AND NEW.embed_excluded IS NOT DISTINCT FROM OLD.embed_excluded
       AND (NEW.is_archived OR NOT OLD.is_archived) THEN     -- not an unarchive
        RETURN NEW;
    END IF;
    NEW.embed_excluded := ctx_block_type_never_embedded(NEW.type_name, NEW.scope);
    IF NEW.embed_excluded THEN
        -- Take the name's lock and the group's lock, then decide again on a
        -- fresh snapshot: a registry change or a scope assignment that turns
        -- the answer either waits for this commit (and its reconcile sees
        -- this row) or has committed before this second look. The group is
        -- read again after its lock: a scope that moved to another tenant
        -- meanwhile takes that group's lock as well, so a second move waits.
        PERFORM pg_advisory_xact_lock_shared(hashtext('ctx:embed_excluded:' || NEW.type_name));
        v_grp := ctx_embed_excluded_group(NEW.scope);
        LOOP
            PERFORM pg_advisory_xact_lock_shared(hashtext('ctx:embed_excluded:grp:' || v_grp));
            v_again := ctx_embed_excluded_group(NEW.scope);
            v_turns := v_turns + 1;
            EXIT WHEN v_again IS NOT DISTINCT FROM v_grp OR v_turns >= 8;
            v_grp := v_again;
        END LOOP;
        NEW.embed_excluded := ctx_block_type_never_embedded(NEW.type_name, NEW.scope);
        -- A block marked here that holds a vector kept over a content change
        -- (stale) drops it: no reader ranks it, and the stale-pick indexes of
        -- migration 173 know no marker (the block would sit in them for good).
        IF NEW.embed_excluded AND NEW.embedding_stale THEN
            NEW.embedding        := NULL;
            NEW.embed_model      := NULL;
            NEW.embedding_stale  := false;
            NEW.embedding_next   := NULL;
            NEW.embed_model_next := NULL;
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

COMMENT ON FUNCTION context_blocks_embed_excluded() IS
    'trg_block_embed_excluded (migrations 175, 176): sets context_blocks.embed_excluded from the registry of the tenant that owns the block''s scope on insert, re-type, scope move and unarchive.';

CREATE OR REPLACE TRIGGER trg_block_embed_excluded
    BEFORE INSERT OR UPDATE OF type_name, scope, embed_excluded, is_archived ON context_blocks
    FOR EACH ROW EXECUTE FUNCTION context_blocks_embed_excluded();

CREATE OR REPLACE FUNCTION ctx_embed_excluded_queue(p_name TEXT, p_grp TEXT) RETURNS void
LANGUAGE sql AS $$
    INSERT INTO context_embed_excluded_queue (name, grp) VALUES (p_name, p_grp)
        ON CONFLICT (name, grp) DO UPDATE SET queued_at = clock_timestamp()
$$;

COMMENT ON FUNCTION ctx_embed_excluded_queue(TEXT, TEXT) IS
    'Queues (name, group) for the embed_excluded reconcile, or renews its queued_at (migration 176).';

CREATE OR REPLACE FUNCTION context_block_types_embed_excluded() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_side  RECORD;
    v_moved BOOLEAN;
    v_grp   TEXT;
    v_again TEXT;
BEGIN
    IF TG_OP = 'UPDATE'
       AND NEW.name IS NOT DISTINCT FROM OLD.name
       AND NEW.scope IS NOT DISTINCT FROM OLD.scope
       AND (NEW.config->'retrieval'->>'policy') IS NOT DISTINCT FROM (OLD.config->'retrieval'->>'policy') THEN
        RETURN NULL;            -- the answer depends on (name, scope, policy) only
    END IF;
    -- moved: the row of (name, scope) appears or disappears (an insert, a
    -- delete, or an update of name or scope), not just its policy.
    v_moved := TG_OP <> 'UPDATE' OR NEW.name IS DISTINCT FROM OLD.name OR NEW.scope IS DISTINCT FROM OLD.scope;
    FOR v_side IN
        SELECT s.name, s.scope, s.policy FROM (VALUES
            (CASE WHEN TG_OP <> 'INSERT' THEN OLD.name END,
             CASE WHEN TG_OP <> 'INSERT' THEN OLD.scope::text END,
             CASE WHEN TG_OP <> 'INSERT' THEN OLD.config->'retrieval'->>'policy' END),
            (CASE WHEN TG_OP <> 'DELETE' THEN NEW.name END,
             CASE WHEN TG_OP <> 'DELETE' THEN NEW.scope::text END,
             CASE WHEN TG_OP <> 'DELETE' THEN NEW.config->'retrieval'->>'policy' END)) AS s(name, scope, policy)
         WHERE s.name IS NOT NULL
    LOOP
        IF v_side.scope = '_global' THEN
            -- Every group's R holds this row. Its answer can turn when the row
            -- is or was excluded, or when the row appears or disappears while
            -- the name has rows in other scopes (a non-excluded _global row
            -- lifts or stops lifting their all-excluded groups).
            IF v_side.policy = 'excluded' THEN
                PERFORM pg_advisory_xact_lock(hashtext('ctx:embed_excluded:' || v_side.name));
                PERFORM ctx_embed_excluded_queue(v_side.name, '*');
            ELSIF v_moved THEN
                -- The lock first, then the read on the next statement's fresh
                -- snapshot: a tenant row written meanwhile is seen, or waits.
                PERFORM pg_advisory_xact_lock(hashtext('ctx:embed_excluded:' || v_side.name));
                IF EXISTS (SELECT 1 FROM context_block_types t
                            WHERE t.name = v_side.name AND t.scope <> '_global') THEN
                    PERFORM ctx_embed_excluded_queue(v_side.name, '*');
                END IF;
            END IF;
        ELSIF v_side.policy = 'excluded' OR v_moved THEN
            -- A row of a tenant or tenantless scope reaches the groups of its
            -- scope only. The name's lock orders it against block writes and
            -- the _global row; the group's lock against an assignment of its
            -- scope, after which the group is read once more.
            PERFORM pg_advisory_xact_lock(hashtext('ctx:embed_excluded:' || v_side.name));
            v_grp := ctx_embed_excluded_group(v_side.scope);
            PERFORM pg_advisory_xact_lock_shared(hashtext('ctx:embed_excluded:grp:' || v_grp));
            PERFORM ctx_embed_excluded_queue(v_side.name, v_grp);
            v_again := ctx_embed_excluded_group(v_side.scope);
            IF v_again IS DISTINCT FROM v_grp THEN
                PERFORM pg_advisory_xact_lock_shared(hashtext('ctx:embed_excluded:grp:' || v_again));
                PERFORM ctx_embed_excluded_queue(v_side.name, v_again);
            END IF;
        END IF;
    END LOOP;
    RETURN NULL;
END;
$$;

COMMENT ON FUNCTION context_block_types_embed_excluded() IS
    'trg_block_types_embed_excluded (migrations 175, 176): queues (type name, group) for the embed_excluded reconcile when a registry change may turn the never-embedded answer of the groups its row is read for.';

CREATE OR REPLACE TRIGGER trg_block_types_embed_excluded
    AFTER INSERT OR UPDATE OF name, scope, config OR DELETE ON context_block_types
    FOR EACH ROW EXECUTE FUNCTION context_block_types_embed_excluded();

CREATE OR REPLACE FUNCTION context_tenant_scopes_embed_excluded() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_groups  TEXT[] := '{}';
    v_scopes  TEXT[] := '{}';
    v_tenants UUID[] := '{}';
    v_g       TEXT;
    v_name    TEXT;
BEGIN
    IF TG_OP = 'UPDATE'
       AND NEW.scope IS NOT DISTINCT FROM OLD.scope
       AND NEW.tenant_id IS NOT DISTINCT FROM OLD.tenant_id THEN
        RETURN NULL;
    END IF;
    IF TG_OP <> 'INSERT' THEN
        v_groups  := v_groups || ('t:' || OLD.tenant_id::text) || ('s:' || OLD.scope::text);
        v_scopes  := v_scopes || OLD.scope::text;
        v_tenants := v_tenants || OLD.tenant_id;
    END IF;
    IF TG_OP <> 'DELETE' THEN
        v_groups  := v_groups || ('t:' || NEW.tenant_id::text) || ('s:' || NEW.scope::text);
        v_scopes  := v_scopes || NEW.scope::text;
        v_tenants := v_tenants || NEW.tenant_id;
    END IF;
    -- The groups' locks first, in one order for every assignment, then the
    -- registry on the next statement's fresh snapshot.
    FOR v_g IN SELECT DISTINCT g FROM unnest(v_groups) AS g ORDER BY g LOOP
        PERFORM pg_advisory_xact_lock(hashtext('ctx:embed_excluded:grp:' || v_g));
    END LOOP;
    FOR v_name IN
        SELECT DISTINCT t.name FROM context_block_types t
         WHERE t.scope <> '_global'
           AND (t.scope = ANY(v_scopes)
                OR t.scope IN (SELECT o.scope FROM context_tenant_scopes o WHERE o.tenant_id = ANY(v_tenants)))
         ORDER BY t.name
    LOOP
        FOR v_g IN SELECT DISTINCT g FROM unnest(v_groups) AS g ORDER BY g LOOP
            PERFORM ctx_embed_excluded_queue(v_name, v_g);
        END LOOP;
    END LOOP;
    RETURN NULL;
END;
$$;

COMMENT ON FUNCTION context_tenant_scopes_embed_excluded() IS
    'trg_tenant_scopes_embed_excluded (migration 176): queues (type name, group) for the embed_excluded reconcile when a scope is assigned to a tenant, leaves it or moves, for every name with a row in a scope of the tenants involved or in the scope.';

CREATE OR REPLACE TRIGGER trg_tenant_scopes_embed_excluded
    AFTER INSERT OR UPDATE OF scope, tenant_id OR DELETE ON context_tenant_scopes
    FOR EACH ROW EXECUTE FUNCTION context_tenant_scopes_embed_excluded();

-- The names whose answer this rule may change: every name with a row outside
-- _global, for every group.
INSERT INTO context_embed_excluded_queue (name, grp)
SELECT DISTINCT t.name, '*' FROM context_block_types t
 WHERE t.scope <> '_global'
ON CONFLICT (name, grp) DO NOTHING;
