-- =============================================================================
-- 127_cluster_super_level.sql — die Ebene über den Themen (Supergraph, W-F)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Achse 02 / Welle W-F (plan-cluster-topicmap design/02 §3.2/§4.7, §7 "W-F").
-- Die Wurzel-Map führt eine Zeile je Cluster. Ab ~840 Clustern trägt sie nur
-- noch einen Bruchteil davon einzeln (design/02 §6.4); die Sammelzeile bleibt
-- der tragende Budget-Mechanismus, aber sie sagt nur noch WIE VIEL fehlt, nicht
-- mehr WORUM es geht. Diese Migration legt die Ebene an, die das beantwortet:
-- ein zweiter Louvain über den Cluster-Supergraph mit einer budget-getriebenen
-- Auflösung γ_super < γ_haupt.
--
-- WARUM γ < γ_haupt UND NICHT "noch eine gonum-Ebene": ReducedGraph.Expanded()
-- liefert die nächst-FEINERE Ebene (gonum louvain_common.go:63-70), und
-- Modularize iteriert Reduktion+Optimierung bereits bis zum Fixpunkt. Ein
-- zweiter Lauf mit demselben γ ist deshalb identisch mit dem ersten — die
-- Auflösung ist die EINZIGE Stellgröße für eine gröbere Ebene.
--
-- ═══ IDENTITÄT: TOPIC, NIEMALS cluster_id (Masterplan-Invariante, K2/K10) ═══
--
-- design/02 §3.2 skizzierte diese Tabellen noch mit `cluster_id` als Member und
-- `lead_cluster_id` als Label-Quelle — geschrieben, BEVOR die Konflikt-Phase
-- die Grundannahmen-Invariante festgeschrieben hat: Cluster-Identität IST das
-- scope-gebundene Topic, und kein persistiertes Artefakt referenziert je
-- cluster_id (= kleinste Member-UUID, pro Lauf neu, Existenz- und Zeit-Orakel).
-- Diese Migration geht deshalb bewusst auf topic_id. Ein Supergraph, dessen
-- Endpunkte bei jedem Rebuild wechseln, wäre exakt die Instabilität, gegen die
-- Migration 124 angetreten ist — eine Ebene höher.
--
-- ═══ SCOPE-REINHEIT: DIE META-EBENE RECHNET PRO SCOPE ═══
--
-- Zweite bewusste Abweichung von der §3.2-Skizze (dort PRIMARY KEY
-- (super_id, scope), also ein Super-Cluster, der sich über Scopes erstreckt und
-- je Scope eine Zeile trägt): der Meta-Louvain läuft PRO SCOPE über den
-- intra-scope Topic-Graphen. Ein Super-Cluster gehört damit per KONSTRUKTION
-- genau einem Scope, nicht per Filter — dieselbe Härtung, die K2 für den Handle
-- entschieden hat ("Sicherheitsinvariante vor API-Eleganz"). Sonst entstünde
-- eine Gruppierung, deren Zusammensetzung von unsichtbaren Fremd-Partitionen
-- abhängt, und die Gruppengröße wäre ein Differenz-Kanal auf fremde
-- Korpusgröße (BP-1) — eingefroren in einem persistierten Block.
-- Live-Kosten der Härtung: null. Von 59 Clustern ist keiner scope-übergreifend
-- (0/59 gemessen, design/01 §9.3-Monitor).
--
-- ═══ KEINE FREMDSCHLÜSSEL (057-Muster) ═══
--
-- Wie graph_cluster_edge tragen diese drei Tabellen keine FK auf
-- graph_cluster_node/-_topic. Sie werden bei JEDEM Rebuild vollständig ersetzt
-- (teardown + INSERT in derselben advisory-gelockten Tx), leben also exakt so
-- lange wie die Partition, die sie beschreiben. Ein FK brächte hier keine
-- Integrität dazu, die die Tx nicht ohnehin herstellt, aber er brächte eine
-- TRUNCATE-Reihenfolge, einen Begleitindex-Zwang für den W8-Purge und einen
-- Trigger-Durchlauf je Zeile am Ziel-Scale.
--
-- ═══ KEIN BACKFILL ═══
--
-- Die Tabellen sind nach der Migration leer; der Renderer fällt bei leerer
-- Meta-Ebene auf die flache Top-N-Darstellung zurück. Leer ⇒ degradierte, aber
-- GÜLTIGE Karte, nie ein Fehler — die Meta-Ebene ist die Qualitäts-, nicht die
-- Sicherheitsschicht (design/02 §4.7, empirische Grenze: live sind 32 von 59
-- Clustern isoliert und bleiben Ein-Element-Gruppen).
--
-- lock_timeout (Muster 123/124/126): CREATE TABLE/INDEX auf leeren Tabellen
-- plus zwei ADD COLUMN (nullable, seit PG11 metadata-only) nehmen kurze Locks,
-- können aber hinter einer laufenden persist-Tx warten (cluster.go: 465 s bei
-- 400k Knoten). 55P03 = gewollter Abbruch, gefahrloser Re-Run.
-- Idempotent: CREATE TABLE/INDEX IF NOT EXISTS, ADD COLUMN IF NOT EXISTS.
-- Forward-only.
-- ⚠ DREI NEUE TABELLEN: test.sh T07_EXPECT_TABLES 51 → 54 nachgezogen und das
--   Schema-Contract-Manifest regeneriert
--   (go test ./internal/schemacontract -tags=genmanifest -run TestGenerateManifest).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- ── K10: die persistente Topic-Kante ─────────────────────────────────────────
-- Bis hierher lebten Topic-Kanten nur im Read-Pfad: store.projectEdgesOntoTopics
-- bildet graph_cluster_edge in Go auf Topics ab, je Request neu. Das reicht für
-- eine Antwort und nicht für einen zweiten Clustering-Lauf, der über genau
-- diesen Graphen läuft — und es ist die Kante, auf die sich ein Retrieval-Signal
-- oder eine Karte dauerhaft beziehen kann (K10: "ein persistentes
-- (topic_a, topic_b)-Kantenschema entsteht erst mit W-F").
--
-- topic_a < topic_b normalisiert die ungerichtete Kante; scope_a/scope_b werden
-- MIT dem Topic-Paar gedreht (die K1-2-Lehre: ein Paar, das nur zur Hälfte
-- normalisiert wird, ordnet den einen Endpunkt dem Scope des anderen zu und
-- löst sich im Read-Pfad in den falschen Knoten auf).
CREATE TABLE IF NOT EXISTS graph_cluster_topic_edge (
    topic_a    UUID    NOT NULL,
    topic_b    UUID    NOT NULL,
    scope_a    TEXT    NOT NULL,   -- Scope von topic_a (Topics sind scope-gebunden)
    scope_b    TEXT    NOT NULL,   -- Scope von topic_b
    link_count INTEGER NOT NULL,
    weight_sum REAL    NOT NULL,
    PRIMARY KEY (topic_a, topic_b),
    CONSTRAINT gcte_ordered CHECK (topic_a < topic_b)
);
CREATE INDEX IF NOT EXISTS idx_gcte_scope_a ON graph_cluster_topic_edge (scope_a);
CREATE INDEX IF NOT EXISTS idx_gcte_scope_b ON graph_cluster_topic_edge (scope_b);

COMMENT ON TABLE graph_cluster_topic_edge IS
    'Ungerichtete Kante zwischen zwei Topics, aggregiert aus graph_cluster_edge über die Topic-Identität (K10). Endpunkte sind topic_id, NIE cluster_id: die Karte und jedes Retrieval-Signal brauchen einen Endpunkt, der den Rebuild überlebt. Wird wie die 057-Familie bei jedem Rebuild ersetzt.';

-- ── Die Meta-Ebene ───────────────────────────────────────────────────────────
-- size/cluster_n sind die Summen ÜBER DIE KIND-TOPICS DIESES SCOPES; lead_topic_id
-- ist das größte Kind-Topic und damit die Label-Quelle der gerenderten Zeile.
CREATE TABLE IF NOT EXISTS graph_cluster_super (
    super_id      UUID    PRIMARY KEY,
    scope         TEXT    NOT NULL,
    size          INTEGER NOT NULL,   -- Σ Blöcke der Kind-Topics
    topic_n       INTEGER NOT NULL,   -- Kind-Topics in dieser Gruppe
    lead_topic_id UUID    NOT NULL    -- größtes Kind-Topic (Label-Quelle)
);
CREATE INDEX IF NOT EXISTS idx_gcs_scope_size
    ON graph_cluster_super (scope, size DESC);

COMMENT ON TABLE graph_cluster_super IS
    'Meta-Cluster-Ebene der Wurzel-Map (W-F): ein zweiter Louvain über den Topic-Supergraphen mit budget-getriebenem γ_super < γ_haupt. PRO SCOPE gerechnet — eine Gruppe gehört per Konstruktion genau einem Scope, nicht per Filter.';

CREATE TABLE IF NOT EXISTS graph_cluster_super_member (
    topic_id UUID PRIMARY KEY,        -- ein Topic hängt in genau EINER Gruppe
    scope    TEXT NOT NULL,           -- denormalisiert für den gescopten teardown
    super_id UUID NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_gcsm_super ON graph_cluster_super_member (super_id);
CREATE INDEX IF NOT EXISTS idx_gcsm_scope ON graph_cluster_super_member (scope);

-- ── Die Meta-Ebene auf der Liveness-Zeile ────────────────────────────────────
-- NULL = keine Meta-Ebene versucht (root_map.super_enabled aus).
-- super_n = 0 UND super_resolution = 0 = VERSUCHT und am Cap
-- (root_map.super_max_nodes) abgebrochen ⇒ flacher Fallback. Der Unterschied ist
-- load-bearing: "nicht gebaut" und "gebaut und zu groß" verlangen verschiedene
-- Reaktionen, und die Karte darf beides nicht als dasselbe rendern (W16).
ALTER TABLE graph_overview_meta
    ADD COLUMN IF NOT EXISTS super_n          INTEGER,
    ADD COLUMN IF NOT EXISTS super_resolution REAL;

COMMENT ON COLUMN graph_overview_meta.super_n IS
    'Meta-Cluster dieses Scopes im letzten ERFOLGSLAUF. NULL = Meta-Ebene nicht versucht (root_map.super_enabled aus). 0 = versucht und am root_map.super_max_nodes-Cap abgebrochen (flacher Fallback, Haupt-Rebuild committet normal).';
COMMENT ON COLUMN graph_overview_meta.super_resolution IS
    'γ_super, das die Budget-Suche für diesen Scope gewählt hat. Getrennt von resolution (γ der Haupt-Partition): die Haupt-Partition soll Q-optimal sein, die Wurzel-Ebene ein Zeilenbudget treffen — zwei Zielfunktionen, ein gemeinsamer Knopf zwänge zur Wahl (design/02 §8-E10).';
