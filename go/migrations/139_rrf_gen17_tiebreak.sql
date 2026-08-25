-- =============================================================================
-- 139_rrf_gen17_tiebreak.sql — ctx_rrf Generation 17
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Achse 04 §4.3, Welle B-W1b. Genau EINE semantische Zeile gegenüber
-- Generation 16 (134_rrf_gen16_ann_embedding_filter.sql): die finale
-- Projektion sortiert `ORDER BY r.score DESC, cb.id` statt `ORDER BY r.score
-- DESC` (134:360). Signatur, Validierung, Cap-Wächter, ann-/exact-Arm,
-- fulltext_de/en, trigram_title, block_mass, type_factor und die rrf-Fusion
-- sind byte-identisch zu Gen 16 übernommen (134:95-363).
--
-- NUMMER: Masterplan §2 K1 — „wer zuerst landet, nimmt die nächste freie".
-- Gelandet sind 137 (ctx_rrf_arms) und 138 (Tool-Typen-Flag
-- retrieval.untrusted), also ist 139 die nächste freie.
--
-- WARUM: Ein bares `ORDER BY r.score DESC` lässt die Reihenfolge bei
-- Score-Gleichstand offen. Postgres sortiert nicht stabil (tuplesort fällt für
-- kleine Mengen auf qsort zurück), und welche der gleichgescorten Zeilen zuerst
-- kommt, hängt an der Eingabereihenfolge des Sortierknotens — also am Plan, an
-- der Heap-Reihenfolge und am work_mem. Bei p_limit schneidet genau diese
-- offene Ordnung ab: eine Tie-Gruppe, die die Limit-Grenze kreuzt, liefert
-- planabhängig einen ANDEREN Block aus.
--
-- DASS ES GREIFT, IST GEMESSEN, NICHT VERMUTET (Welle B-W1, 137_rrf_arms.sql +
-- internal/rrf/arms_parity_integration_test.go): auf der 220-Block-Fixture
-- tragen 13 von 54 Queries Score-Gleichstände, 17 Tie-Gruppen à 2 Blöcke, bei
-- exakter Score-Parität zwischen SQL-Fusion und Offline-Nachrechnung (max
-- |Δscore| = 0). Der Paritätstest musste innerhalb dieser Gruppen deshalb
-- mengen- statt reihenfolgenbasiert vergleichen — eine Toleranz, die diese
-- Migration überflüssig macht.
--
-- Ties werden mit dem Korpus HÄUFIGER, nicht seltener. Ein Score ist eine Summe
-- aus vier reziproken Rängen mal zwei Faktoren; je mehr Blöcke sich dieselben
-- Rang-Kombinationen und dieselben mass_/type_factor-Werte teilen, desto mehr
-- exakte Gleichstände. Auf dem 1M+-Ziel-Korpus ist ein deterministischer
-- Tiebreak Voraussetzung dafür, dass zwei Läufe derselben Query dieselbe
-- Antwort geben — und damit Voraussetzung für jede reproduzierbare Messung
-- (Arm-Gewichts-Sweep B-W5, Schwellen-Kalibrierung B-W7). Ohne ihn muss ein
-- Messlauf verworfen werden, sobald sich der Plan zwischen zwei Läufen ändert.
--
-- WARUM `cb.id`: UUIDv7 (context_blocks.id, 113_baseline.sql:59, DEFAULT
-- uuidv7()) ist zeitlich monoton, damit ist die Tiebreak-Ordnung innerhalb
-- einer Gruppe die Anlagereihenfolge — der älteste Block gewinnt den
-- Gleichstand. Die Spalte ist PRIMARY KEY: NOT NULL, eindeutig (der Tiebreak
-- ist damit TOTAL, es bleibt kein zweiter Gleichstand offen) und durch
-- context_blocks_pkey indiziert. Sie steht ohnehin in der Projektion, kostet
-- also keine zusätzliche Spalte im Sortier-Tupel.
--
-- PRÄZEDENZ IM EIGENEN KÖRPER: der exact-Arm sortiert seit Generation 15
-- `ORDER BY ep.dist, ep.id` mit dem Kommentar „deterministischer Tiebreak"
-- (134:207 und 134:210, hier unverändert übernommen). Gen 17 zieht die finale
-- Projektion auf dieselbe Regel nach; der ann-Arm bleibt bewusst ohne
-- id-Tiebreak, weil dort ROW_NUMBER() über `<=>` ohnehin verschiedene Ränge
-- vergibt und ein Eingriff den HNSW-Pfad anfassen würde.
--
-- KOSTEN: der Sortierschlüssel wächst um einen UUID-Vergleich, der NUR
-- ausgewertet wird, wenn die erste Spalte gleich ist — also innerhalb der
-- Gleichstände, die es vorher schon gab. Kein Plan-Wechsel erwartet: der
-- Sortierknoten bleibt derselbe Knotentyp über derselben Eingabe, es kommt
-- kein Knoten hinzu und keine Scan-Methode wechselt. Belegt per EXPLAIN
-- vorher/nachher auf der Fixture (internal/rrf,
-- TestBW1bGen17ExplainPlanShape).
--
-- ZUSAGE: Kandidatenmenge und Scores sind byte-gleich zu Generation 16 — die
-- Änderung steht hinter der gesamten Fusion und kann weder einen Kandidaten
-- hinzufügen noch einen entfernen noch einen Score verschieben. Definiert ist
-- jetzt nur die Ordnung INNERHALB von Gleichständen. Einzige beobachtbare
-- Folge: kreuzt eine Tie-Gruppe die p_limit-Grenze, ist ab jetzt festgelegt,
-- welcher ihrer Blöcke ausgeliefert wird (der mit der kleineren id), statt
-- planabhängig zu wechseln. Gate: TestBW1bGen17StrictOrderParity vergleicht
-- Gen-16- und Gen-17-Ergebnisse in DERSELBEN Test-Datenbank über 54 Queries.
--
-- ctx_rrf_arms (137) BLEIBT UNVERÄNDERT. Die Schwester fusioniert nicht und
-- schneidet nicht ab — sie liefert die volle Kandidatenmenge mit rohen
-- Arm-Rängen, die ihre Aufrufer ohnehin selbst sortieren. Ein Tiebreak dort
-- hätte kein Verhalten zu definieren.
--
-- DIE ZWEI INLINE-MARKER „Gen 16 (#40 Bug 5)" IM KÖRPER BLEIBEN STEHEN. Sie
-- schreiben die `embedding IS NOT NULL`-Zeile ihrer Herkunfts-Generation zu,
-- und diese Herkunft ändert sich nicht dadurch, dass die Funktion als Ganzes
-- eine Generation weiterrückt. Sie auf „Gen 17" zu ziehen wäre eine falsche
-- Provenienz.
--
-- KEIN DROP FUNCTION: die Signatur ist gegenüber Gen 16 unverändert, und die
-- Overload-Bereinigung (15-Parameter-Signatur aus Gen 14) hat 134 bereits
-- erledigt — jede Installation, die 139 sieht, hat 134 durchlaufen.
-- CREATE OR REPLACE allein genügt und hält den Eingriff minimal.
--
-- Function-only, kein Tabellen-/Spalten-Change: der test.sh-T07-Zähler bleibt
-- unverändert. Das Schema-Contract-Manifest ändert sich trotzdem — der
-- Funktions-Fingerprint von ctx_rrf und migration_max (138 → 139). Idempotent
-- (CREATE OR REPLACE), forward-only. Registrierung übernimmt der Runner
-- (store.RunMigrations, 108+-Konvention).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

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
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY similarity(cb.title, p_query) DESC
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
          AND similarity(cb.title, p_query) > 0.05
        LIMIT 30
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
