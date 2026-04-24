-- Meta-Block-Exclude: flag blocks that produce systematic NO_REL noise in Dream.
-- Validated empirically on 2026-04-20 post-reset audit:
--   - 6/7 NO_REL-Links stammten aus Meta-Block-Beteiligung
--   - 0/28 CORRECT-Links würden durch den Filter verloren
--   - Accuracy-Delta: +12.4pp
--
-- Meta = generische Selbstbeschreibung, Methodik-Reflexion, oder Meta-Index
-- ohne spezifisches Thema. Breite semantische Nähe zu vielem, inhaltlich nicht verbunden.
--
-- Definition (OR):
--   1. category ∈ ('agent-briefing', 'index')
--   2. title startet mit 'GottZ CV '
--   3. title matcht '(Origin-Story|Motivations-Geschichte|Persönlichkeitsprofil|Agent Briefing|Compound-Loop-Modell)'
--   4. title = 'integration-upgrade'
--
-- Reviewbar via: SELECT id, title, category FROM context_blocks WHERE is_meta;

ALTER TABLE context_blocks
    ADD COLUMN is_meta BOOLEAN NOT NULL DEFAULT false;

-- Initial-Population nach validierter Definition
UPDATE context_blocks SET is_meta = true
WHERE NOT is_archived
  AND (
    category IN ('agent-briefing', 'index')
    OR title LIKE 'GottZ CV %'
    OR title ~* '(Origin-Story|Motivations-Geschichte|Persönlichkeitsprofil|Agent Briefing|Compound-Loop-Modell)'
    OR title = 'integration-upgrade'
  );

-- Picker-friendly index für Dream-Eligibility-Query
CREATE INDEX IF NOT EXISTS idx_blocks_is_meta
    ON context_blocks (id)
    WHERE is_meta;
