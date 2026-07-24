-- 110_recall_runs.sql
-- Achse 01 W01-1: recall_check-Messläufe — ANN (HNSW, produktionsidentische
-- Prädikate bis auf den Grant-Arm) vs. exakte Brute-Force-Referenz,
-- scope-geschichtet. Aggregate only — die Tabelle darf nie Query-Texte,
-- Block-IDs oder Vektoren tragen (Leak-Schutz = Allowlist-Assertion im
-- Insert-Pfad, fail-closed). scope ist hier Mess-Objekt-Bezeichner (welcher
-- Scope wurde vermessen), kein Sichtbarkeits-Diskriminator — die Tabelle ist
-- ausschließlich server-admin-sichtbar. Kein Backfill-Pfad: nötig ist keiner,
-- erlaubt ist keiner — historische Rankings aus context_access_log wären
-- ANN-gegen-sich-selbst (kein Brute-Force-Referenzleg existierte je) und
-- damit als Recall-Backfill Scheindaten.

CREATE TABLE IF NOT EXISTS context_recall_runs (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    ran_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    run_group       UUID NOT NULL,            -- ein Scheduler-Lauf = eine Gruppe über alle Strata
    stratum         TEXT NOT NULL,            -- 'small' | 'medium' | 'large' | 'all'
    scope           VARCHAR(50),              -- vermessener Scope; NULL beim Pseudo-Stratum 'all'
    corpus_embedded INT  NOT NULL,            -- sichtbare embeddete Blöcke im Mess-Fenster
    k               SMALLINT NOT NULL,        -- 10 und 75 (zwei Zeilen pro Probe-Satz)
    n_queries       SMALLINT NOT NULL,        -- tatsächlich gemessene Queries (nach Sampling/Budget)
    query_source    TEXT NOT NULL,            -- 'log' | 'loo' | 'mixed'
    ef_search       INT  NOT NULL,            -- effektiver Wert des ANN-Legs (0 = Default 40)
    iterative_scan  TEXT NOT NULL,            -- 'off' | 'strict_order' | 'relaxed_order'
    valid           BOOLEAN NOT NULL,         -- false => Plan-Assertion verletzt / Budget-/Demand-Abbruch
    recall_avg      DOUBLE PRECISION,         -- NULL wenn NOT valid
    recall_min      DOUBLE PRECISION,         -- schlechtester Einzel-Query-Wert (Frühwarnung)
    ann_ms_p50      DOUBLE PRECISION,
    ann_ms_p95      DOUBLE PRECISION,
    exact_ms_p50    DOUBLE PRECISION,
    meta            JSONB NOT NULL DEFAULT '{}'::jsonb
    -- meta-Schlüssel (kanonische ALLOWLIST): pgvector_version, pg_version,
    -- index_reloptions, embed_model, invalid_reason, budget_exhausted,
    -- strata_bounds, epsilon, n_eff_min, exact_touch_bytes,
    -- buffercache_delta, scope_changed. Jeder andere Schlüssel = Test ROT.
    -- Werte: nur Skalare/kurze Strings. NIE: query_text, block_ids, vectors.
);

CREATE INDEX IF NOT EXISTS idx_recall_runs_ran_at
    ON context_recall_runs (ran_at DESC);
CREATE INDEX IF NOT EXISTS idx_recall_runs_stratum
    ON context_recall_runs (stratum, scope, k, ran_at DESC); -- Status-Read: latest je (stratum,scope,k)
