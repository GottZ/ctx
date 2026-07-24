-- 115_hnsw_ef_construction_canonical.sql
-- Evokoa-Clean-Room design/03 §3.3, W03-6, E-03-2-Entscheid (DECISIONS.md:
-- "128 — Mig 115 kodifiziert den Live-Stand; Prod-No-op, Fresh-DB baut
-- künftig 128"). Kanonisiert den seit Session 3 live gefahrenen Build-
-- Parameter (128) in der Migrations-Kette; schließt den einzigen bekannten
-- Definitions-Drift (idx_embedding_hnsw ef_construction: Migrations-Wahrheit
-- 64 seit 001_initial.sql:250-252, Live-Ist 128 seit Session 3 — W11).
--
-- Guard: Inline-Rebuild NUR bei bekannt-kleiner Tabelle. reltuples < 0 (nie
-- analysiert — seit PG14 Default für frische Tabellen, UND der Zustand nach
-- einem pg_restore, der keine Planner-Statistiken überträgt) zählt als
-- unbekannt und weicht auf einen bounded count aus (LIMIT 500000 — stoppt
-- früh, nie ein Vollscan). Ohne diese Umkehr hätte ein per Restore
-- eingespielter 10M-Bestand (reltuples=-1, echte Zeilenzahl weit über der
-- Schwelle) die naive Bedingung COALESCE(reltuples,0) < 500000 erfüllt und
-- einen Inline-Stunden-Build IM Boot-Pfad ausgelöst — exakt den Fall, den
-- dieser Guard verhindern soll.
--
-- Namespace-Filter (n.nspname = 'public'): ohne ihn liefert SELECT INTO bei
-- einer Namens-Kollision (z. B. ein gleichnamiges Objekt in einem anderen
-- Schema) eine beliebige Zeile statt der öffentlichen Relation.
--
-- Idempotent: der reloptions-Check am Anfang macht jeden Re-Lauf (inkl.
-- Prod-Boot nach dieser Migration) zum No-op, sobald ef_construction=128
-- einmal erreicht ist.
DO $$
DECLARE
    v_opts TEXT[];
    v_rows BIGINT;
BEGIN
    SELECT c.reloptions INTO v_opts
      FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname = 'public' AND c.relname = 'idx_embedding_hnsw';

    IF v_opts IS NULL OR NOT ('ef_construction=128' = ANY (v_opts)) THEN
        SELECT c.reltuples::BIGINT INTO v_rows
          FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = 'public' AND c.relname = 'context_blocks';

        IF v_rows IS NULL OR v_rows < 0 THEN
            -- Größe unbekannt: bounded count statt Schätzung (Fresh-DB: 0,
            -- Millisekunden; 10M-Restore: stoppt nach 500k gescannten
            -- Zeilen, einmalige Migrations-Kosten, keine Vollscan-Gefahr).
            SELECT count(*) INTO v_rows
              FROM (SELECT 1 FROM context_blocks LIMIT 500000) t;
        END IF;

        IF v_rows < 500000 THEN
            DROP INDEX IF EXISTS idx_embedding_hnsw;
            CREATE INDEX idx_embedding_hnsw
                ON context_blocks USING hnsw ((embedding::halfvec(1024)) halfvec_cosine_ops)
                WITH (m = 16, ef_construction = 128);
        ELSE
            RAISE WARNING 'idx_embedding_hnsw: ef_construction != 128 at % rows — rebuild out-of-band (CREATE INDEX CONCURRENTLY, Runbook design/03 §3.3), contract bleibt rot bis dahin', v_rows;
        END IF;
    END IF;
END $$;
