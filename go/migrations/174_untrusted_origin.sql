-- =============================================================================
-- 174_untrusted_origin.sql — a re-type out of a type marked untrusted keeps
-- the type it left on the block, and a comment or an issue keeps its type
-- (issue #280)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- A block of a type whose registry policy carries retrieval.untrusted
-- (tool-evidence, tool-overview, checkpoint, insight on the shipped registry)
-- holds text an agent session produced: captured tool output, transcripts and
-- what was distilled from them. Readers frame such a block by its TYPE, so a
-- re-type to a type without the flag ends the framing. The re-type stays
-- possible on every write surface (owner decision on issue #280: the corpus
-- takes up knowledge an agent worked out); what this migration adds is the
-- record of where the text came from:
--
--   context_blocks.untrusted_origin  TEXT NULL
--
-- NULL: the block never left a type marked untrusted (every block before this
-- migration, and every block written since that was not re-typed out of one).
-- Otherwise the name of the FIRST type marked untrusted that the block left.
-- It survives every later re-type and every edit of title, content, tags or
-- metadata; the read surfaces (get, search, recent, the query sources) carry
-- it as `untrusted_origin` so a citation still says the text came from an
-- agent.
--
-- WHO WRITES IT: only the trigger below. trg_block_retype runs BEFORE UPDATE
-- OF type_name, untrusted_origin, FOR EACH ROW:
--   1. untrusted_origin is set back to the stored value — a statement that
--      names the column (no code path does) cannot set, overwrite or clear it.
--   2. A statement that leaves type_name as it is returns here.
--   3. A block of the issue domain's two types, `issue` and `comment`,
--      keeps its type: the statement fails with check_violation (23514) on
--      the constraint name ck_context_blocks_retype_bound. Out of comment, a
--      re-type took the block out of its issue's comment list (which filters
--      by type_name) while it kept the parent; out of issue, it left the
--      workflow status, the "#L<seq>" number and the issue's comments under a
--      block that is no issue, and took it out of the issue listings — also
--      for an issue whose forge state maps to no status (workflow_status
--      NULL). The names are the domain's own constants (store.IssueTypeName,
--      store.CommentTypeName), the two names trg_project_write (baseline)
--      fires on: the domain code is written for exactly these two types, not
--      for a registry property. A parent alone pins nothing — any type may
--      carry one since NZ-3. The domain paths never re-type such a row; the
--      store maps the error to a refusal (store.ErrRetypeBound). The way INTO
--      `issue` is a claim gate in Go (handler issueTypeReject), the way into
--      `comment` the parent gate (parentRequiredReject).
--   4. If the block has no record yet and the type it leaves is marked
--      untrusted by a registry row of that name, the record becomes that type.
--      The rows consulted are the _global row and the rows of the namespaces
--      of the tenant that owns the block's scope (context_tenant_scopes; a
--      tenant's types are keyed by the home scope of its keys,
--      Registry.SnapshotForRequest), plus the row of the block's own scope:
--      the trigger has no reader identity, and those are the registries the
--      block's own readers frame it by (overlays cannot clear the flag of a
--      _global type, D-INV N6). Rows of ANOTHER tenant are deliberately not
--      read: a writer could otherwise learn from its own block's record
--      whether some other tenant holds a type of a given name marked
--      untrusted — the oracle the claim gates close for unknown_type. A
--      reader of another tenant (a block grant, a key of another tenant that
--      may read the shared layer) is therefore not consulted; the builtin
--      types are _global rows and always are. The
--      predicate is DecodePolicy's: retrieval.untrusted absent is false, a
--      present value is a JSON boolean (a string is refused at decode).
--
-- Why a column and a trigger, not a reserved metadata key set by Go: metadata
-- is replaced wholesale by the upsert's content-changing conflict branch and
-- by every by-id metadata update, so a key would need a carry in every writer
-- of the map, present and future; and four statements re-type a block today
-- (manage update, the MCP update tool and its confirm, the store upsert, the
-- auto-classifier), each of which would have to remember the rule. The trigger
-- covers each one, decides on the locked row inside the statement (no read
-- ahead of the write), and fires only for a statement whose SET list names
-- type_name — inserts and every other update pay nothing.
--
-- COST at the 10M target: ADD COLUMN without a default is a catalog change
-- (no rewrite, NULL costs a bit in the null bitmap the row already has).
-- No index: nothing filters by the column. The trigger function runs once per
-- re-typed row; past the early return it reads context_block_types by name
-- (uq_block_types_name_scope leads with name; the table holds one row per type
-- and scope) and context_tenant_scopes by its primary key and tenant.
--
-- LOCKS. SET LOCAL lock_timeout = '3s' (house pattern 116/117/126/128/140/145/
-- 171/173). ADD COLUMN takes ACCESS EXCLUSIVE for the catalog write; CREATE OR
-- REPLACE TRIGGER takes SHARE ROW EXCLUSIVE. A transaction holding the table
-- longer fails the migration with 55P03, the file rolls back, the next boot
-- tries again.
--
-- No backfill: a block re-typed out of a type marked untrusted before this
-- migration left no trace to rebuild the record from.
--
-- REVERT (forward-only tree; to undo temporarily):
--     DROP TRIGGER IF EXISTS trg_block_retype ON context_blocks;
-- (re-types then neither record nor refuse; the column stays, a binary of this
-- change reads it). The column cannot be dropped while such a binary runs.
-- Rolling back to a binary from before this change: drop the trigger as
-- well, or a re-type of a comment or an issue answers that binary's uncoded
-- 500 instead of being written.
--
-- One column on context_blocks: the test.sh T07 column count moves by one.
-- The schema-contract manifest gains the column, the function, the trigger
-- and migration_max 173 -> 174.
-- Idempotent, forward-only. Registration by the runner (store.RunMigrations).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE context_blocks
    ADD COLUMN IF NOT EXISTS untrusted_origin TEXT;

COMMENT ON COLUMN context_blocks.untrusted_origin IS
    'The first type marked retrieval.untrusted that a re-type took this block out of, NULL if none (migration 174). Written only by trg_block_retype.';

CREATE OR REPLACE FUNCTION context_blocks_retype() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.untrusted_origin := OLD.untrusted_origin;
    IF NEW.type_name IS NOT DISTINCT FROM OLD.type_name THEN
        RETURN NEW;
    END IF;
    IF OLD.type_name IN ('issue', 'comment') THEN
        RAISE EXCEPTION 'block % is of the issue domain type % and keeps it', OLD.id, OLD.type_name
            USING ERRCODE = 'check_violation',
                  CONSTRAINT = 'ck_context_blocks_retype_bound';
    END IF;
    IF OLD.untrusted_origin IS NULL AND EXISTS (
        SELECT 1 FROM context_block_types t
         WHERE t.name = OLD.type_name
           AND t.config->'retrieval'->'untrusted' = 'true'::jsonb
           AND (t.scope IN ('_global', OLD.scope)
                OR t.scope IN (SELECT o.scope
                                 FROM context_tenant_scopes b
                                 JOIN context_tenant_scopes o ON o.tenant_id = b.tenant_id
                                WHERE b.scope = OLD.scope))
    ) THEN
        NEW.untrusted_origin := OLD.type_name;
    END IF;
    RETURN NEW;
END;
$$;

COMMENT ON FUNCTION context_blocks_retype() IS
    'trg_block_retype (migration 174): keeps untrusted_origin server-written, refuses a re-type of an issue or a comment (23514 ck_context_blocks_retype_bound), records the first untrusted type a block leaves.';

CREATE OR REPLACE TRIGGER trg_block_retype
    BEFORE UPDATE OF type_name, untrusted_origin ON context_blocks
    FOR EACH ROW EXECUTE FUNCTION context_blocks_retype();
