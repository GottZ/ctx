-- =============================================================================
-- 032_audit_blocks_is_meta.sql — Korpus-Hygiene: ctx-system audit-blocks (Welle 38b)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Persistiert die ad-hoc is_meta=TRUE Markierung der 12 ctx-system-meta-blocks,
-- die während Welle 38b (2026-05-06) als top-5-noise für Cyclic-Queries
-- identifiziert wurden. Pattern bekannt aus S32 (MEMORY.md): welle-/session-/
-- audit-blocks dominieren als semantic-Distraktoren bei "donnerstags|mittwochs|
-- am wochenende"-Queries → C-Bucket Regression -16.7pp.
--
-- Kurzfrist-Mitigation (jetzt): is_meta=TRUE excludiert sie aus PickBlock
-- (dream pipeline schreibt keine neuen Links zu/von ihnen).
--
-- Strukturelle Lösung folgt in Welle 39: ctx_rrf is_meta-aware Filter (M033),
-- damit auch Retrieval die Drift-blocks ignoriert.
--
-- Idempotent: WHERE NOT is_meta — bei Re-Run kein Side-Effect.
--
-- Folge-Welle 40: ctx_save sollte automatic is_meta=TRUE für category=learnings
-- + tag~welle|session|audit setzen (kein reaktive Migration mehr nötig).
-- =============================================================================

UPDATE context_blocks SET is_meta = TRUE
WHERE id::text IN (
  '019dfeca-15f6-7534-b752-fd00b907e304',  -- Welle 38b HOLD-Audit (heute)
  '019dfe9e-5790-7196-ab10-adb47d03d22d',  -- Welle 38a NULL-RESULT (heute)
  '019dfa6b-cb82-7bb4-841a-34c9e008c161',  -- Welle 35/36/37 + v1.1.0
  '019df9e4-1070-746f-a0de-4801e164b324',  -- Welle 34/34a/34b
  '019df997-dbc9-7ab3-96ab-551f1ca67334',  -- Welle 33
  '019df938-2074-7a69-9950-181664744cc8',  -- Session 31 Welle 32
  '019df92c-eb19-7016-a175-2bf9a093e547',  -- Session 30 Welle 31
  '019df8ee-023d-74ee-8f64-ba39c5ff441c',  -- Session 28 Multi-Path-Bench
  '019df910-6647-7c5e-abfc-65fb9a9f29e9',  -- Session 29 Performance-Bench
  '019df858-e16e-7c7b-abb0-b119f3853a66',  -- Session 27 Behaviour-Layer
  '019defa4-c0f6-7c09-8e67-618f4d2af081',  -- Dream V3 Performance Audit
  '019df3e4-253a-7f0f-86e2-bc0eba4a6539',  -- Bench-Falle endogenes Sampling
  '019df23d-149a-7439-87f8-dcd3a0be47bb'   -- Bench-Setup qwen3.6:27b
)
AND NOT is_meta;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (32, '032_audit_blocks_is_meta.sql', now())
  ON CONFLICT (version) DO NOTHING;
