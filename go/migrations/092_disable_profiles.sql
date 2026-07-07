-- =============================================================================
-- 092_disable_profiles.sql — Abschaltprofile (Design plan-webux 01, User-Auftrag
-- 2026-07-06; Wellen-Achse U01-W1)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Benannte Profile nehmen je eine Menge Backends aus jeder Chain (Wartungs-/
-- Eject-Abschaltung). Der bisher hartkodierte Gaming-Sonderfall (gaming.active +
-- Namensliste gaming.disabled_backends, config.go:531/537) wird zu EINEM Profil
-- unter mehreren; der Backfill unten kopiert Wert + Menge verlustfrei.
--
-- AM-5 (U01-E4=scoped): context_disable_profiles trägt scope ab W1; W1 legt nur
-- das Schema, der Sichtbarkeits-/Rechte-Filter kommt in W3. Das Backfill-Profil
-- ist scope='_global'. UNIQUE ist (scope, name).
-- AM-7 (Rename gaming→eject): das Backfill-Profil heißt 'eject' / 'Eject-Modus';
-- der gaming-mode-Shim mappt in W3 auf dieses Profil (gaming = Alias).
--
-- Doktrin: Profile gaten physische Hosts (dieselbe Linie wie gaming.active
-- "gates a physical GPU host"). Tenant-Selbst-Abschaltung bleibt das
-- enabled-Flag (053:41), kein Profil.
-- =============================================================================

CREATE TABLE IF NOT EXISTS context_disable_profiles (
    id          UUID PRIMARY KEY DEFAULT uuidv7(),
    scope       VARCHAR(50) NOT NULL DEFAULT '_global',      -- AM-5: Sichtbarkeit ab W3;
                                                             -- W1-Backfill = '_global'
    name        TEXT NOT NULL
                CHECK (name ~ '^[a-z0-9][a-z0-9_-]{0,62}$'),  -- slug: URL-/CLI-tauglich
    label       TEXT NOT NULL DEFAULT ''                      -- Anzeige, frei
                CHECK (char_length(label) <= 120),            -- Layout-/title-Schranke (§5.3)
    description TEXT NOT NULL DEFAULT ''                       -- Erstnutzer-Hint (§4.6)
                CHECK (char_length(description) <= 500),       -- fremd-tenant-sichtbar
    active      BOOLEAN NOT NULL DEFAULT false,               -- fail-closed: neu = wirkungslos
    reserved    BOOLEAN NOT NULL DEFAULT false,               -- Break-Glass-Schutz: eject-Profil
                                                              -- ist im Cutover-Fenster nicht löschbar (§4.3)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_disable_profiles_scope_name UNIQUE (scope, name)  -- AM-5: (scope,name) statt name
);

CREATE TABLE IF NOT EXISTS context_disable_profile_backends (
    profile_id UUID NOT NULL REFERENCES context_disable_profiles(id) ON DELETE CASCADE,
    backend_id UUID NOT NULL REFERENCES context_backends(id)         ON DELETE CASCADE,
    PRIMARY KEY (profile_id, backend_id)
);

-- Hot-Reload: gleicher Kanal/gleiche Funktion wie 051/053/065 — notify_settings_write()
-- liest entity=TG_TABLE_NAME und identität via to_jsonb(COALESCE(NEW,OLD))->>'key'/'name'.
-- Der Go-Listener (events/listener.go, N9) dispatcht beide Entities in den
-- Pool-Reload-Arm (§4.1). Die früher erwogene COALESCE(key,name,'')-Anpassung
-- entfällt: ein NULL-key ist im Payload bereits inert (Listener liest entity/scope,
-- nie key), und die Funktion ist von 5+ Entities geteilt.
DROP TRIGGER IF EXISTS trg_disable_profiles_notify ON context_disable_profiles;
CREATE TRIGGER trg_disable_profiles_notify
    AFTER INSERT OR UPDATE OR DELETE ON context_disable_profiles
    FOR EACH ROW EXECUTE FUNCTION notify_settings_write();

-- Join-Trigger: der Payload-`key` ist hier BEWUSST NULL — die Tabelle trägt weder
-- eine key- noch eine name-Spalte, to_jsonb(...)->>'key'/'name' liefert SQL NULL
-- (kein Fehler). Der Listener routet allein über entity=TG_TABLE_NAME.
DROP TRIGGER IF EXISTS trg_disable_profile_backends_notify ON context_disable_profile_backends;
CREATE TRIGGER trg_disable_profile_backends_notify
    AFTER INSERT OR UPDATE OR DELETE ON context_disable_profile_backends
    FOR EACH ROW EXECUTE FUNCTION notify_settings_write();

-- Audit im Trigger (Muster audit_backends_write, 053:71-116) OHNE Redaction:
-- Profile/Memberships tragen keine Secrets. Append-only in dieselbe
-- context_settings_audit-Tabelle; deckt API, psql und break-glass gleichermaßen.
CREATE OR REPLACE FUNCTION audit_disable_profiles_write() RETURNS TRIGGER AS $$
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
        'disable_profile',
        COALESCE(v_new->>'name', v_old->>'name'),
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

DROP TRIGGER IF EXISTS trg_disable_profiles_audit ON context_disable_profiles;
CREATE TRIGGER trg_disable_profiles_audit
    AFTER INSERT OR UPDATE OR DELETE ON context_disable_profiles
    FOR EACH ROW EXECUTE FUNCTION audit_disable_profiles_write();

CREATE OR REPLACE FUNCTION audit_disable_profile_backends_write() RETURNS TRIGGER AS $$
DECLARE
    v_new   JSONB := to_jsonb(NEW);
    v_old   JSONB := to_jsonb(OLD);
    v_actor UUID  := NULLIF(current_setting('ctx.api_key_id', true), '')::uuid;
    v_label TEXT;
BEGIN
    IF v_actor IS NOT NULL THEN
        SELECT label INTO v_label FROM context_api_keys WHERE id = v_actor;
    END IF;
    -- scope: Memberships gaten physische _global-Hosts (§3.2-Doktrin); der Join
    -- trägt selbst kein scope, das Audit schreibt daher '_global'. entity_key =
    -- profile_id (das getroffene Profil).
    INSERT INTO context_settings_audit
        (entity_type, entity_key, scope, action, old_value, new_value,
         api_key_id, actor_label, metadata)
    VALUES (
        'disable_profile_backend',
        COALESCE(v_new->>'profile_id', v_old->>'profile_id'),
        '_global',
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

DROP TRIGGER IF EXISTS trg_disable_profile_backends_audit ON context_disable_profile_backends;
CREATE TRIGGER trg_disable_profile_backends_audit
    AFTER INSERT OR UPDATE OR DELETE ON context_disable_profile_backends
    FOR EACH ROW EXECUTE FUNCTION audit_disable_profile_backends_write();

-- ---------------------------------------------------------------------------
-- Backfill (Bestandsdaten-Pfad, idempotent): der letzte Auftritt des
-- Namens-Match-Typo-Problems — danach strukturell unmöglich (FK + CASCADE).
-- Idempotent per WHERE NOT EXISTS name='eject' (RETURN beim Zweitlauf).
-- Kein Settings-Delete: gaming.active bleibt liegen bis U01-W5 den Reader zieht.
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    v_profile_id UUID;
    v_active     BOOLEAN;
    v_names      TEXT[];
    v_name       TEXT;
    v_backend_id UUID;
    v_matched    INT := 0;
BEGIN
    IF EXISTS (SELECT 1 FROM context_disable_profiles WHERE scope = '_global' AND name = 'eject') THEN
        RAISE NOTICE '092 Backfill: Profil eject existiert bereits — übersprungen.';
        RETURN;
    END IF;

    -- active aus der Settings-Row gaming.active (scope '_global'), sonst false.
    SELECT (value #>> '{}')::boolean INTO v_active
      FROM context_settings
     WHERE key = 'gaming.active' AND scope = '_global';
    IF v_active IS NULL THEN
        v_active := false;
    END IF;

    -- Member-Quelle: Settings-Row gaming.disabled_backends (comma-split), sonst
    -- der Code-Default aus config.go:537 (herbert-chat,herbert-rerank) als Literal.
    SELECT string_to_array(value #>> '{}', ',') INTO v_names
      FROM context_settings
     WHERE key = 'gaming.disabled_backends' AND scope = '_global';
    IF v_names IS NULL THEN
        v_names := ARRAY['herbert-chat', 'herbert-rerank'];
    END IF;

    INSERT INTO context_disable_profiles (scope, name, label, description, active, reserved)
    VALUES ('_global', 'eject', 'Eject-Modus',
            'Nimmt die GPU-Backends aus jeder Chain — Failover (CPU/extern) übernimmt. Laufende Requests beenden normal.',
            v_active, true)
    RETURNING id INTO v_profile_id;

    FOREACH v_name IN ARRAY v_names LOOP
        v_name := btrim(v_name);
        CONTINUE WHEN v_name = '';
        SELECT id INTO v_backend_id
          FROM context_backends
         WHERE scope = '_global' AND name = v_name;
        IF v_backend_id IS NULL THEN
            RAISE NOTICE '092 Backfill: Backend % (scope _global) nicht gefunden — Membership übersprungen.', v_name;
            CONTINUE;
        END IF;
        INSERT INTO context_disable_profile_backends (profile_id, backend_id)
        VALUES (v_profile_id, v_backend_id)
        ON CONFLICT DO NOTHING;
        v_matched := v_matched + 1;
    END LOOP;

    RAISE NOTICE '092 Backfill: Profil eject angelegt (active=%), % Member verknüpft.', v_active, v_matched;
END $$;

INSERT INTO _migrations (version, filename, applied_at)
VALUES (92, '092_disable_profiles.sql', now()) ON CONFLICT (version) DO NOTHING;
