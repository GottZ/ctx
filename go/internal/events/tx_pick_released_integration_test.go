//go:build integration

// K35-Wächter (Tree-Shaking-Run, Welle T04-4c): errPickReleased ist die
// paket-private Bedeutung „diese Transaktion endet OHNE Commit, und der Arm
// meldet trotzdem keinen Fehler". Vor der Welle war dieser Ausgang ein
// schlichtes `return false, nil` bzw. `return deferred, nil` unter dem
// defer-Rollback; seit die Klammer in pgxdb.Write liegt, ist er ein
// Sentinel, den beide Arme DIREKT hinter der Klammer per errors.Is in genau
// dieses alte Ergebnis übersetzen (scheduler.go backfillOneEmbedding,
// embed_migrate.go migrateOneEmbedding).
//
// Warum ein eigener Test: fällt eine der beiden Übersetzungen weg, kommt der
// Sentinel als Fehler beim Aufrufer an — der Backfill-Arm loggt ihn als
// „embed backfill error" (scheduler.go:1845), der Migrations-Batch bricht ab
// (embed_migrate.go:286) — und die übrige Suite bleibt grün, weil kein
// vorhandener Fall diesen Ausgang unter gehaltener Fremdsperre fährt.
//
// Rot-Probe (W10, von Hand gefahren und in reports/bau/T04-4c.md §7c
// protokolliert): je Übersetzung die `if errors.Is(err, errPickReleased)`-
// Klammer entfernen ⇒ genau der zugehörige Unterfall wird rot.
//
// Lauf:
//
//	go test -tags=integration ./internal/events/ -run TestPickReleased -count=1 -v
package events

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embedmigration"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lockAllBlocks hält jede Zeile von context_blocks in einer FREMDEN
// Transaktion. Der lock-freie Peek der Arme sieht die Zeile weiterhin (MVCC),
// ihr Pick mit FOR UPDATE SKIP LOCKED überspringt sie — das ist der
// „verlorener Pick"-Ausgang, den errPickReleased benennt. Rückgabe: der
// Freigeber.
func lockAllBlocks(t *testing.T, pool *pgxpool.Pool) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("Sperr-Tx öffnen: %v", err)
	}
	rows, err := tx.Query(ctx, `SELECT id FROM context_blocks FOR UPDATE`)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("Zeilen sperren: %v", err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("Sperr-Iteration: %v", err)
	}
	if n == 0 {
		_ = tx.Rollback(ctx)
		t.Fatal("Fixture: keine Zeile zu sperren — der Pick fiele aus dem falschen Grund leer aus")
	}
	return func() { _ = tx.Rollback(ctx) }
}

func TestPickReleasedTranslatesToNoWork_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	s := NewScheduler(pool, config.NewStore(&config.Config{}), backends.NewPool(nil, nil), StartupConfig{})

	// backfill_arm: scheduler.go — Übersetzung nach `false, nil`.
	t.Run("backfill_arm", func(t *testing.T) {
		seedPendingBlock(t, pool, "pick-released-backfill", time.Hour)
		defer clearBlocks(t, pool)

		srv := newQI3EmbedServer(t, http.StatusOK)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedPoolRow("embed-a", srv.srv.URL, 100)})
		d := dispatch.New(nil, dispatch.DefaultSettings())
		t.Cleanup(d.Close)

		release := lockAllBlocks(t, pool)
		defer release()

		ok, err := s.backfillOneEmbedding(ctx, backfillRouter(bpool, d), qi3Cfg())
		if err != nil {
			t.Fatalf("backfillOneEmbedding = %v, want nil (der verlorene Pick ist kein Fehler — Übersetzung von errPickReleased fehlt?)", err)
		}
		if ok {
			t.Error("backfillOneEmbedding meldet Arbeit, obwohl der Pick leer blieb")
		}
		if got := len(srv.recorded()); got != 0 {
			t.Errorf("Wire-Calls = %d, want 0 (ohne Pick kein Embed)", got)
		}
		if got := pendingCount(t, pool); got != 1 {
			t.Errorf("pending = %d, want 1 (der Block bleibt unangetastet in der Schlange)", got)
		}
	})

	// migration_arm: embed_migrate.go — Übersetzung nach `deferred, nil`.
	t.Run("migration_arm", func(t *testing.T) {
		defer resetMigrationFixture(t, pool)
		seedMigrationRow(t, pool, "running")
		seedMigratableBlock(t, pool, "pick-released-migrate", "content", "internal", time.Hour)

		srv := newMigrateEmbedServer(t)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedNextPoolRow("embed-a", srv.srv.URL, 100)})
		d := dispatch.New(nil, dispatch.DefaultSettings())
		t.Cleanup(d.Close)

		mig, err := embedmigration.Active(ctx, pool)
		if err != nil {
			t.Fatalf("aktive Migration lesen: %v", err)
		}
		if mig == nil {
			t.Fatal("Fixture: keine aktive Migration")
		}

		release := lockAllBlocks(t, pool)
		defer release()

		out, err := s.migrateOneEmbedding(ctx, backfillRouter(bpool, d), migCfg(8, 0), mig, nil)
		if err != nil {
			t.Fatalf("migrateOneEmbedding = %v, want nil (der verlorene Pick ist kein Fehler — Übersetzung von errPickReleased fehlt?)", err)
		}
		if out.kind != migrateOutcomeDeferred {
			t.Errorf("outcome = %v, want %v (verlorener Pick verschiebt, er zählt nicht)", out.kind, migrateOutcomeDeferred)
		}
		if got := len(srv.recorded()); got != 0 {
			t.Errorf("Wire-Calls = %d, want 0 (ohne Pick kein Embed)", got)
		}
		if got := countNextEmbedded(t, pool); got != 0 {
			t.Errorf("embedding_next-Zeilen = %d, want 0", got)
		}
	})
}

// TestVerifyRedRaceLostRollsBackSilently_Integration bewacht den zweiten
// Commit-losen Ausgang dieser Welle: finishVerifyRed schreibt den roten
// Report und die CAS `verifying → paused` in EINER Transaktion; verliert die
// CAS das Rennen (die Zeile hat `verifying` inzwischen verlassen), fällt der
// Report-Write mit dem Rollback, und der Arm meldet KEINEN Fehler. Seit die
// Klammer in pgxdb.Write liegt, reist embedmigration.ErrTransitionRaceLost
// unverpackt aus der Closure und wird direkt hinter der Klammer übersetzt
// (embed_verify.go); ohne diesen Test bliebe die Suite grün, wenn die
// Übersetzung entfiele oder der Report-Write plötzlich mitcommittete.
//
// Rot-Probe (W10, protokolliert in reports/bau/T04-4c.md §7c): die
// errors.Is-Klammer auf ErrTransitionRaceLost entfernen ⇒ dieser Test wird
// rot.
func TestVerifyRedRaceLostRollsBackSilently_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	s := NewScheduler(pool, config.NewStore(&config.Config{}), backends.NewPool(nil, nil), StartupConfig{})
	defer resetMigrationFixture(t, pool)

	migID := seedMigrationRow(t, pool, "verifying")
	mig, err := embedmigration.Active(ctx, pool)
	if err != nil {
		t.Fatalf("aktive Migration lesen: %v", err)
	}
	if mig == nil {
		t.Fatal("Fixture: keine aktive Migration")
	}

	// Das Rennen: die Zeile verlässt `verifying`, NACHDEM der Arm sie
	// gelesen hat — der Fall, den §4.7 mit „das Verdikt gehörte zu einem
	// Zustand, den es nicht mehr gibt" beschreibt.
	if _, err := pool.Exec(ctx,
		`UPDATE context_embed_migrations SET status = 'paused' WHERE id = $1::uuid`, migID); err != nil {
		t.Fatalf("Rennen stellen: %v", err)
	}

	if err := s.finishVerifyRed(ctx, mig, &verifyReport{}); err != nil {
		t.Fatalf("finishVerifyRed = %v, want nil (die verlorene CAS ist kein Fehler — Übersetzung des Sentinels fehlt?)", err)
	}

	var report *string
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status, verify_report::text FROM context_embed_migrations WHERE id = $1::uuid`, migID).
		Scan(&status, &report); err != nil {
		t.Fatalf("Migrationszeile lesen: %v", err)
	}
	if report != nil {
		t.Errorf("verify_report = %q, want NULL (der Rollback muss den Report-Write mitnehmen)", *report)
	}
	if status != "paused" {
		t.Errorf("status = %q, want paused (der fremde Übergang bleibt stehen)", status)
	}
}
