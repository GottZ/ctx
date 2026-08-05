-- =============================================================================
-- 126_overview_skip_reason_empty_cut.sql — skip_reason 'empty-node-cut'
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- W3-Nachzug (Review-Befund K2-2/K2-3). Der Rebuild kennt seit W3 einen sechsten
-- Ausgang: der Knotenschnitt ist LEER, obwohl die Partition lebende Topics hat.
-- Erreichbar rein über Konfiguration — schneiden sich overview.include und die
-- Retrieval-Sichtbarkeit zu einer leeren Menge, liefert loadNodes null Zeilen
-- (type_name = ANY('{}') ist ein deterministisches FALSE, visibility.go).
--
-- Ohne Guard liefe persist mit leerer Eingabe durch und die Sterbe-Anweisung
-- der Identitätsvergabe würde JEDES Topic der Partition retirieren, samt
-- anlaufender Retention-Uhr — ein Konfigurationsfehler, der die Identität des
-- Korpus löscht. Der Guard kehrt jetzt VOR persist zurück, wie der node-cap
-- (A01-5: eine eingefrorene Karte fasst keine Identität an).
--
-- Ein Skip, der nicht gestempelt werden kann, ist ein UNSICHTBARES Einfrieren —
-- genau der Zustand, den W-A (Migration 123) abgeschafft hat: StampAttempt
-- würde am CHECK mit 23514 scheitern, der Scheduler loggt und schreibt nichts,
-- und graph_overview_meta behauptet weiter Frische. Deshalb muss das Vokabular
-- den Wert kennen, bevor der Guard ihn schreiben darf.
--
-- Migration 123 hat den Vorwärtspfad selbst benannt ("Drop-vor-Add ist zugleich
-- der Vorwärtspfad, falls das Enum später wächst") — diese Datei geht ihn.
--
-- NUMMER: Der Plan-Korpus reserviert 126 für W-F (Supergraph, Phase P3). Der
-- Vorflug-Check gegen den Ist-Stand zeigt 126 als frei (W-B/W-C/C2–C4 tragen
-- keine Migration), und dieser Nachzug muss MIT W3 deployen, nicht nach P3.
-- Folge: die Plan-Nummern ab W-F rücken um eins — beim Bau der jeweiligen Welle
-- ohnehin gegen Live zu prüfen (Masterplan §3, Bau-Pflicht).
--
-- lock_timeout (Muster 123): ein ADD CONSTRAINT auf graph_overview_meta
-- validiert die Tabelle (eine Zeile je Scope, also einstellig bis
-- niedrig-zweistellig) und nimmt einen kurzen ACCESS EXCLUSIVE. 55P03 =
-- gewollter Abbruch, gefahrloser Re-Run.
-- Idempotent: DROP CONSTRAINT IF EXISTS vor ADD. Forward-only. Keine neue
-- Tabelle, keine neue Spalte, kein neuer Index ⇒ test.sh-Tabellenzahl (51)
-- unverändert; das Schema-Contract-Manifest führt keine CHECK-Constraints und
-- braucht keine Regeneration.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE graph_overview_meta DROP CONSTRAINT IF EXISTS chk_gom_skip_reason;
ALTER TABLE graph_overview_meta
    ADD CONSTRAINT chk_gom_skip_reason
    CHECK (skip_reason IS NULL OR skip_reason IN
           ('node-cap', 'advisory-lock', 'timeout', 'error', 'disabled',
            'registry-unwired', 'empty-node-cut'));

COMMENT ON COLUMN graph_overview_meta.skip_reason IS
    'NULL = letzter Versuch erfolgreich. node-cap/empty-node-cut/timeout/error/disabled/registry-unwired = Partition eingefroren, computed_at ist alt. empty-node-cut = der Typ-/Sichtbarkeitsschnitt lieferte null Knoten, obwohl die Partition lebende Topics hat (Konfigurations-Anomalie, W3). advisory-lock = KONTENTION, nicht Einfrieren — nie als Deckel rendern.';
