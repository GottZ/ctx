-- =============================================================================
-- 134_rrf_gen16_ann_embedding_filter.sql — ctx_rrf Generation 16
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Issue #40 „Bug 5: The semantic search channel includes blocks with no
-- embedding". Genau EINE semantische Zeile gegenüber Generation 15
-- (112_rrf_gen15_dual_arm.sql, gefoldet in 113_baseline.sql): der
-- semantic_ann-CTE bekommt `AND cb.embedding IS NOT NULL`. Alles andere —
-- Signatur, Validierung, Cap-Wächter, exact-Arm, fulltext_de/en,
-- trigram_title, block_mass, type_factor, rrf-Fusion, finale Projektion —
-- ist byte-identisch zu Gen 15.
--
-- WARUM: `ORDER BY cb.embedding <=> p_embedding` ist ASC, und ASC sortiert in
-- Postgres per Default NULLS LAST. Unter einem Seq-Scan-Plan reicht das nicht:
-- sobald weniger als 75 embeddete Kandidaten im Scope stehen, füllen
-- unembeddete Blöcke den Rest des LIMIT 75 in Heap-Reihenfolge auf. Sie tragen
-- cos_sim NULL, bekommen aber echte Ränge — und die Fusion gattert nirgends auf
-- cos_sim, sie zählt nur den Rang. Der Semantik-Kanal ist mit 0.45 das
-- schwerste Gewicht: ein unembeddeter Block auf Semantik-Rang 5 liefert
-- 0.45/65 = 0,00692 und schlägt damit einen perfekten deutschen FTS-Rang-1-
-- Treffer (0.20/61 = 0,00328) um Faktor 2,1. Das ist keine kosmetische
-- Verunreinigung, sondern eine Rangumkehr.
--
-- WANN ES GREIFT: Seq-Scan-Plan UND embeddete Kandidaten < 75, beides
-- gleichzeitig. Genau die frische Installation (winzige Tabelle → Planner nimmt
-- Seq-Scan, fast nichts embeddet), dazu kleine Korpora und schmal gefilterte
-- Scopes. Neue Blöcke entstehen mit NULL-Vektor (internal/store/blocks.go:283
-- schreibt die Spalte nicht), und ein inhaltsänderndes Re-Upsert nullt einen
-- bestehenden Vektor aktiv (blocks.go:462) — das Fenster ist nicht auf Tag 1
-- beschränkt, es öffnet sich nach jeder Inhaltsänderung erneut, bis der
-- Backfill greift. Unter einem HNSW-Index-Scan ist das Leck strukturell
-- unmöglich: pgvector indiziert NULL-Vektoren nicht.
--
-- DIESE MIGRATION HEBT DIE „ABWÄRTSKOMPATIBILITÄT BY DEFAULT"-ZUSAGE DER
-- GENERATION 15 FÜR DEN ANN-ARM AUF, UND ZWAR BEWUSST. Gen 15 hat zugesagt,
-- bei Default-Parametern semantisch identisch zu Gen 14 zu bleiben, damit
-- Migration und Go-Binary entkoppelt deploybar sind. Diese Zusage schützt einen
-- Deploy-ABLAUF, keine Semantik — und die Semantik, die sie hier einfriert, ist
-- ein Defekt. Der Gen-15-Kommentar zum „Semantik-Delta 2" beschreibt exakt
-- denselben Defekt, den der Melder findet, inklusive Bedingung
-- („Seq-Scan-Pläne, kleine Kardinalität") und Folge („erhalten
-- NULL-Embedding-Zeilen dort heute Ränge"). Er behauptet nirgends, das
-- Verhalten sei richtig; er ist eine bewusst zurückgestellte Altlast. Gegen das
-- Szenario „frische Installation" trägt die Zusage ohnehin nicht: dort gibt es
-- keinen Gen-14-Zustand, zu dem Kompatibilität zu wahren wäre.
--
-- DER GEN-15-DEKLARATIONS-KOMMENTAR IST DAMIT ÜBERHOLT: der Block in
-- 113_baseline.sql:10961-10965 („embedding IS NOT NULL NUR im exakt-Arm …
-- bewusst NICHT Teil dieser Generation") gilt ab Gen 16 nicht mehr. Er wird
-- dort NICHT korrigiert — 113_baseline.sql ist gefoldete Historie mit
-- Checksum-Ledger (migrations/fold.go, TestFoldedChainIntegrity hasht jede
-- Sektion gegen die Identität, die _migrations trägt); eine Kommentar-Edit im
-- 112er-Abschnitt bräche den Ledger. Diese Datei ist der Ort, an dem der
-- aktuelle Stand steht.
--
-- AUF DEM LIVE-KORPUS IST DIE ÄNDERUNG EIN NO-OP, DREIFACH BELEGT (25.08.):
--   1. Alle 5354 aktiven Blöcke mit embedding IS NULL gehören zu `checkpoint`
--      (5352) und `system-meta` (2). Beide tragen retrieval.policy = 'excluded'
--      in context_block_types, stehen damit nicht in p_types_visible und sind
--      für beide Semantik-Arme strukturell unerreichbar.
--   2. 1996 aktive embeddete Blöcke > 75 — die zweite Bedingung des Lecks ist
--      nicht erfüllt, auch unter erzwungenem Seq-Scan (gemessen: 0 NULL-Zeilen
--      in den Top 75).
--   3. Der reale Plan ist ein HNSW-Index-Scan (idx_embedding_hnsw).
-- Das Gate ist deshalb eine geseedete Fixture (internal/rrf,
-- TestGen16AW2_ANNArmExcludesNullEmbeddings: 20 embeddete + 55 unembeddete
-- Blöcke eines sichtbaren Typs, Index- und Bitmap-Scans in der Test-Tx
-- abgeschaltet), nicht die Live-DB.
--
-- INVARIANTE Nr. 1 (§5.1, aus dem 073-Header fortgeschrieben) bleibt gewahrt
-- und wird sogar strenger: beide Arme tragen jetzt den wörtlich identischen
-- Prädikat-Block inklusive Klammerungs-Invariante (archived + beide
-- Type-Konjunkte strikt VOR der (scope OR grant)-Klammer). Der Paritäts-
-- Sentinel (Gate W02-1-G2) verliert damit seine NULL-Ausnahme: die beiden
-- Semantik-Arme sind auf der Sentinel-Fixture echt mengengleich.
--
-- Function-only, kein Tabellen-/Spalten-Change: der test.sh-T07-Zähler bleibt
-- unverändert. Das Schema-Contract-Manifest ändert sich trotzdem — der
-- Funktions-Fingerprint von ctx_rrf und migration_max (133 → 134). Idempotent
-- (DROP IF EXISTS beider Signaturen + CREATE OR REPLACE), forward-only.
-- Registrierung übernimmt der Runner (store.RunMigrations, 108+-Konvention).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- Drop der Gen-14-Signatur (15 Parameter) und der 18-Parameter-Signatur
-- (idempotente Re-Runs) — 048/068/073/112-Muster gegen 42725-Overload-
-- Ambiguität. Die Signatur selbst ist gegenüber Gen 15 unverändert; der DROP
-- steht hier, weil CREATE OR REPLACE eine bestehende Überladung nicht
-- entfernen würde, wenn eine Installation je auf der 15er-Signatur stehen
-- bleibt.
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT, TEXT[], TEXT[], DOUBLE PRECISION[], TEXT[], TEXT[], UUID[]);
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT, TEXT[], TEXT[], DOUBLE PRECISION[], TEXT[], TEXT[], UUID[], TEXT, INTEGER, INTEGER);

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
    ORDER BY r.score DESC
    LIMIT p_limit;
END;
$$;
