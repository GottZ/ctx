package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/migrations"
)

// checksum is carried in the CREATE from the start (not added via ALTER
// here) because a fresh DB applies 001-107 before 108 ever runs (since the
// v5 fold: their sections, inside 113_baseline.sql, in that order) — the
// record-INSERT below writes checksum from migration 001 onward, so the
// column must exist before the first INSERT. Migration 108 backfills it
// idempotently (ADD COLUMN IF NOT EXISTS) for pre-existing databases; both
// paths converge on the same schema.
const migrationsTable = `
CREATE TABLE IF NOT EXISTS _migrations (
    version     INT          PRIMARY KEY,
    filename    TEXT         NOT NULL,
    applied_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    checksum    CHAR(64)
);`

// RunMigrations applies all pending SQL migrations from the embedded FS.
// Each migration runs in its own transaction. Already-applied migrations
// (tracked by version number in _migrations) are skipped.
//
// Since the v5 fold the chain starts with 113_baseline.sql, which carries
// the effect of migrations 001-113 and records every one of them in
// _migrations under its original filename and checksum. Nothing about the
// loop below changes: a fresh database has no version 113 and applies the
// baseline, a database upgraded through v4.38.0 has it and skips it exactly
// like any other applied migration. Only the third case — a database that
// stopped inside the folded range — needs a word of its own, and that is
// checkBaselinePrecondition.
func RunMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	return RunMigrationsUpTo(ctx, pool, math.MaxInt)
}

// RunMigrationsUpTo applies pending SQL migrations from the embedded FS
// whose version is <= maxVersion, in version order (each in its own
// transaction, already-applied versions skipped exactly like RunMigrations).
// RunMigrations is simply the maxVersion=unbounded case.
//
// The capped variant exists for integration tests that need to reproduce a
// database at a specific point in the migration chain rather than at HEAD —
// most concretely, Evokoa-Clean-Room design/03 §7 W03-6 Gate 1, which
// simulates the exact pre-115 Prod state (chain applied through 114, HNSW
// index rebuilt out-of-band to ef_construction=128 the way Session 3 really
// did it) that the historic ef_construction drift Migration 115 reconciles
// was never visible against. No production caller needs this — main.go's
// boot path and every other test keep calling RunMigrations.
func RunMigrationsUpTo(ctx context.Context, pool *pgxpool.Pool, maxVersion int) error {
	// Ensure the tracking table exists (idempotent).
	if _, err := pool.Exec(ctx, migrationsTable); err != nil {
		return fmt.Errorf("creating _migrations table: %w", err)
	}

	if err := checkBaselinePrecondition(ctx, pool, maxVersion); err != nil {
		return err
	}

	// Collect and sort .sql files from the embedded FS.
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("reading embedded migrations: %w", err)
	}

	type migration struct {
		version  int
		filename string
	}
	var migs []migration

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		// Extract version from prefix: "001_initial.sql" -> 1
		parts := strings.SplitN(name, "_", 2)
		if len(parts) < 2 {
			continue
		}
		v, err := strconv.Atoi(parts[0])
		if err != nil {
			slog.Warn("skipping migration file with non-numeric prefix", "file", name)
			continue
		}
		if v > maxVersion {
			continue
		}
		migs = append(migs, migration{version: v, filename: name})
	}

	sort.Slice(migs, func(i, j int) bool {
		return migs[i].version < migs[j].version
	})

	// Apply each pending migration in its own transaction.
	for _, m := range migs {
		var exists bool
		err := pool.QueryRow(ctx,
			"SELECT EXISTS(SELECT 1 FROM _migrations WHERE version = $1)",
			m.version,
		).Scan(&exists)
		if err != nil {
			return fmt.Errorf("checking migration %d: %w", m.version, err)
		}
		if exists {
			slog.Debug("migration already applied", "version", m.version, "file", m.filename)
			continue
		}

		sql, err := fs.ReadFile(migrations.FS, m.filename)
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", m.filename, err)
		}

		// One migration, one transaction, one commit exit — the shape
		// pgxdb.Write brackets. The stage labels carry the version exactly as
		// the hand-written fmt.Errorf pair did (the helper appends the ": %w");
		// the body's own two errors travel back unwrapped. The rollback that
		// stood before each of those returns is the bracket's deferred one now,
		// which also covers what the explicit calls never did (failed commit,
		// panic). It runs detached from ctx, which buys nothing for a SIGTERM
		// mid-statement (cmd/ctxd/main.go:353 NotifyContext, :370): pgx's deadline
		// watcher has closed the connection before the rollback runs, on the old
		// form and this one alike — and the process exits right after anyway.
		if err := pgxdb.Write(ctx, pool, pgxdb.Stages{
			Begin:  fmt.Sprintf("beginning transaction for migration %d", m.version),
			Commit: fmt.Sprintf("committing migration %d", m.version),
		}, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("executing migration %d (%s): %w", m.version, m.filename, err)
			}

			// ON CONFLICT DO NOTHING tolerates migrations that record
			// themselves in the file body (M031+ pattern). Without it, the
			// file's INSERT and this INSERT collide on the version primary key.
			sum := sha256.Sum256(sql)
			if _, err := tx.Exec(ctx,
				"INSERT INTO _migrations (version, filename, checksum) VALUES ($1, $2, $3) ON CONFLICT (version) DO NOTHING",
				m.version, m.filename, hex.EncodeToString(sum[:]),
			); err != nil {
				return fmt.Errorf("recording migration %d: %w", m.version, err)
			}
			return nil
		}); err != nil {
			return err
		}

		slog.Info("migration applied", "version", m.version, "file", m.filename)
	}

	return nil
}

// ErrPreBaselineDatabase reports a database that carries migration history
// but not the version the v5 baseline folds up to. Such a database is below
// the supported upgrade floor: the SQL that would carry it from where it
// stands to the baseline no longer ships as individual files.
var ErrPreBaselineDatabase = errors.New("database predates the migration baseline")

// checkBaselinePrecondition enforces the one contract the fold introduces:
// the baseline replaces migrations 001-113, so it can only be applied to a
// database that has none of them, and it may only be SKIPPED by a database
// that has all of them. The runner's per-version bookkeeping decides both
// cases on its own — this check exists for the third case, a database that
// stopped somewhere inside the folded range. Without it that database would
// either re-run the baseline over existing objects or run 114+ against a
// half-built schema; with it, it gets told what to do instead.
//
// Deliberately keyed on _migrations rows, not on schema shape: a database
// with zero rows is a fresh install by the same definition the runner has
// always used, and everything else has to be able to name where it stands.
func checkBaselinePrecondition(ctx context.Context, pool *pgxpool.Pool, maxVersion int) error {
	if maxVersion < migrations.BaselineVersion {
		// The caller asked for a prefix of the chain that does not reach the
		// baseline at all (capped test setups). Nothing to enforce.
		return nil
	}

	// One aggregate, one round trip. All three verdicts come from the same
	// scan of the same table snapshot instead of from two sequential queries
	// — the boot pays one query where it used to pay two, and no state can
	// change between the row count and the baseline probe.
	//
	// count(*) stays the fresh-install discriminator rather than
	// max(version) = 0: "zero rows" is the definition the runner has always
	// used (see above), and folding it into the max would silently reclassify
	// a table whose only row is version 0 from pre-baseline to fresh.
	var applied, maxApplied int
	var hasBaseline bool
	if err := pool.QueryRow(ctx,
		"SELECT count(*), COALESCE(max(version), 0), COALESCE(bool_or(version = $1), false) FROM _migrations",
		migrations.BaselineVersion,
	).Scan(&applied, &maxApplied, &hasBaseline); err != nil {
		return fmt.Errorf("reading migration history: %w", err)
	}
	if applied == 0 {
		return nil // fresh install — the baseline is the whole history
	}
	if hasBaseline {
		return nil
	}

	return fmt.Errorf("%w: highest applied migration is %d, baseline folds 001-%d — "+
		"upgrade this database with the last v4.x release (v4.38.0) first, then come back "+
		"(docs/operations.md, \"The v4.x hop is mandatory\")",
		ErrPreBaselineDatabase, maxApplied, migrations.BaselineVersion)
}

// BackfillChecksums stamps sha256(hex) checksums onto _migrations rows that
// still carry NULL — pre-108 applies (the column did not exist yet) and
// M031+ self-record rows (the file's own INSERT predates this package's
// checksum write). It attests the file AS IT EXISTS TODAY in the embedded
// FS, not the historic apply (W11 boundary): a row's checksum can outlive
// the SQL that originally produced it if the migration file is later edited
// (it should not be, but this function cannot see history, only the present
// binary). Idempotent — after the first boot, every UPDATE affects 0 rows.
//
// Rows for folded versions (001-113) are outside its reach by construction:
// their files are gone, so it never sees them. They get their checksums from
// the baseline's own ledger instead, which writes the same values the files
// hashed to before the fold — migrations.Folded() carries them, and the
// schema contract checks them from there.
//
// The steady-state cost is one SELECT. Both loops below are keyed on
// `checksum IS NULL`, so on a database that has completed one boot every one
// of their ~130 UPDATEs matches zero rows — 130 round trips, 130 write-path
// statements and 130 WAL-eligible transactions on EVERY boot, to do nothing.
// The EXISTS probe collapses that to a single indexed read; the loops keep
// their own IS NULL predicates, which stay the actual correctness gate (the
// probe only decides whether there is anything to do at all, and it is
// re-evaluated on every call — a row forced back to NULL still gets repaired
// by the next one).
func BackfillChecksums(ctx context.Context, pool *pgxpool.Pool) error {
	var pending bool
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM _migrations WHERE checksum IS NULL)",
	).Scan(&pending); err != nil {
		return fmt.Errorf("probing for unstamped migration rows: %w", err)
	}
	if !pending {
		return nil
	}

	// Folded versions first: their files are gone, so the checksum has to
	// come from the folded chain. A fresh install already got these from the
	// baseline's ledger; this is the repair path for a database that reached
	// v4.38.0 but never completed a boot there, whose rows would otherwise
	// stay NULL forever and read as drift to the schema contract.
	for _, f := range migrations.Folded() {
		if _, err := pool.Exec(ctx,
			"UPDATE _migrations SET checksum = $1 WHERE version = $2 AND filename = $3 AND checksum IS NULL",
			f.Checksum, f.Version, f.Filename,
		); err != nil {
			return fmt.Errorf("backfilling checksum for folded migration %d: %w", f.Version, err)
		}
	}

	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("reading embedded migrations: %w", err)
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") || name == migrations.BaselineFile {
			// The baseline shares its version with the last folded migration
			// and the row belongs to that one — stamping the baseline's own
			// hash onto it would invent a third identity for version 113.
			continue
		}
		parts := strings.SplitN(name, "_", 2)
		if len(parts) < 2 {
			continue
		}
		v, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}

		sql, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", name, err)
		}
		sum := sha256.Sum256(sql)

		if _, err := pool.Exec(ctx,
			"UPDATE _migrations SET checksum = $1 WHERE version = $2 AND checksum IS NULL",
			hex.EncodeToString(sum[:]), v,
		); err != nil {
			return fmt.Errorf("backfilling checksum for migration %d: %w", v, err)
		}
	}

	return nil
}
