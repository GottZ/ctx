-- =============================================================================
-- 137_rrf_arms.sql — ctx_rrf_arms: die Arm-Rang-Schwester von ctx_rrf
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Achse 04 §4.2 (Arm-Gewichts-Sweep), Welle B-W1. Provenienz-Naht: diese
-- Funktion liefert je Kandidat die ROHEN Arm-Ränge plus die beiden
-- multiplikativen Faktoren (block_mass, type_factor), aus denen ctx_rrf seinen
-- Score baut. Damit lässt sich die Fusion OFFLINE mit anderen Gewichten
-- nachrechnen, ohne einen einzigen Live-Request anders zu beantworten.
--
-- NUMMER: Masterplan §2 K1 — „wer zuerst landet, nimmt die nächste freie".
-- Gelandet sind 134 (ctx_rrf Gen 16), 135 (distill_run) und 136 (Tool-Evidenz-
-- Blocktypen), also ist 137 die nächste freie.
--
-- WARUM EINE EIGENE FUNKTION UND KEIN EINGRIFF IN ctx_rrf:
-- ctx_rrf ist der Live-Fusionskörper. Jede Erweiterung dort — ein
-- Debug-Ausgabemodus, zusätzliche OUT-Spalten, ein Schalter — ändert entweder
-- die Signatur (und damit den Go-Aufrufpfad und das Deploy-Fenster, in dem
-- Migration und Binary entkoppelt sein müssen) oder den Plan, den der Planner
-- für den Live-Pfad wählt. Die Gen-16-Zusagen (Sichtbarkeits-Invariante,
-- MATERIALIZED-Barriere, Cap-Wächter, fail-closed Validierung) sind teuer
-- erkauft und durch eine Batterie von Gates gepinnt; ein Mess-Instrument darf
-- sie nicht anfassen. Eine Schwester-Funktion kostet dafür Duplikation — siehe
-- DRIFT-RISIKO unten.
--
-- WARUM KEINE INHALTE IM RETURN:
-- Defense-in-Depth. Die Funktion projiziert ausschließlich UUIDs, Ränge und
-- zwei Faktoren — kein title, kein content, keine category, kein scope. Ein
-- Prädikat-Fehler in dieser Funktion (oder in einem späteren HTTP-Pfad davor)
-- leckt damit höchstens die EXISTENZ einer UUID, nie Text. Der bewusste Preis:
-- der Aufrufer muss die IDs separat und regulär auflösen, was ihn erneut durch
-- die reguläre Sichtbarkeitsprüfung zwingt.
--
-- INVARIANTE Nr. 1 (§5.1, aus dem 073-Header über 113/134 fortgeschrieben)
-- gilt hier wörtlich weiter: jeder Arm und block_mass/type_factor tragen
-- denselben Prädikat-Block — NOT is_archived + type_name = ANY(p_types_visible)
-- + types_exclude, und zwar strikt VOR der Klammer
-- (scope = ANY(p_scopes) OR granted), dazu p_category/p_tags/
-- p_categories_exclude. p_types_visible NULL oder leer ⇒ 0 Treffer
-- (Allowlist, fail-closed). Die Prädikate sind aus 134 wörtlich übernommen,
-- INKLUSIVE der bestehenden Asymmetrie des type_factor-CTE (der trägt in 134
-- weder types_exclude noch category/tags/categories_exclude —
-- 134_rrf_gen16_ann_embedding_filter.sql:326-329). Diese Asymmetrie wird hier
-- NICHT repariert: eine Reparatur wäre eine Semantik-Änderung an der Fusion
-- und würde die Parität, die diese Funktion beweisen soll, genau aufheben.
-- Sie ist als Nebenbefund notiert, nicht als Bau.
--
-- WARUM DIE GUCs MITMÜSSEN:
-- Der ann-Arm setzt hnsw.iterative_scan = 'relaxed_order' (und optional
-- hnsw.max_scan_tuples) per SET LOCAL. SET LOCAL in einer Funktion OHNE
-- eigene SET-Klausel wirkt bis zum Transaktionsende, nicht bis zum
-- Funktionsende. Genau deshalb ist die Tx-Naht die Messvorschrift: nur wenn
-- ctx_rrf und ctx_rrf_arms in EINER Transaktion aufgerufen werden, sehen
-- beide dieselbe GUC-Lage und damit denselben ann-Kandidatenraum. Fehlten die
-- GUCs hier, würde die Schwester bei getrenntem Aufruf einen anderen ANN-Pfad
-- laufen und die Paritäts-Messung wäre still falsch.
--
-- p_limit WIRD ENTGEGENGENOMMEN UND IGNORIERT:
-- Die Funktion schneidet nichts ab — sie liefert die volle Kandidatenmenge der
-- vier Arme, weil das Abschneiden genau die Information wäre, die der Sweep
-- rekonstruieren will (mit anderen Gewichten steht eine andere Menge im
-- Top-k). Der Parameter bleibt trotzdem in der Signatur, damit sie Position
-- für Position zu ctx_rrf passt: die Go-Seite (Welle B-W2) reicht dieselben
-- 18 Argumente an beide Funktionen durch, ohne eine zweite Argumentliste zu
-- pflegen. Eine weggelassene Position wäre eine stille Falle für jeden, der
-- die beiden Aufrufe nebeneinander schreibt.
--
-- DIE FÜNF NEUEN PARAMETER (Position 19-23) sind die vier Arm-Deckel und die
-- Trigram-Schwelle, die in ctx_rrf Literale sind. Ihre Defaults sind exakt
-- diese Literale (134:185 und :211 → 75, :250 → 100, :284 → 100, :302 → 30,
-- :301 → 0.05), der Default-Aufruf ist also deckungsgleich mit dem Ist. Sie
-- existieren, weil der Sweep auch die Deckel als Achse braucht: ein Gewicht
-- lässt sich nur bewerten, wenn bekannt ist, wie tief der Arm überhaupt
-- geliefert hat.
--
-- DRIFT-RISIKO UND SEIN WÄCHTER:
-- Der Körper dupliziert Prolog und CTEs aus 134. Driftet 134 später, ohne dass
-- diese Datei nachzieht, misst der Sweep eine Fusion, die es nicht gibt. Der
-- Wächter ist kein Kommentar, sondern das Paritäts-Gate
-- (internal/rrf/arms_parity_integration_test.go): es rechnet die Fusion aus
-- den Arm-Rängen in Go nach und vergleicht sie zeilenweise mit der Ausgabe von
-- ctx_rrf auf derselben Fixture und in derselben Transaktion. Jede
-- Körper-Drift, die die Fusion verändert, macht dieses Gate rot.
--
-- KEIN HTTP-PFAD IN DIESER WELLE. Die Funktion ist ab hier nur per SQL
-- erreichbar. Die Go- und REST-Naht ist Welle B-W2 und kommt admin-gated und
-- READ ONLY.
--
-- Function-only, kein Tabellen-/Spalten-Change: der test.sh-T07-Zähler bleibt
-- unverändert. Das Schema-Contract-Manifest ändert sich (neue Funktion +
-- migration_max 136 → 137). Idempotent (DROP IF EXISTS + CREATE OR REPLACE),
-- forward-only. Registrierung übernimmt der Runner (store.RunMigrations,
-- 108+-Konvention).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- Idempotenter Re-Run: CREATE OR REPLACE kann die OUT-Spalten einer
-- bestehenden Definition nicht umbauen, deshalb der DROP davor (048/068/073/
-- 112/134-Muster).
DROP FUNCTION IF EXISTS ctx_rrf_arms(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT, TEXT[], TEXT[], DOUBLE PRECISION[], TEXT[], TEXT[], UUID[], TEXT, INTEGER, INTEGER, INTEGER, INTEGER, INTEGER, INTEGER, DOUBLE PRECISION);

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
          AND similarity(cb.title, p_query) > p_trgm_threshold
        LIMIT p_cap_trigram
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
