-- =============================================================================
-- 077_workflow_status.sql — generic per-block workflow state (Achse 02, I-B)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- VALUE per block = column (mechanism); the SET of valid states, transitions,
-- board order and terminal flags = Achse-01 type config (policy=data). The Go
-- transition validator (blocktype.Set.ValidateTransition) enforces the per-type
-- state machine against the registry — a CHECK constraint would hard-couple the
-- schema to policy (M045 line: validation is a runtime concern, no CHECK).
--
-- Nullable, NO backfill: a non-workflow type stays NULL at zero storage cost
-- (PG null bitmap). NULL is the correct semantics "this block has no workflow"
-- for the entire knowledge corpus — lifting an existing type into workflow
-- semantics is a later, idempotent, own-wave UPDATE (§3.3).
--
-- Column name type_name per D1 (01 §3.4) — the policy axis; it already carries
-- the issue/comment discriminator (migration 071).
--
-- INDEX NOTE (063/069 house norm, tenant.go N11 comment): plain CREATE INDEX in
-- the single-Tx runner (store/migrations.go, CONCURRENTLY forbidden) holds a
-- SHARE lock for the build. Fine at today's ~1k rows; at 1M+ rows build the
-- index OUT-OF-BAND first with
--   CREATE INDEX CONCURRENTLY idx_blocks_workflow_board ...
-- and the IF NOT EXISTS below then finds it — this migration becomes a no-op.
-- Deploy-Runbook-Eintrag: §9.5.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE context_blocks
    ADD COLUMN IF NOT EXISTS workflow_status VARCHAR(50);

-- Board-/Listen-Pfad @10k+ Issues pro Repo: keyset-fähig, partial (nur
-- Workflow-Rows). Equality on (scope, type_name, workflow_status) plus the
-- keyset ordering (updated_at DESC, id DESC) makes ONE ordered index range scan
-- per board column — Sort-free with a row-comparison cursor (updated_at, id) <
-- (cur_updated, cur_id). The status-UNGEFILTERTE Liste runs as a per-status
-- merge in Go (one range scan per config status, k-way merge), never a Sort over
-- the whole scope (§3.3 Listen-Semantik, §6.2).
--
-- DEVIATION from the §3.3 sketch: the sketch wrote `... updated_at DESC, id`
-- (id ASC). A mixed-direction keyset (updated_at DESC, id ASC) is NOT a single
-- ordered btree range — Postgres plans it as a BitmapOr of the OR-form keyset
-- predicate + a top-N Sort (measured). id DESC makes the cursor a clean tuple
-- comparison matching the index direction ⇒ ordered range scan, no Sort. id is
-- only a unique tie-break, so the direction is semantically free (§7-I-B gate).
CREATE INDEX IF NOT EXISTS idx_blocks_workflow_board
    ON context_blocks (scope, type_name, workflow_status, updated_at DESC, id DESC)
    WHERE workflow_status IS NOT NULL AND NOT is_archived;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (77, '077_workflow_status.sql', now())
  ON CONFLICT (version) DO NOTHING;
