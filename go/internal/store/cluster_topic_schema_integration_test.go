//go:build integration

// Integration test for the Cluster-Topic-Map Achse 01 W1 schema (migration
// 124): graph_cluster_topic (die Identität, die der 057-Teardown NICHT
// anfasst) + die drei Partial-Indizes + die Zuordnungs-/Kern-Spalten auf
// graph_cluster_node + das Injektivitäts-Gate uq_gcn_scope_topic.
//
// Reine Schema-Pinnung im Muster von embed_migrations_schema_integration_test.go
// ("schema_objects_present"): W1 liefert nur den Schemaplatz, kein Go-Code
// liest oder schreibt ihn. Die Zuordnungs-Semantik (Overlap-Matching, Geburt/
// Tod/Split/Merge) ist W3 und wird dort geprüft.
//
// Run: go test -tags=integration ./internal/store/ -run TestClusterTopicSchema -count=1 -v
package store_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/GottZ/ctx/migrations"

	"github.com/GottZ/ctx/internal/testdb"
)

// grepInt zieht die erste Capture-Gruppe als Ganzzahl aus s.
func grepInt(t *testing.T, s, pattern string) int {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("pattern %q not found", pattern)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("pattern %q: %v", pattern, err)
	}
	return n
}

const (
	topicMigrationFile = "124_cluster_topic_identity.sql"

	// Fixture-UUIDs im v7-Muster der bestehenden Cluster-Tests
	// (overview/cluster_test.go): sie stehen für Block-IDs, nicht für Topics.
	tsBlockA = "01900000-0000-7000-8000-0000000000a1"
	tsBlockB = "01900000-0000-7000-8000-0000000000a2"
)

func TestClusterTopicSchema_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	t.Run("schema_objects_present", func(t *testing.T) {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.tables WHERE table_name = 'graph_cluster_topic'`,
		).Scan(&n); err != nil {
			t.Fatalf("table probe: %v", err)
		}
		if n != 1 {
			t.Fatalf("graph_cluster_topic missing")
		}

		for _, idx := range []string{"idx_gct_retired", "idx_gct_origin", "idx_gct_merged_into"} {
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM pg_indexes WHERE tablename = 'graph_cluster_topic' AND indexname = $1`, idx,
			).Scan(&n); err != nil {
				t.Fatalf("index probe %s: %v", idx, err)
			}
			if n != 1 {
				t.Errorf("index %s missing", idx)
			}
		}
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_indexes WHERE tablename = 'graph_cluster_node' AND indexname = 'uq_gcn_scope_topic'`,
		).Scan(&n); err != nil {
			t.Fatalf("uq index probe: %v", err)
		}
		if n != 1 {
			t.Errorf("uq_gcn_scope_topic missing")
		}

		for _, col := range []string{"topic_id", "core_hash", "core_blocks"} {
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM information_schema.columns
				  WHERE table_name = 'graph_cluster_node' AND column_name = $1`, col,
			).Scan(&n); err != nil {
				t.Fatalf("column probe graph_cluster_node.%s: %v", col, err)
			}
			if n != 1 {
				t.Errorf("graph_cluster_node.%s missing", col)
			}
		}
	})

	// A01-2 (E2-01-Entscheid): core_blocks liegt auf der TOPIC-Zeile, nicht nur
	// auf der Node-Zeile. Ohne diese Spalte gibt es keine Grabstein-Substanz,
	// gegen die W3 einen Import-zerrissenen Cluster re-attachen könnte.
	t.Run("core_blocks_on_topic_row", func(t *testing.T) {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.columns
			  WHERE table_name = 'graph_cluster_topic' AND column_name = 'core_blocks'
			    AND is_nullable = 'NO' AND data_type = 'ARRAY'`,
		).Scan(&n); err != nil {
			t.Fatalf("core_blocks probe: %v", err)
		}
		if n != 1 {
			t.Fatalf("graph_cluster_topic.core_blocks missing or nullable")
		}
	})

	// K1/R2: der Handle ist gen_random_uuid() v4. uuidv7 trüge einen
	// Zeitstempel ("wann sah der Rebuild diese Community zuerst") auf die
	// Leitung — ein Orakel eine Ebene über handler/overview.go:5-6.
	t.Run("topic_id_default_is_uuid_v4", func(t *testing.T) {
		var version int
		if err := pool.QueryRow(ctx,
			`INSERT INTO graph_cluster_topic (scope) VALUES ('private')
			   RETURNING get_byte(uuid_send(topic_id), 6) >> 4`,
		).Scan(&version); err != nil {
			t.Fatalf("insert topic: %v", err)
		}
		if version != 4 {
			t.Errorf("topic_id version nibble = %d, want 4 (gen_random_uuid, not uuidv7)", version)
		}
	})

	t.Run("checks_effective", func(t *testing.T) {
		var seed string
		if err := pool.QueryRow(ctx,
			`INSERT INTO graph_cluster_topic (scope) VALUES ('private') RETURNING topic_id`,
		).Scan(&seed); err != nil {
			t.Fatalf("seed topic: %v", err)
		}

		cases := []struct {
			name string
			sql  string
			args []any
		}{
			{"origin_kind_vocab",
				`INSERT INTO graph_cluster_topic (scope, origin_kind) VALUES ('private', 'bogus')`, nil},
			{"merge_implies_retired",
				`INSERT INTO graph_cluster_topic (scope, merged_into) VALUES ('private', $1)`, []any{seed}},
			{"no_self_merge",
				`UPDATE graph_cluster_topic SET retired_at = now(), merged_into = topic_id WHERE topic_id = $1`, []any{seed}},
			{"no_self_origin",
				`UPDATE graph_cluster_topic SET origin_topic_id = topic_id WHERE topic_id = $1`, []any{seed}},
		}
		for _, tc := range cases {
			_, err := pool.Exec(ctx, tc.sql, tc.args...)
			if err == nil {
				t.Errorf("%s: expected 23514, got nil", tc.name)
				continue
			}
			if !strings.Contains(err.Error(), "23514") && !strings.Contains(err.Error(), "violates check constraint") {
				t.Errorf("%s: expected check violation, got %v", tc.name, err)
			}
		}
	})

	// Die beiden Selbstreferenzen tragen ON DELETE SET NULL, damit die
	// Retention (W8) Grabsteine wegräumen kann, ohne eine Lineage-Kette zu
	// zerreißen.
	t.Run("lineage_fk_set_null", func(t *testing.T) {
		var parent, child string
		if err := pool.QueryRow(ctx,
			`INSERT INTO graph_cluster_topic (scope) VALUES ('private') RETURNING topic_id`,
		).Scan(&parent); err != nil {
			t.Fatalf("insert parent: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`INSERT INTO graph_cluster_topic (scope, origin_kind, origin_topic_id)
			   VALUES ('private', 'split', $1) RETURNING topic_id`, parent,
		).Scan(&child); err != nil {
			t.Fatalf("insert child: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM graph_cluster_topic WHERE topic_id = $1`, parent); err != nil {
			t.Fatalf("delete parent: %v", err)
		}
		var origin *string
		if err := pool.QueryRow(ctx,
			`SELECT origin_topic_id FROM graph_cluster_topic WHERE topic_id = $1`, child,
		).Scan(&origin); err != nil {
			t.Fatalf("read child: %v", err)
		}
		if origin != nil {
			t.Errorf("origin_topic_id = %v after parent delete, want NULL", *origin)
		}
	})

	// Das Injektivitäts-Gate: zwei Cluster desselben Scopes dürfen nie dasselbe
	// Topic beanspruchen. Ein Zuordnungs-Bug in W3 bricht damit mit 23505 statt
	// still zwei Identitäten zu verschmelzen.
	t.Run("uq_gcn_scope_topic_rejects_duplicate_claim", func(t *testing.T) {
		var topic string
		if err := pool.QueryRow(ctx,
			`INSERT INTO graph_cluster_topic (scope) VALUES ('private') RETURNING topic_id`,
		).Scan(&topic); err != nil {
			t.Fatalf("insert topic: %v", err)
		}
		ins := `INSERT INTO graph_cluster_node
		          (cluster_id, scope, size, repr_block_id, repr_title, topic_id)
		        VALUES ($1, 'private', 1, $2, 't', $3)`
		if _, err := pool.Exec(ctx, ins, tsBlockA, tsBlockA, topic); err != nil {
			t.Fatalf("first claim: %v", err)
		}
		_, err := pool.Exec(ctx, ins, tsBlockB, tsBlockB, topic)
		if err == nil {
			t.Fatalf("second claim of the same topic accepted, want 23505")
		}
		if !strings.Contains(err.Error(), "23505") && !strings.Contains(err.Error(), "duplicate key") {
			t.Fatalf("second claim: expected 23505, got %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM graph_cluster_node WHERE cluster_id = $1`, tsBlockA); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	})

	// Der Kern der Achse: der 057-Teardown (cluster.go:508-531) ersetzt die drei
	// Tabellen bei JEDEM Rebuild. Die Topic-Zeile inklusive core_blocks muss ihn
	// überleben — sonst gibt es weder Identität noch Grabstein.
	t.Run("topic_row_survives_teardown", func(t *testing.T) {
		var topic string
		if err := pool.QueryRow(ctx,
			`INSERT INTO graph_cluster_topic (scope, core_blocks) VALUES ('private', ARRAY[$1::uuid, $2::uuid])
			   RETURNING topic_id`, tsBlockA, tsBlockB,
		).Scan(&topic); err != nil {
			t.Fatalf("insert topic: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`TRUNCATE graph_cluster_member, graph_cluster_node, graph_cluster_edge`,
		); err != nil {
			t.Fatalf("teardown: %v", err)
		}
		var core []string
		if err := pool.QueryRow(ctx,
			`SELECT core_blocks::text[] FROM graph_cluster_topic WHERE topic_id = $1`, topic,
		).Scan(&core); err != nil {
			t.Fatalf("read topic after teardown: %v", err)
		}
		if len(core) != 2 {
			t.Errorf("core_blocks = %v after teardown, want 2 entries", core)
		}
	})

	// G3a: die Zähl-Fallback-Erwartung in test.sh ist handgepflegt und hängt am
	// Migrations-Stand. Sie greift live nur, wenn `ctx contract` fehlt oder
	// fehlschlägt — auf einer Maschine mit installierter CLI kann sie also weder
	// rot werden noch die neue Zahl bestätigen. Dieselbe Zählung gegen eine
	// frisch migrierte DB macht sie prüfbar: eine neue Tabelle ohne Nachzug in
	// test.sh:305 färbt ab hier diesen Test rot statt still durchzugehen.
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
			t.Errorf("fresh DB has %d tables, test.sh T07_EXPECT_TABLES = %d — nachziehen (test.sh:305)", got, want)
		}

		wantCols := grepInt(t, string(script), `T07_EXPECT_COLUMNS=(\d+)`)
		var gotCols int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.columns WHERE table_name = 'context_blocks'`,
		).Scan(&gotCols); err != nil {
			t.Fatalf("column count: %v", err)
		}
		if gotCols != wantCols {
			t.Errorf("context_blocks has %d columns, test.sh T07_EXPECT_COLUMNS = %d", gotCols, wantCols)
		}
	})

	// G1: Idempotenz. Der Runner überspringt bereits angewandte Versionen, also
	// wird der Dateikörper hier von Hand ein zweites Mal ausgeführt — in einer
	// zurückgerollten Tx, exakt wie der Runner ihn hält (store/migrations.go).
	t.Run("idempotent_second_apply", func(t *testing.T) {
		body, err := migrations.FS.ReadFile(topicMigrationFile)
		if err != nil {
			t.Fatalf("read %s: %v", topicMigrationFile, err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			t.Fatalf("second apply of %s: %v", topicMigrationFile, err)
		}
	})
}
