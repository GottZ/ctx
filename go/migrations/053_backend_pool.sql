-- =============================================================================
-- 053_backend_pool.sql — deklarativer LLM-Backend-Pool mit Trust-Stufen (F3-P1)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Ersetzt das hartverdrahtete Paar CTX_CHAT_HOST + CTX_CHAT_FALLBACK_* durch
-- eine Rolle→Backends-Routing-Tabelle. Trust-Stufen gaten, welcher Content
-- (context_blocks.sensitivity, Migration 055) ein Backend erreichen darf.
-- Secrets liegen NICHT hier: api_key_ref referenziert ein F2-Secret per Name
-- (context_secrets); die Tabelle trägt damit by construction kein Key-Material.
-- Reihenfolge/Priorität sind reine DATEN (E2: kein Magic-Value-Pattern) —
-- der Go-Bootstrap schreibt nur Initialwerte für die heutigen env-Backends.
-- limits: reservierte Naht für F6 (K7: chat_max_tokens der CPU-Klasse) und
-- die Kapazitäts-Welle (metadata: max_parallel, ctx_per_slot).
-- =============================================================================

CREATE TABLE IF NOT EXISTS context_backends (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    -- TODO(multi-tenant): pool visibility — today every backend row is
    -- server-global; per-tenant pools/quotas need a scope dimension here.
    name            TEXT NOT NULL,
    base_url        TEXT NOT NULL,
    protocol        TEXT NOT NULL DEFAULT 'openai'
                    CHECK (protocol IN ('openai','ollama','rerank')),
    provider_class  TEXT NOT NULL DEFAULT 'generic'
                    CHECK (provider_class IN ('generic','llamacpp','openrouter')),
    api_key_ref     TEXT,                                -- F2-Secret-Name; NULL = keyless (lokal)
    -- trust/sensitivity gehen in einen numerischen Rang-Vergleich: ein
    -- unbekannter Wert wäre kein "neuer Tenant-Wert", sondern ein kaputtes
    -- Gate — CHECK bewusst hart (wie block_role, anders als scope).
    trust           TEXT NOT NULL DEFAULT 'public'       -- fail-closed: neue Backends explizit hochstufen
                    CHECK (trust IN ('full-trust','no-credentials','non-personal','public')),
    locality        TEXT NOT NULL DEFAULT 'external'     -- Egress-Audit-Dimension; NICHT frei wählbar:
                    CHECK (locality IN ('local','lan','external')),  -- gegen base_url validiert (Go)
    roles           TEXT[] NOT NULL DEFAULT '{}',        -- synthesis|translate|embed|rerank|dream|digest|chat|frei
    model_map       JSONB NOT NULL DEFAULT '{}',         -- Rolle→ModelSpec: "modell-id" (Kurzform) ODER
                                                         -- {"model":"…","params":{"top_p":0.8,…}}
    timeouts        JSONB NOT NULL DEFAULT '{}',         -- {"synthesis":420} Sekunden; fehlend = Code-Default der Rolle
    num_ctx         INTEGER,                             -- nur ollama-Protokoll wire-relevant
    priority        INTEGER NOT NULL DEFAULT 0,          -- höher = bevorzugt
    enabled         BOOLEAN NOT NULL DEFAULT true,
    extra_headers   JSONB NOT NULL DEFAULT '{}',         -- z. B. HTTP-Referer/X-Title; Denylist-validiert (Go):
                                                         -- KEINE Credential-Header
    extra_body      JSONB NOT NULL DEFAULT '{}',         -- z. B. {"provider":{"require_parameters":true}};
                                                         -- zdr/deny wird bei provider_class=openrouter IMMER
                                                         -- erzwungen (trust-UNABHÄNGIG), Feld kann nur verschärfen
    limits          JSONB NOT NULL DEFAULT '{}',         -- K7-Naht: chat_max_tokens (F6-C2 CPU-Klasse)
    metadata        JSONB NOT NULL DEFAULT '{}',         -- reservierte Keys: max_parallel, ctx_per_slot,
                                                         -- embed_equivalence_verified, allow_data_collection
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_backends_name UNIQUE (name)
);

-- Hot-Reload: Backend-Mutationen müssen den Pool-Snapshot OHNE Restart
-- erreichen (User-Kernanforderung "on-the-fly"; deckt auch Break-Glass-
-- psql-Edits). Gleicher Kanal + gleiche Funktion wie 051: die bestehende
-- notify_settings_write() liest entity=TG_TABLE_NAME und den Namen via
-- COALESCE(key, name) — context_backends.name passt ohne Anpassung. Der
-- Go-Listener dispatcht auf entity='context_backends' → Pool-Reload.
DROP TRIGGER IF EXISTS trg_backends_notify ON context_backends;
CREATE TRIGGER trg_backends_notify
    AFTER INSERT OR UPDATE OR DELETE ON context_backends
    FOR EACH ROW EXECUTE FUNCTION notify_settings_write();

-- Audit im Trigger (Muster 051): atomar mit der Mutation, deckt API, psql
-- und break-glass gleichermaßen. Defense-in-Depth: extra_headers-WERTE
-- werden redacted, bevor old/new in die append-only Audit-Tabelle gehen —
-- falls die Go-Denylist je ein Credential-Carrier-Pattern verpasst, liegt
-- der Wert wenigstens nicht für immer im Audit (Header-NAMEN bleiben
-- sichtbar: das Audit zeigt WELCHE Header sich änderten, nie Werte).
CREATE OR REPLACE FUNCTION audit_backends_write() RETURNS TRIGGER AS $$
DECLARE
    v_new   JSONB := to_jsonb(NEW);
    v_old   JSONB := to_jsonb(OLD);
    v_actor UUID  := NULLIF(current_setting('ctx.api_key_id', true), '')::uuid;
    v_label TEXT;
BEGIN
    IF v_new ? 'extra_headers' THEN
        SELECT jsonb_set(v_new, '{extra_headers}',
                         COALESCE(jsonb_object_agg(k, '"[redacted]"'::jsonb), '{}'::jsonb))
          INTO v_new
          FROM jsonb_object_keys(v_new->'extra_headers') AS k;
    END IF;
    IF v_old ? 'extra_headers' THEN
        SELECT jsonb_set(v_old, '{extra_headers}',
                         COALESCE(jsonb_object_agg(k, '"[redacted]"'::jsonb), '{}'::jsonb))
          INTO v_old
          FROM jsonb_object_keys(v_old->'extra_headers') AS k;
    END IF;
    IF v_actor IS NOT NULL THEN
        SELECT label INTO v_label FROM context_api_keys WHERE id = v_actor;
    END IF;
    INSERT INTO context_settings_audit
        (entity_type, entity_key, scope, action, old_value, new_value,
         api_key_id, actor_label, metadata)
    VALUES (
        'backend',
        COALESCE(v_new->>'name', v_old->>'name'),
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

DROP TRIGGER IF EXISTS trg_backends_audit ON context_backends;
CREATE TRIGGER trg_backends_audit
    AFTER INSERT OR UPDATE OR DELETE ON context_backends
    FOR EACH ROW EXECUTE FUNCTION audit_backends_write();

INSERT INTO _migrations (version, filename, applied_at)
VALUES (53, '053_backend_pool.sql', now()) ON CONFLICT (version) DO NOTHING;
