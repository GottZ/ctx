-- =============================================================================
-- 123_overview_meta_liveness.sql — die Landkarte lernt, ihren Deckel zu melden
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Bei Ziel-Scale ein Wartungsfenster-Schritt (083-Header-Vokabel): die Tabelle
-- ist winzig, aber ALTER wartet hinter einer laufenden persist-Tx (cluster.go:
-- 69-73: 465 s bei 400k Knoten). lock_timeout scheitert laut (55P03) statt zu
-- stauen; der Runner rollt die Datei zurück, der nächste Start wiederholt.
--
-- graph_overview_meta beschreibt bisher ausschließlich ERFOLGREICHE Rebuilds:
-- persist schreibt die Zeile (cluster.go:645-663); ein per max_nodes oder
-- advisory-lock ÜBERSPRUNGENER Lauf (cluster.go:177-181, :557-566), ein per
-- rebuild_timeout SIGKILLter Lauf (scheduler.go:967-972, overview_worker.go:
-- 89-97) und die zwei frühen Returns (scheduler.go:920, :928) schreiben gar
-- nichts. Ein Konsument der Aggregate kann deshalb "frisch" nicht von "seit
-- Wochen eingefroren" unterscheiden — computed_at rückt in allen fünf Fällen
-- gleich (nämlich nicht) vor.
--
-- Drei additive Spalten schließen das:
--   last_attempt_at  — wann zuletzt ein Rebuild-VERSUCH lief (jeder Ausgang)
--   skip_reason      — NULL = letzter Versuch war erfolgreich; sonst der Grund.
--                      Enum bewusst VOLLSTÄNDIG über alle fünf Ausgänge, nicht
--                      nur über die zwei heute beobachtbaren: bei 1M+ ist
--                      'timeout' der Regelfall, und ein zu enger CHECK macht
--                      genau den unaufzeichenbar, für den die Spalte da ist.
--   candidate_n      — Knotenkandidaten DIESES SCOPES beim letzten Versuch
--                      (visible ∩ overview.include, gezählt über loadNodes'
--                      nodeScopes). PER SCOPE, nie der Lauf-Gesamtwert:
--                      die Zeilen sind per-Scope (GROUP BY n.scope,
--                      cluster.go:491), ein durchgereichter Gesamtwert wäre ein
--                      Differenz-Kanal auf fremde Korpusgröße (BP-1).
--
-- computed_at wird NULLable UND verliert seinen DEFAULT: eine Partition, die
-- NIE erfolgreich gebaut hat, aber bereits einen Versuch hinter sich (fresh
-- deploy über dem Cap), braucht eine Zeile OHNE Erfolgs-Zeitstempel. Der
-- DEFAULT now() aus 057_graph_overview.sql:69 würde einen Skip-Upsert, der die
-- Spalte nicht nennt, still auf "gerade eben gebaut" setzen — live verifiziert
-- per \d. store/overview.go:88-90 liest max(computed_at) und ignoriert NULL —
-- Zero-Time bleibt "nie gebaut", exakt wie bisher; das Wire-Feld ist bereits
-- nullable (handler/overview.go:84).
--
-- Idempotent: ADD COLUMN IF NOT EXISTS, DROP NOT NULL/DEFAULT, UPDATE …
-- WHERE … IS NULL und DROP CONSTRAINT IF EXISTS vor ADD CONSTRAINT. Kein neuer
-- Table → test.sh T07-Zählung UNVERÄNDERT.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE graph_overview_meta ALTER COLUMN computed_at DROP NOT NULL;
ALTER TABLE graph_overview_meta ALTER COLUMN computed_at DROP DEFAULT;

ALTER TABLE graph_overview_meta
    ADD COLUMN IF NOT EXISTS last_attempt_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS skip_reason     TEXT,
    ADD COLUMN IF NOT EXISTS candidate_n     INTEGER;

-- Backfill: die bestehenden Zeilen beschreiben allesamt einen ERFOLGREICHEN Lauf
-- (nur persist konnte sie schreiben). last_attempt_at = computed_at ist damit
-- historisch korrekt, skip_reason bleibt NULL, candidate_n = node_n ist die
-- exakte Wahrheit für einen Erfolgslauf (jeder Kandidat wird Louvain-Member —
-- auch isolierte Knoten landen in einer Ein-Element-Community) UND per-Scope
-- konsistent, weil node_n bereits sum(n.size) je Scope ist (cluster.go:486).
UPDATE graph_overview_meta SET last_attempt_at = computed_at WHERE last_attempt_at IS NULL;
UPDATE graph_overview_meta SET candidate_n     = node_n      WHERE candidate_n     IS NULL;

-- Drop-vor-Add ist zugleich der Vorwärtspfad, falls das Enum später wächst.
ALTER TABLE graph_overview_meta DROP CONSTRAINT IF EXISTS chk_gom_skip_reason;
ALTER TABLE graph_overview_meta
    ADD CONSTRAINT chk_gom_skip_reason
    CHECK (skip_reason IS NULL OR skip_reason IN
           ('node-cap', 'advisory-lock', 'timeout', 'error', 'disabled', 'registry-unwired'));

COMMENT ON COLUMN graph_overview_meta.last_attempt_at IS
    'Letzter Rebuild-VERSUCH dieser Partition (jeder der fünf Ausgänge von rebuildOverviewOnce). computed_at bleibt der letzte ERFOLG.';
COMMENT ON COLUMN graph_overview_meta.skip_reason IS
    'NULL = letzter Versuch erfolgreich. node-cap/timeout/error/disabled/registry-unwired = Partition eingefroren, computed_at ist alt. advisory-lock = KONTENTION, nicht Einfrieren (eine andere Instanz baute gerade erfolgreich) — nie als Deckel rendern.';
COMMENT ON COLUMN graph_overview_meta.candidate_n IS
    'Knotenkandidaten DIESES SCOPES beim letzten Versuch (visible ∩ overview.include). Nenner der Deckungsrechnung, Abstand zu max_nodes. Nie ein scope-übergreifender Gesamtwert.';
