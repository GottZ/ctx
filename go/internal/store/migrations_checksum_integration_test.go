//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// embeddedMigrationCount counts the .sql files the runner and the backfill
// both iterate — the oracle for "every row got stamped".
func embeddedMigrationCount(t *testing.T) int {
	t.Helper()
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("reading embedded migrations: %v", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			n++
		}
	}
	return n
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

	wantCount := embeddedMigrationCount(t)

	// (1) 0 NULL checksums, row count == embedded file count, every checksum
	// is 64 hex chars and matches its file's sha256.
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
		want := sha256Hex(t, filename)
		if *checksum != want {
			t.Errorf("version %d (%s): checksum = %s, want %s", version, filename, *checksum, want)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate _migrations: %v", err)
	}
	if gotCount != wantCount {
		t.Errorf("_migrations row count = %d, want %d (embedded .sql files)", gotCount, wantCount)
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
	// backfill with the checksum of its embedded file (version 1).
	if _, err := pool.Exec(ctx, `UPDATE _migrations SET checksum = NULL WHERE version = 1`); err != nil {
		t.Fatalf("force version 1 checksum to NULL: %v", err)
	}
	if err := store.BackfillChecksums(ctx, pool); err != nil {
		t.Fatalf("BackfillChecksums after forcing version 1 NULL: %v", err)
	}

	var filenameV1 string
	var checksumV1 *string
	if err := pool.QueryRow(ctx,
		`SELECT filename, checksum FROM _migrations WHERE version = 1`,
	).Scan(&filenameV1, &checksumV1); err != nil {
		t.Fatalf("select version 1 after re-backfill: %v", err)
	}
	if checksumV1 == nil {
		t.Fatal("version 1 checksum still NULL after re-backfill")
	}
	if want := sha256Hex(t, filenameV1); *checksumV1 != want {
		t.Errorf("version 1 re-backfilled checksum = %s, want %s", *checksumV1, want)
	}
}
