-- =============================================================================
-- 130_overview_run_journal.sql — das Lauf-Journal der Wurzel-Map
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Achse 04 (Skalen-Pfad), Welle S2. design/04 §3.2.
--
-- WARUM EINE EIGENE TABELLE UND KEINE SPALTEN AUF graph_overview_meta:
-- _meta ist ZUSTAND JE SCOPE (PK scope, Mig 057/088) und wird bei jedem Lauf
-- ersetzt; seit Mig 123 trägt es zusätzlich skip_reason/last_attempt_at/
-- candidate_n. Ein Skalen-Pfad braucht dagegen HISTORIE JE LAUF — Phasendauern,
-- Peak-RSS, Engine, Partitions-Hash. Beides in eine Zeile zu zwingen hieße, die
-- Historie bei jedem Lauf zu löschen. Getrennte Lebensdauern ⇒ getrennte
-- Tabellen.
--
-- DIE ZEILE ENTSTEHT ZWEIPHASIG UND IM ELTERNPROZESS (design/04 §4.10). Das ist
-- der Kern dieser Migration, nicht ein Implementierungsdetail:
--
--   Der Rebuild läuft im Kindprozess (events/overview_worker.go:98). Der
--   rebuild_timeout ist ein CommandContext-SIGKILL ohne SIGTERM-Grace
--   (overview_worker.go:89-97); ein per Timeout ODER per cgroup-OOM getötetes
--   Kind liefert KEINE Stats über stdout. Würde die Zeile erst NACH dem Lauf
--   geschrieben, wäre ausgerechnet der Lauf unsichtbar, der am Speicher- oder
--   Zeitbudget stirbt — also genau der Lauf, für den dieses Journal gebaut wird.
--
--   Deshalb: INSERT mit outcome='running' VOR dem Spawn, UPDATE danach. Eine
--   Start-Zeile ohne Abschluss IST der OOM-/Kill-Beleg. Kind-seitige Felder
--   bleiben dann NULL — NULL ist die ehrliche Antwort, eine fehlende Zeile
--   ist es nicht.
--
-- Ein Startup-Sweep setzt beim Boot verwaiste 'running'-Zeilen (Daemon-Crash)
-- auf 'failed'/'killed'.
--
-- Additiv, forward-only, CREATE TABLE IF NOT EXISTS + CREATE INDEX IF NOT
-- EXISTS — gefahrloser Re-Run (Muster 119). SET LOCAL lock_timeout, obwohl
-- eine NEUE Tabelle keine fremden Locks braucht: der Runner fährt die Datei in
-- einer Transaktion, und die spätere COMMENT-Strecke berührt dieselbe Relation.
-- =============================================================================

SET LOCAL lock_timeout = '2s';

CREATE TABLE IF NOT EXISTS graph_overview_run (
    -- v4, NIE uuidv7 (K1-Doktrin): ein Zeitstempel im Schlüssel wäre ein
    -- Existenz-/Zeit-Orakel. started_at trägt die Zeit offen und ehrlich.
    run_id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- ── Identität des Laufs ────────────────────────────────────────────────
    scope_set       TEXT[]      NOT NULL,   -- ScopeFilter; leeres Array = globaler Lauf
    scope_key       BIGINT      NOT NULL,   -- lockKeyForScopes(scope_set), cluster.go:242-261
    engine          TEXT        NOT NULL,   -- 'gonum' | 'ctx'
    resolution      REAL        NOT NULL,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at     TIMESTAMPTZ,            -- NULL = laufend ODER hart gestorben
    outcome         TEXT        NOT NULL,   -- 'running' | 'ok' | 'skipped' | 'failed'
    skip_reason     TEXT,

    -- ── Eingangsgrößen ─────────────────────────────────────────────────────
    candidate_n     INTEGER,                -- Knotenkandidaten VOR dem Cap
    -- max_nodes_eff ist der EFFEKTIVE Cap-Wert DIESES Laufs (UD-07-04). Er
    -- steht hier, weil ab S6+S7 zwei Keys existieren (max_nodes für gonum,
    -- max_nodes_ctx für den eigenen Kern) und die ENGINE den Key wählt, nicht
    -- der Wert. Ohne die Spalte wäre im Betrieb nicht unterscheidbar, ob 200000
    -- gewollt oder geerbt ist.
    max_nodes_eff   INTEGER,
    node_n          INTEGER,
    edge_n          INTEGER,                -- ungerichtete Paare im Schnitt
    dangling_n      INTEGER,                -- ein Endpunkt außerhalb des Schnitts
    selfloop_n      INTEGER,                -- übersprungene Self-Loops

    -- ── Ergebnisgrößen ─────────────────────────────────────────────────────
    cross_scope_components INTEGER,         -- SP-1, gefüllt ab S8
    component_n     INTEGER,                -- ab S8
    cluster_n       INTEGER,
    level_n         SMALLINT,               -- Reduktionsebenen (ab S4; gonum meldet sie nicht)
    sweep_n         INTEGER,                -- Sweeps über alle Ebenen (ab S4)
    modularity      REAL,                   -- IMMER globales Q über die zusammengesetzte Partition

    -- ── Phasendauern ───────────────────────────────────────────────────────
    load_ms         INTEGER,
    cluster_ms      INTEGER,
    persist_ms      INTEGER,
    copy_ms         INTEGER,                -- CopyFrom-Anteil, getrennt (ab S9a)
    -- lock_held_ms ist PFLICHTFELD DES ENTWURFS und heute die einzige Größe,
    -- an der ein Skalen-Pfad im BETRIEB scheitert: sie serialisiert alle
    -- Rebuilds derselben Partition und blockiert die Aggregat-Tabellen. Sie
    -- ist bislang NIRGENDS gemessen — weder overview.Stats (cluster.go:74-133)
    -- noch das Log (scheduler.go:1258-1261) kennen sie.
    lock_held_ms    INTEGER,

    -- ── Speicher ───────────────────────────────────────────────────────────
    peak_rss_kb     BIGINT,                 -- VmHWM des KINDprozesses
    -- parent_rss_kb ist kein Beiwerk: das Limit gilt für die cgroup, nicht für
    -- den Prozess (design/04 §6.3(c)). Ohne die Elternlast ist ein Kind-VmHWM
    -- eine Zahl ohne Nenner. S1 hat gemessen, dass der Ist-Rechenpfad am
    -- heutigen 200k-Cap 423 MB belegt — bei 512 MiB cgroup und einem Elternteil,
    -- der die graphcache-CSR ungedeckelt hält (UD-09-04).
    parent_rss_kb   BIGINT,

    -- ── Messgrößen, die spätere Wellen füllen ──────────────────────────────
    wal_bytes       BIGINT,                 -- NUR bei graph_overview.measure_wal (bench-only, §3.2)
    temp_bytes      BIGINT,                 -- pg_stat_database.temp_bytes-Delta der persist-Tx
    sigma_drift     REAL,                   -- max. relative σ-Abweichung (Gate S4-G6)
    -- partition_hash ist der Determinismus-Ersatzanker A1 (§4.6): SHA-256 über
    -- die kanonisierte Partition (Member nach block_id sortiert, je Zeile
    -- block_id||cluster_id). Er färbt JEDE Verhaltensänderung des Movers rot —
    -- Tie-Break, Reduktionsordnung, Zugreihenfolge. Der 50-Lauf-Determinismus-
    -- Test leistet das nicht: er prüft Wiederholbarkeit, nicht Konstanz über
    -- die Zeit.
    partition_hash  BYTEA,
    members_changed    INTEGER,             -- Delta-Persist (ab S9b)
    members_reassigned INTEGER,             -- davon echte Zugehörigkeits-Wechsel (K13)
    topics_reattached  INTEGER,             -- Kontinuitäts-Kennzahl (W3, MP-5)
    topics_born        INTEGER,

    -- change_log_seq und delta_fallback werden HIER mit angelegt (kostenlos,
    -- NULL/false bis S10) statt per ALTER nachgezogen; damit ist die spätere
    -- Delta-Migration genau eine Tabelle plus Trigger.
    change_log_seq  BIGINT,
    delta_fallback  BOOLEAN     NOT NULL DEFAULT false,
    engine_switch   BOOLEAN     NOT NULL DEFAULT false,

    -- Der CHECK ist bewusst VOLLSTÄNDIG über alle vier Ausgänge und nicht nur
    -- über die heute beobachtbaren — dieselbe Lehre wie 123 (skip_reason):
    -- ein zu enger CHECK macht genau den Lauf unaufzeichenbar, für den die
    -- Spalte da ist. skip_reason bleibt bewusst OHNE CHECK: die Wertemenge
    -- wächst mit S6/S7/S7b ('time-budget', 'mem-budget') und mit design/02 W-A
    -- (MP-2), und ein Enum-Nachzug wäre eine Migration ohne Nutzen.
    CONSTRAINT gor_outcome_known CHECK (outcome IN ('running', 'ok', 'skipped', 'failed')),
    -- Ein abgeschlossener Lauf hat einen Abschlusszeitpunkt; ein laufender
    -- nicht. Das ist die Invariante, auf der der Startup-Sweep steht.
    CONSTRAINT gor_finished_iff_done CHECK ((outcome = 'running') = (finished_at IS NULL))
);

COMMENT ON TABLE graph_overview_run IS
    'Lauf-Journal der Wurzel-Map (Achse 04/S2). Eine Zeile JE REBUILD-VERSUCH, zweiphasig geschrieben: INSERT running vor dem Kind-Spawn, UPDATE nach der Rückkehr. Eine running-Zeile ohne Abschluss ist der SIGKILL-/OOM-Beleg — genau der Lauf, den ein einphasiges Journal verliert.';
COMMENT ON COLUMN graph_overview_run.outcome IS
    'running | ok | skipped | failed. running + finished_at IS NULL = der Lauf lebt oder ist hart gestorben; der Startup-Sweep entscheidet das beim naechsten Boot.';
COMMENT ON COLUMN graph_overview_run.max_nodes_eff IS
    'Effektiver Knoten-Cap DIESES Laufs. Ab S6+S7 waehlt die ENGINE den Key (max_nodes fuer gonum, max_nodes_ctx fuer den eigenen Kern) — ohne diese Spalte waere im Betrieb nicht unterscheidbar, ob der Wert gewollt oder geerbt ist (UD-07-04).';
COMMENT ON COLUMN graph_overview_run.lock_held_ms IS
    'Haltezeit des Advisory-Locks der persist-Tx. Die Groesse, an der ein Skalen-Pfad im Betrieb scheitert — sie serialisiert alle Rebuilds derselben Partition und war bis Mig 130 nirgends gemessen.';
COMMENT ON COLUMN graph_overview_run.parent_rss_kb IS
    'RSS des ELTERNprozesses zum Spawn-Zeitpunkt. Das Speicherlimit gilt der cgroup, nicht dem Prozess: ohne die Elternlast ist peak_rss_kb eine Zahl ohne Nenner (design/04 §6.3(c)).';
COMMENT ON COLUMN graph_overview_run.partition_hash IS
    'SHA-256 ueber die kanonisierte Partition (Member nach block_id sortiert, je Zeile block_id||cluster_id). Determinismus-Ersatzanker A1 — er faerbt jede Verhaltensaenderung des Rechenkerns rot, nicht nur eine nicht-wiederholbare.';
COMMENT ON COLUMN graph_overview_run.scope_set IS
    'Der ScopeFilter dieses Laufs. Leeres Array = globaler Lauf (nil-Filter) — bewusst nicht NULL, damit der Index ohne NULL-Sonderfall arbeitet.';

-- Neuester-Lauf-Lookup je Partition (Status-Route, Diagnose).
CREATE INDEX IF NOT EXISTS idx_gor_scope_started
    ON graph_overview_run (scope_key, started_at DESC);

-- Retention-Purge und der Startup-Sweep fahren beide ueber die Zeitachse.
CREATE INDEX IF NOT EXISTS idx_gor_started
    ON graph_overview_run (started_at);
