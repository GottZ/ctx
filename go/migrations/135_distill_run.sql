-- =============================================================================
-- 135_distill_run.sql — Lauf-Journal + Dedup-Ledger des Distillers (Stufe 2)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Achse 03, Welle W03-1. design/03 §3.1/§3.2.
--
-- NUMMER: das Design führt diese Migration als "134 (Nummer vorläufig)", der
-- Masterplan §2 K1 als "136". Beide Zahlen waren Reservierungen dreier
-- paralleler Achsen; K1 entscheidet nach Landung ("wer zuerst landet, nimmt
-- die nächste freie"). Gelandet ist 134 (ctx_rrf Gen 16, A04 A-W2), also ist
-- 135 die nächste freie. Der Design-Text bleibt unverändert; verbindlich ist
-- die Nummer hier.
--
-- WARUM EIN JOURNAL UND KEINE ZUSTANDSZEILE (Muster 130): der Distiller liest
-- eine FREMDE Datei, die ein FREMDER Prozess unter ihm mutiert. Genau die
-- Läufe, die man verstehen muss — abgebrochen, budget-gedrosselt, an einer
-- unlesbaren Quelle gescheitert — sind die, die keinen Abschluss schreiben.
-- Eine Zustandszeile, die der nächste Lauf überschreibt, löscht sie.
--
-- ZWEIPHASIG UND FORTSCHREIBEND (die Abweichung zu 130): 130 schreibt
-- outcome='running' vor dem Spawn und UPDATEt danach EINMAL, weil sein Kind
-- atomar ist. Der Distiller ist es nicht — er verarbeitet einen Zeilenbereich
-- in Batches. Er schreibt watermark_to nach jedem Batch fort, ABER ERST,
-- NACHDEM die Insights dieses Batches durabel sind (design §3.2, Eigenschaft
-- 1). Eine 'running'-Zeile mit watermark_to > watermark_from ist ein
-- TEILERFOLG, kein Fehler; der Startup-Sweep (§4.7) stuft sie auf 'killed'
-- herunter, OHNE das Wasserzeichen zu verwerfen.
--
-- DAS WASSERZEICHEN IST ABGELEITET, NICHT GESPEICHERT (§3.0/§3.2). Die
-- KANONISCHE Definition — es gibt genau diese eine, und §3.2 führt dasselbe
-- SQL:
--
--     SELECT COALESCE(max(watermark_to), 0)
--       FROM distill_run
--      WHERE source_key = $1
--        AND outcome <> 'running';
--
-- 'skipped'- und 'failed'-Zeilen sind eingeschlossen, weil beide echten
-- Fortschritt tragen KÖNNEN (ein failed-Lauf nach zwei guten Batches) und
-- eine skipped-Zeile per Regel unten invariant ist. Es gibt keine zweite
-- Zustandsquelle — kein Settings-Key, keine _state-Zeile, keine Datei.
--
-- SKIP-ZEILEN SIND WASSERZEICHEN-INVARIANT: eine Zeile mit outcome='skipped'
-- trägt watermark_from = watermark_to = das zum Skip-Zeitpunkt ABGELEITETE
-- Wasserzeichen. Ist die Quelle unlesbar (Tor 1), ist das der letzte bekannte
-- Wert aus dem Journal — nie 0, nie NULL. max() bleibt damit unberührt.
--
-- RETENTION (§6.2): distill_run über distill.retention_days (Default 90),
-- distill_seen über distill.seen_retention_days (Default 30, kürzer — ein
-- Hash nützt nur so lange, wie derselbe Output wiederkehrt). Beide hängen im
-- 6h-Janitor-Bündel nach dem Muster von runRecallRetention
-- (recall_check.go:352-367: retention=0 ist ein No-op, Fehler werden geloggt,
-- nie fatal). Die Retention selbst kommt mit Welle W03-11; diese Migration
-- legt nur die Indizes, über die sie fahren wird.
--
-- INDEX-KOSTEN, GEMESSEN STATT GESCHÄTZT (Gate W03-1): der Ausdrucks-Index
-- idx_blocks_checkpoint_root ist der einzige Eingriff dieser Migration auf
-- einer BESTANDStabelle. Gemessen auf einer synthetischen Kopie von
-- context_blocks mit 1 000 000 Zeilen, davon 10 000 (1 %) mit
-- metadata->>'root_session_id' über 400 Wurzel-Sessions (25 Blöcke je Session,
-- davon je einer mit Tag 'checkpoint-manifest'), Postgres 18 im Testcontainer:
-- CREATE INDEX in 0,16 s (Gate-Schwelle 30 s; siehe
-- internal/store/migration135_integration_test.go). Damit bleibt der Index in
-- dieser Migration und braucht keinen CONCURRENTLY-Sonderweg — was auch gut
-- ist, denn der Runner fährt jede Datei in genau einer Transaktion
-- (internal/store/migrations.go: pool.Begin je Migration), und CONCURRENTLY
-- kann dort nicht laufen.
--
-- Derselbe Lauf misst den Nutzen: die §4.5-Abfrage fällt von 10 000 Heap-Blöcken
-- / 46,8 ms auf 25 Heap-Blöcke / 1,0 ms.
--
-- Additiv, forward-only, IF NOT EXISTS (Muster 119/130). Ein Re-Run ist
-- folgenlos.
-- =============================================================================

SET LOCAL lock_timeout = '2s';

CREATE TABLE IF NOT EXISTS distill_run (
    -- v4, NIE uuidv7 (K1-Doktrin, Muster 130): ein Zeitstempel im Schlüssel
    -- wäre ein Existenz-/Zeit-Orakel. started_at trägt die Zeit offen.
    run_id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- ── Identität der Quelle ───────────────────────────────────────────────
    -- source_key = "<state_db_label>:<session_id>". Das Label kommt aus
    -- distill.source_label (Config), NICHT aus dem Dateipfad: ein Pfad im
    -- Journal wäre eine Infrastruktur-Aussage in einer Datenzeile, und ein
    -- Mount-Wechsel würde die Wasserzeichen-Ableitung still zerreißen.
    -- GRANULARITÄT: eine Zeile je (Lauf × Quelle). Ein Tick, der bis zu
    -- distill.max_sessions_per_run Quellen anfasst, schreibt bis zu so viele
    -- Zeilen — §6.2 rechnet damit.
    source_key      TEXT        NOT NULL,
    -- aus sessions.parent_session_id aufgelöst; NULL = nicht auflösbar.
    root_session_id TEXT,

    -- ── Verlauf ────────────────────────────────────────────────────────────
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at     TIMESTAMPTZ,            -- NULL = laufend ODER hart gestorben
    outcome         TEXT        NOT NULL,
    skip_reason     TEXT,

    -- ── Zeilenbereich (die tragende Größe) ─────────────────────────────────
    -- watermark_from ist exklusiv, watermark_to inklusiv: der Lauf hat
    -- (from, to] verarbeitet. from = to bedeutet "nichts geschafft".
    watermark_from  BIGINT      NOT NULL,
    watermark_to    BIGINT      NOT NULL,

    -- gen: die Generationsnummer, die im Block-Titel steht. Wird beim INSERT
    -- der running-Zeile MITGESCHRIEBEN (nicht später gezählt), damit ein
    -- nachträglicher Outcome-Wechsel durch den Sweep sie nicht verschiebt.
    -- Der Titel selbst ist am Wasserzeichen verankert (§4.5) — gen ist reine
    -- Lesbarkeit, keine Identität.
    gen             INTEGER     NOT NULL DEFAULT 0,

    -- ── Ledger (die Zahlen, die §6 messbar machen statt annehmen) ──────────
    rows_seen         INTEGER   NOT NULL DEFAULT 0,   -- compacted=1-Zeilen im Bereich
    rows_selected     INTEGER   NOT NULL DEFAULT 0,   -- nach Auswahl+Dedup (§4.3)
    rows_dropped_cred INTEGER   NOT NULL DEFAULT 0,   -- G40-Treffer, verworfen (§5.3)
    rows_dropped_dup  INTEGER   NOT NULL DEFAULT 0,   -- Dedup, lauf- UND laufübergreifend (§4.3 R4)
    rows_dropped_enc  INTEGER   NOT NULL DEFAULT 0,   -- \x00json:-Content nicht dekodierbar (§0.4)
    chars_selected    BIGINT    NOT NULL DEFAULT 0,
    calls             INTEGER   NOT NULL DEFAULT 0,   -- LLM-Calls dieses Laufs
    call_budget       INTEGER   NOT NULL DEFAULT 0,   -- die per-Quelle-Klemme dieses Laufs (§4.6.2)
    insights_kept     INTEGER   NOT NULL DEFAULT 0,
    insights_rejected INTEGER   NOT NULL DEFAULT 0,   -- W18-Gate (§4.4)
    blocks_written    INTEGER   NOT NULL DEFAULT 0,
    manifest_id       UUID,                 -- aufgelöstes Checkpoint-Manifest (§4.5); NULL = keins gefunden
    model             TEXT,                 -- WER geantwortet hat (OnServed), nicht wer gewählt worden wäre
    plan_strategy     TEXT,                 -- 'index-seek'|'rowid-range' (§4.1) — WELCHER Plan gefahren wurde

    -- error trägt AUSSCHLIESSLICH einen Klassen-String aus einer festen
    -- Taxonomie, nie einen Fremdtext. Ein Fremdtext (SQLite-Fehler, HTTP-Body)
    -- landet im Log, NIE hier — das Journal ist per /api lesbar und 90 Tage
    -- langlebig. Der CHECK unten ist die Durchsetzung dieser Zusage: ohne ihn
    -- wäre "nur die Klasse" eine Bitte an den Aufrufer.
    error             TEXT,

    -- ── CHECKs ─────────────────────────────────────────────────────────────
    -- Vollständig über alle sieben Ausgänge, nicht nur über die heute
    -- beobachtbaren (Lehre aus 123/130): ein zu enger CHECK macht genau den
    -- Lauf unaufzeichenbar, für den die Spalte da ist.
    CONSTRAINT dr_outcome_known CHECK (outcome IN (
        'running', 'ok', 'partial', 'skipped', 'failed', 'killed', 'budget_tripped'
    )),
    -- Anders als 130 (skip_reason bewusst ohne CHECK, weil dort die Wertemenge
    -- noch wuchs) ist das Distiller-Enum in §4.2/§5.2 B8 vollständig
    -- ausgeschrieben und Teil der Wellen-Abnahme.
    CONSTRAINT dr_skip_reason_known CHECK (skip_reason IS NULL OR skip_reason IN (
        'source_unreachable', 'no_new_rows', 'demand', 'session_live', 'disabled',
        'budget', 'breaker', 'watermark_regression', 'scope_forbidden'
    )),
    CONSTRAINT dr_error_class_known CHECK (error IS NULL OR error IN (
        'open_failed', 'wal_index_unavailable', 'query_failed', 'schema_untrusted',
        'llm_timeout', 'llm_error', 'parse_error', 'no_eligible_backend',
        'block_write_failed', 'daemon_restart'
    )),
    CONSTRAINT dr_plan_strategy_known CHECK (plan_strategy IS NULL OR plan_strategy IN (
        'index-seek', 'rowid-range'
    )),
    -- Ein abgeschlossener Lauf hat einen Abschlusszeitpunkt; ein laufender
    -- nicht. Das ist die Invariante, auf der der Startup-Sweep (§4.7) steht:
    -- er findet verwaiste Zeilen ausschließlich über outcome='running'.
    CONSTRAINT dr_finished_iff_done CHECK ((outcome = 'running') = (finished_at IS NULL)),
    -- Das Wasserzeichen läuft je Quelle vorwärts oder gar nicht (§3.2,
    -- Eigenschaft 2). Ein Batch, der ein kleineres watermark_to fortschreibt,
    -- ist ein Bug — hier stirbt er beim Schreiben statt still im max().
    CONSTRAINT dr_watermark_forward CHECK (watermark_to >= watermark_from)
);

COMMENT ON TABLE distill_run IS
    'Lauf-Journal des Distillers (Achse 03). Zweiphasig + fortschreibend: INSERT running vor dem Lauf, watermark_to je Batch NACH Durabilitaet der Insights fortgeschrieben, UPDATE beim Abschluss. Das Wasserzeichen einer Quelle ist max(watermark_to) ueber outcome <> ''running'' — es gibt keine zweite Zustandsquelle.';
COMMENT ON COLUMN distill_run.source_key IS
    '"<state_db_label>:<session_id>". Das Label kommt aus distill.source_label, NICHT aus dem Dateipfad — ein Mount-Wechsel wuerde die Wasserzeichen-Ableitung sonst still zerreissen.';
COMMENT ON COLUMN distill_run.outcome IS
    'running | ok | partial | skipped | failed | killed | budget_tripped. running + finished_at IS NULL = der Lauf lebt oder ist hart gestorben; der Startup-Sweep entscheidet das beim naechsten Boot (Muster 130).';
COMMENT ON COLUMN distill_run.watermark_from IS
    'Exklusive Untergrenze des verarbeiteten Bereichs, innerhalb eines Laufs KONSTANT — der Insight-Block ist an ihr verankert (Titel, §4.5), nicht an einem Lauf-Zaehler.';
COMMENT ON COLUMN distill_run.watermark_to IS
    'Inklusive Obergrenze, batchweise fortgeschrieben und ERST nach Durabilitaet der Insights des Batches. max(watermark_to) ueber outcome <> ''running'' IST das Wasserzeichen der Quelle.';
COMMENT ON COLUMN distill_run.error IS
    'AUSSCHLIESSLICH eine Fehlerklasse aus der Taxonomie (dr_error_class_known), nie ein Fremdtext: das Journal ist per /api lesbar und langlebig. SQLite-Fehler und HTTP-Bodies gehoeren ins Log.';
COMMENT ON COLUMN distill_run.gen IS
    'Generationsnummer zum Zeitpunkt des INSERT der running-Zeile. Reine Lesbarkeit — Lauf != Generation, die Identitaet traegt watermark_from.';

-- Der Hot-Path: das Wasserzeichen einer Quelle. Ein Index-Only-Scan über
-- wenige Zeilen je Quelle, nie über die Tabelle.
CREATE INDEX IF NOT EXISTS idx_distill_run_source
    ON distill_run (source_key, watermark_to DESC);

-- Der Budget-/Backoff-Pfad (§4.6.2): der jüngste Trip je Quelle.
CREATE INDEX IF NOT EXISTS idx_distill_run_tripped
    ON distill_run (source_key, started_at DESC)
    WHERE outcome = 'budget_tripped';

-- Der Startup-Sweep (§4.7). Bewusst PARTIELL und nicht der volle
-- started_at-Index aus 130: der Sweep sucht ausschließlich outcome='running',
-- und der Retention-Purge (§6.2) fährt gegen eine Tabelle in der
-- Größenordnung "einige tausend Zeilen/Jahr je Quelle" — ein zweiter,
-- vollständiger Zeitachsen-Index wäre dort Schreiblast ohne Leser.
CREATE INDEX IF NOT EXISTS idx_distill_run_running
    ON distill_run (started_at)
    WHERE outcome = 'running';

-- ── Laufübergreifender Dedup (§4.3 Regel 4) ───────────────────────────────
-- Ohne diese Tabelle wird derselbe wiederholte Tool-Output (dieselbe
-- Testbatterie, derselbe state.sh-Lauf) in JEDER Generation erneut gebatcht
-- und erneut gegen das Spend-Budget gefahren. Bei 52 Generationen einer
-- Wurzel-Session ist das der dominante Kostenposten, nicht ein Randfall.
CREATE TABLE IF NOT EXISTS distill_seen (
    source_key  TEXT        NOT NULL,
    row_hash    BYTEA       NOT NULL,       -- SHA-256 über den normalisierten, gekappten Text
    last_seen   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (source_key, row_hash)
);

COMMENT ON TABLE distill_seen IS
    'Laufuebergreifender Dedup-Ledger des Distillers: Hashes bereits destillierter Tool-Zeilen je Quelle. Retention im 6h-Janitor-Buendel (distill.seen_retention_days, Default 30).';

-- Der Retention-Purge (§6.2) fährt über die Zeitachse, nicht über den PK.
CREATE INDEX IF NOT EXISTS idx_distill_seen_age
    ON distill_seen (last_seen);

-- ── Manifest-Auflösung (§4.5) ─────────────────────────────────────────────
-- Der Distiller sucht das jüngste Manifest EINER Wurzel-Session. Live sind das
-- 319 Manifest-Blöcke unter 7 801 Blöcken; bei 1M+ Korpus ist das eine Nadel.
--
-- DAS PRÄDIKAT IST BEWUSST `IS NOT NULL` UND NICHT `metadata ? 'root_session_id'`:
-- Postgres' Prädikat-Beweiser leitet aus `metadata->>'root_session_id' = $1`
-- die Bedingung `(metadata->>'root_session_id') IS NOT NULL` ab (strikter
-- Operator auf DEMSELBEN Ausdruck) — aus der `?`-Existenzbedingung dagegen
-- nicht (anderer Ausdruck, anderer Operator). Mit `?` wäre der Index für die
-- §4.5-Abfrage unbenutzbar und die Zusage "korpus-unabhängig" still leer.
--
-- Das Gate prüft die NUTZUNG per EXPLAIN, nicht die Existenz. Gemessen auf der
-- 1M-Fixture (migration135_integration_test.go), und der Ist-Befund weicht in
-- zwei Punkten von der Erwartung des Designs ab:
--
--   * Die `?`-Fassung fällt wie vorhergesagt aus dem Plan — der Zugriff bleibt
--     bei den Referenzkosten ohne jeden Checkpoint-Index (top cost 25 6xx,
--     10 000 Heap-Blöcke). Ein Seq Scan ist es dabei NICHT: idx_context_category
--     ist im echten Schema attraktiver. Die Probe misst deshalb, dass der Plan
--     unverändert der breite bleibt, nicht welchen Knoten er trägt.
--   * Die Abfrage OHNE die IS-NOT-NULL-Zeile nutzt den Index TROTZDEM — Plan
--     identisch zur vollen Fassung. Dieselbe Ableitung aus dem strikten
--     Operator, die das Index-Prädikat beweisbar macht, gilt auch für die
--     Abfrage; design/03 §4.5 nimmt dort das Gegenteil an. Die Zeile in der
--     Abfrage ist damit Dokumentation, kein Mechanismus.
--   * Die tragende Negativ-Probe ist deshalb eine NICHT-strikte Bedingung auf
--     demselben Ausdruck (`IS DISTINCT FROM`): dort hat der Beweiser nichts in
--     der Hand, und der Index fällt aus dem Plan.
--
-- created_at DESC ist die zweite Spalte, weil §4.5 innerhalb EINER Wurzel-
-- Session nach created_at DESC sortiert und LIMIT 1 nimmt. Gemessen wird der
-- Index als Bitmap Index Scan unter einem BitmapAnd mit idx_context_category
-- (25 Heap-Blöcke statt 10 000) — die zweite Spalte spart dort keinen Sort,
-- sondern schneidet den created_at-Bereich schon im Index mit.
--
-- Der Name root_session_id ist per Masterplan §2 K6 eingefrorene Schnittstelle
-- zwischen Achse 02 und 03 — eine Umbenennung ist ein K-Fall und muss diesen
-- Index mitziehen.
CREATE INDEX IF NOT EXISTS idx_blocks_checkpoint_root
    ON context_blocks ((metadata->>'root_session_id'), created_at DESC)
    WHERE (metadata->>'root_session_id') IS NOT NULL;
