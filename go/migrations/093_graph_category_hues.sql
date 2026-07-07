-- =============================================================================
-- 093_graph_category_hues.sql — per-category HUE override for the graph (AM-2,
-- Design plan-webux 02a, Wellen-Achse U02-W5)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- AM-2: the node/cluster colour of a block CATEGORY is a seeded hash by default
-- (categoryColor → hashHue, graph-client.ts); this table is the OPTIONAL override
-- layer. Resolution chain (02a §A3): tenant-override → global-override → hash
-- seed. Only the HUE (HSL degree 0–359) is overridden — sat/lum stay theme
-- tokens, so every override lands inside the range the G1a contrast sweep already
-- covers (02a §A2: no override-specific contrast gate needed).
--
-- Scope discriminator like 092:34 (VARCHAR(50), Modell C — NO tenant_id on data
-- tables). UNIQUE(scope, category): one row per (scope, category), NOT a JSON
-- map — this kills the read-modify-write patch race (02a §A5) and gives per-key
-- precedence structurally (two concurrent overrides of different categories hit
-- different rows via ON CONFLICT).
--
-- No slug-CHECK on category: context_blocks.category is DB-side free (a Bestands-
-- Kategorie must not be excluded from an override); render-safety comes from the
-- FE-pin (02a §A1: Map structure + Svelte text-interpolation, {@html} banned).
-- The CHECK is length + no control chars only.
--
-- BEWUSST KEIN notify_settings_write-Trigger (02a §A1, Review-Korrektur, 2 Linsen
-- konvergent): there is NO server cache — the GET reads live (02a §A3/§A4-W5) —
-- and the Ist-Listener (events/listener.go:175-188) routes UNKNOWN entities into
-- the config-reload fall-through, so a _global hue edit would fire a full
-- settings.Reload + Dispatch-UpdateSettings across every tenant per PUT, zero
-- benefit at real churn. Should a server cache ever land, it needs its OWN
-- listener branch (entity='context_graph_category_hues'), never the fall-through.
-- =============================================================================

CREATE TABLE IF NOT EXISTS context_graph_category_hues (
    scope      VARCHAR(50) NOT NULL DEFAULT '_global',   -- discriminator like 092:34
    category   TEXT NOT NULL CHECK (char_length(category) BETWEEN 1 AND 128
                                    AND category !~ '[[:cntrl:]]'),
    hue        SMALLINT NOT NULL CHECK (hue >= 0 AND hue <= 359),  -- HSL degree (02a §A2)
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by UUID,
    CONSTRAINT uq_graph_cat_hue_scope_cat UNIQUE (scope, category)
);
-- The UNIQUE(scope, category) index already backs the precedence read
-- (WHERE scope = ANY(...)) — no additional index (02a §A1).

-- Audit WITHOUT redaction (a hue carries no secret) into the shared
-- context_settings_audit — function skeleton 1:1 from audit_disable_profiles_write
-- (092, EXECUTE-Muster 092:55,63): entity_type='graph_category_hue',
-- entity_key=COALESCE(NEW,OLD)->>'category', scope from the row, action per TG_OP,
-- via='api'|'sql' from ctx.api_key_id (NULL ⇒ 'sql' — covers psql/break-glass,
-- 092:72,91,129).
CREATE OR REPLACE FUNCTION audit_graph_category_hues_write() RETURNS TRIGGER AS $$
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
    VALUES (
        'graph_category_hue',
        COALESCE(v_new->>'category', v_old->>'category'),
        COALESCE(v_new->>'scope', v_old->>'scope', '_global'),
        CASE TG_OP WHEN 'INSERT' THEN 'create'
                   WHEN 'UPDATE' THEN 'update'
                   ELSE 'delete' END,
        v_old, v_new,
        v_actor, v_label,
        jsonb_strip_nulls(jsonb_build_object(
            'via',        CASE WHEN v_actor IS NULL THEN 'sql' ELSE 'api' END,
            'request_id', NULLIF(current_setting('ctx.request_id', true), '')))
    );
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_graph_category_hues_audit ON context_graph_category_hues;
CREATE TRIGGER trg_graph_category_hues_audit
    AFTER INSERT OR UPDATE OR DELETE ON context_graph_category_hues
    FOR EACH ROW EXECUTE FUNCTION audit_graph_category_hues_write();

-- No backfill: overrides are the exception layer — an empty table means every
-- category renders on its hash seed (the correct start behaviour, 02a §A1).

INSERT INTO _migrations (version, filename, applied_at)
VALUES (93, '093_graph_category_hues.sql', now()) ON CONFLICT (version) DO NOTHING;
