//go:build integration

// Migration 151 — der Zaehler des Per-Claim-Novelty-Floors (Welle C5-E). Die
// Sonde ueber die FORM; die Sonde ueber die BEDEUTUNG — dass die Spalte im
// Betrieb die richtige Zahl traegt und die erweiterte Gleichung haelt — steht in
// internal/events/distill_novelty_integration_test.go.
//
//	go test -tags=integration ./internal/store/ -run TestMigration151 -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/testdb"
)

// TestMigration151Shape: rej_novelty existiert als INTEGER NOT NULL mit
// Default 0 — dieselbe tragende Eigenschaft wie bei 149 und 150: ein NULL waere
// ein drittes Ergebnis neben "null Verwuerfe" und "nicht gemessen", und ein
// sum() ueber ein Retention-Fenster muesste COALESCE rechnen.
func TestMigration151Shape(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var dataType, defaultExpr string
	var notNull bool
	if err := pool.QueryRow(ctx, `
		SELECT data_type, is_nullable = 'NO', COALESCE(column_default, '')
		  FROM information_schema.columns
		 WHERE table_name = 'distill_run' AND column_name = 'rej_novelty'`).
		Scan(&dataType, &notNull, &defaultExpr); err != nil {
		t.Fatalf("column rej_novelty: %v", err)
	}
	if dataType != "integer" {
		t.Errorf("data_type = %q, want integer", dataType)
	}
	if !notNull {
		t.Error("rej_novelty ist nullable")
	}
	if defaultExpr != "0" {
		t.Errorf("default = %q, want 0", defaultExpr)
	}
}

// TestMigration151ZeroOnExistingRows: eine Zeile aus der Zeit vor dieser Welle
// behaelt ihr insights_rejected und traegt einen Null-Zaehler. Das ist dieselbe
// Entscheidung gegen einen Backfill, die 149 und 150 getroffen haben — die
// frueheren Laeufe hatten kein Tor, dessen Verwuerfe man nachtragen koennte, und
// eine berechnete Zahl waere eine Behauptung ueber Claims, die laengst
// geschrieben sind.
//
// SIE HAT HIER EINE ZWEITE SEITE: die erweiterte Gleichung
// sum(g1..g7)+schema+novelty = insights_rejected gilt fuer die Bestandszeilen
// unveraendert, weil der neue Summand 0 ist. Die Erweiterung bricht die alten
// Zeilen also nicht — sie erbt sie.
func TestMigration151ZeroOnExistingRows(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var runID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO distill_run (source_key, outcome, watermark_from, watermark_to,
		                         finished_at, insights_rejected, rej_g3, rej_g7)
		VALUES ('m151:probe', 'ok', 0, 10, now(), 9, 5, 4)
		RETURNING run_id::text`).Scan(&runID); err != nil {
		t.Fatalf("seed a pre-wave row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM distill_run WHERE run_id = $1::uuid`, runID)
	})

	var rejected, novelty, sum int
	if err := pool.QueryRow(ctx, `
		SELECT insights_rejected, rej_novelty,
		       rej_g1 + rej_g2 + rej_g3 + rej_g4 + rej_g5 + rej_g6 + rej_g7
		           + rej_schema + rej_novelty
		  FROM distill_run WHERE run_id = $1::uuid`, runID).Scan(&rejected, &novelty, &sum); err != nil {
		t.Fatalf("read the row back: %v", err)
	}
	if rejected != 9 {
		t.Errorf("insights_rejected = %d, want 9 — das Aggregat bleibt unangetastet", rejected)
	}
	if novelty != 0 {
		t.Errorf("rej_novelty = %d, want 0 — die Migration backfillt nicht", novelty)
	}
	if sum != rejected {
		t.Errorf("erweiterte Summe = %d, want %d — die neue Gleichung gilt auch fuer eine "+
			"Bestandszeile", sum, rejected)
	}
}

// TestMigration151Idempotent: das zweite Anwenden der DDL ist folgenlos und
// setzt insbesondere KEINEN Wert zurueck — festgemacht an einer beschriebenen
// Zeile.
func TestMigration151Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var runID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO distill_run (source_key, outcome, watermark_from, watermark_to,
		                         finished_at, insights_rejected, rej_g7, rej_novelty)
		VALUES ('m151:idem', 'ok', 0, 10, now(), 7, 3, 4)
		RETURNING run_id::text`).Scan(&runID); err != nil {
		t.Fatalf("seed a written row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM distill_run WHERE run_id = $1::uuid`, runID)
	})

	// Zweiter Lauf der Migration, Anweisung fuer Anweisung: pgx bereitet vor,
	// und ein Prepared Statement traegt genau ein Kommando (SQLSTATE 42601).
	if _, err := pool.Exec(ctx,
		`ALTER TABLE distill_run ADD COLUMN IF NOT EXISTS rej_novelty INTEGER NOT NULL DEFAULT 0`); err != nil {
		t.Fatalf("re-apply rej_novelty: %v", err)
	}

	var novelty, g7, rejected int
	if err := pool.QueryRow(ctx, `
		SELECT rej_novelty, rej_g7, insights_rejected
		  FROM distill_run WHERE run_id = $1::uuid`, runID).Scan(&novelty, &g7, &rejected); err != nil {
		t.Fatalf("read the row back: %v", err)
	}
	if novelty != 4 || g7 != 3 || rejected != 7 {
		t.Errorf("nach dem zweiten Lauf novelty/g7/rejected = %d/%d/%d, want 4/3/7 — das "+
			"Re-Apply hat Daten angefasst", novelty, g7, rejected)
	}

	// Und die Journal-Zeile der Migration steht genau einmal (PK auf version).
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM _migrations WHERE version = 151`).Scan(&n); err != nil {
		t.Fatalf("count migration row: %v", err)
	}
	if n != 1 {
		t.Errorf("_migrations version 151 count = %d, want 1", n)
	}
}
