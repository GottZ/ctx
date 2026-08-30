//go:build integration

// Migration 150 — die Zerlegung von rej_g3 nach dem Ort des Zitats (Welle
// C5-A, Entscheid C5-3). Die Sonde ueber die FORM; die Sonde ueber die
// BEDEUTUNG — dass die Spalten im Betrieb die richtigen Zahlen tragen — steht
// in internal/events/distill_g3class_integration_test.go.
//
//	go test -tags=integration ./internal/store/ -run TestMigration150 -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/testdb"
)

// m150Columns sind die vier Zaehlerspalten in der Reihenfolge der Migration —
// und das ist zugleich die Praezedenz der Klassifikation (kleinster
// Adressierungsfehler zuerst).
var m150Columns = []string{"rej_g3_chunk", "rej_g3_span", "rej_g3_part", "rej_g3_none"}

// TestMigration150Shape: jede der vier Spalten existiert als INTEGER NOT NULL
// mit Default 0 — dieselbe tragende Eigenschaft wie bei 149: ein NULL waere ein
// drittes Ergebnis neben "null Verwuerfe" und "nicht gemessen", und ein sum()
// ueber ein Retention-Fenster muesste COALESCE rechnen.
func TestMigration150Shape(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for _, col := range m150Columns {
		var dataType, defaultExpr string
		var notNull bool
		err := pool.QueryRow(ctx, `
			SELECT data_type, is_nullable = 'NO', COALESCE(column_default, '')
			  FROM information_schema.columns
			 WHERE table_name = 'distill_run' AND column_name = $1`, col).
			Scan(&dataType, &notNull, &defaultExpr)
		if err != nil {
			t.Errorf("column %s: %v", col, err)
			continue
		}
		if dataType != "integer" {
			t.Errorf("column %s: data_type = %q, want integer", col, dataType)
		}
		if !notNull {
			t.Errorf("column %s ist nullable", col)
		}
		if defaultExpr != "0" {
			t.Errorf("column %s: default = %q, want 0", col, defaultExpr)
		}
	}
}

// TestMigration150ZeroDecompositionOnExistingRows: eine Zeile aus der Zeit vor
// dieser Welle behaelt ihr rej_g3 und traegt eine Null-Zerlegung. Das ist die
// bewusste Entscheidung gegen einen Backfill — die C4-R-Laeufe haben den Ort
// ihrer Zitate nie gemessen, und eine nachtraeglich berechnete Zuordnung waere
// eine Behauptung ueber Material, das der Lauf laengst verworfen hat.
func TestMigration150ZeroDecompositionOnExistingRows(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var runID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO distill_run (source_key, outcome, watermark_from, watermark_to,
		                         finished_at, insights_rejected, rej_g3)
		VALUES ('m150:probe', 'ok', 0, 10, now(), 9, 5)
		RETURNING run_id::text`).Scan(&runID); err != nil {
		t.Fatalf("seed a pre-wave row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM distill_run WHERE run_id = $1::uuid`, runID)
	})

	var g3, sum int
	if err := pool.QueryRow(ctx, `
		SELECT rej_g3, rej_g3_chunk + rej_g3_span + rej_g3_part + rej_g3_none
		  FROM distill_run WHERE run_id = $1::uuid`, runID).Scan(&g3, &sum); err != nil {
		t.Fatalf("read the row back: %v", err)
	}
	if g3 != 5 {
		t.Errorf("rej_g3 = %d, want 5 — das Aggregat bleibt unangetastet", g3)
	}
	if sum != 0 {
		t.Errorf("Zerlegungs-Summe = %d, want 0 — die Migration backfillt nicht", sum)
	}
}

// TestMigration150Idempotent: das zweite Anwenden der DDL-Fragmente ist
// folgenlos, und es setzt insbesondere KEINE Werte zurueck — was die Sonde an
// einer beschriebenen Zeile festmacht.
func TestMigration150Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var runID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO distill_run (source_key, outcome, watermark_from, watermark_to,
		                         finished_at, insights_rejected, rej_g3,
		                         rej_g3_chunk, rej_g3_span, rej_g3_part, rej_g3_none)
		VALUES ('m150:idem', 'ok', 0, 10, now(), 6, 6, 3, 1, 1, 1)
		RETURNING run_id::text`).Scan(&runID); err != nil {
		t.Fatalf("seed a written row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM distill_run WHERE run_id = $1::uuid`, runID)
	})

	// Zweiter Lauf der Migration, Anweisung fuer Anweisung: pgx bereitet vor,
	// und ein Prepared Statement traegt genau ein Kommando (SQLSTATE 42601).
	for _, col := range m150Columns {
		if _, err := pool.Exec(ctx,
			`ALTER TABLE distill_run ADD COLUMN IF NOT EXISTS `+col+` INTEGER NOT NULL DEFAULT 0`); err != nil {
			t.Fatalf("re-apply %s: %v", col, err)
		}
	}

	var chunk, span, part, none, g3 int
	if err := pool.QueryRow(ctx, `
		SELECT rej_g3_chunk, rej_g3_span, rej_g3_part, rej_g3_none, rej_g3
		  FROM distill_run WHERE run_id = $1::uuid`, runID).
		Scan(&chunk, &span, &part, &none, &g3); err != nil {
		t.Fatalf("read the row back: %v", err)
	}
	if chunk != 3 || span != 1 || part != 1 || none != 1 || g3 != 6 {
		t.Errorf("nach dem zweiten Lauf chunk/span/part/none/g3 = %d/%d/%d/%d/%d, want 3/1/1/1/6 — "+
			"das Re-Apply hat Daten angefasst", chunk, span, part, none, g3)
	}

	// Und die Journal-Zeile der Migration steht genau einmal (PK auf version).
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM _migrations WHERE version = 150`).Scan(&n); err != nil {
		t.Fatalf("count migration row: %v", err)
	}
	if n != 1 {
		t.Errorf("_migrations version 150 count = %d, want 1", n)
	}
}
