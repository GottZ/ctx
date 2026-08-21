//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// expectedRow is the identity a _migrations row should carry for one version.
type expectedRow struct {
	filename string
	checksum string
}

// expectedMigrationRows is the oracle for "every row got stamped, with the
// right value". It has two halves since the fold: versions 001-113 live
// inside 113_baseline.sql and are expected under the filename and checksum
// they had as files (the baseline's ledger writes exactly those), every
// version above the fold line is expected under its own file's sha256.
//
// The baseline file itself is deliberately NOT an entry: its version is
// covered by the folded half, which is why a fresh database and one upgraded
// through v4.38.0 end up with identical _migrations content.
func expectedMigrationRows(t *testing.T) map[int]expectedRow {
	t.Helper()
	want := map[int]expectedRow{}
	for _, f := range migrations.Folded() {
		want[f.Version] = expectedRow{filename: f.Filename, checksum: f.Checksum}
	}

	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("reading embedded migrations: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") || name == migrations.BaselineFile {
			continue
		}
		v, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			continue
		}
		if _, dup := want[v]; dup {
			t.Fatalf("version %d ships both as %s and inside the baseline", v, name)
		}
		want[v] = expectedRow{filename: name, checksum: sha256Hex(t, name)}
	}
	return want
}

// sha256Hex returns the lowercase hex sha256 of the named embedded file, the
// same computation BackfillChecksums and RunMigrations perform.
func sha256Hex(t *testing.T, filename string) string {
	t.Helper()
	sql, err := migrations.FS.ReadFile(filename)
	if err != nil {
		t.Fatalf("read embedded %s: %v", filename, err)
	}
	sum := sha256.Sum256(sql)
	return hex.EncodeToString(sum[:])
}

// TestMigrationsChecksum pins W03-1 (migration 108): every _migrations row
// ends up with a 64-hex-char sha256 checksum matching the embedded file's
// present content, the backfill is idempotent, and it never stamps a version
// with no corresponding embedded file (self-record rows from removed/
// hypothetical migrations stay NULL by design).
func TestMigrationsChecksum(t *testing.T) {
	pool := testdb.SetupTestDB(t) // already runs RunMigrations + BackfillChecksums
	ctx := context.Background()

	wantRows := expectedMigrationRows(t)
	wantCount := len(wantRows)

	// (1) 0 NULL checksums, one row per chain version (files above the fold
	// line plus the versions folded into the baseline), every checksum 64 hex
	// chars and matching the sha256 of the SQL that defined that version.
	rows, err := pool.Query(ctx, `SELECT version, filename, checksum FROM _migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("select _migrations: %v", err)
	}
	gotCount := 0
	for rows.Next() {
		var version int
		var filename string
		var checksum *string
		if err := rows.Scan(&version, &filename, &checksum); err != nil {
			rows.Close()
			t.Fatalf("scan row: %v", err)
		}
		gotCount++
		if checksum == nil {
			t.Errorf("version %d (%s): checksum is NULL, want stamped", version, filename)
			continue
		}
		if len(*checksum) != 64 {
			t.Errorf("version %d (%s): checksum length = %d, want 64", version, filename, len(*checksum))
		}
		want, known := wantRows[version]
		if !known {
			t.Errorf("version %d (%s): recorded but not part of the chain", version, filename)
			continue
		}
		if filename != want.filename {
			t.Errorf("version %d: filename = %s, want %s", version, filename, want.filename)
		}
		if *checksum != want.checksum {
			t.Errorf("version %d (%s): checksum = %s, want %s", version, filename, *checksum, want.checksum)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate _migrations: %v", err)
	}
	if gotCount != wantCount {
		t.Errorf("_migrations row count = %d, want %d (folded versions + embedded .sql files above the fold)", gotCount, wantCount)
	}

	var nullCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM _migrations WHERE checksum IS NULL`).Scan(&nullCount); err != nil {
		t.Fatalf("null checksum count: %v", err)
	}
	if nullCount != 0 {
		t.Errorf("NULL checksum count = %d, want 0", nullCount)
	}

	// (2) Idempotency: a second RunMigrations + BackfillChecksums pass changes
	// nothing (same row count, no errors).
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("second RunMigrations: %v", err)
	}
	if err := store.BackfillChecksums(ctx, pool); err != nil {
		t.Fatalf("second BackfillChecksums: %v", err)
	}
	var afterCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM _migrations`).Scan(&afterCount); err != nil {
		t.Fatalf("count after second pass: %v", err)
	}
	if afterCount != wantCount {
		t.Errorf("_migrations row count after idempotent re-run = %d, want %d", afterCount, wantCount)
	}

	// (3a) Self-record simulation: a row for a version with NO embedded file
	// (99999) must stay NULL after BackfillChecksums — the backfill only
	// stamps versions it can find in migrations.FS.
	const fakeVersion = 99999
	if _, err := pool.Exec(ctx,
		`INSERT INTO _migrations (version, filename) VALUES ($1, $2)`,
		fakeVersion, "test_self_record.sql",
	); err != nil {
		t.Fatalf("insert fake self-record row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM _migrations WHERE version = $1`, fakeVersion)
	})

	if err := store.BackfillChecksums(ctx, pool); err != nil {
		t.Fatalf("BackfillChecksums with fake row present: %v", err)
	}

	var fakeChecksum *string
	if err := pool.QueryRow(ctx,
		`SELECT checksum FROM _migrations WHERE version = $1`, fakeVersion,
	).Scan(&fakeChecksum); err != nil {
		t.Fatalf("select fake row checksum: %v", err)
	}
	if fakeChecksum != nil {
		t.Errorf("fake row (no embedded file) checksum = %q, want NULL", *fakeChecksum)
	}

	// (3b) A pre-existing row forced back to NULL gets re-filled by the next
	// backfill. Both halves of the chain have to survive that: version 1 is
	// folded (its checksum comes from migrations.Folded(), its file is gone)
	// and version 114 is an ordinary embedded file. Before the fold only the
	// second half existed; a folded row that could not be re-filled would
	// read as permanent drift to the schema contract.
	for _, version := range []int{1, 114} {
		if _, err := pool.Exec(ctx, `UPDATE _migrations SET checksum = NULL WHERE version = $1`, version); err != nil {
			t.Fatalf("force version %d checksum to NULL: %v", version, err)
		}
		if err := store.BackfillChecksums(ctx, pool); err != nil {
			t.Fatalf("BackfillChecksums after forcing version %d NULL: %v", version, err)
		}

		var filename string
		var checksum *string
		if err := pool.QueryRow(ctx,
			`SELECT filename, checksum FROM _migrations WHERE version = $1`, version,
		).Scan(&filename, &checksum); err != nil {
			t.Fatalf("select version %d after re-backfill: %v", version, err)
		}
		if checksum == nil {
			t.Fatalf("version %d checksum still NULL after re-backfill", version)
		}
		if want := wantRows[version]; filename != want.filename || *checksum != want.checksum {
			t.Errorf("version %d re-backfilled to (%s, %s), want (%s, %s)",
				version, filename, *checksum, want.filename, want.checksum)
		}
	}
}

// queryCounter counts every statement a pool sends, via pgx's query tracer.
// It is the only honest instrument for this property: pg_stat_database's
// counters are flushed on a timer (PGSTAT_MIN_INTERVAL) and would lag a
// sub-second test, and BackfillChecksums takes a *pgxpool.Pool, so there is
// nothing else to intercept.
type queryCounter struct{ n atomic.Int64 }

func (c *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// tracedPool opens a second pool onto the same test database, with a counter
// on every statement.
func tracedPool(t *testing.T, dsn string) (*pgxpool.Pool, *queryCounter) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	counter := &queryCounter{}
	cfg.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open traced pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, counter
}

// TestBackfillChecksumsSkipsWhenNothingIsPending pins the boot cost of the
// backfill. Both of its loops are keyed on `checksum IS NULL`, so on any
// database that has completed one boot every single one of their ~130 UPDATEs
// matches zero rows — the function was ~130 round trips and ~130 write-path
// statements per boot to change nothing. The EXISTS probe makes the ordinary
// case exactly one statement.
//
// The second half is what keeps the optimisation honest: the probe is
// re-evaluated per call, so a row that goes back to NULL is still repaired.
// A guard cached across calls, or hoisted to a package-level "already done",
// would pass the first subtest and fail this one.
//
// Mutation probe: delete the EXISTS guard and the no-op subtest goes red with
// a statement count in the hundreds.
func TestBackfillChecksumsSkipsWhenNothingIsPending(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t) // fully migrated AND backfilled
	ctx := context.Background()

	traced, counter := tracedPool(t, dsn)

	t.Run("no NULL checksums costs one statement", func(t *testing.T) {
		var pending int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM _migrations WHERE checksum IS NULL`).Scan(&pending); err != nil {
			t.Fatalf("precondition: %v", err)
		}
		if pending != 0 {
			t.Fatalf("precondition: %d NULL checksums, want 0 — the setup did not backfill", pending)
		}

		counter.n.Store(0)
		if err := store.BackfillChecksums(ctx, traced); err != nil {
			t.Fatalf("BackfillChecksums: %v", err)
		}
		if got := counter.n.Load(); got != 1 {
			t.Errorf("statements sent = %d, want 1 (the EXISTS probe) — a boot with nothing to stamp "+
				"must not send one UPDATE per chain version", got)
		}
	})

	t.Run("a row back at NULL is still repaired", func(t *testing.T) {
		const version = 114 // an ordinary file above the fold line
		if _, err := pool.Exec(ctx, `UPDATE _migrations SET checksum = NULL WHERE version = $1`, version); err != nil {
			t.Fatalf("force version %d to NULL: %v", version, err)
		}

		counter.n.Store(0)
		if err := store.BackfillChecksums(ctx, traced); err != nil {
			t.Fatalf("BackfillChecksums after forcing NULL: %v", err)
		}
		if got := counter.n.Load(); got <= 1 {
			t.Errorf("statements sent = %d, want the loops to run — the guard cached its verdict", got)
		}

		var checksum *string
		if err := pool.QueryRow(ctx, `SELECT checksum FROM _migrations WHERE version = $1`, version).Scan(&checksum); err != nil {
			t.Fatalf("read back version %d: %v", version, err)
		}
		if checksum == nil {
			t.Fatal("version 114 is still NULL — the guard skipped a database that had work to do")
		}
		if want := sha256Hex(t, "114_embed_migrations.sql"); *checksum != want {
			t.Errorf("version 114 re-stamped to %s, want %s", *checksum, want)
		}
	})
}
