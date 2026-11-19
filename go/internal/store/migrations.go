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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	return runMigrations(ctx, pool, migrations.FS, maxVersion, migrationLockWait)
}

// The migration lock (issue #398). Several ctxd instances on one database
// that boot at the same moment each run the loop of runMigrations. Without a
// lock both can find a version unrecorded and both run its file: the second
// one ends with 23505 (an index or table name the first one created) or with
// 55P03 (the file's lock_timeout, behind the first one's locks) and its boot
// stops — or, for a file that does not collide with itself, its effect lands
// twice. Each pending file therefore runs in a transaction that first takes
// this transaction-level advisory lock and then reads _migrations again: a
// runner that waited finds the version the other one recorded and skips the
// file; a runner whose predecessor failed inside the file finds it
// unrecorded and runs it itself. The lock ends with the file's transaction,
// on commit, on rollback and when its session dies.
//
// The key is the two-key form of the class 'ctx:migrations'. PostgreSQL keeps
// one-key (bigint) and two-key advisory locks apart (pg_locks.objsubid 1 and
// 2), so it cannot meet the one-key locks of the tree (block counts, issue
// sequences, model aliases, supersedes per scope, the embed_excluded
// triggers, the overview rebuild, the 94094094 of the baseline); among the
// two-key classes (ctx:distiller, ctx:runtime_index, ctx:embed_verify_run,
// ctx:guard-archive, ctx:supersedes-guess, ctx:write-budget) its first key
// hashtext('ctx:migrations') is one of its own. Only this runner takes it.
const migrationLockTrySQL = `SELECT pg_try_advisory_xact_lock(hashtext('ctx:migrations'), hashtext('runner'))`

// migrationLockHolderSQL names the backend that holds the lock, for the line
// a waiting runner logs. pg_locks shows a two-key lock as classid = first
// key, objid = second key, objsubid = 2, each key as its unsigned 32 bits.
// It runs on the pool, outside the waiting transaction, which therefore holds
// no catalog lock of its own while it waits.
const migrationLockHolderSQL = `SELECT l.pid FROM pg_locks l
 WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 2
   AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
   AND l.classid = hashtext('ctx:migrations')::oid AND l.objid = hashtext('runner')::oid
 LIMIT 1`

// migrationLockWait bounds how long a runner waits for the lock held by
// another one: the time one file of the other runner may take. It is fixed,
// sized on the longest inline path of the chain (issue #398, measured on a
// testcontainer of 2 CPUs): 115 rebuilds idx_embedding_hnsw inline below
// 500 000 rows, 8.3–8.8 s at 10 000 random 1024-dimensional vectors, 64–77 s
// at 50 000, 232–267 s at 100 000 (host under load) — extrapolated 23–31 min
// at its threshold, a lower bound: beyond about 97 000 vectors the graph no
// longer fits a maintenance_work_mem of 256 MB, and that slower part is not
// measured. The hour is twice that extrapolation and no upper bound for 115
// above about 100 000 rows. Every other inline path that docs/operations.md
// measures or extrapolates stays within a minute at the target scale (the
// longest: 158, 40–60 s for a 30 GB llm_log heap read cold; 170, 1.4 s per
// 1 000 000 blocks). A rebuild of 115 that takes longer, or a holder that
// stopped working inside its file, ends the waiting runner with
// ErrMigrationLockTimeout before it runs that file; a restarted instance
// waits again.
const migrationLockWait = time.Hour

// migrationLockWaitSQL waits for the lock, at most wait. lock_timeout bounds
// this one wait and is then set back to what the session had: a file that
// sets its own (SET LOCAL lock_timeout = '3s', the house pattern) does so
// after the wait and cannot cut it short, and a file that sets none runs
// under the session's value as it did before the lock existed. On a timeout
// the transaction is aborted, which reverts the setting anyway.
func migrationLockWaitSQL(wait time.Duration) string {
	return fmt.Sprintf(`DO $lock$
DECLARE
    v_prev text := current_setting('lock_timeout');
BEGIN
    PERFORM set_config('lock_timeout', '%d', true);
    PERFORM pg_advisory_xact_lock(hashtext('ctx:migrations'), hashtext('runner'));
    PERFORM set_config('lock_timeout', v_prev, true);
END
$lock$`, max(wait.Milliseconds(), 1))
}

// ErrMigrationLockTimeout reports a runner that waited the whole bound for the
// migration lock: another runner — another ctxd instance on this database —
// held it inside one migration file for that long.
var ErrMigrationLockTimeout = errors.New("timed out waiting for the migration lock")

// errAppliedMeanwhile is the commit-less exit of a file's transaction whose
// version another runner recorded while this one waited for the lock.
var errAppliedMeanwhile = errors.New("migration applied by another runner")

// runMigrations is RunMigrationsUpTo over the migration files of fsys, with
// lockWait as the bound of the wait for the migration lock.
func runMigrations(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, maxVersion int, lockWait time.Duration) error {
	if err := ensureMigrationsTable(ctx, pool); err != nil {
		return err
	}

	if err := checkBaselinePrecondition(ctx, pool, maxVersion); err != nil {
		return err
	}

	// Collect and sort .sql files from the embedded FS.
	entries, err := fs.ReadDir(fsys, ".")
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

	// Apply each pending migration in its own transaction. The check here,
	// outside any lock, is the steady-state path: a boot with nothing pending
	// reads the recorded versions and never touches the migration lock.
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

		sql, err := fs.ReadFile(fsys, m.filename)
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", m.filename, err)
		}

		err = applyMigration(ctx, pool, m.version, m.filename, sql, lockWait)
		if errors.Is(err, errAppliedMeanwhile) {
			slog.Info("migration applied by another instance", "version", m.version, "file", m.filename)
			continue
		}
		if err != nil {
			return err
		}

		slog.Info("migration applied", "version", m.version, "file", m.filename)
	}

	return nil
}

// ensureMigrationsTable creates the tracking table. CREATE TABLE IF NOT EXISTS
// does not hold against a creation in flight: on a fresh database where two
// runners start together, the second one does not see the first one's
// uncommitted table, inserts the same catalog rows, waits for the first
// transaction and meets them on its commit — 23505 on
// pg_type_typname_nsp_index, or 42P07/42710 when the commit lands between the
// IF NOT EXISTS check and the catalog insert. Each of these means the table
// stands now, so the statement runs once more and finds it.
func ensureMigrationsTable(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, migrationsTable)
	if err != nil && createdBesideUs(err) {
		_, err = pool.Exec(ctx, migrationsTable)
	}
	if err != nil {
		return fmt.Errorf("creating _migrations table: %w", err)
	}
	return nil
}

// createdBesideUs reports the errors a CREATE TABLE IF NOT EXISTS raises when
// another session created the same table at the same moment.
func createdBesideUs(err error) bool {
	switch sqlState(err) {
	case "23505", "42P07", "42710": // unique_violation, duplicate_table, duplicate_object
		return true
	}
	return false
}

// sqlState is the SQLSTATE of a server error, "" for anything else.
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// applyMigration runs one file in its own transaction under the migration
// lock. It returns errAppliedMeanwhile, with nothing done, when the version
// was recorded by another runner while this one waited for the lock.
//
// One migration, one transaction, one commit exit — the shape pgxdb.WriteOpts
// brackets. The stage labels carry the version exactly as the hand-written
// fmt.Errorf pair did (the helper appends the ": %w"); the body's own errors
// travel back unwrapped. The deferred rollback runs detached from ctx, which
// buys nothing for a SIGTERM mid-statement (cmd/ctxd/main.go:575
// NotifyContext, :612): pgx's deadline watcher has closed the connection
// before the rollback runs — and the process exits right after anyway.
//
// READ COMMITTED explicitly: the check under the lock must read a snapshot
// taken after the lock was granted. Under REPEATABLE READ or SERIALIZABLE (a
// database or role default) the transaction's snapshot would be the one of
// its first statement, taken before the wait, and the check would miss the
// version the other runner committed meanwhile.
func applyMigration(ctx context.Context, pool *pgxpool.Pool, version int, filename string, sql []byte, lockWait time.Duration) error {
	return pgxdb.WriteOpts(ctx, pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, pgxdb.Stages{
		Begin:  fmt.Sprintf("beginning transaction for migration %d", version),
		Commit: fmt.Sprintf("committing migration %d", version),
	}, func(tx pgx.Tx) error {
		if err := takeMigrationLock(ctx, pool, tx, version, filename, lockWait); err != nil {
			return err
		}
		// A statement of its own, after the lock — see above.
		var recorded bool
		if err := tx.QueryRow(ctx,
			"SELECT EXISTS(SELECT 1 FROM _migrations WHERE version = $1)",
			version,
		).Scan(&recorded); err != nil {
			return fmt.Errorf("checking migration %d under the migration lock: %w", version, err)
		}
		if recorded {
			return errAppliedMeanwhile
		}

		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("executing migration %d (%s): %w", version, filename, err)
		}

		// ON CONFLICT DO NOTHING tolerates migrations that record
		// themselves in the file body (M031+ pattern). Without it, the
		// file's INSERT and this INSERT collide on the version primary key.
		sum := sha256.Sum256(sql)
		if _, err := tx.Exec(ctx,
			"INSERT INTO _migrations (version, filename, checksum) VALUES ($1, $2, $3) ON CONFLICT (version) DO NOTHING",
			version, filename, hex.EncodeToString(sum[:]),
		); err != nil {
			return fmt.Errorf("recording migration %d: %w", version, err)
		}
		return nil
	})
}

// takeMigrationLock takes the migration lock in tx. Free, it costs one
// statement; held by another runner, the runner logs who holds it (looked up
// on pool) and waits for it, at most wait.
func takeMigrationLock(ctx context.Context, pool *pgxpool.Pool, tx pgx.Tx, version int, filename string, wait time.Duration) error {
	var got bool
	if err := tx.QueryRow(ctx, migrationLockTrySQL).Scan(&got); err != nil {
		return fmt.Errorf("taking the migration lock for migration %d: %w", version, err)
	}
	if got {
		return nil
	}

	var holder int
	if err := pool.QueryRow(ctx, migrationLockHolderSQL).Scan(&holder); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("looking up the holder of the migration lock for migration %d: %w", version, err)
	}
	slog.Info("migration lock held by another instance, waiting",
		"version", version, "file", filename, "holder_pid", holder, "max_wait", wait)

	if _, err := tx.Exec(ctx, migrationLockWaitSQL(wait)); err != nil {
		if sqlState(err) == "55P03" { // lock_not_available: the bound ran out
			return fmt.Errorf("%w: migration %d (%s): another ctxd instance on this database (backend pid %d) "+
				"has held it inside one migration file for longer than %s; this instance stops before it runs this file, "+
				"its next start waits again (docs/operations.md, \"Several instances booting together\")",
				ErrMigrationLockTimeout, version, filename, holder, wait)
		}
		return fmt.Errorf("waiting for the migration lock for migration %d: %w", version, err)
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
