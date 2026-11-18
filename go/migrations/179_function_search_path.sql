-- =============================================================================
-- 179_function_search_path.sql — the functions PostgreSQL calls on a row write
-- resolve what they name under an empty search_path (issue #351)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- A trigger function runs with the search_path of the session that writes the
-- row. pg_restore sets search_path = '' (pg_dump puts it at the head of every
-- dump), and the trigger functions of ctx named their tables and helper
-- functions without a schema: a data-only restore with the triggers on failed
-- on its first block with 42883 (context_blocks_embed_excluded() calls
-- ctx_block_type_never_embedded), and every other function below fails the
-- same way, with 42883 for a function or 42P01 for a table, on the first row
-- of its table. The only way to restore data alone was --disable-triggers,
-- under which nothing keeps embed_excluded (migration 175) or the full-text
-- vectors (migration 178).
--
-- Every function that a trigger, a CHECK constraint, a default, a generated
-- column, an index expression or a trigger's WHEN clause calls, and that
-- names an object of its schema without a prefix, now carries
--   SET search_path = pg_catalog, <its schema>, pg_temp
-- the form of the PostgreSQL manual for functions that must not depend on the
-- caller's path: pg_catalog first, as it is searched anyway; the function's
-- own schema; pg_temp named last, so a temporary table of the writing session
-- cannot stand in for one of ctx's tables (without it pg_temp is searched
-- first for relations). The schema is the one each function lives in, read
-- from the catalog, not a literal: ctx creates its objects in the first schema
-- of the migrating role's search_path, which is public on every install the
-- schema contract knows. A helper a pinned function calls (ctx_block_type_
-- never_embedded, ctx_embed_excluded_group, ctx_embed_excluded_queue,
-- audit_actor_meta, audit_state_*, hist_add, llm_usage_hist_*) runs under its
-- caller's setting and keeps none of its own: a SET clause would cost it on
-- every call from the reconcile and the read paths, where the session's path
-- already finds it. context_blocks_fulltext() (178, SET search_path =
-- pg_catalog, names no table) and the functions that name nothing of the
-- schema (notify_*, model_cidrs_private) need nothing.
--
-- ALTER FUNCTION … SET adds the setting to the function's proconfig and keeps
-- what is there: mark_guard_dirty() keeps enable_nestloop = off and jit = off
-- (migration 177). CREATE OR REPLACE FUNCTION without the SET clauses would
-- drop them; this file replaces no body.
--
-- COST. A SET clause makes the function manager save, set and restore the
-- setting around every call (fmgr_security_definer). Measured on the store
-- (TestFunctionSearchPathCost in internal/store; numbers in the PR of #351):
-- see docs/operations.md, Migration 179.
--
-- LOCKS. ALTER FUNCTION updates the function's pg_proc row and takes no lock
-- on a table: a writer that holds context_blocks rows does not block it and
-- is not blocked by it; a statement that starts after the commit runs the
-- function with the new setting, one that is running keeps the old one.
-- `SET LOCAL lock_timeout = '3s'` as in the migrations before it bounds the
-- wait for another ALTER of the same function.
--
-- OLDER BINARY. A binary before this migration writes and reads as before:
-- the functions do what they did, under a fixed path that finds the same
-- objects. Its migration runner leaves version 179 alone. Its schema-contract
-- check finds _migrations:179 without an embedded file (breaking) and a
-- function body hash drift (param) for every function listed here, since
-- pg_get_functiondef prints the SET clause: in mode warn (the default) it
-- starts, logs them and /health reports schema_contract attention; in mode
-- enforce it refuses to start until CTX_CONTRACT_MODE=warn or off is set.
--
-- REVERT: ALTER FUNCTION <name>() RESET search_path for each function below;
-- DELETE FROM _migrations WHERE version = 179.
--
-- No table, column, index or trigger changes (test.sh T07 counts stay). The
-- schema-contract manifest records the new function definitions and
-- migration_max 178 -> 179. Idempotent, forward-only.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

DO $do$
DECLARE
    v_fn  regprocedure;
    v_nsp name;
BEGIN
    FOREACH v_fn IN ARRAY ARRAY[
        -- context_blocks
        'context_blocks_embed_excluded()',
        'context_blocks_retype()',
        'context_block_counts_ins()',
        'context_block_counts_upd()',
        'context_block_counts_del()',
        'context_block_counts_trunc()',
        'mark_guard_dirty()',
        -- registry, tenants, scopes
        'context_block_types_embed_excluded()',
        'context_tenant_scopes_embed_excluded()',
        'notify_model_access_write()',
        -- audit
        'audit_api_keys_write()',
        'audit_backends_write()',
        'audit_block_type_write()',
        'audit_disable_profile_backends_write()',
        'audit_disable_profiles_write()',
        'audit_gateway_limits_write()',
        'audit_graph_category_hues_write()',
        'audit_instance_write()',
        'audit_model_aliases_write()',
        'audit_model_consumers_write()',
        'audit_model_grants_write()',
        'audit_model_keys_write()',
        'audit_settings_write()',
        'audit_tenant_quota_write()',
        -- instance, LLM log, model gateway
        'instance_row_guard()',
        'llm_usage_rollup_ins()',
        'model_aliases_identity_immutable()',
        'model_consumer_deactivate_revokes()',
        'model_consumers_identity_immutable()',
        'model_grants_identity_immutable()',
        'model_keys_consumer_active()',
        'model_keys_identity_immutable()'
    ]::regprocedure[]
    LOOP
        SELECT n.nspname INTO v_nsp
          FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
         WHERE p.oid = v_fn;
        EXECUTE format('ALTER FUNCTION %s SET search_path = pg_catalog, %I, pg_temp', v_fn, v_nsp);
    END LOOP;
END
$do$;
