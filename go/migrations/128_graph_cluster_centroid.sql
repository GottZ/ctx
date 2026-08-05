-- =============================================================================
-- 128_graph_cluster_centroid.sql — Zentroid je (topic_id, scope)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Zweck: der query-UNABHÄNGIGE Cluster-Prior (design/03 §4.6 M2). Der C3-Boost
-- leitet die Cluster einer Frage aus ihren eigenen RRF-Treffern ab — das ist
-- zirkulär: liefert RRF nichts Brauchbares, hat die kategorische Stufe nichts,
-- woran sie sich festhalten kann. Der Zentroid ist der Mittelwert der
-- Member-Embeddings; das Query-Embedding trifft ihn direkt, ohne den Umweg über
-- die Trefferliste. Berechnet wird er SERVER-SEITIG (pgvector avg()) — der
-- Vektor überquert nie die Go-Grenze (6,9 GB Embedding-I/O je Vollbau @10M
-- gingen sonst durch einen 512-MiB-Container).
--
-- SCHLÜSSEL: topic_id (die stabile Identität aus Migration 124), NICHT
-- cluster_id. cluster_id ist die kleinste Member-UUID der Community und wird pro
-- Lauf neu bestimmt (overview/cluster.go:296-307, 057-Header); verschiebt sich
-- ein EINZIGER Member, kann sich der Schlüssel des gesamten Clusters ändern. Ein
-- Mitgliedschafts-Diff über cluster_id kann "unverändert" nicht von "neu"
-- unterscheiden — der inkrementelle Bau wäre auf ihm nicht formulierbar
-- (Masterplan K7, design/03 §3.2). cluster_id bleibt als NUTZSPALTE des
-- jeweiligen Laufs erhalten: sie ist der Join-Schlüssel gegen
-- graph_cluster_member INNERHALB eines Zyklus und macht den minUUID-Rename
-- (Masterplan K13) zu einem Ein-Spalten-UPDATE statt zu einem Re-Compute.
--
-- SPEICHERFORM halfvec(1024): exakt die Form, in der der Block-Index rechnet
-- (idx_embedding_hnsw auf (embedding::halfvec(1024)), Migration 001). Ein
-- vector(1024)-Zentroid läge in einem anderen Zahlenraum als die Blöcke, gegen
-- die er verglichen wird, und kostete doppelten Speicher. avg(vector) existiert
-- (pgvector 0.8.5) und der Cast nach halfvec trägt; <=> normiert beide
-- Operanden, die fehlende Renormierung des Mittels ist also wirkungslos —
-- avg() IST der Richtungs-Zentroid des sphärischen k-Means-Schritts.
--
-- member_hash (Masterplan K7) ist der DIFF-TRÄGER: sha256 über das sortierte
-- Member-Set der Partition. Eine Charge überspringt jeden Cluster, dessen Hash
-- unverändert ist — das ist der einzige Grund, warum die Tabelle auf die stabile
-- Identität schlüsselt. bytea statt text: 32 Byte roh statt 64 Byte Hex.
-- sha256() ist PG-Builtin (seit PG11), NICHT pgcrypto-digest() — die Extension
-- ist in dieser DB nicht installiert und ein Schema-Objekt für einen Hash
-- anzufordern wäre eine unnötige Abhängigkeit.
--
-- embedded_n ist der EHRLICHKEITS-ZÄHLER: Member MIT Embedding gegen member_n
-- (alle Member). Live ist der Deckungsgrad heute 100 % (1.190/1.190 gemessen),
-- weil der Louvain-Knotenschnitt genau die retrieval-sichtbaren Typen sind. Er
-- wird trotzdem mitgeschrieben, damit ein späterer Deckungsverlust sichtbar wird
-- statt sich als stiller Qualitätsverfall zu tarnen.
--
-- KEIN HNSW-INDEX in dieser Migration — gemessen statt behauptet (UD-02-03).
-- Default ist der exakte Scan: am oberen Ende der §3.3-Bandbreite (~83.000
-- Zeilen à ~2 kB) sind das ~170 MB Brute-Force, ohne Recall-Frage, ohne
-- Wartungs-Churn, ohne Bloat-Pfad. Übersteigt die Zentroid-Zahl
-- cluster.centroid_ann_threshold, legt der Bau-Arm den Index selbst an
-- (overview/centroid.go) — als deklarierte Ressourcen-Grenze, nicht als
-- Default. CREATE INDEX CONCURRENTLY wäre hier ohnehin verboten: der Runner
-- fährt jede Datei in GENAU EINER Transaktion (store/migrations.go:97-115).
--
-- FK MIT ON DELETE CASCADE auf graph_cluster_topic: die Retention (W8) purgt
-- Grabsteine, und ein Zentroid ohne Topic wäre eine Karteileiche, die jede
-- Zeilen-/Größenrechnung nach oben verfälscht (design/03 §3.2 Linse 1 / A14).
-- Der PK (topic_id, scope) führt topic_id FÜHREND und ist damit zugleich der
-- Begleitindex, den PostgreSQL für die referenzierende FK-Seite nicht selbst
-- anlegt — ohne ihn wäre der W8-Purge quadratisch (dieselbe Lehre wie
-- idx_gct_origin/idx_gct_merged_into in Migration 124).
--
-- scope ist bei scope-gebundener Identität (Masterplan K2) funktional von
-- topic_id abhängig und steht trotzdem als eigene Spalte im Schlüssel: JEDER
-- Lese-, Sweep- und Teardown-Pfad filtert auf scope (fail-closed), und ein
-- Prädikat, das erst über einen Join auf graph_cluster_topic auflösbar wäre,
-- wäre auf dem Retrieval-Hot-Path ein zusätzlicher Join für nichts.
--
-- KEIN BACKFILL: die Tabelle startet leer. Der Read-Pfad behandelt eine
-- FEHLENDE Zeile als "kein Signal", nie als "Distanz unendlich" — Cold-Start und
-- Teilbefüllung sind gültige Zustände (§3.2), und genau das ist die
-- negativ-geprobte Fallback-Achse der Welle C8. Gefüllt wird sie vom nächsten
-- Overview-Rebuild, in einer EIGENEN Transaktion NACH dem Commit der
-- persist-Tx (Masterplan K5) und nur bei cluster.centroid_build=true.
--
-- lock_timeout (Muster 116/119/124): CREATE TABLE + CREATE INDEX auf einer
-- leeren Tabelle nehmen kurze Locks; der Runner hält die Datei in EINER Tx, SET
-- LOCAL ist damit selbst-revertierend. SQLSTATE 55P03 = gewollter Abbruch,
-- gefahrloser Re-Run. Idempotent: CREATE TABLE/INDEX IF NOT EXISTS.
-- Forward-only.
-- ⚠ NEUE TABELLE: test.sh T07_EXPECT_TABLES 51 → 52 nachgezogen (test.sh:301),
--   der 51-Pin in store/cluster_topic_label_schema_integration_test.go
--   mitgezogen und das Schema-Contract-Manifest regeneriert
--   (go test ./internal/schemacontract -tags=genmanifest -run TestGenerateManifest).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS graph_cluster_centroid (
    topic_id    UUID        NOT NULL REFERENCES graph_cluster_topic(topic_id) ON DELETE CASCADE,
    scope       TEXT        NOT NULL,
    cluster_id  UUID        NOT NULL,
    centroid    halfvec(1024) NOT NULL,
    member_n    INTEGER     NOT NULL,
    embedded_n  INTEGER     NOT NULL,
    member_hash BYTEA       NOT NULL,
    computed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (topic_id, scope),
    CONSTRAINT gcc_member_n_positive  CHECK (member_n > 0),
    CONSTRAINT gcc_embedded_n_covered CHECK (embedded_n > 0 AND embedded_n <= member_n)
);

COMMENT ON COLUMN graph_cluster_centroid.member_hash IS
    'sha256 über das sortierte Member-Set dieser Partition. Diff-Träger des inkrementellen Baus (K7): unveränderter Hash ⇒ kein Re-Compute des Zentroids.';
COMMENT ON COLUMN graph_cluster_centroid.cluster_id IS
    'Lauf-lokaler Louvain-Schlüssel (kleinste Member-UUID). NUTZSPALTE für den Join gegen graph_cluster_member innerhalb eines Zyklus — NIE Identität und nie auf der Leitung.';
COMMENT ON COLUMN graph_cluster_centroid.embedded_n IS
    'Member MIT Embedding gegen member_n (alle Member) — der Ehrlichkeits-Zähler des Deckungsgrads.';

-- Scope-Pfad: der Diff-Read, der Orphan-Sweep und die Zentroid-Probe filtern
-- alle auf scope. KEIN ANN-Vorfilter — ein btree lässt sich mit einem
-- HNSW-ORDER-BY-Scan nicht kombinieren (§6.3); dieser Index dient dem exakten
-- Scan und den Wartungspfaden, nicht der Vektorsuche.
CREATE INDEX IF NOT EXISTS idx_gcc_scope_topic
    ON graph_cluster_centroid (scope, topic_id);
