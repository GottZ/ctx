//go:build integration

// Migration 169 under a concurrent ingest. The boot that runs 169 is not
// necessarily the only process writing the database: another ctxd on the
// same database (a replica that has not been upgraded yet, a rolling deploy)
// keeps ingesting. InsertChunk's arbiter only sees live 'chunk' rows, so it
// does not wait for the repair: a changed-title ingest into the slot of a
// moved row inserts a fresh live chunk, and the restore of the moved row then
// meets it on uq_source_chunk. That must not fail the migration (and with it
// every boot): the fresh row is the slot's current version, so the moved row
// is archived instead, as for a slot that already had a live chunk.
//
// Two interleavings, both forced deterministically with an advisory lock that
// the test splices in front of the restore step of the shipped file:
//   - committed: the ingest commits after the moved rows were collected and
//     before the restore runs (through store.InsertChunk);
//   - in flight: the ingest's insert is still open when the restore reaches
//     the row, so the restore waits on it and sees its commit (a raw INSERT
//     in an open transaction — exactly the row InsertChunk writes, held open
//     to place its commit inside the restore).
//
//	go test -tags=integration -p 1 ./internal/store/ -run TestMigration169_ConcurrentIngest -count=1 -v
package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const m169RaceGate = 191191

// m169WaitFor polls q (a count query) until it returns > 0 or done fires.
func m169WaitFor(t *testing.T, pool *pgxpool.Pool, q string, done <-chan error) bool {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("migration finished before the expected wait (%s): err=%v", q, err)
		default:
		}
		var n int
		if err := pool.QueryRow(context.Background(), q).Scan(&n); err != nil {
			t.Fatalf("poll %s: %v", q, err)
		}
		if n > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", q)
	return false
}

func TestMigration169_ConcurrentIngestDoesNotFailTheRepair(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, 168)

	// Two moved rows, each alone in its slot.
	srcA := m169Source(t, pool, "/m169/race-committed.md")
	movedA := m169Chunk(t, pool, srcA, "m169 race committed old", "m169 race A v1")
	m169Damage(t, pool, movedA, "knowledge", "")
	srcB := m169Source(t, pool, "/m169/race-inflight.md")
	movedB := m169Chunk(t, pool, srcB, "m169 race inflight old", "m169 race B v1")
	m169Damage(t, pool, movedB, "knowledge", "")

	sql, err := migrations.FS.ReadFile(m169File)
	if err != nil {
		t.Fatalf("read embedded %s: %v", m169File, err)
	}
	const anchor = "    -- (b) restore"
	if strings.Count(string(sql), anchor) != 1 {
		t.Fatalf("%s: restore anchor %q not found exactly once", m169File, anchor)
	}
	gated := strings.Replace(string(sql), anchor,
		"    PERFORM pg_advisory_xact_lock(191191);\n"+anchor, 1)

	gate, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire gate conn: %v", err)
	}
	defer gate.Release()
	if _, err := gate.Exec(ctx, `SELECT pg_advisory_lock($1)`, m169RaceGate); err != nil {
		t.Fatalf("take gate: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- pgxdb.Write(ctx, pool, pgxdb.At("m169 gated apply"), func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, gated)
			return err
		})
	}()
	// The repair has collected the moved rows and archived nothing (each is
	// alone in its slot); it now waits in front of the restore.
	m169WaitFor(t, pool, `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid WHERE a.datname = current_database() AND l.locktype = 'advisory' AND l.objid = 191191 AND NOT l.granted`, done)

	// committed: a changed-title ingest of slot A lands as a fresh live chunk.
	freshA, err := m169Ingest(pool, srcA, "m169 race committed new", "m169 race A v2")
	if err != nil || freshA == movedA {
		t.Fatalf("concurrent ingest into slot A: id=%s err=%v, want a fresh row", freshA, err)
	}

	// in flight: the same row for slot B, its transaction left open.
	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin in-flight ingest: %v", err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	var freshB string
	if err := writer.QueryRow(ctx,
		`INSERT INTO context_blocks (category, tags, title, content, metadata, scope, source_id, chunk_index, lifecycle_state)
		 VALUES ('vault', '{}', 'm169 race inflight new', 'm169 race B v2', '{}', 'private', $1::uuid, 0, 'chunk')
		 RETURNING id::text`, srcB).Scan(&freshB); err != nil {
		t.Fatalf("in-flight ingest into slot B: %v", err)
	}

	if _, err := gate.Exec(ctx, `SELECT pg_advisory_unlock($1)`, m169RaceGate); err != nil {
		t.Fatalf("release gate: %v", err)
	}
	// The restore of slot B's moved row now waits for the open insert. On a
	// restore that already failed on slot A there is nothing to wait for.
	waiting := false
	deadline := time.Now().Add(10 * time.Second)
	var applyErr error
	finished := false
	for !waiting && !finished && time.Now().Before(deadline) {
		select {
		case applyErr = <-done:
			finished = true
		default:
			var n int
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
				  WHERE a.datname = current_database() AND l.locktype = 'transactionid' AND NOT l.granted`).Scan(&n); err != nil {
				t.Fatalf("poll transactionid wait: %v", err)
			}
			waiting = n > 0
			if !waiting {
				time.Sleep(20 * time.Millisecond)
			}
		}
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("commit in-flight ingest: %v", err)
	}
	if !finished {
		select {
		case applyErr = <-done:
		case <-time.After(15 * time.Second):
			t.Fatal("migration did not finish after the in-flight ingest committed")
		}
	}
	if applyErr != nil {
		t.Fatalf("migration 169 under a concurrent ingest: %v", applyErr)
	}
	if !waiting {
		t.Errorf("the restore never waited on the in-flight ingest — the in-flight interleaving was not exercised")
	}

	for _, c := range []struct{ name, moved, fresh, src, title string }{
		{"committed", movedA, freshA, srcA, "m169 race committed old"},
		{"in flight", movedB, freshB, srcB, "m169 race inflight old"},
	} {
		if got := m169Read(t, pool, c.moved); got.lifecycle != "chunk" || !got.archived || !strings.Contains(got.repair, `"archived": true`) {
			t.Errorf("%s: moved row %+v, want chunk, archived, repair record", c.name, got)
		}
		if got := m169Read(t, pool, c.fresh); got.lifecycle != "chunk" || got.archived || got.repair != "" {
			t.Errorf("%s: concurrent fresh chunk %+v, want an untouched live chunk", c.name, got)
		}
		// The slot takes the old title again, on the fresh row.
		if id, err := m169Ingest(pool, c.src, c.title, "m169 race v3"); err != nil || id != c.fresh {
			t.Errorf("%s: post-repair ingest of the old title: id=%s err=%v, want %s", c.name, id, err, c.fresh)
		}
	}
}
