-- =============================================================================
-- 166_backend_provider_class_ctx.sql — 'ctx' joins the backend provider
-- classes
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- A peer ctx instance is a backend of its own kind (design/04 §4.12, wave
-- F-W5): an openai-wire row under a model key of the peer (ctxm_), whose
-- gateway error codes the classifier reads (backends.Classify, W02-11) and
-- whose dispatcher row counts as a local target for the Enforcing coverage
-- term (F-W7). The baseline (113, context_backends) pins provider_class to
-- the three classes the pool was designed with: generic, llamacpp,
-- openrouter. Without this migration the CHECK refuses the row the handler's
-- validation (backends.validateCtx) now accepts, and the create fails one
-- layer too late with a bare constraint error.
--
-- The constraint was declared inline and unnamed in the baseline, so its name
-- is whatever the server assigned (context_backends_provider_class_check on
-- every PostgreSQL this project supports). As in 155, the DO block resolves it
-- from the catalog by table + definition instead of trusting the derived
-- name, so a database whose constraint was ever renamed by hand migrates the
-- same way. Exactly one CHECK of the table mentions provider_class (113:
-- 5323-5324; no later migration touches the table's CHECKs but 155, whose
-- definition names protocol only), so the lookup is unambiguous.
--
-- Nothing else changes: the column keeps its DEFAULT 'generic', the schema
-- manifest records column types only (no CHECK clauses), existing rows are
-- untouched and no row is written. Without a ctx row the migration has no
-- effect on routing, admission or the wire.
--
-- Cost and locks. DROP CONSTRAINT and ADD CONSTRAINT ... CHECK take ACCESS
-- EXCLUSIVE on context_backends until the commit, and ADD CONSTRAINT checks
-- every existing row under it — a handful of pool rows (live: 8),
-- milliseconds, during which the pool reload and backend writes of another
-- ctx process on the same database wait. lock_timeout 3s fails the boot
-- instead of queueing behind a long transaction; the next start retries.
--
-- Idempotent: the catalog lookup finds the named constraint of a previous run
-- and replaces it with the identical definition.
--
-- Way back, in this order: delete or re-class every provider_class 'ctx' row
-- (an older binary refuses such a row in ValidateBackend on its next write
-- and does not know the class), then ALTER TABLE context_backends DROP
-- CONSTRAINT context_backends_provider_class_check; ALTER TABLE
-- context_backends ADD CONSTRAINT context_backends_provider_class_check CHECK
-- (provider_class IN ('generic','llamacpp','openrouter')); DELETE FROM
-- _migrations WHERE version = 166 (so a later upgrade applies it again).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

DO $$
DECLARE
    con_name text;
BEGIN
    SELECT conname INTO con_name
      FROM pg_constraint
     WHERE conrelid = 'context_backends'::regclass
       AND contype = 'c'
       AND pg_get_constraintdef(oid) LIKE '%provider_class%';
    IF con_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE context_backends DROP CONSTRAINT %I', con_name);
    END IF;
    ALTER TABLE context_backends
        ADD CONSTRAINT context_backends_provider_class_check
        CHECK (provider_class IN ('generic','llamacpp','openrouter','ctx'));
END
$$;
