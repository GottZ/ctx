-- =============================================================================
-- 055_block_sensitivity.sql — Content-Sensitivität pro Block (F3-P3 Trust-Gating)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Default 'credentials' = fail-closed (E1): unklassifizierte Blöcke erreichen
-- ausschließlich full-trust-Backends; der Normalbetrieb (lokal + LAN, alle
-- full-trust) ist davon unberührt, nur das künftige externe Netz (G29) bleibt
-- dunkel, bis klassifiziert ist. PG18: ADD COLUMN mit non-volatile DEFAULT
-- ist metadata-only (kein Rewrite) — gilt auch bei 1M+ Zeilen.
-- CHECK bewusst hart (wie block_role, anders als das generalisierte scope):
-- die Stufe geht in einen numerischen Rang-Vergleich — ein unbekannter Wert
-- wäre kein "neuer Tenant-Wert", sondern ein kaputtes Gate.
-- sensitivity_source + sensitivity_audited_at (K7, G41-Naht): der LLM-Audit
-- klassifiziert nur source='default'-Blöcke; 'manual' ist unantastbar;
-- audited_at trägt Idempotenz + Re-Audit-Fähigkeit.
-- =============================================================================

ALTER TABLE context_blocks
    ADD COLUMN IF NOT EXISTS sensitivity TEXT NOT NULL DEFAULT 'credentials'
        CHECK (sensitivity IN ('credentials','personal','internal','public')),
    ADD COLUMN IF NOT EXISTS sensitivity_source TEXT NOT NULL DEFAULT 'default'
        CHECK (sensitivity_source IN ('default','llm-audit','pattern','manual')),
    ADD COLUMN IF NOT EXISTS sensitivity_audited_at TIMESTAMPTZ;

-- Klassifizierungs-Fortschritt für UI/Stats (count GROUP BY sensitivity):
CREATE INDEX IF NOT EXISTS idx_blocks_sensitivity
    ON context_blocks (sensitivity) WHERE NOT is_archived;

INSERT INTO _migrations (version, filename, applied_at)
VALUES (55, '055_block_sensitivity.sql', now()) ON CONFLICT (version) DO NOTHING;
