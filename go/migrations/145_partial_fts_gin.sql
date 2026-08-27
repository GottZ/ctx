-- =============================================================================
-- 145_partial_fts_gin.sql — partielle FTS-GIN-Indexe (Deny-Listen-Prädikat)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Wissens-Ebenen, Welle OPS-W1 (Achse 05 / D-05 F-22, 0b-5-Freigabe, Weg (a)).
-- design/05 Changelog F-22 (der Befund) + §4.2 G5 / Changelog F-1 (warum
-- ausgerechnet `checkpoint` und `system-meta` die harte Deny-Liste sind).
--
-- NUMMER: Masterplan §2 K1 — „wer zuerst landet, nimmt die nächste freie".
-- Gelandet sind 142, 143 und 144, also ist 145 die nächste freie. Keine zweite
-- Migrations-Welle läuft parallel (K1).
--
-- DER BEFUND (F-22, live nachgemessen am 27.08.2026 gegen context_store):
-- idx_context_ts_de ist 41 MB, idx_context_ts_en 40 MB, und beide sind VOLL —
-- sie indizieren jede Zeile von context_blocks. Die Textmasse dahinter verteilt
-- sich so:
--
--     checkpoint    5 961 Blöcke   185 MB   97,2 %
--     knowledge     1 425 Blöcke   4,2 MB    2,2 %
--     audit-trail     289 Blöcke   760 kB    0,4 %
--     reference       134 Blöcke   304 kB    0,2 %
--     system-meta      25 Blöcke    71 kB    0,0 %
--
-- 97,2 % der indizierten Nutzlast gehört zu zwei Typen, die KEIN FTS-Arm je
-- zurückgeben kann: beide tragen retrieval.policy='excluded' und stehen damit
-- nicht in Set.VisibleTypes() (blocktype/set.go:92-95), also nie in
-- p_types_visible. Der Index trägt sie trotzdem — bei jedem Schreiber, jedem
-- VACUUM, jedem Neuaufbau, und am Ziel-Scale (1M+ Blöcke) wächst genau dieser
-- Anteil am schnellsten, weil Checkpoints der volumenstärkste Typ sind.
--
-- WARUM DAS PRÄDIKAT DIE HARTE DENY-LISTE IST UND NICHT „alle excluded"
-- (bindende Vorgabe des Haupt-Leads, design/05 §4.2 G5):
-- `catalog` und `insight` sind live EBENFALLS excluded (Registry-Zensus oben,
-- Spalte policy), aber sie sind die Schatten-Typen der M-W2-Mess-Naht: ein
-- Aufrufer mit shadow_types erweitert p_types_visible um sie
-- (handler/query_shadow.go:80-91, measureVisibleTypesFor), und der FTS-Arm muss
-- sie dann finden. Ein Prädikat „alle excluded" würde sie aus dem Index werfen
-- und die B/C-Messung um den FTS-Beitrag der Derivate verfälschen — still, weil
-- das Ergebnis nicht falsch AUSSIEHT, nur kleiner ist.
-- `checkpoint` und `system-meta` können das per Konstruktion nicht: sie stehen
-- in shadowDenyTypes (handler/query_shadow.go:50-53), einer nicht
-- konfigurierbaren Map, die VOR der Flag-Prüfung greift (:189-191) — auch eine
-- versehentlich auf shadow_measurable gekippte Registry-Zeile öffnet sie nicht.
--
-- DAS PLANNER-PROBLEM, UND WARUM DIE FUNKTIONSKÖRPER MITZIEHEN:
-- Ein partieller Index wird nur gewählt, wenn das QUERY-Prädikat das
-- INDEX-Prädikat IMPLIZIERT. Die FTS-CTEs filtern über
-- `cb.type_name = ANY(p_types_visible)` — einen plpgsql-PARAMETER. Gemessen
-- (Testcontainer pg18, TestOPSW1ImplicationIsLoadBearing, 40 000 Zeilen):
--
--   * Solange plpgsql einen CUSTOM PLAN fährt, faltet der Planner den Parameter
--     zu einem Konstanten-Array und BEWEIST die Implikation selbst — der
--     partielle Index wird gezogen, ohne dass die Funktion etwas dazutut.
--   * Sobald der plancache auf den GENERIC PLAN umstellt (ab dem 6. Aufruf je
--     Verbindung möglich, plancache.c choose_custom_plan), bleibt der Parameter
--     ein Parameter und die Implikation ist unbeweisbar: der FTS-Index
--     verschwindet aus dem Plan. WORAUF der Arm dann fällt, hängt am Schema —
--     hier auf `Index Scan using idx_blocks_type_name` mit dem @@-Prädikat als
--     heap-Filter (auf einem Schema ohne diesen Index wäre es ein Seq Scan).
--     Gemessen: geschätzte Gesamtkosten 3 866,41 ohne gegen 387,92 mit dem
--     statischen Prädikat, Faktor 10,0.
--
-- Die Index-Nutzung hinge damit an einer Cache-Entscheidung, die pro Verbindung
-- und pro Kostenschätzung kippt — genau die Sorte Abhängigkeit, die in einer
-- Messung als Latenz-Rauschen erscheint und in Produktion als Ausreißer. Das
-- statische Prädikat macht die Implikation klausel-identisch und damit
-- planunabhängig. Es steht in genau VIER CTEs: fulltext_de und fulltext_en in
-- ctx_rrf und in ctx_rrf_arms. Kein anderer Arm bekommt es (semantic fährt
-- HNSW, trigram_title fährt den GiST aus 140, block_mass und type_factor sind
-- keine Index-Pfade), keine Signatur ändert sich, kein Gewicht, kein Deckel,
-- keine Schwelle, keine Rangvergabe.
--
-- WOHER DIE KÖRPER STAMMEN: ctx_rrf aus 140_trigram_gist_knn.sql:208-493 (139
-- ist überholt, 142 hat ctx_rrf nicht angefasst), ctx_rrf_arms aus
-- 142_arms_typename.sql:145-430. Wer hier aus einer älteren Datei abschriebe,
-- drehte den Trigramm-Arm (V-W4) oder die neunte Spalte type_name (M-W1) still
-- zurück. Der Diff dieser Datei gegen jene beiden Blöcke ist genau vier Zeilen
-- plus ihre Kommentare.
--
-- KEIN DROP FUNCTION: beide OUT-Spaltenlisten bleiben unverändert, also genügt
-- CREATE OR REPLACE (139:84-87). Der DROP in 142 war nötig, WEIL dort eine
-- Spalte hinzukam (42P13); hier kommt keine hinzu.
--
-- WAS DAS PRÄDIKAT SEMANTISCH TUT — UND WO SEINE GRENZE LIEGT:
-- Auf jeder Registry, in der `checkpoint`/`system-meta` nicht sichtbar sind,
-- ist es ein exakter No-op: sie stehen dann weder in p_types_visible (der
-- Produktionsfall) noch in measureVisibleTypes (der Mess-Fall, per
-- shadowDenyTypes verschlossen), also entfernt der neue Konjunkt keine Zeile,
-- die die bestehenden Konjunkte durchgelassen hätten. Das ist der Zustand, den
-- die Mengen-Identitäts-Gates prüfen, und live der einzige existierende: die
-- Registry führt 11 Zeilen, alle scope='_global'.
--
-- BEFUND, DER DAZUGEHÖRT (Code schlägt Vorgabentext, im Bericht der Welle
-- ausgeführt): ein TENANT-Overlay darf eine _global-Exklusion ANHEBEN. D6
-- „Overlay gewinnt" ist ausdrücklich so entschieden und mit der T12-Probe
-- festgenagelt (blocktype/registry.go:252-256), und der Create-Pfad der
-- Typ-API prüft den Namen nicht gegen die _global-Builtins
-- (handler/types_write.go:150-172). Eine Tenant-Zeile
-- name='checkpoint', scope='<tenant>', retrieval.policy='full-pass' hebt
-- `checkpoint` also in Set.VisibleTypes() dieses Tenants — und ab dann ist der
-- Konjunkt KEIN No-op mehr, sondern schneidet die Checkpoint-Zeilen aus beiden
-- FTS-Armen (gemessen: 0 statt 100 Treffer). Semantischer und Trigramm-Arm
-- liefern sie weiter, der FTS-Beitrag fehlt. Das ist eine bewusste,
-- geprüfte Folge dieser Welle, kein Versehen: sie ist in
-- TestOPSW1TenantOverlayShadowsFTS festgenagelt, damit eine spätere
-- D6-Entscheidung sie nicht still überfährt. Der gegenläufige Präzedenzfall im
-- Baum ist store/blocktypes.go:113-125, wo dieselbe Overlay-Möglichkeit im
-- Embed-Backfill mit einem inneren NOT EXISTS berücksichtigt wird — in einem
-- INDEX-Prädikat ist das unmöglich (Index-Prädikate müssen immutable sein und
-- dürfen keine Unterabfrage tragen), weshalb dieselbe Rücksicht hier nicht
-- nachgebaut werden KANN, sondern nur benannt werden kann.
--
-- INDEX-AUFBAU UND SEIN RUNBOOK (115/140-Muster). `DROP INDEX` nimmt ACCESS
-- EXCLUSIVE auf context_blocks, `CREATE INDEX` ohne CONCURRENTLY einen SHARE —
-- beide für die Dauer der Migrationstransaktion, und RunMigrations läuft im
-- ctxd-Boot-Pfad (store/migrations.go:132-156, EINE Transaktion je Datei, also
-- ist CONCURRENTLY hier strukturell unmöglich). Gemessen: 397 ms für die GANZE
-- Migration auf einer 100 000-Zeilen-Fixture (TestOPSW1PlanShapeAndSize), 58 ms
-- auf der live-nachgebildeten 783-Zeilen-/52-MB-Fixture
-- (TestOPSW1LiveShapedIndexSize). Live sind es ~7 800 Zeilen. Am Ziel-Scale ist
-- das nicht mehr vertretbar, deshalb ein Guard — und seine ACHSE ist die
-- INDEX-MASSE, nicht die Zeilenzahl.
--
-- WARUM NICHT `reltuples`, WIE IN 115 UND 140: dort fallen die Baukosten je
-- ZEILE an (HNSW über ein Embedding, GiST über `title`). Ein tsvector-GIN
-- kostet je LEXEM, und genau das ist die Prämisse dieser Welle — Gate 5 misst
-- auf einer zeilenproportionalen Fixture 53,9 % Ersparnis und auf einer
-- live-nachgebildeten 97,0 %, bei derselben Zeilenzahl. Eine Zeilen-Schwelle
-- winkt deshalb genau den Korpus durch, den dieser Guard aufhalten soll: bei
-- Live-Form (7 834 Zeilen, 81 MB GIN) trägt ein Bestand knapp unter 500 000
-- Zeilen rund 5 GB GIN, und die Migration hätte beide Indexe inline neu gebaut.
-- Gegenprobe im Kleinen (TestOPSW1MassGuard): 600 Zeilen — vier Größenordnungen
-- unter jener Schwelle — tragen bereits 53 MiB GIN.
--
-- WOHER DIE 256 MiB KOMMEN: gemessen auf diesem Image (pg18,
-- maintenance_work_mem 2047 MB) baut ein tsvector-GIN mit ~26 MB Index je
-- Sekunde — 6 000 Zeilen mit 181 MB Text aus lauter EINMALIGEN md5-Lexemen
-- (der ungünstigste Fall für GIN) ergaben 669 MB Index in 25,86 s. 256 MiB
-- entsprechen damit rund 10 s Bau je Index, also im schlimmsten Fall ~20 s
-- ACCESS EXCLUSIVE für beide. Das ist die Obergrenze dessen, was ein Boot-Pfad
-- unbeaufsichtigt nehmen darf. Live liegt der Ist mit 41 MB je Index um Faktor
-- 6 darunter, der Ziel-Scale-Fall (≈2,5 GB je Index) um Faktor 10 darüber —
-- die Schwelle trennt also beide Seiten deutlich.
--
-- WARUM DIE INDEX-GRÖSSE UND NICHT DIE TABELLEN- ODER TEXTGRÖSSE: `pg_table_size`
-- wäre billig, ist aber kein tragfähiger Proxy. Live sind es 358 MB Tabelle
-- gegen 41 MB GIN (Verhältnis 0,11), auf der md5-Fixture 416 MB gegen 669 MB
-- (Verhältnis 1,6) — eine Spanne von Faktor 15, die keine Schwelle trägt, weil
-- sie davon abhängt, wie oft sich Lexeme wiederholen. Σ octet_length über die
-- nicht-deny-Zeilen wäre genauer, kostet aber einen Scan über genau die
-- Tabelle, deren Sperrzeit hier minimiert werden soll. Die Größe des
-- BESTEHENDEN Index ist beides: exakt die Masse, die der Neubau erzeugt, und
-- ein reiner Katalog-Lookup.
--
--   * Steht der Ziel-Index bereits (GIN, gültig, auf context_blocks, auf der
--     richtigen Spalte, mit GENAU diesem Prädikat), ist die Migration ein
--     No-op. Ein Operator darf ihn also vorher out-of-band bauen:
--
--       CREATE INDEX CONCURRENTLY idx_context_ts_de_partial
--           ON context_blocks USING GIN (ts_de)
--           WHERE type_name NOT IN ('checkpoint','system-meta');
--       DROP INDEX CONCURRENTLY idx_context_ts_de;
--       ALTER INDEX idx_context_ts_de_partial RENAME TO idx_context_ts_de;
--
--     (Der Umweg über den zweiten Namen ist Pflicht: derselbe Name kann nicht
--     zweimal existieren, und CONCURRENTLY verbietet die Kombination aus DROP
--     und CREATE in einer Transaktion. Ein abgebrochener CONCURRENTLY-Bau
--     hinterlässt einen INVALID gebliebenen Index — mit DROP INDEX entfernen,
--     dann neu starten. Analog für ts_en.)
--   * Steht dort der heutige VOLLE GIN und misst er weniger als
--     opsw1_mass_guard Bytes, ersetzt die Migration ihn inline.
--   * Ist er größer, WARNT sie und lässt ihn stehen. Der Baum bleibt lauffähig
--     und ist nie schlechter als vorher: ein VOLLER GIN trägt jedes Prädikat,
--     das ein partieller trägt — inklusive des neuen statischen Konjunkts. Nur
--     der Speicher-Gewinn bleibt aus, bis der Operator out-of-band nachzieht.
--     Das Schema-Contract-Manifest bleibt bis dahin rot (dieselbe bewusste
--     Kröte wie in 115 und 140). **Die FUNKTIONEN ziehen in JEDEM Fall nach** —
--     nur der Index-Neubau wird aufgeschoben, nie der Konjunkt; sonst hätte ein
--     großer Korpus den Speicherpreis des alten Index UND keinen Konjunkt.
--     Gepinnt in TestOPSW1MassGuard.
--   * Ist der Name frei, ist die Achse der GESCHWISTER-Index (beide indizieren
--     dieselbe Textmasse). Fehlen beide, baut die Migration NICHT — ohne einen
--     vorhandenen GIN gibt es keine belastbare Massenschätzung.
--   * Ist der Name mit etwas anderem belegt (falsche Zugriffsmethode, INVALID,
--     fremde Tabelle, fremde Spalte, fremdes Prädikat), WARNT sie und fasst
--     nichts an — nie eine EXCEPTION, der Boot-Pfad muss lauffähig bleiben.
--
-- WAS `SET LOCAL lock_timeout = '3s'` BEDEUTET (Hausmuster 116/117/126/128/140):
-- die 3 s gelten für den ERWERB der Sperre, nicht für den Bau. Blockiert ein
-- laufender Schreiber länger, bricht die Migration mit lock_not_available
-- (55P03) ab, die ganze Datei rollt zurück und der ctxd-Boot scheitert laut.
-- Gewollt fail-closed: der nächste Boot versucht es erneut, und niemand wartet
-- unbemerkt auf einer Sperre.
--
-- REVERT (der Baum kennt keine Down-Migrationen, forward-only bleibt die
-- Regel). Wer den Zustand temporär zurückdrehen muss:
--
--     DROP INDEX IF EXISTS idx_context_ts_de;
--     DROP INDEX IF EXISTS idx_context_ts_en;
--     CREATE INDEX idx_context_ts_de ON context_blocks USING GIN(ts_de);
--     CREATE INDEX idx_context_ts_en ON context_blocks USING GIN(ts_en);
--     -- danach den ctx_rrf-Block aus 140_trigram_gist_knn.sql und den
--     -- ctx_rrf_arms-Block aus 142_arms_typename.sql erneut ausführen; beide
--     -- liegen unverändert im Baum.
--
-- `_migrations` bleibt dabei unangetastet — ein erneuter Boot spielt 145 nicht
-- noch einmal ein. Wer den Rückweg dauerhaft will, braucht eine neue Migration.
--
-- Function- und Index-Change, kein Tabellen-/Spalten-Change: der
-- test.sh-T07-Zähler bleibt unverändert. Das Schema-Contract-Manifest ändert
-- sich an vier Stellen — die def_hash von idx_context_ts_de und
-- idx_context_ts_en, die src_hash von ctx_rrf und ctx_rrf_arms — plus
-- migration_max 144 → 145. Idempotent, forward-only. Registrierung übernimmt
-- der Runner (store.RunMigrations, 108+-Konvention).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- (a) Die beiden FTS-GIN-Indexe. Guard nach 115/140-Muster: Ziel-Zustand
-- vorhanden ⇒ No-op; heutiger voller GIN + kleine Tabelle ⇒ inline ersetzen;
-- heutiger voller GIN + grosse Tabelle ⇒ WARNUNG, nichts anfassen; Name fremd
-- belegt ⇒ WARNUNG, nichts anfassen.
--
-- „Ziel-Zustand" wird an der DEFINITION geprueft, nicht am Namen (115:33-35,
-- 140:144-154): Zugriffsmethode gin, gueltig (ein abgebrochener
-- CONCURRENTLY-Bau hinterlaesst einen INVALID gebliebenen Index), Tabelle
-- context_blocks (Review-Note #7 aus V-W4: der Name allein adoptiert sonst
-- einen Index auf einer FREMDEN Tabelle), die richtige Spalte, und das Praedikat
-- in der Normalform, die PostgreSQL selbst fuer
-- `WHERE type_name NOT IN ('checkpoint','system-meta')` ausgibt. Diese
-- Normalform ist in TestOPSW1PredicateNormalForm gegen einen frisch gebauten
-- Index gepinnt: aendert eine PG-Version sie, wird der Test rot statt der Guard
-- still.
DO $do$
DECLARE
    -- pg_get_expr(indpred, indrelid) fuer `type_name NOT IN ('checkpoint','system-meta')`.
    opsw1_target_pred CONSTANT TEXT :=
        '(type_name <> ALL (ARRAY[''checkpoint''::text, ''system-meta''::text]))';
    -- MASSE-Schwelle in Bytes: 256 MiB. Herleitung im Dateikopf. Als dezimales
    -- Literal geschrieben (nicht 256*1024*1024), damit die Gate-Sonde
    -- TestOPSW1MassGuard genau ein Substitutionsziel hat.
    opsw1_mass_guard   CONSTANT BIGINT := 268435456;

    opsw1_idx     RECORD;
    opsw1_am      TEXT;
    opsw1_valid   BOOLEAN;
    opsw1_pred    TEXT;
    opsw1_def     TEXT;
    opsw1_rel     REGCLASS;
    opsw1_size    BIGINT;   -- Groesse des Index dieses Schleifendurchlaufs
    opsw1_peer    BIGINT;   -- groesster vorhandener FTS-GIN VOR jeder Aenderung
BEGIN
    -- Ersatz-Achse fuer den Fall „Name frei": dann gibt es keine eigene
    -- Index-Groesse, wohl aber die des Geschwister-Index — beide indizieren
    -- dieselbe Textmasse, nur mit anderem Woerterbuch, und sind live praktisch
    -- gleich gross (41 MB / 40 MB). EINMAL vor der Schleife erhoben: nach dem
    -- ersten Durchlauf ist ts_de bereits partiell und damit klein, ein spaeteres
    -- GREATEST waere verfaelscht. `to_regclass` liefert NULL statt einer
    -- Ausnahme, wenn der Name nicht existiert.
    -- Randfall, bewusst fail-safe: liegt unter einem der beiden Namen ein
    -- grosser FREMD-Index, faellt diese Achse zu hoch aus und der Name-frei-Zweig
    -- warnt, statt zu bauen — die sichere Richtung.
    SELECT GREATEST(
             COALESCE(pg_relation_size(to_regclass('public.idx_context_ts_de')), 0),
             COALESCE(pg_relation_size(to_regclass('public.idx_context_ts_en')), 0))
      INTO opsw1_peer;

    FOR opsw1_idx IN
        SELECT * FROM (VALUES ('idx_context_ts_de', 'ts_de'),
                              ('idx_context_ts_en', 'ts_en')) AS v(idxname, colname)
    LOOP
        -- FOUND, nicht eine eigene Flag-Spalte: ein SELECT INTO ohne Treffer
        -- setzt JEDES INTO-Ziel auf NULL, also auch ein mitselektiertes TRUE —
        -- und `IF NOT <NULL>` ist NULL, nimmt den Zweig also NICHT. Gemessen
        -- beim Bau dieser Welle: der Name-frei-Zweig lief damit nie, die
        -- Migration landete still ohne Index. FOUND ist unmittelbar nach dem
        -- SELECT INTO gültig (die FOR-Schleife setzt es erst beim naechsten
        -- Durchlauf neu) und ist dieselbe Form wie 140:172.
        SELECT am.amname, i.indisvalid, pg_get_expr(i.indpred, i.indrelid),
               pg_get_indexdef(c.oid), i.indrelid, pg_relation_size(c.oid)
          INTO opsw1_am, opsw1_valid, opsw1_pred, opsw1_def, opsw1_rel, opsw1_size
          FROM pg_class c
          JOIN pg_namespace n ON n.oid = c.relnamespace
          JOIN pg_am        am ON am.oid = c.relam
          JOIN pg_index      i ON i.indexrelid = c.oid
         WHERE n.nspname = 'public' AND c.relname = opsw1_idx.idxname;

        IF NOT FOUND THEN
            -- Name frei. Das ist nicht der Ist-Zustand dieser Welle (beide
            -- Indexe stehen seit 113:241-242), sondern der Fall „jemand hat den
            -- Index von Hand verworfen". Achse ist der Geschwister-Index; fehlt
            -- auch der, gibt es keine belastbare Massenschaetzung (siehe
            -- Dateikopf: das Verhaeltnis GIN/Tabelle schwankt gemessen um Faktor
            -- 15), und die Migration baut NICHT — die konservative Richtung.
            IF opsw1_peer = 0 THEN
                RAISE WARNING '% fehlt, und der Geschwister-Index fehlt ebenfalls — ohne einen vorhandenen FTS-GIN gibt es keine belastbare Groessenschaetzung fuer den Neubau, also baut die Migration nicht. Beide out-of-band anlegen (CREATE INDEX CONCURRENTLY, Runbook im Dateikopf). Der FTS-Arm auf %.% hat bis dahin GAR KEINEN tsvector-Index; das Schema-Contract-Manifest bleibt bis dahin rot.',
                    opsw1_idx.idxname, 'context_blocks', opsw1_idx.colname;
            ELSIF opsw1_peer < opsw1_mass_guard THEN
                EXECUTE format(
                    'CREATE INDEX %I ON context_blocks USING GIN(%I) WHERE type_name NOT IN (''checkpoint'',''system-meta'')',
                    opsw1_idx.idxname, opsw1_idx.colname);
            ELSE
                RAISE WARNING '% fehlt, und der Geschwister-Index misst % Bytes (Schwelle % Bytes) — der Neubau waere zu teuer fuer den Boot-Pfad und wird nicht inline gefahren. Out-of-band anlegen (CREATE INDEX CONCURRENTLY, Runbook im Dateikopf). Der FTS-Arm auf %.% hat bis dahin GAR KEINEN tsvector-Index; das Schema-Contract-Manifest bleibt bis dahin rot.',
                    opsw1_idx.idxname, opsw1_peer, opsw1_mass_guard, 'context_blocks', opsw1_idx.colname;
            END IF;
            CONTINUE;
        END IF;

        -- (a1) Bereits der Ziel-Index? Dann nichts tun — ein out-of-band per
        -- CONCURRENTLY gebauter Index muss die Migration ueberleben.
        IF opsw1_am = 'gin'
           AND opsw1_valid
           AND opsw1_rel = 'public.context_blocks'::regclass
           AND opsw1_pred IS NOT DISTINCT FROM opsw1_target_pred
           AND position('USING gin (' || opsw1_idx.colname || ')' in opsw1_def) > 0
        THEN
            CONTINUE;
        END IF;

        -- (a2) Der heutige VOLLE GIN auf der richtigen Spalte: der Fall, fuer
        -- den diese Migration geschrieben ist.
        IF opsw1_am = 'gin'
           AND opsw1_valid
           AND opsw1_rel = 'public.context_blocks'::regclass
           AND opsw1_pred IS NULL
           AND position('USING gin (' || opsw1_idx.colname || ')' in opsw1_def) > 0
        THEN
            -- Achse ist die Groesse GENAU DIESES Index: sie ist die Masse, die
            -- der Neubau erzeugen muss, und damit direkt proportional zur
            -- Haltedauer des ACCESS-EXCLUSIVE-Locks (Herleitung im Dateikopf).
            IF opsw1_size < opsw1_mass_guard THEN
                EXECUTE format('DROP INDEX %I', opsw1_idx.idxname);
                EXECUTE format(
                    'CREATE INDEX %I ON context_blocks USING GIN(%I) WHERE type_name NOT IN (''checkpoint'',''system-meta'')',
                    opsw1_idx.idxname, opsw1_idx.colname);
            ELSE
                RAISE WARNING '% misst % Bytes (Schwelle % Bytes) und wird deshalb nicht inline ersetzt — der Neubau haelt ACCESS EXCLUSIVE auf context_blocks fuer seine ganze Dauer, und RunMigrations laeuft im ctxd-Boot-Pfad. Out-of-band nachziehen (Runbook im Dateikopf). Der volle Index traegt das neue statische Praedikat weiterhin, der Arm ist also nie langsamer als vorher; nur der Speicher-Gewinn bleibt aus und das Schema-Contract-Manifest bleibt rot.',
                    opsw1_idx.idxname, opsw1_size, opsw1_mass_guard;
            END IF;
            CONTINUE;
        END IF;

        -- (a3) Name belegt, aber weder Ziel noch der erwartete volle GIN.
        RAISE WARNING '% existiert, ist aber weder der Ziel-Index noch der erwartete volle GIN auf context_blocks(%) (amname=%, valid=%, praedikat=%, tabelle=%): %. Die Migration fasst ihn nicht an — den Fremd-Index umbenennen oder verwerfen, dann nach dem Runbook im Dateikopf neu anlegen; das Schema-Contract-Manifest bleibt bis dahin rot.',
            opsw1_idx.idxname, opsw1_idx.colname, opsw1_am, opsw1_valid,
            coalesce(opsw1_pred, '(keins)'), opsw1_rel, opsw1_def;
    END LOOP;
END
$do$;

-- (b) ctx_rrf. Koerper aus 140_trigram_gist_knn.sql:208-493, erweitert um genau
-- ZWEI Zeilen (fulltext_de und fulltext_en). Alles andere ist Kopie.
CREATE OR REPLACE FUNCTION ctx_rrf(
    p_embedding             halfvec(1024),
    p_query                 TEXT,
    p_query_spaced          TEXT,
    p_scopes                TEXT[],
    p_category              TEXT DEFAULT NULL,
    p_tags                  TEXT[] DEFAULT NULL,
    p_limit                 INT DEFAULT 5,
    p_temporal              TEXT DEFAULT NULL,
    p_query_or              TEXT DEFAULT NULL,
    p_types_visible         TEXT[] DEFAULT NULL,   -- ALLOWLIST (fail-closed): NULL/leer ⇒ 0 Treffer
    p_damped_types          TEXT[] DEFAULT NULL,   -- parallel zu p_damped_factors
    p_damped_factors        DOUBLE PRECISION[] DEFAULT NULL,
    p_categories_exclude    TEXT[] DEFAULT NULL,
    p_types_exclude         TEXT[] DEFAULT NULL,   -- Request-Level-Exclude
    p_granted_block_ids     UUID[] DEFAULT NULL,
    p_semantic_mode         TEXT DEFAULT 'ann',    -- 'ann' | 'exact' (Gen 15, §3.2)
    p_scan_tuples           INTEGER DEFAULT NULL,  -- Grauzonen-Budget, nur ann-Arm
    p_exact_cap             INTEGER DEFAULT NULL   -- Pflicht-Deckel im exact-Modus
) RETURNS TABLE (
    rrf_score   DOUBLE PRECISION,
    cosine_sim  DOUBLE PRECISION,
    id          UUID,
    title       TEXT,
    category    VARCHAR(100),
    tags        TEXT[],
    content     TEXT,
    scope       VARCHAR(50),
    updated_at  TIMESTAMPTZ,
    type_name   TEXT
) LANGUAGE plpgsql AS $$
DECLARE
    v_n BIGINT;
BEGIN
    -- Validierung als erste Anweisungen, vor jeder GUC-Setzung (§5.2):
    -- unbekannter Mode fällt NIE still auf ann.
    IF p_semantic_mode NOT IN ('ann', 'exact') THEN
        RAISE EXCEPTION 'ctx_rrf: unknown semantic mode %', p_semantic_mode;
    END IF;
    -- Obergrenze 200000 = letzte Verteidigungslinie im SQL-Body; Wert
    -- synchron zum Go-Clamp (§5.4, W02-2) halten.
    IF p_scan_tuples IS NOT NULL AND (p_scan_tuples <= 0 OR p_scan_tuples > 200000) THEN
        RAISE EXCEPTION 'ctx_rrf: invalid scan tuples budget %', p_scan_tuples;
    END IF;

    IF p_semantic_mode = 'exact' THEN
        IF p_exact_cap IS NULL OR p_exact_cap <= 0 THEN
            RAISE EXCEPTION 'ctx_rrf: exact mode requires positive p_exact_cap';
        END IF;
        -- Cap-Wächter (§5.6): in-Tx-Wiederholung der gedeckelten Probe
        -- (Index idx_context_scope_active) + Grant-Addition. Erkennt
        -- Scope-Wachstum zwischen Go-Probe und Ausführung (TOCTOU).
        SELECT count(*) INTO v_n FROM (
            SELECT 1 FROM context_blocks cb
            WHERE cb.scope = ANY(p_scopes) AND NOT cb.is_archived
            LIMIT p_exact_cap + 1
        ) t;
        IF v_n + COALESCE(array_length(p_granted_block_ids, 1), 0) > p_exact_cap THEN
            RAISE EXCEPTION 'ctx_rrf: exact_cap_hit (cap=%)', p_exact_cap
                USING ERRCODE = '54000';  -- program_limit_exceeded; Go-Retry als ann (§5.6)
        END IF;
    END IF;

    IF p_semantic_mode = 'ann' THEN
        SET LOCAL hnsw.iterative_scan = 'relaxed_order';  -- Nachfolger der unkonditionalen 073-Zeile
        IF p_scan_tuples IS NOT NULL THEN
            PERFORM set_config('hnsw.max_scan_tuples', p_scan_tuples::text, true);  -- true = SET LOCAL
        END IF;
    END IF;

    RETURN QUERY
    WITH semantic_ann AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
            ) AS rank,
            1.0 - (cb.embedding::halfvec(1024) <=> p_embedding)::DOUBLE PRECISION AS cos_sim
        FROM context_blocks cb
        WHERE p_semantic_mode = 'ann'          -- One-Time-Filter-Gate (nur Params → gating qual)
          AND cb.embedding IS NOT NULL         -- Gen 16 (#40 Bug 5): in BEIDEN Semantik-Armen
          AND NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
        ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
        LIMIT 75
    ),
    exact_pool AS MATERIALIZED (               -- Materialisierungs-Barriere: HNSW strukturell unerreichbar
        SELECT
            cb.id,
            (cb.embedding::halfvec(1024) <=> p_embedding)::DOUBLE PRECISION AS dist
        FROM context_blocks cb
        WHERE p_semantic_mode = 'exact'        -- One-Time-Filter-Gate
          AND cb.embedding IS NOT NULL         -- Gen 16 (#40 Bug 5): in BEIDEN Semantik-Armen
          AND NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
        LIMIT p_exact_cap                      -- konstruktiver Deckel (§4.3a); bindet nach Wächter-Pass nur im Rest-Race (§5.6)
    ),
    semantic_exact AS (
        SELECT
            ep.id,
            ROW_NUMBER() OVER (ORDER BY ep.dist, ep.id) AS rank,   -- deterministischer Tiebreak
            1.0 - ep.dist AS cos_sim
        FROM exact_pool ep
        ORDER BY ep.dist, ep.id
        LIMIT 75
    ),
    semantic AS (
        SELECT sa.id, sa.rank, sa.cos_sim FROM semantic_ann sa
        UNION ALL
        SELECT se.id, se.rank, se.cos_sim FROM semantic_exact se
    ),
    fulltext_de AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query)),
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          -- OPS-W1: static deny-list conjunct, clause-identical to the partial
          -- index predicate of idx_context_ts_de. Without it the implication
          -- holds only while the plan cache serves a CUSTOM plan; under a
          -- generic plan p_types_visible stays a Param, the proof is impossible
          -- and the FTS index drops out of the plan. See the file header for the
          -- measurement.
          AND cb.type_name NOT IN ('checkpoint','system-meta')
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (
              cb.ts_de @@ plainto_tsquery('german', p_query)
              OR cb.ts_de @@ plainto_tsquery('german', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    fulltext_en AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query)),
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          -- OPS-W1: same conjunct, for idx_context_ts_en.
          AND cb.type_name NOT IN ('checkpoint','system-meta')
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (
              cb.ts_en @@ plainto_tsquery('english', p_query)
              OR cb.ts_en @@ plainto_tsquery('english', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    trigram_title AS (
        -- V-W4: KNN statt Schwellen-Scan. Der innere Block liefert ueber
        -- idx_trgm_title_gist die 30 naechsten Titel (Order By `<->`,
        -- pg_trgm-Distanz = 1 - similarity); der aeussere haelt die bisherige
        -- Schwelle als Nachfilter und vergibt die Raenge. `, cb.id` in beiden
        -- Ordnungen ist die Gen-17-Doktrin des exact-Arms (:210/:213), hier auf
        -- den Trigramm-Arm gezogen: Auswahl UND Rang sind damit total geordnet.
        -- Der Nachfilter rechnet similarity() neu statt `1 - dist`: `<->`
        -- liefert float4, der Rundtrip `1 - dist` verschoebe die Schwelle um bis
        -- zu ~6e-8 gegen die Ist-Semantik.
        SELECT
            knn.id,
            ROW_NUMBER() OVER (ORDER BY knn.dist, knn.id) AS rank
        FROM (
            SELECT
                cb.id,
                cb.title                AS title,
                cb.title <-> p_query    AS dist
            FROM context_blocks cb
            WHERE NOT cb.is_archived
              AND cb.type_name = ANY(p_types_visible)
              AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
              AND ( cb.scope = ANY(p_scopes)
                    OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
              AND (p_category IS NULL OR cb.category = p_category)
              AND (p_tags IS NULL OR cb.tags && p_tags)
              AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
            ORDER BY cb.title <-> p_query, cb.id
            LIMIT 30
        ) knn
        WHERE similarity(knn.title, p_query) > 0.05
    ),
    block_mass AS (
        SELECT cb.id,
               CASE
                   WHEN array_length(cb.content_times, 1) IS NULL THEN 1.0
                   WHEN array_length(cb.content_times, 1) = 0    THEN 1.0
                   ELSE 1.0 / sqrt(array_length(cb.content_times, 1)::DOUBLE PRECISION)
               END AS mass_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
    ),
    type_factor AS (
        SELECT cb.id, COALESCE(f.factor, 1.0) AS factor
        FROM context_blocks cb
        LEFT JOIN unnest(p_damped_types, p_damped_factors) AS f(tname, factor)
               ON cb.type_name = f.tname
        WHERE NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
    ),
    rrf AS (
        SELECT
            COALESCE(s.id, d.id, e.id, g.id) AS block_id,
            (   0.45 * COALESCE(m.mass_factor, 1.0) * COALESCE(tf.factor, 1.0) * COALESCE(1.0 / (60 + s.rank), 0)
              + 0.20 * COALESCE(m.mass_factor, 1.0) * COALESCE(tf.factor, 1.0) * COALESCE(1.0 / (60 + d.rank), 0)
              + 0.25 * COALESCE(m.mass_factor, 1.0) * COALESCE(tf.factor, 1.0) * COALESCE(1.0 / (60 + e.rank), 0)
              + 0.10 * COALESCE(m.mass_factor, 1.0) * COALESCE(tf.factor, 1.0) * COALESCE(1.0 / (60 + g.rank), 0)
            )::DOUBLE PRECISION AS score,
            s.cos_sim::DOUBLE PRECISION
        FROM semantic s
        FULL OUTER JOIN fulltext_de d USING (id)
        FULL OUTER JOIN fulltext_en e USING (id)
        FULL OUTER JOIN trigram_title g USING (id)
        LEFT JOIN block_mass m ON m.id = COALESCE(s.id, d.id, e.id, g.id)
        LEFT JOIN type_factor tf ON tf.id = COALESCE(s.id, d.id, e.id, g.id)
    )
    SELECT
        r.score                    AS rrf_score,
        r.cos_sim                  AS cosine_sim,
        cb.id,
        cb.title::TEXT             AS title,
        cb.category,
        cb.tags,
        cb.content,
        cb.scope::VARCHAR(50)      AS scope,
        cb.updated_at,
        cb.type_name::TEXT         AS type_name
    FROM rrf r
    JOIN context_blocks cb ON cb.id = r.block_id
    -- Gen 17 (Achse 04 §4.3): deterministischer Tiebreak. cb.id ist PK
    -- (UUIDv7, NOT NULL, eindeutig) — die Ordnung ist damit total, und der
    -- Vergleich fällt nur bei Score-Gleichstand an. Präzedenz: :207/:210.
    ORDER BY r.score DESC, cb.id
    LIMIT p_limit;
END;
$$;

-- (c) ctx_rrf_arms. Koerper aus 142_arms_typename.sql:145-430, erweitert um
-- dieselben ZWEI Zeilen. Beide Funktionen muessen mitziehen: ctx_rrf_arms ist
-- das Mess-Instrument, aus dem der Sweep die Fusion offline nachrechnet — zoege
-- nur eine der beiden nach, misst der Sweep eine Fusion, die es nicht gibt
-- (140:123-127). Der Waechter dagegen ist das Paritaets-Gate
-- internal/rrf/arms_parity_integration_test.go (137:76-83).
CREATE OR REPLACE FUNCTION ctx_rrf_arms(
    -- Position 1-18: byte-identisch zu ctx_rrf (134:96-113). Reihenfolge,
    -- Typen und Defaults sind Vertrag, nicht Geschmack.
    p_embedding             halfvec(1024),
    p_query                 TEXT,
    p_query_spaced          TEXT,
    p_scopes                TEXT[],
    p_category              TEXT DEFAULT NULL,
    p_tags                  TEXT[] DEFAULT NULL,
    p_limit                 INT DEFAULT 5,         -- entgegengenommen, IGNORIERT (siehe Kopf)
    p_temporal              TEXT DEFAULT NULL,
    p_query_or              TEXT DEFAULT NULL,
    p_types_visible         TEXT[] DEFAULT NULL,   -- ALLOWLIST (fail-closed): NULL/leer ⇒ 0 Treffer
    p_damped_types          TEXT[] DEFAULT NULL,   -- parallel zu p_damped_factors
    p_damped_factors        DOUBLE PRECISION[] DEFAULT NULL,
    p_categories_exclude    TEXT[] DEFAULT NULL,
    p_types_exclude         TEXT[] DEFAULT NULL,   -- Request-Level-Exclude
    p_granted_block_ids     UUID[] DEFAULT NULL,
    p_semantic_mode         TEXT DEFAULT 'ann',    -- 'ann' | 'exact' (Gen 15, §3.2)
    p_scan_tuples           INTEGER DEFAULT NULL,  -- Grauzonen-Budget, nur ann-Arm
    p_exact_cap             INTEGER DEFAULT NULL,  -- Pflicht-Deckel im exact-Modus
    -- Position 19-23: NEU. Defaults = die Ist-Literale aus 134.
    p_cap_semantic          INTEGER DEFAULT 75,               -- 134:185 / 134:211
    p_cap_fts_de            INTEGER DEFAULT 100,              -- 134:250
    p_cap_fts_en            INTEGER DEFAULT 100,              -- 134:284
    p_cap_trigram           INTEGER DEFAULT 30,               -- 134:302
    p_trgm_threshold        DOUBLE PRECISION DEFAULT 0.05     -- 134:301
) RETURNS TABLE (
    id              UUID,
    rank_semantic   INT,               -- NULL = Block nicht im Arm
    rank_fts_de     INT,
    rank_fts_en     INT,
    rank_trigram    INT,
    cos_sim         DOUBLE PRECISION,  -- NULL = nur lexikalisch gefunden (E-M6-Rettungsklausel)
    mass_factor     DOUBLE PRECISION,  -- bereits COALESCEd wie in der Fusion
    type_factor     DOUBLE PRECISION,  -- bereits COALESCEd wie in der Fusion
    type_name       TEXT               -- M-W1: Generation 3. Registry-Klassifikation,
                                       -- kein Inhalt; type_factor ist nicht injektiv
) LANGUAGE plpgsql AS $$
DECLARE
    v_n BIGINT;
BEGIN
    -- Validierung als erste Anweisungen, vor jeder GUC-Setzung (§5.2):
    -- unbekannter Mode fällt NIE still auf ann.
    IF p_semantic_mode NOT IN ('ann', 'exact') THEN
        RAISE EXCEPTION 'ctx_rrf_arms: unknown semantic mode %', p_semantic_mode;
    END IF;
    -- Obergrenze 200000 = letzte Verteidigungslinie im SQL-Body; Wert
    -- synchron zum Go-Clamp (§5.4, W02-2) halten.
    IF p_scan_tuples IS NOT NULL AND (p_scan_tuples <= 0 OR p_scan_tuples > 200000) THEN
        RAISE EXCEPTION 'ctx_rrf_arms: invalid scan tuples budget %', p_scan_tuples;
    END IF;

    IF p_semantic_mode = 'exact' THEN
        IF p_exact_cap IS NULL OR p_exact_cap <= 0 THEN
            RAISE EXCEPTION 'ctx_rrf_arms: exact mode requires positive p_exact_cap';
        END IF;
        -- Cap-Wächter (§5.6): in-Tx-Wiederholung der gedeckelten Probe
        -- (Index idx_context_scope_active) + Grant-Addition. Erkennt
        -- Scope-Wachstum zwischen Go-Probe und Ausführung (TOCTOU).
        SELECT count(*) INTO v_n FROM (
            SELECT 1 FROM context_blocks cb
            WHERE cb.scope = ANY(p_scopes) AND NOT cb.is_archived
            LIMIT p_exact_cap + 1
        ) t;
        IF v_n + COALESCE(array_length(p_granted_block_ids, 1), 0) > p_exact_cap THEN
            RAISE EXCEPTION 'ctx_rrf_arms: exact_cap_hit (cap=%)', p_exact_cap
                USING ERRCODE = '54000';  -- program_limit_exceeded; Go-Retry als ann (§5.6)
        END IF;
    END IF;

    IF p_semantic_mode = 'ann' THEN
        SET LOCAL hnsw.iterative_scan = 'relaxed_order';  -- Nachfolger der unkonditionalen 073-Zeile
        IF p_scan_tuples IS NOT NULL THEN
            PERFORM set_config('hnsw.max_scan_tuples', p_scan_tuples::text, true);  -- true = SET LOCAL
        END IF;
    END IF;

    RETURN QUERY
    WITH semantic_ann AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
            ) AS rank,
            1.0 - (cb.embedding::halfvec(1024) <=> p_embedding)::DOUBLE PRECISION AS cos_sim
        FROM context_blocks cb
        WHERE p_semantic_mode = 'ann'          -- One-Time-Filter-Gate (nur Params → gating qual)
          AND cb.embedding IS NOT NULL         -- Gen 16 (#40 Bug 5): in BEIDEN Semantik-Armen
          AND NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
        ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
        LIMIT p_cap_semantic
    ),
    exact_pool AS MATERIALIZED (               -- Materialisierungs-Barriere: HNSW strukturell unerreichbar
        SELECT
            cb.id,
            (cb.embedding::halfvec(1024) <=> p_embedding)::DOUBLE PRECISION AS dist
        FROM context_blocks cb
        WHERE p_semantic_mode = 'exact'        -- One-Time-Filter-Gate
          AND cb.embedding IS NOT NULL         -- Gen 16 (#40 Bug 5): in BEIDEN Semantik-Armen
          AND NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
        LIMIT p_exact_cap                      -- konstruktiver Deckel (§4.3a); bindet nach Wächter-Pass nur im Rest-Race (§5.6)
    ),
    semantic_exact AS (
        SELECT
            ep.id,
            ROW_NUMBER() OVER (ORDER BY ep.dist, ep.id) AS rank,   -- deterministischer Tiebreak
            1.0 - ep.dist AS cos_sim
        FROM exact_pool ep
        ORDER BY ep.dist, ep.id
        LIMIT p_cap_semantic
    ),
    semantic AS (
        SELECT sa.id, sa.rank, sa.cos_sim FROM semantic_ann sa
        UNION ALL
        SELECT se.id, se.rank, se.cos_sim FROM semantic_exact se
    ),
    fulltext_de AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query)),
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          -- OPS-W1: static deny-list conjunct, clause-identical to the partial
          -- index predicate of idx_context_ts_de and to ctx_rrf's. The
          -- measurement seam widens p_types_visible with shadow types
          -- (handler/query_shadow.go:80), and checkpoint/system-meta can never
          -- be among them (shadowDenyTypes, :50-53, checked before the flag) —
          -- so this arm stays parity-identical to ctx_rrf's.
          AND cb.type_name NOT IN ('checkpoint','system-meta')
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (
              cb.ts_de @@ plainto_tsquery('german', p_query)
              OR cb.ts_de @@ plainto_tsquery('german', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT p_cap_fts_de
    ),
    fulltext_en AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query)),
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          -- OPS-W1: same conjunct, for idx_context_ts_en.
          AND cb.type_name NOT IN ('checkpoint','system-meta')
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (
              cb.ts_en @@ plainto_tsquery('english', p_query)
              OR cb.ts_en @@ plainto_tsquery('english', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT p_cap_fts_en
    ),
    trigram_title AS (
        -- V-W4: KNN statt Schwellen-Scan. Der innere Block liefert ueber
        -- idx_trgm_title_gist die p_cap_trigram naechsten Titel (Order By `<->`,
        -- pg_trgm-Distanz = 1 - similarity); der aeussere haelt die bisherige
        -- Schwelle als Nachfilter und vergibt die Raenge. `, cb.id` in beiden
        -- Ordnungen ist die Gen-17-Doktrin des exact-Arms (:210/:213), hier auf
        -- den Trigramm-Arm gezogen: Auswahl UND Rang sind damit total geordnet.
        -- Der Nachfilter rechnet similarity() neu statt `1 - dist`: `<->`
        -- liefert float4, der Rundtrip `1 - dist` verschoebe die Schwelle um bis
        -- zu ~6e-8 gegen die Ist-Semantik.
        SELECT
            knn.id,
            ROW_NUMBER() OVER (ORDER BY knn.dist, knn.id) AS rank
        FROM (
            SELECT
                cb.id,
                cb.title                AS title,
                cb.title <-> p_query    AS dist
            FROM context_blocks cb
            WHERE NOT cb.is_archived
              AND cb.type_name = ANY(p_types_visible)
              AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
              AND ( cb.scope = ANY(p_scopes)
                    OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
              AND (p_category IS NULL OR cb.category = p_category)
              AND (p_tags IS NULL OR cb.tags && p_tags)
              AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
            ORDER BY cb.title <-> p_query, cb.id
            LIMIT p_cap_trigram
        ) knn
        WHERE similarity(knn.title, p_query) > p_trgm_threshold
    ),
    block_mass AS (
        SELECT cb.id,
               CASE
                   WHEN array_length(cb.content_times, 1) IS NULL THEN 1.0
                   WHEN array_length(cb.content_times, 1) = 0    THEN 1.0
                   ELSE 1.0 / sqrt(array_length(cb.content_times, 1)::DOUBLE PRECISION)
               END AS mass_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
    ),
    type_factor AS (
        -- M-W1: derselbe CTE wie in 140, um EINE Projektions-Spalte erweitert.
        -- Das WHERE ist unverändert und bleibt damit die Obermenge jedes Arms
        -- (Begründung im Dateikopf von 142) — die Kandidatenmenge ändert sich
        -- nicht. OPS-W1 fasst diesen CTE nicht an: der neue Konjunkt gehört in
        -- die Arme, nicht in die Faktor-Quelle. Der CTE bleibt damit auch für
        -- die FTS-Arme eine echte Obermenge (er verlangt weniger, nicht mehr).
        SELECT cb.id, COALESCE(f.factor, 1.0) AS factor, cb.type_name AS type_name
        FROM context_blocks cb
        LEFT JOIN unnest(p_damped_types, p_damped_factors) AS f(tname, factor)
               ON cb.type_name = f.tname
        WHERE NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
    )
    -- Dieselbe Join-Naht wie der rrf-CTE in 134:331-345 — FULL OUTER JOIN über
    -- die vier Arme, block_mass und type_factor per LEFT JOIN daran. Was fehlt,
    -- ist genau der Gewichts-Ausdruck (134:334-337), der JOIN auf
    -- context_blocks (134:359) und ORDER BY/LIMIT (134:360-361).
    SELECT
        COALESCE(s.id, d.id, e.id, g.id)        AS id,
        s.rank::INT                             AS rank_semantic,
        d.rank::INT                             AS rank_fts_de,
        e.rank::INT                             AS rank_fts_en,
        g.rank::INT                             AS rank_trigram,
        s.cos_sim::DOUBLE PRECISION             AS cos_sim,
        COALESCE(m.mass_factor, 1.0)::DOUBLE PRECISION AS mass_factor,
        COALESCE(tf.factor, 1.0)::DOUBLE PRECISION     AS type_factor,
        -- M-W1: KEIN COALESCE. Ein tf-Fehlgriff ist strukturell unmöglich
        -- (Obermengen-Argument im Dateikopf von 142); wäre er es doch, soll er
        -- als NULL sichtbar werden statt hinter einem Ersatzwert zu verschwinden.
        tf.type_name::TEXT                             AS type_name
    FROM semantic s
    FULL OUTER JOIN fulltext_de d USING (id)
    FULL OUTER JOIN fulltext_en e USING (id)
    FULL OUTER JOIN trigram_title g USING (id)
    LEFT JOIN block_mass m ON m.id = COALESCE(s.id, d.id, e.id, g.id)
    LEFT JOIN type_factor tf ON tf.id = COALESCE(s.id, d.id, e.id, g.id);
END;
$$;
