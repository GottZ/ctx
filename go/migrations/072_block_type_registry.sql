-- =============================================================================
-- 072_block_type_registry.sql — dynamic block-type registry (workflow-engine
-- foundation). Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Workflow-engine axis 01, wave T3 (design/01-type-registry.md §3.2).
--
-- context_block_types: declarative per-type behaviour config. Mechanism stays
-- code (RRF fusion, guard similarity, dream loop, digest); HOW a type is
-- treated becomes data. Successor of the M035 block_role CHECK enum — the four
-- enum classes ship as builtin seed rows whose configs reproduce today's
-- hardcoded behaviour byte-equivalently (eval.sh is the gate). NOTE: no
-- consumer reads this table before wave T4+ — the M035 CHECK constraint on
-- context_blocks.type_name stays in force until 073 (fail-closed sequence,
-- design §3.4).
--
-- scope: '_global' sentinel = shipped/global namespace (F2/051 pattern; the
-- '_' prefix is enforced Go-side at api-key-create since M052). UNIQUE(name,
-- scope) carries all three tenancy tiers without a later constraint swap:
-- tier 1 global-only, tier 2 tenant overrides (tenant row shadows global row
-- of the same name), tier 3 tenant-own type names. Which tier is ENABLED is a
-- Go/actionTier concern, not a schema concern.
--
-- config JSONB, not typed columns: the policy vocabulary will grow with the
-- workflow engine (hooks envelope, board policy). Validation is Go-side
-- against a versioned schema (v2.0.0 line, M045: no hard value lists in the
-- schema). SQL never reads this JSONB — ctx_rrf/ctx_guard_check receive the
-- RESOLVED policy as bind parameters (policy-as-parameter pattern that
-- p_audit_trail_factor established in 039).
--
-- builtin rows: name+scope immutable, undeletable (Go-enforced); their config
-- IS editable (that is the point of the migration: damping factors, patterns
-- and thresholds become runtime-tunable). is_default: exactly one default
-- type per scope namespace (partial unique index) — the classifier falls back
-- to it when no rule matches.
--
-- updated_by deliberately without FK (051 audit line: references never
-- cascade; a key delete must not null history).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_block_types (
    id           UUID PRIMARY KEY DEFAULT uuidv7(),
    name         VARCHAR(50) NOT NULL,      -- Format-Gate in Go: ^[a-z0-9][a-z0-9-]{0,49}$
    scope        VARCHAR(50) NOT NULL DEFAULT '_global',
    display_name TEXT NOT NULL DEFAULT '',
    description  TEXT NOT NULL DEFAULT '',
    builtin      BOOLEAN NOT NULL DEFAULT false,
    is_default   BOOLEAN NOT NULL DEFAULT false,
    config       JSONB NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by   UUID,                      -- ohne FK (051-Linie)
    metadata     JSONB NOT NULL DEFAULT '{}',
    CONSTRAINT uq_block_types_name_scope UNIQUE (name, scope)
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_block_types_default
    ON context_block_types(scope) WHERE is_default;

-- Hot-Reload: derselbe Kanal wie Settings/Secrets/Backends/Quota.
-- notify_settings_write() (051, scope-erweitert 065) liest generisch
-- COALESCE(row->>'key', row->>'name') + row->>'scope' — passt für diese
-- Tabelle unverändert. Der Listener bekommt in T3 einen Entity-Branch
-- 'context_block_types' → Registry-Reload/InvalidateTenant (§4.3).
DROP TRIGGER IF EXISTS trg_block_types_notify ON context_block_types;
CREATE TRIGGER trg_block_types_notify
    AFTER INSERT OR UPDATE OR DELETE ON context_block_types
    FOR EACH ROW EXECUTE FUNCTION notify_settings_write();

-- Audit: eigene Trigger-Funktion (audit_settings_write() CASEt TG_TABLE_NAME
-- hart auf setting|secret — nicht wiederverwendbar ohne Umbau). Schreibt in
-- die bestehende generische History context_settings_audit (entity_type ist
-- CHECK-frei by design, 051-Kommentar). Type-Configs sind nie sensitiv →
-- old/new dürfen voll hinein.
--
-- Actor-Attribution (R1): current_setting('ctx.api_key_id') ist NUR gesetzt,
-- wenn der Go-Write in einer Tx mit setTxActor + SetTxRequestID läuft
-- (internal/store/settings.go; zweiter Nutzer: store/backends.go). T10 MUSS
-- alle type-*-Mutationen über dieses Muster verdrahten — plain pool.Exec
-- ergäbe via='sql', api_key_id NULL auf genau den Writes, die
-- Sichtbarkeits-Policy schalten (Provenienz-Verlust; das T10-Gate probt
-- via='api' positiv, design §7-T10).
CREATE OR REPLACE FUNCTION audit_block_type_write() RETURNS TRIGGER AS $$
DECLARE
    v_new   JSONB := to_jsonb(NEW);
    v_old   JSONB := to_jsonb(OLD);
    v_actor UUID  := NULLIF(current_setting('ctx.api_key_id', true), '')::uuid;
    v_label TEXT;
BEGIN
    IF v_actor IS NOT NULL THEN
        SELECT label INTO v_label FROM context_api_keys WHERE id = v_actor;
    END IF;
    INSERT INTO context_settings_audit
        (entity_type, entity_key, scope, action, old_value, new_value,
         api_key_id, actor_label, metadata)
    VALUES ('block_type',
        COALESCE(v_new->>'name', v_old->>'name'),
        COALESCE(v_new->>'scope', v_old->>'scope'),
        LOWER(TG_OP),
        v_old->'config', v_new->'config',
        v_actor, v_label,
        jsonb_strip_nulls(jsonb_build_object(
            'via', CASE WHEN v_actor IS NULL THEN 'sql' ELSE 'api' END,
            'request_id', NULLIF(current_setting('ctx.request_id', true), ''))));
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_block_types_audit ON context_block_types;
CREATE TRIGGER trg_block_types_audit
    AFTER INSERT OR UPDATE OR DELETE ON context_block_types
    FOR EACH ROW EXECUTE FUNCTION audit_block_type_write();

-- Seed: die 4 Enum-Klassen als builtin rows. Configs = heutiges Verhalten
-- BYTE-ÄQUIVALENT (Werte-Herkunft: Damping 0.3 = rrf.AuditTrailFactor;
-- Pattern-Liste = internal/rrf/pattern.go:25-36, 2× eingesetzt —
-- intent_patterns read-seitig, classify.title_patterns write-seitig;
-- guard.check/candidate=true überall = heutiger role-freier Guard-Batch;
-- dream.linkable=false nur system-meta = NOT is_meta; digest/overview
-- include=true überall = heutige Nicht-Filter; classify.priority 10 < 20 =
-- Reihenfolge des Decision-Trees internal/store/classify.go).
-- Das compiled-in Builtin-Set in internal/blocktype/builtin.go MUSS diesen
-- Seeds entsprechen — der Golden-Test appliziert diese Datei aus migrations.FS
-- und diff't die Rows gegen das Builtin-Set (Drift-Gate, design §4.1).
-- ON CONFLICT DO NOTHING = idempotent, überschreibt nie User-Tuning bei Re-Run.
INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config) VALUES
('knowledge', '_global', 'Knowledge', true, true, '{
  "v": 1,
  "retrieval": {"policy": "full-pass"},
  "guard":     {"check": true, "candidate": true},
  "dream":     {"linkable": true},
  "digest":    {"include": true},
  "overview":  {"include": true},
  "parent":    {"mode": "none"},
  "classify":  {}
}'::jsonb),
('reference', '_global', 'Reference', true, false, '{
  "v": 1,
  "retrieval": {"policy": "full-pass"},
  "guard":     {"check": true, "candidate": true},
  "dream":     {"linkable": true},
  "digest":    {"include": true},
  "overview":  {"include": true},
  "parent":    {"mode": "none"},
  "classify":  {}
}'::jsonb),
('audit-trail', '_global', 'Audit-Trail', true, false, '{
  "v": 1,
  "retrieval": {"policy": "damped", "damping_factor": 0.3,
                "intent_patterns": ["session","welle","audit","recurrent","handover",
                                    "self-audit","dream v","performance","reset","baseline"]},
  "guard":     {"check": true, "candidate": true},
  "dream":     {"linkable": true},
  "digest":    {"include": true},
  "overview":  {"include": true},
  "parent":    {"mode": "none"},
  "classify":  {"priority": 20,
                "source_prefixes": ["dream-"],
                "title_patterns": ["session","welle","audit","recurrent","handover",
                                   "self-audit","dream v","performance","reset","baseline"]}
}'::jsonb),
('system-meta', '_global', 'System-Meta', true, false, '{
  "v": 1,
  "retrieval": {"policy": "excluded"},
  "guard":     {"check": true, "candidate": true},
  "dream":     {"linkable": false},
  "digest":    {"include": true},
  "overview":  {"include": true},
  "parent":    {"mode": "none"},
  "classify":  {"priority": 10, "metadata_flags": ["is_meta"]}
}'::jsonb)
ON CONFLICT (name, scope) DO NOTHING;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (72, '072_block_type_registry.sql', now())
  ON CONFLICT (version) DO NOTHING;
