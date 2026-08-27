-- =============================================================================
-- 147_fts_tiebreak.sql — deterministischer Id-Tiebreak in den FTS-Arm-CTEs
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Wissens-Ebenen, Welle C2-3 (Board-Entscheid E2-5 vom 2026-08-27: bauen,
-- NICHT deployen — der Deploy hängt an RC-A). Anlass ist Befund N10 aus
-- reports/bau/x-w1.md Teil B.
--
-- NUMMER: Masterplan §2 K1 — „wer zuerst landet, nimmt die nächste freie". 146
-- (audit-trail-Dämpfung) ist gelandet, also ist 147 die nächste freie. Keine
-- zweite Migrations-Welle läuft parallel (K1).
--
-- DER BEFUND (X-W1 Teil B, N10, 1 000 Gold-Fälle, zwei Replikat-Läufe V0/V0'
-- derselben Instanz auf demselben Korpus):
-- 84 von 1 000 Fällen liefern in den beiden Läufen verschiedene Ergebnisse.
-- ALLE 84 haben in beiden Läufen genau 100 FTS-Kandidaten — also exakt die
-- Arm-Kappe —, 83 von ihnen wechseln die KANDIDATENMENGE, und der erste
-- Unterschied liegt im Median schon auf Rang 2. Der ANN-Anteil an dieser
-- Streuung ist exakt 0; sie sitzt vollständig in fts_de und fts_en.
--
-- WARUM EIN TIEBREAK UND KEINE GRÖSSERE KAPPE:
-- Die FTS-Arme ranken mit `ROW_NUMBER() OVER (ORDER BY GREATEST(ts_rank_cd
-- …) DESC)` — ohne zweiten Sortier-Schlüssel. Bei Score-Gleichstand ist diese
-- Ordnung nicht total, und der Sortierknoten von PostgreSQL ist nicht stabil:
-- welche der gleich gescorten Zeilen die Ränge bekommt, hängt an der
-- Eingangsreihenfolge, also an der physischen Heap-Ordnung. Die verschiebt
-- sich im laufenden Betrieb bei jedem UPDATE. Läuft die Gleichstands-Gruppe
-- über die Kappe hinaus, entscheidet dieselbe Instabilität zusätzlich, WELCHE
-- Zeilen überhaupt in den Arm kommen. Eine größere Kappe verschiebt diese
-- Grenze nur; total geordnet wird der Arm erst durch einen Tiebreak.
--
-- DIE ÄNDERUNG, UND WARUM SIE AN ZWEI STELLEN JE ARM STEHT:
-- Vorbild ist der semantische exact-Pool aus Generation 17
-- (139_rrf_gen17_tiebreak.sql:210/:213, in 145 unverändert als :465/:468) und
-- der Trigramm-Arm aus 140 (145:565/:580): dort steht `, <id>` sowohl im
-- Fenster-ORDER-BY als auch im äußeren ORDER BY vor dem LIMIT.
--   * `, cb.id` im ROW_NUMBER-Fenster macht den RANG total geordnet.
--   * `, cb.id` im äußeren ORDER BY vor dem LIMIT macht die MENGE strukturell
--     bestimmt, statt sie an der Ausgabereihenfolge des WindowAgg-Knotens
--     hängen zu lassen. Dass dieser Knoten seine Eingangssortierung durchreicht,
--     ist eine Eigenschaft der Implementierung, keine Zusage von SQL — und die
--     Menge ist genau das, was N10 in 83 von 84 Fällen kippen sah.
-- cb.id ist Primärschlüssel (UUIDv7, NOT NULL, eindeutig), die Ordnung ist mit
-- ihm total. Als LETZTER Schlüssel kann er eine echte Score-Ordnung nie ändern;
-- er wird nur bei bitgleichem GREATEST-Wert überhaupt verglichen.
--
-- WAS DIE ZWEITE STELLE KOSTET (gemessen, nicht geschätzt):
-- Nichts. TestC23PlanShape EXPLAINt den RETURN-QUERY-Rumpf von 145 und den von
-- 147 mit denselben Argumenten auf derselben Fixture: der Knoten-Zensus ist
-- identisch (u. a. Sort=5, WindowAgg=5), und genau ZWEI Sortier-Schlüssel
-- ändern sich — die WindowAgg-Eingangssortierungen von fulltext_de und
-- fulltext_en, jeweils um ein angehängtes `, cb_N.id`. Das äußere ORDER BY
-- erzeugt keinen eigenen Sortier-Knoten, weil seine Sortier-Schlüssel die des
-- WindowAgg-Eingangs sind und der Planner das erkennt. Am Ziel-Scale (1M+) ist
-- das der Unterschied zwischen einer Schlüssel-Erweiterung und einem zweiten
-- Sortierlauf über die volle FTS-Kandidatenmenge.
--
-- WARUM BEIDE FUNKTIONEN:
-- ctx_rrf ist der Serving-Pfad, ctx_rrf_arms das Mess-Instrument, aus dem der
-- Sweep die Fusion offline nachrechnet. Zöge nur eine der beiden nach, misst
-- der Sweep eine Fusion, die es nicht gibt (140:123-127). Der Wächter dagegen
-- ist das Paritäts-Gate internal/rrf/arms_parity_integration_test.go
-- (137:76-83); die Welle selbst pinnt beide Körper zusätzlich strukturell in
-- internal/rrf/fts_tiebreak_c23_integration_test.go.
--
-- WARUM CREATE OR REPLACE UND KEIN EDIT AN 139/142/145:
-- Dieselbe forward-only-Doktrin wie in 138/146: die Grenze ist der Commit,
-- nicht der Applikationszustand irgendeiner Instanz. .hooks/pre-commit Gate 3
-- zieht dieselbe Linie. Die beiden Körper unten sind byte-genaue Kopien aus
-- 145_partial_fts_gin.sql (ctx_rrf :353-647, ctx_rrf_arms :655-952); der
-- einzige Unterschied sind die acht Tiebreak-Stellen, vier je Funktion.
--
-- NICHT IN DIESER MIGRATION (bewusst, jeweils eigene Welle):
--   * das Partitions-Prädikat der FTS-Arme — W-BF7;
--   * jede Kappen-Änderung (100/100/75/30 bleiben, wo sie stehen);
--   * ein Tiebreak im ANN-Arm (semantic_ann, 145:427-429/:442). Der ist
--     approximativ und plan-abhängig; X-W1 hat seinen Streuungs-Anteil mit
--     exakt 0 gemessen, also gibt es dafür weder Befund noch Messung.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- (a) ctx_rrf — Serving-Pfad. Körper aus 145_partial_fts_gin.sql:353-647,
-- erweitert um genau vier Tiebreak-Stellen (fulltext_de und fulltext_en, je
-- Fenster-ORDER-BY und äußeres ORDER BY). Alles andere ist Kopie.
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
                ) DESC, cb.id
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
        ORDER BY GREATEST(
            ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query)),
            ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query_spaced)),
            CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                 THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_query_or))
                 ELSE 0.0 END,
            CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                 THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_temporal))
                 ELSE 0.0 END
        ) DESC, cb.id
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
                ) DESC, cb.id
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
        ORDER BY GREATEST(
            ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query)),
            ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query_spaced)),
            CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                 THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_query_or))
                 ELSE 0.0 END,
            CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                 THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_temporal))
                 ELSE 0.0 END
        ) DESC, cb.id
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

-- (b) ctx_rrf_arms — Mess-Instrument. Körper aus 145_partial_fts_gin.sql:655-952,
-- erweitert um dieselben vier Tiebreak-Stellen. Die FTS-Arme der beiden
-- Funktionen bleiben damit klausel-identisch, was das Paritäts-Gate prüft.
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
                ) DESC, cb.id
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
        ORDER BY GREATEST(
            ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query)),
            ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query_spaced)),
            CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                 THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_query_or))
                 ELSE 0.0 END,
            CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                 THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_temporal))
                 ELSE 0.0 END
        ) DESC, cb.id
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
                ) DESC, cb.id
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
        ORDER BY GREATEST(
            ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query)),
            ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query_spaced)),
            CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                 THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_query_or))
                 ELSE 0.0 END,
            CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                 THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_temporal))
                 ELSE 0.0 END
        ) DESC, cb.id
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
