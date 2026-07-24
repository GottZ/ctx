-- =============================================================================
-- 112_rrf_gen15_dual_arm.sql — ctx_rrf Generation 15: Dual-Arm-semantic-CTE
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Achse 02 W02-1 (Evokoa-Clean-Room, design/02-strategy-selektor.md §3.2/§4.4).
-- Die 15. ctx_rrf-Generation: der semantic-Kanal wird zum Dual-Arm — der
-- heutige HNSW-Pfad (`ann`) und ein exakter Materialisierungs-Arm (`exact`)
-- für kleine Scopes (Recall 1,0, deterministische Ränge). Drei neue
-- Parameter am Ende, alle defaulted:
--
--   p_semantic_mode  TEXT    DEFAULT 'ann'   — 'ann' | 'exact'; alles andere
--                                              → RAISE (fail-closed, §5.2).
--   p_scan_tuples    INTEGER DEFAULT NULL    — >0 = SET LOCAL
--                                              hnsw.max_scan_tuples (nur im
--                                              ann-Arm, Grauzonen-Budget);
--                                              ≤0 oder >200000 → RAISE.
--                                              Obergrenze 200000 = SQL-seitige
--                                              letzte Verteidigungslinie;
--                                              Wert synchron zum Go-Clamp
--                                              (§5.4, W02-2) halten.
--   p_exact_cap      INTEGER DEFAULT NULL    — Pflicht im exact-Modus:
--                                              konstruktiver Deckel des
--                                              exakt-Arms (LIMIT) + In-Body-
--                                              Wächter (§5.6); im ann-Modus
--                                              ignoriert.
--
-- ABWÄRTSKOMPATIBILITÄT BY DEFAULT: der bestehende 15-Argument-Call
-- (rrf/search.go) läuft über die Parameter-Defaults semantisch identisch
-- weiter — Migration und Go-Binary sind entkoppelt deploybar (der Selektor
-- kommt erst mit W02-2 in rrf.Search).
--
-- Mechanik des Dual-Arms (ein Statement, kein dupliziertes RETURN-QUERY-Paar
-- — Anti-Divergenz-Entscheid §5.1):
--
--   * One-Time-Filter-Gating: `p_semantic_mode = '…'` referenziert nur einen
--     Parameter — der Planner hebt es als gating qual über den Scan; der
--     inaktive Arm wird zur Ausführungszeit komplett übersprungen (auch im
--     generischen Plan des PL/pgSQL-Plan-Caches).
--   * MATERIALIZED-Barriere: der exakt-Arm sortiert über eine materialisierte
--     Pool-CTE ohne Index — der HNSW-Index ist für ihn strukturell
--     unerreichbar, nicht bloß unattraktiv (robust gegen künftige
--     Planner-/pgvector-Kostenmodell-Änderungen). Die Pool-CTE berechnet die
--     Distanz beim Materialisieren und trägt nur (id, dist) ≈ ~16 B/Zeile
--     statt ~2 KB halfvec.
--   * Cap-Wächter (TOCTOU, §5.6): Go-Probe und dieser Call sind getrennte
--     Transaktionen ohne gemeinsamen Snapshot — wächst der Scope im Fenster
--     dazwischen (Un-Archive-Sweep, Bulk-Restore), wiederholt der exact-Zweig
--     die gedeckelte Probe in derselben Transaktion (Index
--     idx_context_scope_active, ≤ p_exact_cap+1 Einträge) inklusive
--     Grant-Addition und failt laut: ERRCODE 54000 (program_limit_exceeded);
--     Go degradiert auf ann (W02-2). `LIMIT p_exact_cap` auf der Pool-CTE
--     bleibt als konstruktive Schranke fürs Rest-Race intra-Call.
--   * Deterministischer Tiebreak (dist, id) NUR im exakt-Arm — macht ihn als
--     Ground-Truth-Leg für Achse 01 reproduzierbar. Der ann-Arm bleibt
--     bewusst ohne Tiebreak (Ist-Verhalten, approximativ per Definition).
--   * `embedding IS NOT NULL` NUR im exakt-Arm (deklariertes Semantik-Delta 2,
--     §4.5): der ann-Arm behält die Ist-Semantik der Gen 14 — bei
--     Seq-Scan-Plänen (kleine Kardinalität) erhalten NULL-Embedding-Zeilen
--     dort heute Ränge (NULLS LAST). Eine Angleichung bräche die
--     G1-Default-Kompatibilität und ist bewusst NICHT Teil dieser Generation.
--
-- INVARIANTE Nr. 1 (§5.1): beide Arme tragen den WÖRTLICH identischen
-- Sichtbarkeits-Prädikat-Block inklusive Klammerungs-Invariante (archived +
-- beide Type-Konjunkte strikt VOR der (scope OR grant)-Klammer, 073-Header).
-- Jede künftige Prädikat-Änderung MUSS beide Arme treffen — der
-- Paritäts-Sentinel (Gate W02-1-G2) reißt sonst.
--
-- `SET LOCAL`-Reichweite (dokumentierte Falle, wie 073): gilt bis
-- Transaktionsende. Heute ist der Call eine implizite
-- Single-Statement-Transaktion — die GUCs sterben mit dem Statement. Bettet
-- je ein Caller ctx_rrf in eine längere Transaktion ein, erben
-- Folge-Statements relaxed_order/max_scan_tuples.
--
-- fulltext_de/en, trigram_title, block_mass, type_factor, rrf-Fusion und
-- finale Projektion: byte-identisch zu Gen 14 (073).
--
-- Function-only, kein Tabellen-/Spalten-Change: test.sh-T07-Zähler
-- unverändert. Idempotent (DROP IF EXISTS beider Signaturen + CREATE OR
-- REPLACE), forward-only. Registrierung übernimmt der Runner
-- (store.RunMigrations, 108+-Konvention).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- Drop der Gen-14-Signatur (15 Parameter) und der neuen 18-Parameter-Signatur
-- (idempotente Re-Runs) — 048/068/073-Muster gegen 42725-Overload-Ambiguität.
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
          AND cb.embedding IS NOT NULL         -- Semantik-Delta 2 (§4.5): nur im exakt-Arm
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
