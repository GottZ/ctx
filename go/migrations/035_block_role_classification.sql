-- =============================================================================
-- 035_block_role_classification.sql — Block-Role 4-Klassen-Enum (Welle 40)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Welle 39 (M033) ctx_rrf is_meta-aware Filter ist binär: hard-exclude oder
-- full-pass. M034 musste 13 fälschlich als is_meta=TRUE markierte legitime
-- retrieval-targets reverten — der binäre Mechanismus konnte audit-trail von
-- knowledge nicht differenzieren.
--
-- Welle 40 Architektur C (Hybrid) löst das durch 4-Klassen-Enum block_role:
--
--   system-meta  — topic-maps, agent-briefings, integration-upgrade, ctx
--                  Origin/Compound-Loop. Hard-exclude in ctx_rrf (analog
--                  is_meta=TRUE). Für Retrieval irrelevant.
--   audit-trail  — Session-Handover, Welle-Audit, Bench-Snapshots. Score-
--                  Damping *0.3 in ctx_rrf — sichtbar bei explicit-target
--                  queries, gedämpft als noise gegen knowledge-blocks.
--   reference    — CV, Profile, Kontaktdaten. Full-pass (gleich wie knowledge).
--                  Eigene Klasse für künftige Filter (z.B. category-only-Mode).
--   knowledge    — Default. Alle Decisions, Erkenntnisse, Projekt-Wissen,
--                  Architektur, Bugs. Full-pass.
--
-- Decision-Quelle: .project/bench-dream-phase0/BRANCH-HYPOTHESIS-Welle-40-
-- Klassifikation.md (Architektur C Hybrid).
--
-- Backfill-Strategie (deterministisch, idempotent):
--   1. is_meta=TRUE → block_role='system-meta' (hard-exclude bleibt, M033-
--      Semantik wird via M036 auf block_role!='system-meta' umgestellt)
--   2. category='reference' UND NOT is_meta → block_role='reference'
--   3. Hardcoded ID-Liste der 12 Sub-Agent-B-klassifizierten audit-trail-
--      blocks → block_role='audit-trail' (nur wenn NOT is_meta; system-meta
--      hat Vorrang, keine Doppelklassifikation)
--   4. Rest bleibt auf DEFAULT 'knowledge'
--
-- Index-Strategie: partial Index nur auf nicht-knowledge blocks (~50 von 449
-- aktiven). Sparse-aware, klein genug um in shared_buffers zu bleiben.
--
-- Idempotent: ALTER ADD COLUMN ... DEFAULT setzt Werte nur initial. CHECK
-- constraint additiv. UPDATE-Backfill mit AND-Bedingungen erlaubt Re-Run
-- ohne Effekt.
-- =============================================================================

ALTER TABLE context_blocks
  ADD COLUMN IF NOT EXISTS block_role TEXT NOT NULL DEFAULT 'knowledge';

ALTER TABLE context_blocks
  DROP CONSTRAINT IF EXISTS context_blocks_block_role_check;

ALTER TABLE context_blocks
  ADD CONSTRAINT context_blocks_block_role_check
  CHECK (block_role IN ('system-meta', 'audit-trail', 'reference', 'knowledge'));

CREATE INDEX IF NOT EXISTS idx_context_blocks_block_role
  ON context_blocks(block_role)
  WHERE block_role != 'knowledge';

-- Backfill 1: system-meta aus is_meta=TRUE (≈ 19 blocks).
-- M033/M036 Semantik bleibt hard-exclude erhalten.
UPDATE context_blocks
   SET block_role = 'system-meta'
 WHERE is_meta = TRUE
   AND block_role = 'knowledge';

-- Backfill 2: reference aus category='reference' (≈ 121 blocks).
-- Nur wenn NOT is_meta (system-meta hat Vorrang).
UPDATE context_blocks
   SET block_role = 'reference'
 WHERE NOT is_meta
   AND category = 'reference'
   AND block_role = 'knowledge';

-- Backfill 3: audit-trail (12 IDs aus Sub-Agent-B Klassifikation).
-- Nur wenn NOT is_meta — system-meta hat Vorrang. UUIDs sind Session-
-- handovers, Welle-Audits, Bench-Setups die als specific-target queries
-- erreichbar bleiben sollen, aber als noise gegen knowledge-blocks gedämpft.
UPDATE context_blocks
   SET block_role = 'audit-trail'
 WHERE id::text IN (
   '019df858-e16e-7c7b-abb0-b119f3853a66', -- Session 27 testcontainer (M034-revert audit-trail)
   '019dc04b-9024-7fdd-8ed7-e6465b5d54dc', -- Session 24 Dream v3 (M034-revert audit-trail)
   '019defa4-c0f6-7c09-8e67-618f4d2af081', -- Dream V3 Performance Audit 2026-05-03 (M034-revert)
   '019d74a6-f1ec-7894-93eb-438bafc5d303', -- ctx Security Audit 2026-04-10
   '019d7430-5319-7d7b-ae61-be11e7967666', -- Claude Chat MCP Feedback Session 23
   '019d73f3-9d31-7593-a13d-0a124ad066e4', -- Session 23 Handover
   '019d6f2c-cfe3-7a2a-8ea1-03727c5fdec0', -- Session 22 T02-T13 Audit-Ergebnis
   '019d4d68-8c28-7cf9-b922-b9783768eec4', -- Session 18 Workflow Improvements
   '019d41c4-d2ff-7023-9087-947f2e1272ab', -- Dream Mode Phase 1 Implementation Session 15
   '019d40fc-c07e-7ce6-b19c-ba0bf6b9f0d1', -- Session 14 CI/CD Fix
   '019d3dd4-0b2c-7014-9c9d-7af073086744', -- Session 11 Warning-Versagen
   '019d3bea-eea1-7c13-a671-41971bdd3392'  -- Session 11 Go CLI + Ingestion Pipeline
 )
   AND NOT is_meta
   AND block_role = 'knowledge';

-- Validation (post-apply, comment-only — nicht executable):
-- SELECT block_role, COUNT(*) FROM context_blocks
--  WHERE NOT is_archived GROUP BY block_role ORDER BY COUNT(*) DESC;
-- Erwartung: knowledge >> reference (~121) > system-meta (~19) > audit-trail (~12)

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (35, '035_block_role_classification.sql', now())
  ON CONFLICT (version) DO NOTHING;
