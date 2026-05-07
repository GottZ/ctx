-- =============================================================================
-- 040_audit_trail_classification_extension.sql — Audit-Trail-Klassifikation
-- erweitert (Welle 43)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Welle 40 hatte 12 audit-trail-Blocks via Sub-Agent-B-Klassifikation (n=20
-- candidates, 11 audit-trail + 9 knowledge identifiziert). Sub-Agent-A SQL-
-- Audit hatte aber 47 audit-trail-Kandidaten gefunden — 35 blieben unaudited.
--
-- Welle 43 schließt die Lücke: Sub-Agent-Audit n=34 candidates (Stand
-- 2026-05-07: 297 knowledge mit audit/welle/session-Heuristik-Match).
-- Klassifikations-Verteilung: 9 audit-trail / 25 knowledge / 2 whitelisted.
--
-- Whitelist (block_role bleibt 'knowledge', explizit geschützt):
--   - 019d33fd-...: "Session 7 — Go Migration gestartet" (M-013/C-006 expected)
--   - 019defb1-...: "ddstatus mediawiki migration audit" (L-023 expected)
-- Begründung: diese blocks sind in eval-cyclic-gold expected, ihre queries
-- haben KEIN audit-pattern — als audit-trail klassifiziert würden sie via
-- Welle-41-query-aware-damping (*0.3 ohne pattern) aus top-5 fallen.
--
-- Cross-Check (welle-43-classification.json): 9 audit-trail-IDs sind 0× in
-- eval-cyclic-gold expected — keine Bench-Regression-Risk durch UPDATE.
--
-- Pattern-Bestätigung der 9 IDs (alle Session-Handover/Audit-Trail-Format):
--   1. Session 4 Projekt-Status-Notiz (019d2bfc)
--   2. Warning #6/#8 RLHF-Bias Process-Audit (019d3980)
--   3. Session 2 Summary mit 59 Sub-Agent-Phasen (019d28e7)
--   4. Session 2 Meta-Erkenntnis Process-Audit (019d29af)
--   5. Session 7 Abschlussbilanz (019d3445)
--   6. Session 7 eval.sh Gate PASSED Bench-Snapshot (019d3444)
--   7. Session 3 Review Selbstkritik gegen warnings.md (019d2afa)
--   8. Session 9 TODO-Backlog priorisiert (019d398d)
--   9. LLM Temporal Normalization Test Results Session 9 (019d394b)
--
-- Idempotent (nur explizite IDs, AND block_role = 'knowledge' guard).
-- =============================================================================

UPDATE context_blocks SET block_role = 'audit-trail'
WHERE id::text IN (
  '019d2bfc-86ca-7310-ac16-650739b6e329',  -- Session 4 — Projekt ctx etabliert
  '019d3980-772b-7b4d-b9bf-6eae952277cd',  -- Warning #6/#8 — Persistenter RLHF-Bias (Session 7+9 Pattern)
  '019d28e7-3990-75a9-9786-bd0f45faa20d',  -- Session 2 Summary (2026-03-26)
  '019d29af-a201-7b5f-9aba-1da631d65c2b',  -- Session 2 Meta-Erkenntnis: Context-Engineering > Feature-Engineering
  '019d3445-6226-7320-affa-ab11c0bdf7c7',  -- Session 7 — Abschlussbilanz
  '019d3444-6c84-7463-98b2-1aec0b7ff2a3',  -- Session 7 — eval.sh Gate PASSED
  '019d2afa-7af0-7089-926b-42f49cdec845',  -- Session 3 Review — Selbstkritik gegen warnings.md
  '019d398d-6a03-76b4-84ef-6bc1060449ee',  -- Session 9 TODO-Backlog — Priorisiert nach Impact
  '019d394b-bc3f-7ac8-9e69-9039aafba31f'   -- LLM Temporal Normalization — Test Results Session 9
)
AND block_role = 'knowledge';

-- Validation (post-apply, comment-only):
-- SELECT block_role, COUNT(*) FROM context_blocks WHERE NOT is_archived
--   GROUP BY block_role ORDER BY COUNT(*) DESC;
-- Erwartung: knowledge ~288 (-9), audit-trail ~22 (+9), reference 121, system-meta 22

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (40, '040_audit_trail_classification_extension.sql', now())
  ON CONFLICT (version) DO NOTHING;
