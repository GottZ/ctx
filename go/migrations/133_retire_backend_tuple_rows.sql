-- =============================================================================
-- 133_retire_backend_tuple_rows.sql — die Bestands-Rows der 29 Backend-Tupel
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Entflechtung Stufe 2, Achse 05 (Daten & API), Welle A05-W-D2 / β11.
-- design/05 §3.1 + §7 „W-D2", User-Entscheid E4 (DECISIONS.md: „Delete-
-- Migration im Schnitt"). Vorbedingung erfüllt: der Registry-Cut ist mit β8
-- gelandet — seit dem letzten Key-Wellen-Commit kennt config.registry() keinen
-- der 29 Namen mehr (Pin: internal/config/retired_test.go,
-- `Registry ∩ retiredSettingKeys = ∅`).
--
-- WARUM EIN DELETE UND KEIN LIEGENLASSEN (die gaming.*-Präzedenz trägt hier
-- nicht): eine Row auf einem unregistrierten Key ist NICHT dauerhaft harmlos,
-- sondern nur so lange, wie niemand den Namen re-registriert — admitOverride
-- admittet jeden registrierten Key (internal/settings/build.go). Ein künftiges
-- `rerank.model` würde auf jeder Fremd-Installation mit Bestands-Row den
-- jahrealten Wert schlagartig wieder zu wirksamer Konfiguration machen: ohne
-- WARN, ohne UI-Sichtbarkeit (HandleList rendert rein registry-getrieben), ohne
-- referencedBy-Spur (die Secrets-API filtert seit dem Cut über config.Keys()).
-- Diese Migration schließt das strukturell: sie läuft vor dem ersten Serve
-- jedes künftigen Binaries, die Rows können nie wieder wirken. design/05 §5.8.
--
-- KEIN SCOPE-FILTER, UND DAS IST DER KERN: sechs der 29 Keys (die *.api_key)
-- waren tenant-overridable, Tenant-Scope-Rows sind eine reale Klasse. Ein
-- `WHERE scope = '_global'` ließe genau die sensitivste Klasse als Orphan
-- zurück — mit danglendem secret_ref, für den globalen Admin per API
-- unerreichbar (deleteSetting läuft im writeScope des Callers) und nach dem
-- Cut vollständig signallos: der volle settings.Reload lädt ausschließlich
-- '_global', ein Tenant-Orphan warnt beim Boot also nicht einmal.
-- design/05 §5.4. Das Integrations-Gate probt den Tenant-Scope explizit.
--
-- KEIN SCHEMA-CHANGE: context_settings, context_settings_audit und beide
-- Trigger bleiben byte-identisch. T07 (test.sh) zählt Tabellen/Spalten und
-- bleibt bei 56/42; das Schema-Contract-Manifest braucht keine Regeneration.
--
-- DIE AUDIT-ROWS SCHREIBT DER TRIGGER, NICHT DIESE DATEI: trg_settings_audit
-- (051:110-151) legt pro gelöschter Row eine Zeile action='unset' an,
-- old_value = der gelöschte Wert, via='sql' (kein ctx.api_key_id gesetzt),
-- metadata.request_id aus dem SET LOCAL unten. Damit ist der Bulk-unset
-- attributierbar OHNE neuen Mechanismus, die append-only-Historie bleibt
-- unangetastet (design/05 §3.3) — und die Audit-Rows sind zugleich der EINZIGE
-- maschinenlesbare Rückweg nach einem Binary-Rollback (design/05 §5.3b):
--
--     SELECT entity_key, scope, old_value
--       FROM context_settings_audit
--      WHERE metadata->>'request_id' = 'migration-133-retire-backend-tuples';
--
-- DIE MELDUNG (User-Auflage zu E4: „die 'migrate to last 4.x' meldung muss es
-- tragen."): nach dem E13-Entscheid antwortet die API auf den 29 Keys schlicht
-- 404 wie auf jeden Tippfehler — kein Tombstone-Status, kein Hint auf dem
-- Draht. Die Ops-Kommunikation des Umzugs tragen deshalb vier Stellen:
-- BREAKING-Tag-Annotation, Runbook, v5-README-Hop-Absatz und DIESE Meldung.
-- Sie ist englisch wie die drei anderen Träger (docs/ ist durchgängig
-- englisch), und sie feuert NUR, wenn wirklich Rows gelöscht wurden: ein
-- Fresh-Install hat nichts umzuziehen und bekommt keinen Hop-Rat, den er nicht
-- braucht. Transport: internal/store.NewPool hängt seit dieser Welle einen
-- OnNotice-Handler an die Verbindung — ohne ihn verwirft pgx jede NOTICE
-- stumm, und die Meldung erreichte niemanden (das gilt rückwirkend auch für
-- die Meldungen aus 092/094/115).
--
-- IDEMPOTENT UND TRANSAKTIONAL: der Runner fährt jede Datei in einer eigenen
-- Transaktion; ein DELETE auf leerer Treffermenge ist ein no-op ohne Meldung.
-- Der Runner verifiziert Checksummen bereits applieder Versionen NICHT (nur
-- EXISTS-Skip) — daraus die Bau-Regel: diese Datei wird nach dem Release nie
-- editiert. Ein nachträglich entdeckter Key ist eine NEUE Migration, sonst
-- liefe die Ergänzung auf Bestands-DBs nie (design/05 §3.1).
--
-- lock_timeout (Muster 119/129): ohne ihn wartet die Migration hinter einem
-- fremden Lock auf context_settings unbegrenzt und hängt den Boot; mit ihm
-- scheitert sie laut (55P03), die Transaktion rollt zurück, die Version bleibt
-- unverzeichnet und der nächste Boot fährt sie erneut.
--
-- ⚠ KEINE T07-NACHZIEHUNG (die Datei ändert weder Tabelle noch Spalte, 56/42
--   bleibt), ABER das Schema-Contract-Manifest zählt Migrationen mit und muss
--   regeneriert werden:
--   go test ./internal/schemacontract -tags=genmanifest -run TestGenerateManifest
--
-- ⚠ ZWEITE AUFZÄHLUNG: das ARRAY unten ist neben config.retiredSettingKeys die
--   zweite Liste derselben 29 Namen. Beide Richtungen sind gepinnt
--   (internal/config/retiredmigration_test.go, Set-Gleichheit gegen
--   config.RetiredKeyNames()): ein verlorener Eintrag ließe eine Row-Klasse
--   still liegen, ein überschüssiger löschte Rows eines LEBENDEN Keys.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- Attribuiert den Bulk-unset in context_settings_audit.metadata.request_id.
-- Muss vor dem DELETE stehen und in derselben Transaktion liegen — der
-- Audit-Trigger liest current_setting('ctx.request_id', true) je Row.
SET LOCAL ctx.request_id = 'migration-133-retire-backend-tuples';

DO $$
DECLARE
    v_deleted INT;
BEGIN
    -- Alle Scopes. Sortiert wie config.RetiredKeyNames(), damit der Drift-Pin
    -- und ein git-diff dieselbe Reihenfolge sehen.
    DELETE FROM context_settings
     WHERE key = ANY (ARRAY[
        'chat.api_key',
        'chat.host',
        'chat.model',
        'chat.num_ctx',
        'chat.protocol',
        'chat.think',
        'chat_fallback.api_key',
        'chat_fallback.host',
        'chat_fallback.protocol',
        'chat_fallback.timeout',
        'dream.api_key',
        'dream.host',
        'dream.model',
        'dream.num_ctx',
        'dream.protocol',
        'dream.think',
        'dream_embed.api_key',
        'dream_embed.host',
        'dream_embed.model',
        'dream_embed.num_ctx',
        'dream_embed.protocol',
        'embed.api_key',
        'embed.host',
        'embed.model',
        'embed.num_ctx',
        'embed.protocol',
        'rerank.api_key',
        'rerank.host',
        'rerank.model'
     ]);

    GET DIAGNOSTICS v_deleted = ROW_COUNT;

    IF v_deleted > 0 THEN
        RAISE NOTICE 'migration 133: deleted % context_settings row(s) on the 29 retired backend tuple keys, across all scopes. Their values live in the backend pool now (context_backends) — check "ctx backends list" before serving.', v_deleted;
        RAISE NOTICE 'migration 133: this upgrade is only supported from the 4.38 line. If you came from anything older, roll back, migrate to last 4.x, verify the pool, then upgrade to v5 — see docs/operations.md, "Migration 133".';
        RAISE NOTICE 'migration 133: the deleted values survive as audit history — SELECT entity_key, scope, old_value FROM context_settings_audit WHERE metadata->>''request_id'' = ''migration-133-retire-backend-tuples'';';
    END IF;
END $$;
