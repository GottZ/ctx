//go:build integration

// Integration test for the Cluster-Topic-Map Achse 01 W4 schema (migration
// 125): die Label-Spalten auf graph_cluster_topic — Label, Quelle,
// Drift-Anker, Drift-Flag, Versuchszähler — plus die drei CHECKs und den
// Selektions-Index der Label-Pipeline.
//
// W4 liefert nur den Schemaplatz; der deterministische Fallback-Label (W5) und
// die LLM-Pipeline (W6) sind eigene Wellen. Geprüft wird deshalb, was das
// Schema selbst garantiert: das Quellen-Vokabular inklusive 'manual' (E5-01),
// die Rune-genaue Längengrenze und die "als gelabelt markiert, aber leer"-
// Sperre.
//
// Run: go test -tags=integration ./internal/store/ -run TestClusterTopicLabelSchema -count=1 -v
package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GottZ/ctx/migrations"

	"github.com/GottZ/ctx/internal/testdb"
)

const labelMigrationFile = "125_cluster_topic_label.sql"

func TestClusterTopicLabelSchema_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	t.Run("columns_present", func(t *testing.T) {
		want := map[string]string{
			"label":           "YES",
			"label_source":    "NO",
			"label_built_at":  "YES",
			"label_core_hash": "YES",
			"label_model":     "YES",
			"label_attempts":  "NO",
			"label_stale":     "NO",
		}
		for col, nullable := range want {
			var got string
			if err := pool.QueryRow(ctx,
				`SELECT is_nullable FROM information_schema.columns
				  WHERE table_name = 'graph_cluster_topic' AND column_name = $1`, col,
			).Scan(&got); err != nil {
				t.Errorf("column %s: %v", col, err)
				continue
			}
			if got != nullable {
				t.Errorf("column %s is_nullable = %s, want %s", col, got, nullable)
			}
		}
	})

	// Ein frisch geborenes Topic ist per Definition label-bedürftig: es hat noch
	// keinen Kern-Vergleich gesehen. Der Default fällt damit in Richtung
	// "labeln", nicht in Richtung "übersehen".
	t.Run("defaults_are_fail_closed_towards_labeling", func(t *testing.T) {
		var source string
		var attempts int
		var stale bool
		if err := pool.QueryRow(ctx,
			`INSERT INTO graph_cluster_topic (scope) VALUES ('private')
			   RETURNING label_source, label_attempts, label_stale`,
		).Scan(&source, &attempts, &stale); err != nil {
			t.Fatalf("insert topic: %v", err)
		}
		if source != "none" || attempts != 0 || !stale {
			t.Errorf("defaults = (%q, %d, %v), want (\"none\", 0, true)", source, attempts, stale)
		}
	})

	t.Run("label_source_vocabulary", func(t *testing.T) {
		// E5-01: 'manual' ist Teil des Vokabulars, obwohl der Schreib-Endpoint
		// erst in einer Folgewelle kommt — der Automatik-Pfad muss den Wert
		// respektieren können, bevor ihn jemand setzen kann.
		for _, src := range []string{"none", "fallback", "llm", "manual"} {
			label := "ein Label"
			if src == "none" {
				label = ""
			}
			var args []any
			sql := `INSERT INTO graph_cluster_topic (scope, label_source, label) VALUES ('private', $1, $2)`
			if src == "none" {
				sql = `INSERT INTO graph_cluster_topic (scope, label_source, label) VALUES ('private', $1, NULL)`
				args = []any{src}
			} else {
				args = []any{src, label}
			}
			if _, err := pool.Exec(ctx, sql, args...); err != nil {
				t.Errorf("label_source = %q rejected: %v", src, err)
			}
		}
		_, err := pool.Exec(ctx,
			`INSERT INTO graph_cluster_topic (scope, label_source, label) VALUES ('private', 'bogus', 'x')`)
		assertCheckViolation(t, "label_source_bogus", err)
	})

	// "als gelabelt markiert, aber leer" wäre in der Wurzel-Map eine unsichtbare
	// Lücke statt eines sichtbaren Fehlers.
	t.Run("label_present_when_source_is_not_none", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO graph_cluster_topic (scope, label_source, label) VALUES ('private', 'llm', NULL)`)
		assertCheckViolation(t, "llm_without_label", err)
	})

	// 120 wie repr_title (057), aber char_length — Zeichen, nicht Bytes.
	// Dieselbe Rune-Genauigkeit, die digest.truncateTitle gegen 22021
	// herstellt: ein 120-Umlaut-Label wiegt 240 Bytes und muss trotzdem passen.
	t.Run("label_length_is_rune_exact", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO graph_cluster_topic (scope, label_source, label) VALUES ('private', 'fallback', $1)`,
			strings.Repeat("ä", 120),
		); err != nil {
			t.Errorf("120 Runen (240 Bytes) abgelehnt — die Grenze zählt Bytes statt Zeichen: %v", err)
		}
		_, err := pool.Exec(ctx,
			`INSERT INTO graph_cluster_topic (scope, label_source, label) VALUES ('private', 'fallback', $1)`,
			strings.Repeat("a", 121))
		assertCheckViolation(t, "label_121_chars", err)

		_, err = pool.Exec(ctx,
			`INSERT INTO graph_cluster_topic (scope, label_source, label) VALUES ('private', 'fallback', '')`)
		assertCheckViolation(t, "label_empty", err)
	})

	// Das Index-Prädikat deckt genau die Selektionsbedingung der Label-Pipeline:
	// lebend, nicht manuell gepinnt. Ein Grabstein steht nie im Index, und ein
	// gepinntes Label kann von der Automatik nicht einmal gefunden werden.
	t.Run("pending_index_shape", func(t *testing.T) {
		var def string
		if err := pool.QueryRow(ctx,
			`SELECT indexdef FROM pg_indexes
			  WHERE tablename = 'graph_cluster_topic' AND indexname = 'idx_gct_label_pending'`,
		).Scan(&def); err != nil {
			t.Fatalf("idx_gct_label_pending missing: %v", err)
		}
		for _, want := range []string{
			"scope", "label_stale", "label_attempts",
			"retired_at IS NULL", "label_source <> 'manual'",
		} {
			if !strings.Contains(def, want) {
				t.Errorf("idx_gct_label_pending lacks %q: %s", want, def)
			}
		}
	})

	// Additive Spalten: die Tabellenzahl bleibt bei 51, nur Spalten kommen dazu.
	t.Run("no_new_table", func(t *testing.T) {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.tables
			  WHERE table_schema = 'public' AND table_name NOT LIKE '%_snapshot_%'`,
		).Scan(&n); err != nil {
			t.Fatalf("table count: %v", err)
		}
		if n != 51 {
			t.Errorf("table count = %d, want 51 (Migration 125 ist rein additiv)", n)
		}
	})

	t.Run("idempotent_second_apply", func(t *testing.T) {
		body, err := migrations.FS.ReadFile(labelMigrationFile)
		if err != nil {
			t.Fatalf("read %s: %v", labelMigrationFile, err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			t.Fatalf("second apply of %s: %v", labelMigrationFile, err)
		}
	})
}

func assertCheckViolation(t *testing.T, name string, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: expected 23514, got nil", name)
		return
	}
	if !strings.Contains(err.Error(), "23514") && !strings.Contains(err.Error(), "violates check constraint") {
		t.Errorf("%s: expected check violation, got %v", name, err)
	}
}
