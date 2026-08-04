-- =============================================================================
-- 124_cluster_topic_identity.sql — stabile Cluster-Identität über Rebuilds
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Die 057-Familie ersetzt bei JEDEM Rebuild ihre drei Tabellen vollständig
-- (overview/cluster.go:508-531). Cluster haben deshalb keine Identität über
-- Läufe hinweg: cluster_id ist die kleinste Member-UUID der Community
-- (cluster.go:296-307) und wechselt lautlos, sobald dieser eine Block die
-- Community verlässt. Diese Tabelle ist die Identität, die der Teardown NICHT
-- anfasst — der Endpunkt, auf den eine Kante zeigen und ein Retrieval-Signal
-- sich beziehen kann. Der Cluster fließt, das Topic bleibt.
--
-- SCOPE-INVARIANTE (Fortsetzung des 057-Headers): ein Topic gehört genau EINEM
-- Scope. graph_cluster_node trägt pro (cluster_id, scope) eine Zeile; die
-- Identität hängt an dieser Zeile, nicht am scope-übergreifenden cluster_id.
-- Damit kann weder ein Label fremde Scope-Inhalte tragen noch zwei Tenants ein
-- gemeinsames Handle bekommen (das wäre ein Existenz-Orakel eine Ebene über
-- dem, das handler/overview.go:5-6 schließt).
--
-- ID-WAHL: gen_random_uuid() (v4), NICHT uuidv7(). Der Zeitstempel-Anteil von
-- uuidv7 wäre ein auslieferbarer Seitenkanal ("wann sah der Rebuild diese
-- Community zuerst"); created_at steht als eigene Spalte da, wo sie
-- scope-gefiltert gelesen wird. Zusätzlich verhindert v4 die Verwechslung mit
-- cluster_id, das de facto eine Block-UUID IST.
--
-- LINEAGE: origin_topic_id/origin_kind beschreiben die Geburt (frisch vs. aus
-- einem Split hervorgegangen), merged_into den Tod durch Verschmelzung. Beide
-- self-referenzierend mit ON DELETE SET NULL, damit die Retention (W8) alte
-- Grabsteine wegräumen kann, ohne eine Kette zu zerreißen.
--
-- core_blocks AUF DER TOPIC-ZEILE (Entscheid E2-01 / Amendment A01-2): die
-- Kern-Mitgliedschaft steht hier und nicht nur auf graph_cluster_node, weil die
-- Node-Zeile im Teardown stirbt. Ein Batch-Import, der eine Partition über
-- mehrere Rebuilds zerreißt, findet seine alte Identität nur wieder, wenn die
-- Substanz des toten Topics den Teardown überlebt: W3 prüft Geburts-Kandidaten
-- gegen die core_blocks retirierter Topics DESSELBEN Scopes innerhalb
-- tombstone_retention und hängt sie re-attach statt neu zu gebären. Organisches
-- Wachstum läuft weiter über die lebende Vorgänger-Generation — beide Pfade
-- gleichzeitig im selben Lauf. Array statt Join-Tabelle: die Kernmenge ist
-- klein (Substanz-Kern nach E4-01) und wird als Ganzes gelesen.
--
-- FK-BEGLEITINDIZES (load-bearing für W8): PostgreSQL legt für die
-- REFERENZIERENDE Seite eines FK KEINEN Index an. Ohne die beiden Indizes unten
-- zwingt jedes DELETE auf graph_cluster_topic den ON-DELETE-SET-NULL-Trigger zu
-- einem Seq-Scan über dieselbe Tabelle — bei sechsstelliger Grabsteinmenge wäre
-- der Purge quadratisch. Die Indizes gehören in DIESE Migration, nicht in die
-- von W8: die Tabelle ist hier leer, der Index-Build kostet nichts.
-- Nicht abgedeckt und bewusst benannt: graph_cluster_node.topic_id hat keinen
-- eigenen Index (uq_gcn_scope_topic führt scope) — W8 löscht nur retirierte
-- Topics, und eine retirierte Zeile hat per Konstruktion keine Node-Zeile mehr.
--
-- KEIN idx_gct_scope_alive (scope) WHERE retired_at IS NULL: der Read-Pfad
-- joint über topic_id (Primary Key), und die Label-Selektion braucht
-- label_stale/label_attempts (Migration 125), nicht scope allein. scope hat drei
-- distinkte Werte und am Ziel-Scale kaum mehr — ein Single-Column-Index darauf
-- würde nie einem Seq-Scan vorgezogen und kostete nur Write-Amplifikation.
--
-- KEIN BACKFILL, bewusst: ein Backfill müsste den heutigen cluster_id als
-- Topic-Identität adoptieren — ausgerechnet den Wert, dessen Instabilität diese
-- Achse behebt. Der erste Rebuild nach W3 vergibt für jedes Cluster ein frisches
-- Topic mit origin_kind='birth'. Bis dahin ist topic_id NULL = "noch nicht
-- zugeordnet"; der Read-Pfad (W7) behandelt das fail-closed.
--
-- lock_timeout (Muster 116/119): CREATE TABLE + ADD COLUMN (nullable bzw. mit
-- konstantem DEFAULT, seit PG11 metadata-only) + CREATE INDEX auf einer
-- Tabelle in Cluster-Größenordnung nehmen kurze Locks; der Runner hält die
-- Datei in EINER Tx (store/migrations.go:97-115), SET LOCAL ist damit
-- selbst-revertierend. SQLSTATE 55P03 = gewollter Abbruch, gefahrloser Re-Run.
-- Idempotent: CREATE TABLE/INDEX IF NOT EXISTS, ADD COLUMN IF NOT EXISTS.
-- Forward-only.
-- ⚠ NEUE TABELLE: test.sh T07_EXPECT_TABLES 50 → 51 nachgezogen (test.sh:301)
--   und das Schema-Contract-Manifest regeneriert
--   (go test ./internal/schemacontract -tags=genmanifest -run TestGenerateManifest).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS graph_cluster_topic (
    topic_id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope           TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    retired_at      TIMESTAMPTZ,                       -- NULL = lebt
    origin_kind     TEXT        NOT NULL DEFAULT 'birth',
    origin_topic_id UUID REFERENCES graph_cluster_topic(topic_id) ON DELETE SET NULL,
    merged_into     UUID REFERENCES graph_cluster_topic(topic_id) ON DELETE SET NULL,
    core_blocks     UUID[]      NOT NULL DEFAULT '{}',
    CONSTRAINT gct_origin_kind_vocab CHECK (origin_kind IN ('birth','split')),
    -- Ein lebendes Topic hat nie ein merged_into; ein verschmolzenes ist tot.
    CONSTRAINT gct_merge_implies_retired CHECK (merged_into IS NULL OR retired_at IS NOT NULL),
    CONSTRAINT gct_no_self_merge CHECK (merged_into IS DISTINCT FROM topic_id),
    CONSTRAINT gct_no_self_origin CHECK (origin_topic_id IS DISTINCT FROM topic_id)
);

COMMENT ON COLUMN graph_cluster_topic.core_blocks IS
    'Substanz-Kern des Topics (Block-IDs), scope-rein. Überlebt den 057-Teardown und ist damit die Grabstein-Substanz, gegen die W3 einen import-zerrissenen Cluster re-attacht (E2-01/A01-2).';

-- Retention-Index (W8): tote Topics nach Todeszeitpunkt.
CREATE INDEX IF NOT EXISTS idx_gct_retired
    ON graph_cluster_topic (retired_at) WHERE retired_at IS NOT NULL;
-- FK-Begleitindizes (s. Header): ohne sie ist der W8-Purge quadratisch.
CREATE INDEX IF NOT EXISTS idx_gct_origin
    ON graph_cluster_topic (origin_topic_id) WHERE origin_topic_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_gct_merged_into
    ON graph_cluster_topic (merged_into) WHERE merged_into IS NOT NULL;

-- graph_cluster_node bekommt die Zuordnung + den Lauf-Kern. topic_id ist
-- NULLABLE: W1 ist reines Schema, W3 füllt sie. core_hash ist der Drift-Anker
-- des laufenden Standes; die Label-Pipeline vergleicht ihn gegen
-- graph_cluster_topic.label_core_hash (Migration 125).
ALTER TABLE graph_cluster_node
    ADD COLUMN IF NOT EXISTS topic_id    UUID REFERENCES graph_cluster_topic(topic_id),
    ADD COLUMN IF NOT EXISTS core_hash   TEXT,
    ADD COLUMN IF NOT EXISTS core_blocks UUID[] NOT NULL DEFAULT '{}';

-- INJEKTIVITÄTS-GATE (load-bearing): die wechselseitige Pluralitätszuordnung
-- (W3) garantiert, dass kein Topic von zwei Clustern beansprucht wird. Ein
-- Zuordnungs-Bug würde ohne diesen Index eine stille Identitäts-Verschmelzung
-- erzeugen; mit ihm bricht die persist-Tx mit 23505 und die vorige Karte bleibt
-- lesbar.
CREATE UNIQUE INDEX IF NOT EXISTS uq_gcn_scope_topic
    ON graph_cluster_node (scope, topic_id) WHERE topic_id IS NOT NULL;
