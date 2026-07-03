-- =============================================================================
-- 076_structural_links.sql — deterministic structural link layer (Achse 02, I-A)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Deterministic, forge-/system-derived relations between blocks. STRICTLY
-- SEPARATE from context_dream_links: dream edges are *discovered* (LLM,
-- confidence-gated; the replace-semantics of writelinks.go:38-78 delete
-- foreign-origin rows of a source), structural edges are *facts* (confidence
-- 1.0 by definition — hence NO confidence column). Vision anchor
-- 019e83df-9666: "Strukturelle Links NICHT in den Dream-Graph mischen".
--   K3 (00-masterplan §2): parent_id + FK CASCADE = the ONE structural parent
--   (comment-of); context_structural_links = all further structural classes
--   (references, duplicate-of, tracks, blocks-issue, …). dream_links stays
--   purely semantic; Dream replace/cleanup never touches this table
--   (negative gate I-A: dream.WriteLinks replace leaves structural edges intact).
--
-- link_class carries NO CHECK (M045 line: validation is a runtime concern — the
-- Achse-01 type registry validates classes in Go via structural_link_classes).
-- v1 seed classes: 'references' (issue→issue cross-ref, body parsing §4.5.7) and
-- 'duplicate-of' (manual / guard-flag confirmation). 'subissue-of', 'pr-linked',
-- 'milestone-of' are BACKLOG (not materialized as blocks / cost per-issue API
-- calls §6.1) — dead vocabulary is not seeded. comment→issue lives on
-- context_blocks.parent_id (001:39), NOT here — one fact, one place.
--
-- Same-scope is enforced app-side in the SAME Tx as the write (§4.2/§5.2):
-- source and target MUST share one scope; the scope column IS that scope.
-- ON DELETE CASCADE (unlike 016's bare FK): tenant prune batch-deletes blocks
-- (store/tenant.go:474-480) and must not gain a new drain step for THIS table;
-- a deliberate scope index avoids repeating the N11 seam (tenant.go:462-473).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_structural_links (
    source_block_id UUID NOT NULL REFERENCES context_blocks(id) ON DELETE CASCADE,
    target_block_id UUID NOT NULL REFERENCES context_blocks(id) ON DELETE CASCADE,
    link_class      TEXT NOT NULL,
    scope           VARCHAR(50) NOT NULL,          -- == Source-Scope == Target-Scope (Tx-validiert, §4.2)
    origin          TEXT NOT NULL DEFAULT 'forge-sync',  -- forge-sync | manual | system
    metadata        JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (source_block_id, target_block_id, link_class),
    CHECK (source_block_id != target_block_id)
);

-- Reverse traversal (issue-get / EgoGraph UNION leg): "who points AT me".
CREATE INDEX IF NOT EXISTS idx_struct_links_rev
    ON context_structural_links (target_block_id, link_class)
    INCLUDE (source_block_id);

-- Scope index from day 1 (N11 lesson, tenant.go:462-473) — @1M+ blocks a
-- scope-scan without it would seq-scan the whole edge table.
CREATE INDEX IF NOT EXISTS idx_struct_links_scope
    ON context_structural_links (scope);

-- parent_id-FK (E8): Achse-01-Annahme "+FK/Index durch Achse 02" (01 §9.1b).
-- The parent_id COLUMN already exists (001:39) and is indexed (idx_parent_id
-- 001:204) — this migration adds ONLY the FK constraint, no column, so
-- context_blocks column count is UNCHANGED (T07 oracle: cols stay 39).
-- NOT VALID + VALIDATE: no full-table-lock scan gated by the constraint add;
-- all parent_id are live NULL (grep: null Konsumenten), VALIDATE is a nullpass.
-- ON DELETE CASCADE: Issue-Delete räumt seine Comments; prune-kompatibel — the
-- batched ctid-deletes (tenant.go:474-480) treffen Parent und Kinder ohnehin
-- (gleicher Scope, Invariante §5.2), CASCADE macht die Reihenfolge egal.
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_blocks_parent') THEN
    ALTER TABLE context_blocks
      ADD CONSTRAINT fk_blocks_parent FOREIGN KEY (parent_id)
      REFERENCES context_blocks(id) ON DELETE CASCADE NOT VALID;
  END IF;
END $$;
ALTER TABLE context_blocks VALIDATE CONSTRAINT fk_blocks_parent;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (76, '076_structural_links.sql', now())
  ON CONFLICT (version) DO NOTHING;
