-- =============================================================================
-- 140_trigram_gist_knn.sql — Trigramm-Arm: GiST-Index + KNN-Anfrage
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Achse 05 (Empirische Validierung), Welle V-W4, Defekt S4. Der vierte
-- RRF-Arm (trigram_title) filtert heute mit `similarity(cb.title, p_query) >
-- 0.05` und deckelt danach auf 30 (139_rrf_gen17_tiebreak.sql:304-305 für
-- ctx_rrf, 137_rrf_arms.sql:315-316 für ctx_rrf_arms). Das Prädikat ist von
-- keinem Index getragen: `idx_trgm_title` ist ein GIN (113_baseline.sql:243),
-- GIN trägt `%`/`similarity`-Filter, aber KEINEN `<->`-KNN-Scan. Der Arm ist
-- damit auf JEDER RRF-Query ein Sequential Scan über den sichtbaren Korpus.
--
-- NUMMER: Masterplan §2 K1 — „wer zuerst landet, nimmt die nächste freie".
-- Gelandet sind 137, 138 und 139, also ist 140 die nächste freie. Keine
-- zweite Migrations-Welle läuft parallel (K1).
--
-- WARUM NICHT NUR `%` STATT `similarity() >`: gemessen (Achse 05 §7, V-W4) an
-- 60 realen Access-Log-Queries trifft `title % q` p50 430 von 1 387
-- retrievbaren Blöcken (31 %, min 48, p95 730). Ein GIN-Bitmap darüber ist
-- unselektiv — für einen CTE, der danach 30 behält, ist das kein Fortschritt.
-- Nur eine KNN-Anfrage (`ORDER BY title <-> p_query LIMIT n`) ist selektiv,
-- und die braucht GiST `gist_trgm_ops`.
--
-- WARUM EINE DATEI STATT 140a/140b: der Runner fährt jede Migrationsdatei in
-- GENAU EINER Transaktion (store/migrations.go:132-155, Präzedenz-Kommentar
-- 128_graph_cluster_centroid.sql:53-54). `CREATE INDEX CONCURRENTLY` ist
-- darin unmöglich. Der Split des Design-Vorschlags diente allein der
-- Pausierbarkeit eines CONCURRENTLY-Aufbaus; ohne CONCURRENTLY bringt er
-- nichts und kostet eine K1-Nummer.
--
-- INDEX-AUFBAU UND SEIN RUNBOOK (115-Muster). `CREATE INDEX` ohne
-- CONCURRENTLY nimmt für die Bau-Dauer einen SHARE-Lock auf context_blocks —
-- Leser laufen weiter, Schreiber warten. Gemessen auf einer 100 000-Zeilen-
-- Fixture (pgvector-timescaledb:pg18): Bau 1,97 s, Index 55 MB. Live sind es
-- ~7,8 k Titel, also Sub-Sekunde. Am Ziel-Scale ist das nicht mehr wahr, und
-- RunMigrations läuft im ctxd-Boot-Pfad. Deshalb nach dem Muster von
-- 115_hnsw_ef_construction_canonical.sql:26-59:
--
--   * Existiert ein BRAUCHBARER Index dieses Namens, ist die Migration ein
--     No-op. Ein Operator kann ihn also VORHER out-of-band anlegen:
--
--       CREATE INDEX CONCURRENTLY idx_trgm_title_gist
--           ON context_blocks USING GIST (title gist_trgm_ops);
--
--     (abbrechbar; ein halb gebauter Index bleibt als INVALID liegen und wird
--     mit DROP INDEX entfernt, danach neu gestartet).
--   * Ist der Name belegt, der Index aber für `ORDER BY title <-> …` nutzlos
--     (falsche Zugriffsmethode, INVALID, partiell, falsche Spalte/Opclass),
--     WARNT die Migration und baut nicht — der Name lässt sich nicht doppelt
--     vergeben. Ohne diese Prüfung landete ein namensgleicher GIN still, und
--     der Arm bliebe auf dem Seq Scan des Ist-Stands stehen (Review V-W4,
--     Befund #1). Die Prüfung ist eine Definitions-Prüfung wie in
--     115:33-35, keine Namensprüfung.
--   * Unterhalb von 500 000 Zeilen baut die Migration inline.
--   * Darüber WARNT sie und baut NICHT. Der Baum bleibt lauffähig: ohne den
--     Index fällt der neue CTE auf einen Seq Scan zurück, also exakt auf den
--     Ist-Stand VOR dieser Migration — nie schlechter (im Review gemessen:
--     102 000 Zeilen ohne Index, 140-Form 980 ms gegen 139-Form 1 683 ms, weil
--     140 einen Top-N-Heapsort fahren darf und 139 unter der WindowAgg
--     vollständig sortieren muss). Das Schema-Contract-Manifest bleibt bis zum
--     out-of-band-Bau rot (dieselbe bewusste Kröte wie in 115).
--
-- WAS `SET LOCAL lock_timeout = '3s'` BEDEUTET (Hausmuster 116/117/126/128):
-- die 3 s gelten für den ERWERB der Sperre, nicht für den Bau. Hält ein
-- laufender Schreiber den nötigen SHARE-Lock auf context_blocks länger als
-- 3 s blockiert, bricht die Migration mit lock_not_available (55P03) ab —
-- und weil der Runner die ganze Datei in EINER Transaktion fährt, rollt sie
-- vollständig zurück und der ctxd-Boot scheitert laut. Das ist gewollt
-- fail-closed: der nächste Boot versucht es erneut, und niemand wartet
-- unbemerkt auf einer Sperre. Es ist NICHT das Zeitbudget des Index-Baus;
-- dafür steht der Zeilen-Guard oben.
--
-- REVERT (der Baum kennt keine Down-Migrationen, forward-only bleibt die
-- Regel). Wer den Zustand temporär zurückdrehen muss:
--
--     DROP INDEX IF EXISTS idx_trgm_title_gist;
--     -- danach die beiden CREATE OR REPLACE FUNCTION-Blöcke aus
--     -- 139_rrf_gen17_tiebreak.sql (ctx_rrf) und 137_rrf_arms.sql
--     -- (ctx_rrf_arms) erneut ausführen; beide liegen unverändert im Baum.
--
-- `_migrations` bleibt dabei unangetastet — ein erneuter Boot spielt 140
-- nicht noch einmal ein. Wer den Rückweg dauerhaft will, braucht eine neue
-- Migration, keine Zeile weniger in dieser hier.
--
-- DIE EINE SEMANTISCHE ÄNDERUNG (identisch in beiden Funktionen):
--   vorher: WHERE … AND similarity(title, p_query) > s   ORDER BY sim DESC   LIMIT n
--   nachher: (ORDER BY title <-> p_query, id LIMIT n)  danach  WHERE similarity(title, p_query) > s
-- Filter und Deckel tauschen die Reihenfolge. Das ist mengengleich:
--   * Liegen ≥ n Zeilen über der Schwelle, liegt auch die n-te beste Zeile
--     über der Schwelle — die n nächsten sind dieselben n, die vorher nach
--     dem Filter oben standen.
--   * Liegen k < n Zeilen über der Schwelle, holt der KNN-Block n Zeilen und
--     der Nachfilter entfernt die n − k darunter; es bleiben dieselben k.
-- Der Rang ist in beiden Fällen 1..k bzw. 1..n, denn ROW_NUMBER() wird nach
-- WHERE ausgewertet.
--
-- OFFEN BLEIBT — UND WAR VORHER AUCH OFFEN — die Auswahl innerhalb einer
-- Gleichstands-Gruppe, die die LIMIT-Grenze kreuzt. `ORDER BY similarity DESC`
-- ohne Tiebreak liess sie planabhängig; live gemessen (26.08., reines SELECT
-- auf context_blocks) tragen 4 von 10 Stichproben-Queries eine solche Gruppe
-- an Rang 30 (Grössen 2, 2, 2, 11). Diese Migration schliesst die Lücke mit
-- `, cb.id` in BEIDEN Ordnungen — der Auswahl (LIMIT) und der Rangvergabe
-- (ROW_NUMBER). Präzedenz im eigenen Baum: der exact-Arm fährt seit
-- Generation 15 genau dieses Paar (139:210 und 139:213). cb.id ist PK, UUIDv7,
-- NOT NULL und eindeutig — die Ordnung ist damit total.
-- FOLGE, DIE BENANNT GEHÖRT: für Queries mit einer solchen Grenz-Gruppe kann
-- sich die gelieferte Kandidatenmenge gegenüber heute ändern. Sie war vorher
-- nicht definiert, ist jetzt definiert; „byte-gleiches eval.sh" ist deshalb
-- NICHT zugesagt, sondern zu messen.
--
-- WARUM DER NACHFILTER similarity() NEU RECHNET statt `1 - dist`: `<->` gibt
-- float4 zurück (pg_trgm: `1.0 - similarity`, auf float4 gerundet). Der
-- Rundtrip `1 - dist` weicht dadurch um bis zu ~6e-8 von der ursprünglichen
-- Ähnlichkeit ab (gemessen: similarity 0,11764706 ⇒ 1 - dist
-- 0,11764705181121826) und verschöbe die Schwelle. Der zweite
-- similarity()-Aufruf läuft auf ≤ n Zeilen und hält die Ist-Semantik exakt.
--
-- SIGNATUREN UNVERÄNDERT: ctx_rrf 18 Parameter, ctx_rrf_arms 23, Positions-
-- Parität 1–18 (Vertrag, 137:104-105). Kein Gewichts-Literal, kein Deckel,
-- keine Schwelle angefasst. Kein DROP FUNCTION nötig — die OUT-Spalten beider
-- Funktionen bleiben gleich, CREATE OR REPLACE genügt (139:84-87).
--
-- BEIDE FUNKTIONEN MÜSSEN MIT: ctx_rrf_arms ist zuletzt in 137 definiert
-- (139 ersetzt nur ctx_rrf) und ist das Mess-Instrument, aus dem der Sweep die
-- Fusion offline nachrechnet. Zöge nur eine der beiden nach, misst der Sweep
-- eine Fusion, die es nicht gibt — der Wächter dagegen ist das Paritäts-Gate
-- internal/rrf/arms_parity_integration_test.go (137:76-83).
--
-- Function- und Index-Change, kein Tabellen-/Spalten-Change: der
-- test.sh-T07-Zähler bleibt unverändert. Das Schema-Contract-Manifest ändert
-- sich (Fingerprints von ctx_rrf und ctx_rrf_arms, neuer Index
-- idx_trgm_title_gist, migration_max 139 → 140). Idempotent, forward-only.
-- Registrierung übernimmt der Runner (store.RunMigrations, 108+-Konvention).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- (a) GiST-Trigramm-Index. Guard nach 115-Muster: brauchbar vorhanden ⇒ No-op
-- (der Operator darf ihn out-of-band per CONCURRENTLY gebaut haben), Name
-- belegt aber unbrauchbar ⇒ WARNUNG (nie EXCEPTION — der Boot-Pfad muss
-- lauffähig bleiben), klein ⇒ inline bauen, gross ⇒ warnen statt den
-- Boot-Pfad zu blockieren.
--
-- „Brauchbar" wird an der DEFINITION geprüft, nicht am Namen: 115 prüft
-- reloptions (115:33-35), hier sind es vier Eigenschaften, ohne die der
-- Planner den Index für `ORDER BY title <-> …` NICHT zieht — Zugriffsmethode
-- gist, gültig (ein abgebrochener CONCURRENTLY-Bau hinterlässt einen INVALID
-- gebliebenen Index), nicht partiell (ein partieller Index wird nur bei
-- implizierendem Prädikat gezogen, und der CTE hat keins) und eine
-- gist_trgm_ops-Opclass auf `title`. Die siglen-Variante
-- (`gist_trgm_ops (siglen='32')`) besteht die Prüfung bewusst: sie trägt
-- denselben KNN-Scan. Ein Namensgleicher GIN dagegen wird abgelehnt — genau
-- der Fall, in dem die Migration sonst still landet und der Arm auf dem
-- Seq Scan des Ist-Stands stehen bliebe.
DO $do$
DECLARE
    v_am      TEXT;
    v_valid   BOOLEAN;
    v_partial BOOLEAN;
    v_def     TEXT;
    v_rel     REGCLASS;
    v_rows    BIGINT;
BEGIN
    SELECT am.amname, i.indisvalid, i.indpred IS NOT NULL, pg_get_indexdef(c.oid), i.indrelid
      INTO v_am, v_valid, v_partial, v_def, v_rel
      FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
      JOIN pg_am        am ON am.oid = c.relam
      JOIN pg_index      i ON i.indexrelid = c.oid
     WHERE n.nspname = 'public' AND c.relname = 'idx_trgm_title_gist';

    IF FOUND THEN
        IF v_am = 'gist'
           AND v_valid
           AND NOT v_partial
           AND v_rel = 'public.context_blocks'::regclass   -- Review-Note #7: Name allein
                                                           -- adoptiert sonst einen Index
                                                           -- auf einer FREMDEN Tabelle
           AND position('USING gist (title gist_trgm_ops' in v_def) > 0
        THEN
            RETURN;                     -- brauchbar vorhanden, nichts zu tun
        END IF;
        RAISE WARNING 'idx_trgm_title_gist existiert, traegt aber keinen KNN-Scan auf context_blocks (amname=%, valid=%, partiell=%, tabelle=%): %. Der trigram_title-CTE faellt damit auf einen Seq Scan zurueck, also auf den Ist-Stand vor dieser Migration. Runbook im Dateikopf: den Fremd-Index umbenennen oder verwerfen, dann CREATE INDEX CONCURRENTLY idx_trgm_title_gist ON context_blocks USING GIST (title gist_trgm_ops); das Schema-Contract-Manifest bleibt bis dahin rot.',
            v_am, v_valid, v_partial, v_rel, v_def;
        RETURN;                         -- Name belegt: bauen ist hier unmoeglich
    END IF;

    SELECT c.reltuples::BIGINT INTO v_rows
      FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname = 'public' AND c.relname = 'context_blocks';

    IF v_rows IS NULL OR v_rows < 0 THEN
        -- reltuples < 0 = nie analysiert (frische Tabelle, pg_restore ohne
        -- Statistiken). Bounded count statt Schätzung, nie ein Vollscan.
        SELECT count(*) INTO v_rows FROM (SELECT 1 FROM context_blocks LIMIT 500000) t;
    END IF;

    IF v_rows < 500000 THEN
        CREATE INDEX IF NOT EXISTS idx_trgm_title_gist
            ON context_blocks USING GIST (title gist_trgm_ops);
    ELSE
        RAISE WARNING 'idx_trgm_title_gist fehlt bei % Zeilen — out-of-band bauen (CREATE INDEX CONCURRENTLY, Runbook im Dateikopf). Der trigram_title-CTE faellt bis dahin auf einen Seq Scan zurueck, also auf den Ist-Stand vor dieser Migration; das Schema-Contract-Manifest bleibt bis dahin rot.', v_rows;
    END IF;
END
$do$;

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
    type_factor     DOUBLE PRECISION   -- bereits COALESCEd wie in der Fusion
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
        SELECT cb.id, COALESCE(f.factor, 1.0) AS factor
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
        COALESCE(tf.factor, 1.0)::DOUBLE PRECISION     AS type_factor
    FROM semantic s
    FULL OUTER JOIN fulltext_de d USING (id)
    FULL OUTER JOIN fulltext_en e USING (id)
    FULL OUTER JOIN trigram_title g USING (id)
    LEFT JOIN block_mass m ON m.id = COALESCE(s.id, d.id, e.id, g.id)
    LEFT JOIN type_factor tf ON tf.id = COALESCE(s.id, d.id, e.id, g.id);
END;
$$;
