//go:build integration

// Integration test for the Cluster-Topic-Map Achse 03 C8 schema (migration
// 128): graph_cluster_centroid — der query-UNABHÄNGIGE Cluster-Prior.
//
// Schema-Pinnung im Muster von cluster_topic_schema_integration_test.go (W1).
// Der Bau-Pfad (eigene Tx nach dem persist-Commit, member_hash-Diff) wird in
// internal/overview/centroid_integration_test.go geprüft, der Lese-Pfad in
// internal/rrf.
//
// Run: go test -tags=integration ./internal/store/ -run TestClusterCentroidSchema -count=1 -v
package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GottZ/ctx/migrations"

	"github.com/GottZ/ctx/internal/testdb"
)

const centroidMigrationFile = "128_graph_cluster_centroid.sql"

func TestClusterCentroidSchema_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	t.Run("schema_objects_present", func(t *testing.T) {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.tables WHERE table_name = 'graph_cluster_centroid'`,
		).Scan(&n); err != nil {
			t.Fatalf("table probe: %v", err)
		}
		if n != 1 {
			t.Fatalf("graph_cluster_centroid missing")
		}

		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_indexes
			  WHERE tablename = 'graph_cluster_centroid' AND indexname = 'idx_gcc_scope_topic'`,
		).Scan(&n); err != nil {
			t.Fatalf("index probe: %v", err)
		}
		if n != 1 {
			t.Errorf("idx_gcc_scope_topic missing")
		}
	})

	// Die Speicherform ist NICHT beliebig: halfvec(1024) ist exakt die Form, in
	// der der Block-HNSW-Index rechnet (idx_embedding_hnsw auf
	// (embedding::halfvec(1024))). Eine vector(1024)-Spalte würde den Zentroid
	// in einem anderen Zahlenraum halten als die Blöcke, gegen die er verglichen
	// wird — und den Speicher verdoppeln.
	t.Run("centroid_is_halfvec_1024", func(t *testing.T) {
		var typ string
		var mod int
		if err := pool.QueryRow(ctx,
			`SELECT t.typname, a.atttypmod
			   FROM pg_attribute a
			   JOIN pg_class c ON c.oid = a.attrelid
			   JOIN pg_type  t ON t.oid = a.atttypid
			  WHERE c.relname = 'graph_cluster_centroid' AND a.attname = 'centroid'`,
		).Scan(&typ, &mod); err != nil {
			t.Fatalf("column probe: %v", err)
		}
		if typ != "halfvec" {
			t.Errorf("centroid type = %q, want halfvec", typ)
		}
		if mod != 1024 {
			t.Errorf("centroid typmod = %d, want 1024", mod)
		}
	})

	// DER SCHLÜSSEL IST DIE STABILE IDENTITÄT, NICHT cluster_id (§3.2). Auf
	// cluster_id — der kleinsten Member-UUID, pro Lauf neu — wäre kein
	// Mitgliedschafts-Diff formulierbar: ein einziger wandernder Member kann den
	// Schlüssel des ganzen Clusters ändern, "unverändert" ist dann von "neu"
	// nicht unterscheidbar. Genau daran hängt der inkrementelle Pfad (K7).
	t.Run("primary_key_is_topic_scope", func(t *testing.T) {
		var cols []string
		if err := pool.QueryRow(ctx,
			`SELECT array_agg(a.attname ORDER BY k.ord)
			   FROM pg_constraint c
			   JOIN LATERAL unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord) ON true
			   JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
			  WHERE c.conrelid = 'graph_cluster_centroid'::regclass AND c.contype = 'p'`,
		).Scan(&cols); err != nil {
			t.Fatalf("pk probe: %v", err)
		}
		if got := strings.Join(cols, ","); got != "topic_id,scope" {
			t.Errorf("primary key = (%s), want (topic_id,scope)", got)
		}
	})

	// FK MIT CASCADE (§3.2 Retention): ohne ihn überlebt der Zentroid eines von
	// W8 gepurgten Grabsteins als Karteileiche und verfälscht jede Zeilen- und
	// Größenrechnung nach oben. Der PK (topic_id, scope) führt topic_id
	// führend — er ist damit zugleich der Begleitindex, den der CASCADE braucht.
	t.Run("fk_cascades_from_topic", func(t *testing.T) {
		var action string
		if err := pool.QueryRow(ctx,
			`SELECT confdeltype FROM pg_constraint
			  WHERE conrelid = 'graph_cluster_centroid'::regclass AND contype = 'f'`,
		).Scan(&action); err != nil {
			t.Fatalf("fk probe: %v", err)
		}
		if action != "c" {
			t.Errorf("FK delete action = %q, want %q (CASCADE)", action, "c")
		}

		if _, err := pool.Exec(ctx,
			`INSERT INTO graph_cluster_topic (topic_id, scope) VALUES ($1, 'private')`,
			ccTopicA); err != nil {
			t.Fatalf("seed topic: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO graph_cluster_centroid
			     (topic_id, scope, cluster_id, centroid, member_n, embedded_n, member_hash)
			 VALUES ($1, 'private', $2, array_fill(0.1::real, ARRAY[1024])::vector::halfvec(1024), 1, 1, sha256('x'))`,
			ccTopicA, ccBlockA); err != nil {
			t.Fatalf("seed centroid: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM graph_cluster_topic WHERE topic_id = $1`, ccTopicA); err != nil {
			t.Fatalf("purge topic: %v", err)
		}
		var left int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph_cluster_centroid`).Scan(&left); err != nil {
			t.Fatalf("count: %v", err)
		}
		if left != 0 {
			t.Errorf("centroid rows after topic purge = %d, want 0 (CASCADE)", left)
		}
	})

	// G3a-Analogon (W1): die handgepflegte Zähl-Fallback-Erwartung in test.sh
	// hängt am Migrations-Stand und greift live nur ohne erreichbare ctx-CLI.
	// Dieselbe Zählung gegen eine frisch migrierte DB macht sie prüfbar.
	t.Run("test_sh_table_count_matches_fresh_db", func(t *testing.T) {
		script, err := os.ReadFile(filepath.Join("..", "..", "..", "test.sh"))
		if err != nil {
			t.Skipf("test.sh not reachable from here: %v", err)
		}
		want := grepInt(t, string(script), `T07_EXPECT_TABLES=(\d+)`)
		var got int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.tables
			  WHERE table_schema = 'public' AND table_name NOT LIKE '%_snapshot_%'`,
		).Scan(&got); err != nil {
			t.Fatalf("table count: %v", err)
		}
		if got != want {
			t.Errorf("fresh DB has %d tables, test.sh T07_EXPECT_TABLES = %d — nachziehen (test.sh:301)", got, want)
		}
	})

	// Idempotenz: der Runner überspringt angewandte Versionen, also wird die
	// Datei hier von Hand ein zweites Mal gefahren. Rot-Probe: ein IF NOT EXISTS
	// entfernen ⇒ 42P07 beim zweiten Lauf.
	t.Run("idempotent_second_apply", func(t *testing.T) {
		body, err := migrations.FS.ReadFile(centroidMigrationFile)
		if err != nil {
			t.Fatalf("read %s: %v", centroidMigrationFile, err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			t.Fatalf("second apply of %s failed (not idempotent): %v", centroidMigrationFile, err)
		}
	})
}

const (
	ccTopicA = "0190aaaa-0000-4000-8000-0000000000c1"
	ccBlockA = "01900000-0000-7000-8000-0000000000c2"
)
