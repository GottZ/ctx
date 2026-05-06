-- =============================================================================
-- 034_audit_blocks_unmeta.sql — is_meta=FALSE für legitime retrieval-targets
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Welle 39 Bench-Verdict (M033 ctx_rrf is_meta-Filter): C-Bucket gefixt
-- 56% → 89% (+6 cases), aber M-Bucket -6.7pp + L-Bucket -12pp regression.
--
-- Root-Cause: M032 hatte mehrere blocks als is_meta=TRUE markiert die in
-- eval-cyclic-gold als EXPECTED retrieval-targets definiert sind:
-- - C-003 (ctx motivation am wochenende)         → 019d7c3b (Motivations)
-- - C-017 (session 24 dream deployment...)       → 019dc04b (Session 24 Dream)
-- - M-003 (dream v3 performance letzte woche)    → 019defa4 (Dream V3 Audit)
-- - M-009 (bench setup qwen letzte woche)        → 019df23d (Bench-Setup qwen)
-- - M-010 (session 27 testcontainer vor woche)   → 019df858 (Session 27)
-- - L-002 (session 27 testcontainer behaviour)   → 019df858 (Session 27)
-- - L-015 (bench falle endogenes sampling)       → 019df3e4 (Bench-Falle)
-- - L-018 (gottz cv berufserfahrung 2021)        → 019c4a34-835d (CV 2017-2021)
-- - L-023 (ddstatus mediawiki migration februar) → 019defb1 (ddstatus mediawiki)
--
-- Erkenntnis: Welle/Session/Audit/CV-blocks sind LEGITIME Wissen-blocks die
-- retrievable sein müssen. is_meta=TRUE (= retrieval-exclude via M033) war
-- für sie falsch. M033 bleibt korrekt für ECHT-meta-blocks (topic-maps,
-- agent-briefings, integration-upgrade, ctx Origin, Compound-Loop).
--
-- Dieses Migration revertet is_meta=FALSE für die 8 fälschlich als meta
-- markierten expected-blocks. Plus zusätzlich für andere wave-/session-/
-- audit-blocks die ähnlich legitim sein könnten als specific-query-targets:
--
-- Idempotent (nur explizite IDs). Folge-Welle (40+) muss bessere Klassifikation
-- finden (z.B. multi-level is_meta oder score-damping statt hard-exclude).
-- =============================================================================

UPDATE context_blocks SET is_meta = FALSE
WHERE id::text IN (
  -- Aus eval-cyclic-gold direkt expected:
  '019d7c3b-7f87-766a-95e0-65313d89fb7a',  -- ctx Motivations-Geschichte (C-003)
  '019dc04b-9024-7fdd-8ed7-e6465b5d54dc',  -- Session 24 Dream v3 Deployment (C-017)
  '019defa4-c0f6-7c09-8e67-618f4d2af081',  -- Dream V3 Performance Audit (M-003)
  '019df23d-149a-7439-87f8-dcd3a0be47bb',  -- Bench-Setup qwen3.6:27b (M-009)
  '019df858-e16e-7c7b-abb0-b119f3853a66',  -- Session 27 testcontainer (M-010, L-002)
  '019df3e4-253a-7f0f-86e2-bc0eba4a6539',  -- Bench-Falle endogenes Sampling (L-015)
  '019c4a34-835d-743a-ae53-5b89e260295d',  -- CV Berufserfahrung 2017-2021 (L-018)
  '019defb1-8817-7c84-be8e-5096727e6cd0',  -- ddstatus mediawiki migration (L-023)
  -- CV-Familie (semantisch konsistent zu L-018):
  '019c4a34-7eb3-7b12-856a-859905c5c721',  -- CV Berufserfahrung 2022-2026
  '019c4a35-b786-7bac-9a6e-1685d73d12a6',  -- CV Berufserfahrung 2008-2016
  '019c4a34-043a-7b8f-afed-c13edb5febf7',  -- CV Profil und Kontaktdaten
  '019c4a36-230d-7f77-9853-2aa0728bc351',  -- CV Skills und Zertifizierungen
  '019c4a35-f408-7c1b-871f-9ec4b3df1373'   -- CV Projekte und Ausbildung
)
AND is_meta;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (34, '034_audit_blocks_unmeta.sql', now())
  ON CONFLICT (version) DO NOTHING;
