-- =============================================================================
-- 142_arms_typename.sql — ctx_rrf_arms Generation 3: neunte Spalte type_name
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Achse 05 (Empirische Validierung), Welle M-W1. design/05 §4.3 (die erste der
-- zwei Instrument-Erweiterungen) + §7 Zeile M-W1; Masterplan §2 K1 (Reihenfolge
-- der Migrations-Wellen) und K10 (ctx_rrf_arms ist ein OFFLINE-Instrument, kein
-- Live-Schalter — diese Migration ändert deshalb keine einzige ausgelieferte
-- Antwort).
--
-- NUMMER: Masterplan §2 K1 — „wer zuerst landet, nimmt die nächste freie".
-- Gelandet sind 139, 140 und 141, also ist 142 die nächste freie. Keine zweite
-- Migrations-Welle läuft parallel (K1). Die Nummer „141" im Design-Text ist ein
-- Platzhalter aus der Planungsphase, kein Anspruch.
--
-- WAS DIESE MIGRATION IST: eine Funktionskörper-Ersetzung an EINER Funktion.
-- ctx_rrf_arms bekommt eine NEUNTE Rückgabespalte, type_name TEXT. Kein Schema,
-- kein Index, kein Schreiber, keine Signatur-Änderung (23 Parameter, Positions-
-- Parität 1–18 zu ctx_rrf bleibt Vertrag, 137:104-105). ctx_rrf wird NICHT
-- angefasst — keine neue Fusions-Generation, kein Gewichts-Literal, kein
-- Deckel, keine Schwelle.
--
-- WOHER DER KÖRPER STAMMT: aus 140_trigram_gist_knn.sql:495-771, nicht aus 137.
-- 137 hat ctx_rrf_arms eingeführt, aber V-W4 (140) hat den Körper zuletzt
-- ersetzt (KNN-Trigramm-Arm). Wer hier von 137 abschriebe, würde den
-- Trigramm-Arm still auf den Ist-Stand VOR V-W4 zurückdrehen. Der Diff dieser
-- Datei gegen den 140-Block ist deshalb bewusst winzig und steht vollständig
-- in den zwei Absätzen unten; alles andere ist Kopie.
--
-- WARUM type_name UND NICHT MEHR (§4.3): der Dump kennt heute die
-- Typ-Zusammensetzung seiner eigenen Kandidaten nicht. Weder „wie viele der
-- Top-10 sind Katalog-Blöcke?" noch „was verdrängt der Katalog?" ist aus einem
-- Dump beantwortbar. type_factor steht bereits im RETURN, taugt dafür aber
-- nicht: der Faktor ist NICHT injektiv — zwei gedämpfte Typen dürfen denselben
-- Faktor tragen, und jeder ungedämpfte Typ trägt 1.0 wie jeder andere
-- ungedämpfte. Der Typname ist die kleinste Ergänzung, die die Frage
-- beantwortet, und er ist genau das, was der Damping-Sweep (M-W8) braucht, um
-- den Faktor je Typ offline zu ERSETZEN statt ihn aus der Zeile zu lesen.
--
-- WARUM DAS DIE LEAK-BILANZ NICHT VERSCHIEBT: der 137-Kopf begründet „KEINE
-- INHALTE IM RETURN" damit, dass ein Prädikat-Fehler höchstens die EXISTENZ
-- einer UUID leckt. Ein Typname ist kein Inhalt — er ist eine
-- Registry-Klassifikation aus context_block_types, und derselbe Aufrufer
-- bekommt sie für dieselbe ID über GET /api/context/<id> ohnehin. Der Aufrufer
-- ist zwingend Admin: handler/query.go:588-593 weist arm_ranks ohne Admin-Key
-- mit 403 ab, bevor irgendetwas anderes am Request geprüft wird. Kein Titel,
-- kein Inhalt, kein Scope kommt hinzu.
--
-- DIE EINE SEMANTISCHE ÄNDERUNG, ZWEITEILIG:
--   (1) RETURNS TABLE bekommt `type_name TEXT` als neunte Spalte, ANS ENDE.
--   (2) Der type_factor-CTE führt cb.type_name mit, und das finale SELECT
--       projiziert tf.type_name::TEXT.
--
-- WARUM EIN DROP FUNCTION DAVORSTEHT — UND WAS „vorwärtskompatibel" TROTZDEM
-- HEISST: PostgreSQL lehnt JEDE Änderung der OUT-Spaltenliste einer
-- bestehenden Funktion ab, auch das blosse ANHÄNGEN einer Spalte am Ende
-- (gemessen beim Bau dieser Welle: „cannot change return type of existing
-- function", SQLSTATE 42P13). CREATE OR REPLACE allein genügt also NICHT; es
-- braucht den DROP davor, exakt nach dem Muster von 137:98-101
-- (048/068/073/112/134). 139:84-87 beschreibt den Gegenfall — dort blieben die
-- OUT-Spalten gleich, deshalb reichte dort CREATE OR REPLACE.
-- Vorwärtskompatibel ist die Änderung auf der AUFRUFER-Seite, nicht in
-- pg_proc: wer seine acht Spalten namentlich projiziert (rrf/arms.go:53-54,
-- arms_parity_integration_test.go:340), sieht keinen Unterschied — die Spalten
-- wachsen am Ende, sie werden nicht umgeordnet. Der DROP trifft ausschliesslich
-- ctx_rrf_arms; die OID von ctx_rrf bleibt unangetastet, und ctx_rrf_arms hat
-- genau EINEN Aufrufer, die admin-gated arm_ranks-Naht.
--
-- WARUM DIE PROJEKTION AM type_factor-CTE HÄNGT UND NICHT AN EINEM ZWEITEN
-- JOIN AUF context_blocks: der CTE liest context_blocks für jeden sichtbaren
-- Block ohnehin bereits (er ist die Damping-Quelle) und ist im finalen SELECT
-- ohnehin schon per LEFT JOIN angebunden. Eine Spalte mehr in seiner
-- Projektionsliste kostet keinen zusätzlichen Plan-Knoten und keinen
-- zusätzlichen Tabellenzugriff — am Ziel-Scale (1M+ Blöcke) ist das der
-- Unterschied zwischen „kostenlos" und „ein weiterer Join, dessen Plan der
-- Planner wählen darf". ctx_rrf löst dieselbe Aufgabe mit einem eigenen
-- `JOIN context_blocks cb` (140:486), aber dort ist der Join ohnehin nötig,
-- weil die Funktion Titel, Inhalt und Scope liefert; hier ist er es nicht.
--
-- WARUM DIE PARITÄT TRÄGT (die Zusage: type_name = context_blocks.type_name für
-- JEDE zurückgegebene ID): das WHERE des type_factor-CTE ist eine ECHTE
-- OBERMENGE des WHERE jedes einzelnen Arms. Der CTE verlangt drei Bedingungen —
-- NOT is_archived, type_name = ANY(p_types_visible), scope/grant —, und jeder
-- der vier Arme verlangt dieselben drei UND zusätzlich p_types_exclude,
-- p_category, p_tags, p_categories_exclude. Jede Kandidaten-ID kommt aus einem
-- dieser Arme, also existiert für sie zwingend eine tf-Zeile, und der LEFT JOIN
-- kann nicht ins Leere greifen. tf.id ist dabei die PK-Spalte, also höchstens
-- eine Zeile je ID — die Zeilenmenge des finalen SELECT bleibt exakt die von
-- 140. Ein tf-Fehlgriff wäre ab jetzt SICHTBAR (type_name NULL) statt still
-- (COALESCE(tf.factor, 1.0) hat ihn bisher als „ungedämpft" maskiert); das
-- Paritäts-Gate prüft ausdrücklich auf NULL.
--
-- KEIN ARM WIRD ANGEFASST: die Arm-Mitgliedschaft hängt nicht vom Typ ab
-- (§4.3 — type_factor wirkt multiplikativ im Fusions-Term, nicht in den
-- Arm-CTEs). Die neue Spalte taucht in keinem Score-Term auf, weder in SQL noch
-- in armsweep/fuse.go:129. Deshalb bleibt das B-W1-Paritäts-Gate
-- (internal/rrf/arms_parity_integration_test.go, TestBW1ArmsFusionParity /
-- TestBW1ArmsVisibilityParity) unverändert grün: es projiziert seine acht
-- Spalten namentlich und rechnet die Fusion daraus nach.
--
-- GEGENGEPROBT (design/05 §7, Konvention „negativ-geprobt"):
-- internal/rrf/arms_typename_mw1_integration_test.go. Der rote Anker
-- TestMW1ArmsTypeNameAbsentAt141 fährt die Kette gedeckelt bei 141 und verlangt
-- SQLSTATE 42703 auf genau dieser Spalte — der Beleg „142 führt sie ein" bleibt
-- damit nach der Landung prüfbar. TestMW1ArmsTypeNameParity prüft die Parität
-- über eine Fixture mit VIER Typen (knowledge, reference, audit-trail,
-- checkpoint), einmal unter der Produktions-Allowlist und einmal mit allen
-- vieren, plus dieselbe Spalte durch die Go-Naht (rrf.ArmRow.TypeName).
-- TestMW1ArmsTypeNameConstantProbe installiert eine Variante dieser Datei, in
-- der `tf.type_name::TEXT` durch eine Konstante ersetzt ist, und verlangt, dass
-- die Parität davon rot wird.
--
-- LOCKSTEP MIT Go: rrf.ArmRow bekommt TypeName string `json:"type_name"`
-- (rrf/arms.go), armsQuery projiziert die neunte Spalte, queryRRFArms scannt
-- sie. Das Dump-Schema ist rrf.ArmRow (armsweep/dump.go:67), zieht also ohne
-- eigenen Code mit; ein VOR dieser Welle geschriebener Dump bleibt lesbar und
-- liefert den leeren String (ReadRecords ist ein json.Unmarshal je Zeile ohne
-- Schema-Version, armsweep/dump.go:180-197 — gepinnt in
-- internal/armsweep/dump_typename_mw1_test.go).
--
-- REVERT (der Baum kennt keine Down-Migrationen, forward-only bleibt die
-- Regel). Wer den Zustand temporär zurückdrehen muss, stellt die DROP-Zeile
-- unten voran und führt danach den ctx_rrf_arms-Block aus
-- 140_trigram_gist_knn.sql erneut aus — er liegt unverändert im Baum. Der DROP
-- ist auf dem Rückweg aus demselben Grund nötig wie hier (42P13).
-- `_migrations` bleibt unangetastet; ein erneuter Boot spielt 142 nicht noch
-- einmal ein.
--
-- Function-Change, kein Tabellen-/Spalten-Change: der test.sh-T07-Zähler bleibt
-- unverändert. Das Schema-Contract-Manifest ändert sich an genau zwei Stellen —
-- src_hash von ctx_rrf_arms und migration_max 141 → 142; der src_hash von
-- ctx_rrf bleibt gleich. Idempotent (DROP IF EXISTS + CREATE OR REPLACE),
-- forward-only. Registrierung übernimmt der Runner (store.RunMigrations,
-- 108+-Konvention).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- Idempotenter Re-Run UND Pflicht für diese Welle: CREATE OR REPLACE kann die
-- OUT-Spalten einer bestehenden Definition nicht umbauen (42P13), deshalb der
-- DROP davor (137:98-101, 048/068/073/112/134-Muster). Die Argumentliste ist
-- gegenüber 137/140 unverändert, also ist es dieselbe DROP-Zeile wie in 137.
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
        -- M-W1: derselbe CTE wie in 140, um EINE Projektions-Spalte erweitert.
        -- Das WHERE ist unverändert und bleibt damit die Obermenge jedes Arms
        -- (Begründung im Dateikopf) — die Kandidatenmenge ändert sich nicht.
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
        -- (Obermengen-Argument im Dateikopf); wäre er es doch, soll er als NULL
        -- sichtbar werden statt hinter einem Ersatzwert zu verschwinden.
        tf.type_name::TEXT                             AS type_name
    FROM semantic s
    FULL OUTER JOIN fulltext_de d USING (id)
    FULL OUTER JOIN fulltext_en e USING (id)
    FULL OUTER JOIN trigram_title g USING (id)
    LEFT JOIN block_mass m ON m.id = COALESCE(s.id, d.id, e.id, g.id)
    LEFT JOIN type_factor tf ON tf.id = COALESCE(s.id, d.id, e.id, g.id);
END;
$$;
