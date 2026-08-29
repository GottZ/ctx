//go:build integration

// Migration 149 — Reject-Histogramm + Gruppen-Verkleinerungen auf distill_run
// (Welle C4-1, Befund N-6). Die Sonde ueber die FORM; die Sonde ueber die
// Bedeutung — dass die Spalten im Betrieb die richtigen Zahlen tragen — steht
// in internal/events/distill_reject_n6_integration_test.go.
//
//	go test -tags=integration ./internal/store/ -run TestMigration149 -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/testdb"
)

// m149Columns sind die neun Zaehlerspalten in der Reihenfolge der Migration.
var m149Columns = []string{
	"rej_g1", "rej_g2", "rej_g3", "rej_g4", "rej_g5", "rej_g6", "rej_g7",
	"rej_schema", "call_groups_shrunk",
}

// TestMigration149Shape: jede der neun Spalten existiert als INTEGER NOT NULL
// mit Default 0. Der Default ist die tragende Eigenschaft, nicht Kosmetik —
// Bestandszeilen sollen ein NULL-Histogramm tragen und keine NULLs, damit ein
// sum() ueber ein Retention-Fenster ohne COALESCE rechnet.
func TestMigration149Shape(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for _, col := range m149Columns {
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
			t.Errorf("column %s ist nullable — ein NULL im Histogramm waere ein "+
				"drittes Ergebnis neben 'null Verwuerfe' und 'nicht gemessen'", col)
		}
		if defaultExpr != "0" {
			t.Errorf("column %s: default = %q, want 0", col, defaultExpr)
		}
	}
}

// TestMigration149ZeroHistogramOnExistingRows: eine Zeile, die vor der Welle
// geschrieben worden waere, traegt ihr Aggregat weiter und ein Null-Histogramm.
// Das ist die bewusste Entscheidung der Migration gegen einen Backfill — eine
// nachtraeglich erfundene Zerlegung waere keine Messung.
func TestMigration149ZeroHistogramOnExistingRows(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var runID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO distill_run (source_key, outcome, watermark_from, watermark_to,
		                         finished_at, insights_rejected)
		VALUES ('m149:probe', 'ok', 0, 10, now(), 7)
		RETURNING run_id::text`).Scan(&runID); err != nil {
		t.Fatalf("seed a pre-wave row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM distill_run WHERE run_id = $1::uuid`, runID)
	})

	var rejected, sum, shrunk int
	if err := pool.QueryRow(ctx, `
		SELECT insights_rejected,
		       rej_g1 + rej_g2 + rej_g3 + rej_g4 + rej_g5 + rej_g6 + rej_g7 + rej_schema,
		       call_groups_shrunk
		  FROM distill_run WHERE run_id = $1::uuid`, runID).Scan(&rejected, &sum, &shrunk); err != nil {
		t.Fatalf("read the row back: %v", err)
	}
	if rejected != 7 {
		t.Errorf("insights_rejected = %d, want 7 — das Aggregat bleibt unangetastet", rejected)
	}
	if sum != 0 || shrunk != 0 {
		t.Errorf("Histogramm-Summe/shrunk = %d/%d, want 0/0 — die Migration backfillt nicht", sum, shrunk)
	}
}

// TestMigration149Idempotent: das zweite Anwenden der DDL-Fragmente ist
// folgenlos (Muster TestMigration093_Idempotent). ADD COLUMN IF NOT EXISTS
// laesst eine bestehende Spalte in Ruhe — insbesondere setzt es KEINE Werte
// zurueck, was die Probe unten an einer beschriebenen Zeile festmacht.
func TestMigration149Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var runID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO distill_run (source_key, outcome, watermark_from, watermark_to,
		                         finished_at, insights_rejected, rej_g3, call_groups_shrunk)
		VALUES ('m149:idem', 'ok', 0, 10, now(), 4, 4, 2)
		RETURNING run_id::text`).Scan(&runID); err != nil {
		t.Fatalf("seed a written row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM distill_run WHERE run_id = $1::uuid`, runID)
	})

	// Zweiter Lauf der Migration, Anweisung fuer Anweisung: pgx bereitet vor,
	// und ein Prepared Statement traegt genau ein Kommando (SQLSTATE 42601).
	for _, col := range m149Columns {
		if _, err := pool.Exec(ctx,
			`ALTER TABLE distill_run ADD COLUMN IF NOT EXISTS `+col+` INTEGER NOT NULL DEFAULT 0`); err != nil {
			t.Fatalf("re-apply %s: %v", col, err)
		}
	}

	var g3, shrunk, rejected int
	if err := pool.QueryRow(ctx, `
		SELECT rej_g3, call_groups_shrunk, insights_rejected
		  FROM distill_run WHERE run_id = $1::uuid`, runID).Scan(&g3, &shrunk, &rejected); err != nil {
		t.Fatalf("read the row back: %v", err)
	}
	if g3 != 4 || shrunk != 2 || rejected != 4 {
		t.Errorf("nach dem zweiten Lauf rej_g3/shrunk/rejected = %d/%d/%d, want 4/2/4 — "+
			"das Re-Apply hat Daten angefasst", g3, shrunk, rejected)
	}

	// Und die Journal-Zeile der Migration steht genau einmal (PK auf version).
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM _migrations WHERE version = 149`).Scan(&n); err != nil {
		t.Fatalf("count migration row: %v", err)
	}
	if n != 1 {
		t.Errorf("_migrations version 149 count = %d, want 1", n)
	}
}
