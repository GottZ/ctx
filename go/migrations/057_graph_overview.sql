-- =============================================================================
-- 057_graph_overview.sql — F5-W6 Übersichts-Landkarte (Louvain-Cluster-Supergraph)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Vorberechnete Louvain-Cluster über context_dream_links als Cluster-Supergraph
-- (Design 07-graph-overview.md). Drei vom Daemon-Job (internal/overview) per
-- TRUNCATE+INSERT in EINER Tx befüllte Tabellen — KEINE Materialized View:
--   * der Migrations-Runner hält jede Datei in EINER Tx (migrations.go:87) →
--     REFRESH ... CONCURRENTLY rollte 057 zurück;
--   * WITH NO DATA wäre bis zum ersten Refresh nicht abfragbar (Boot-Falle).
-- Normale Tabellen sind nach der Migration sofort abfragbar (leer = leere
-- Landkarte, kein Fehler), bis der erste Rebuild-Tick läuft.
--
-- SCOPE-INVARIANTE (der gelöste Count-Leak, Design §2): die Aggregate sind PRO
-- SCOPE partitioniert. graph_cluster_node trägt eine Zeile je (cluster, scope),
-- graph_cluster_edge eine je (cluster-paar, scope-paar). Der Read-Pfad (W2)
-- summiert NUR Zeilen mit scope = ANY(readScopes) (Kanten: BEIDE Endpunkt-
-- Scopes sichtbar, wie inducedEdges). Es existiert nie ein globaler Total, aus
-- dem sich per Differenz ein fremder privater Anteil rekonstruieren ließe.
-- Sichtbarkeit gilt gegen context_blocks.scope, NIE context_dream_links.scope
-- (Promotion lässt l.scope abweichen — visibility.go:21).
--
-- cluster_id = kleinste Member-UUID der Community (inhaltsstabil bei identischem
-- Korpus; NICHT der gonum-Slice-Index, der über Läufe instabil ist). cluster_id
-- ist ein interner GROUP-BY-Key und wird NIE roh an einen Tenant ausgeliefert
-- (Existenz-Orakel, Design §6.1) — der Handler vergibt per Request einen dichten
-- Ordinal.
-- =============================================================================

-- (A) Block → Cluster-Zuordnung.
CREATE TABLE IF NOT EXISTS graph_cluster_member (
    block_id    UUID PRIMARY KEY REFERENCES context_blocks(id) ON DELETE CASCADE,
    cluster_id  UUID NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_gcm_cluster ON graph_cluster_member (cluster_id);

-- (B) Meta-Knoten-Aggregate, PARTITIONIERT NACH SCOPE.
--     size/category_counts/repr nur über Member dieses einen Scopes.
CREATE TABLE IF NOT EXISTS graph_cluster_node (
    cluster_id       UUID    NOT NULL,
    scope            TEXT    NOT NULL,
    size             INT     NOT NULL,
    category_counts  JSONB   NOT NULL DEFAULT '{}',
    repr_block_id    UUID    NOT NULL,
    repr_title       VARCHAR(120) NOT NULL,
    repr_quality     REAL    NOT NULL DEFAULT 0,
    PRIMARY KEY (cluster_id, scope)
);
CREATE INDEX IF NOT EXISTS idx_gcn_scope ON graph_cluster_node (scope);

-- (C) Inter-Cluster-Meta-Kanten-Aggregate, PARTITIONIERT NACH SCOPE-PAAR.
--     Ungerichtet normiert (cluster_a < cluster_b). scope_s/scope_t = Scopes
--     der beiden Endpunkt-BLÖCKE. Sichtbar gdw. BEIDE Scopes in readScopes.
CREATE TABLE IF NOT EXISTS graph_cluster_edge (
    cluster_a   UUID NOT NULL,
    cluster_b   UUID NOT NULL,
    scope_s     TEXT NOT NULL,
    scope_t     TEXT NOT NULL,
    link_count  INT  NOT NULL,
    weight_sum  REAL NOT NULL,
    PRIMARY KEY (cluster_a, cluster_b, scope_s, scope_t),
    CHECK (cluster_a < cluster_b)
);
CREATE INDEX IF NOT EXISTS idx_gce_scopes ON graph_cluster_edge (scope_s, scope_t);

-- (D) Metadaten des letzten Rebuilds (Single-Row; ETag-Kandidat + Q-Score-Smoke).
CREATE TABLE IF NOT EXISTS graph_overview_meta (
    singleton   BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    computed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    modularity  REAL NOT NULL DEFAULT 0,
    cluster_n   INT  NOT NULL DEFAULT 0,
    node_n      INT  NOT NULL DEFAULT 0,
    edge_n      INT  NOT NULL DEFAULT 0,
    resolution  REAL NOT NULL DEFAULT 1.0
);

INSERT INTO _migrations (version, filename, applied_at)
VALUES (57, '057_graph_overview.sql', now()) ON CONFLICT (version) DO NOTHING;
