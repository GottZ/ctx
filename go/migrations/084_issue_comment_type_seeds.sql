-- =============================================================================
-- 084_issue_comment_type_seeds.sql — issue/comment block-type seeds
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Workflow-engine axis 02, Welle I-C (design/02-issue-workflow.md §4.1). Two
-- builtin type-config rows registered in the context_block_types registry
-- (migration 072). Behaviour stays code (RRF, guard, dream, digest, overview);
-- HOW an issue/comment is treated is this DATA. The NOTIFY + audit triggers and
-- the table itself come from 072 — this migration only INSERTs rows.
--
-- The configs MUST decode byte-equivalently to the compiled-in builtin set in
-- internal/blocktype/builtin.go (issue/comment entries): the golden integration
-- test applies THIS file from migrations.FS and diffs the decoded rows against
-- the builtin set (drift gate, design/01 §4.1 R1). ON CONFLICT DO NOTHING keeps
-- the seed idempotent and never overwrites operator tuning on re-run.
--
-- ── issue ────────────────────────────────────────────────────────────────────
-- retrieval full-pass; guard participates in FLAG mode (a duplicate issue is
-- surfaced via a possible_duplicate flag, NEVER auto-archived, §4.7) restricted
-- to its own scope (guard.candidates=same-scope — an issue never matches a
-- cross-tenant block), with per-type thresholds 0.97/0.90; dream links issues.
-- digest.include=false AND overview.include=false: at 10k+ issues/repo the
-- topic-map (digest) and the Louvain overview clustering would drown otherwise
-- (§6.8 — the LOOP overview gate). workflow is the backlog→in-progress→done
-- state machine with the forge open/closed mapping (§4.2). structural_link_
-- classes = the write allowlist for context_structural_links edges of issues.
--
-- ── comment (INTERIM at I-C, FLIPPED by migration 085 at I-E) ─────────────────
-- Kept out of every autonomous pipeline: guard.check=false, guard.candidate=
-- false, dream.linkable=false, digest.include=false, overview.include=false
-- (all exact §4.1). At I-C this row DEVIATED from §4.1 in TWO fields, because
-- their mechanisms did not ship yet and the strict decoder rejects them fail-
-- closed:
--   * §4.1 wants retrieval=aggregate-to-parent, but the fold mechanism (Achse-01
--     T11) had NO consumer (Set.AggregateTypes unused) — accepting it would have
--     let comments leak raw into results. Interim: retrieval=excluded (the safe
--     subset: comment invisible, never leaked).
--   * §4.1 wants parent.mode=required + relationship=comment-of, but the
--     parent_id WRITE path (store.PutBlockParent) had no production caller (its
--     consumer is I-D's InsertCommentBlock) — required would have been silently
--     ineffective (§5.2). Interim: parent.mode=none.
-- RESOLVED: Welle I-E ships migration 085, which UPDATEs THIS row (in the same
-- lockstep as builtin.go) to the §4.1 target retrieval=aggregate-to-parent +
-- parent.mode=required/comment-of, now that T11 (fold) and I-D (parent_id write)
-- are both live. 084 stays the INTERIM seed (ON CONFLICT DO NOTHING, no rewrite);
-- 085 is the deliberate correcting UPDATE. Handoff recorded in design §9.1a.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config) VALUES
('issue', '_global', 'Issue', true, false, '{
  "v": 1,
  "retrieval": {"policy": "full-pass"},
  "guard":     {"check": true, "candidate": true, "mode": "flag", "candidates": "same-scope",
                "threshold_duplicate": 0.97, "threshold_review": 0.90},
  "dream":     {"linkable": true},
  "digest":    {"include": false},
  "overview":  {"include": false},
  "parent":    {"mode": "none"},
  "workflow":  {"states": ["backlog", "in-progress", "done"], "initial": "backlog",
                "terminal": ["done"], "forge_state_map": {"open": "backlog", "closed": "done"}},
  "structural_link_classes": ["references", "duplicate-of"],
  "classify":  {}
}'::jsonb),
('comment', '_global', 'Comment', true, false, '{
  "v": 1,
  "retrieval": {"policy": "excluded"},
  "guard":     {"check": false, "candidate": false},
  "dream":     {"linkable": false},
  "digest":    {"include": false},
  "overview":  {"include": false},
  "parent":    {"mode": "none"},
  "classify":  {}
}'::jsonb)
ON CONFLICT (name, scope) DO NOTHING;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (84, '084_issue_comment_type_seeds.sql', now())
  ON CONFLICT (version) DO NOTHING;
