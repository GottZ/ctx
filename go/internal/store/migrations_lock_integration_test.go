//go:build integration

package store_test

import (
	"context"
	"errors"
	"io/fs"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// Issue #398: two ctxd instances that boot at the same moment against one
// database. Every test here starts two REAL runners, each on a pool of its own
// (two instances), against one database, and holds the first one inside a
// migration file while the second one arrives — the moment the issue measured:
// both past the "is this version recorded?" check before either commits.

const (
	// The gate is a two-key advisory lock the test holds on a connection of
	// its own; a probe file waits on it, so the test decides how long the
	// first runner stays inside the file.
	gateLockSQL   = `SELECT pg_advisory_lock(398, 1)`
	gateUnlockSQL = `SELECT pg_advisory_unlock(398, 1)`
	gateInFile    = "SELECT pg_advisory_xact_lock(398, 1);\n"

	probeTableSQL = `CREATE TABLE lock_probe (
	    id           bigserial PRIMARY KEY,
	    who          text NOT NULL,
	    lock_timeout text NOT NULL,
	    isolation    text NOT NULL)`
	// The row a probe file writes: which runner ran it, and the lock_timeout
	// and isolation level the file itself saw.
	probeInsert = "INSERT INTO lock_probe (who, lock_timeout, isolation) VALUES " +
		"(current_setting('application_name'), current_setting('lock_timeout'), current_setting('transaction_isolation'));\n"

	// Bounds of the test's own waits, far above what the steps take.
	reachWithin  = 5 * time.Second
	finishWithin = 60 * time.Second
)

// instancePool opens a pool of its own on dsn, the way a second ctxd opens
// its own: application_name names the runner (the probe files read it), and
// params are further session settings of that instance.
func instancePool(t *testing.T, dsn, name string, params map[string]string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = name
	for k, v := range params {
		cfg.ConnConfig.RuntimeParams[k] = v
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping %s: %v", name, err)
	}
	return pool
}

type runOutcome struct {
	err   error
	ended time.Time
}

// startRunner runs one runner in the background and hands back its outcome.
func startRunner(run func(ctx context.Context) error) <-chan runOutcome {
	ch := make(chan runOutcome, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), finishWithin)
		defer cancel()
		err := run(ctx)
		ch <- runOutcome{err: err, ended: time.Now()}
	}()
	return ch
}

func await(t *testing.T, name string, ch <-chan runOutcome) runOutcome {
	t.Helper()
	select {
	case out := <-ch:
		return out
	case <-time.After(finishWithin + 5*time.Second):
		t.Fatalf("%s did not return", name)
		return runOutcome{}
	}
}

// waitForWaiters waits until at least n sessions of this database, other than
// the asking one, wait for a lock — a relation, an advisory lock or the end of
// another transaction. It reports whether they did within the bound.
func waitForWaiters(t *testing.T, pool *pgxpool.Pool, n int, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var got int
		if err := pool.QueryRow(context.Background(), `
			SELECT count(DISTINCT l.pid)
			  FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
			 WHERE NOT l.granted AND a.datname = current_database() AND l.pid <> pg_backend_pid()`).Scan(&got); err != nil {
			t.Fatalf("count waiters: %v", err)
		}
		if got >= n {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// holdGate takes the gate on a connection of its own; release lets every
// probe file that waits on it go on.
func holdGate(t *testing.T, pool *pgxpool.Pool) (release func()) {
	t.Helper()
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire gate connection: %v", err)
	}
	if _, err := conn.Exec(context.Background(), gateLockSQL); err != nil {
		conn.Release()
		t.Fatalf("take gate: %v", err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			if _, err := conn.Exec(context.Background(), gateUnlockSQL); err != nil {
				t.Errorf("release gate: %v", err)
			}
			conn.Release()
		})
	}
	t.Cleanup(release)
	return release
}

type probeRow struct{ who, lockTimeout, isolation string }

func probeRows(t *testing.T, pool *pgxpool.Pool) []probeRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT who, lock_timeout, isolation FROM lock_probe ORDER BY id`)
	if err != nil {
		t.Fatalf("read lock_probe: %v", err)
	}
	defer rows.Close()
	var out []probeRow
	for rows.Next() {
		var r probeRow
		if err := rows.Scan(&r.who, &r.lockTimeout, &r.isolation); err != nil {
			t.Fatalf("scan lock_probe: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read lock_probe: %v", err)
	}
	return out
}

func versionRecorded(t *testing.T, pool *pgxpool.Pool, version int) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM _migrations WHERE version = $1)`, version).Scan(&ok); err != nil {
		t.Fatalf("read _migrations: %v", err)
	}
	return ok
}

func runFiles(pool *pgxpool.Pool, files fs.FS, wait time.Duration) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		return store.RunMigrationFilesForTest(ctx, pool, files, wait)
	}
}

// TestMigrationRunnersBootingTogetherBuildMigration184Once is the measurement
// of the issue as a test: both runners at state 183, both inside 184 before
// either commits. The blocker holds ROW EXCLUSIVE on context_access_tokens, so
// the file's CREATE INDEX (SHARE) waits for it while its name check before it
// does not; it is released within the file's lock_timeout of 3 s. Without a
// lock the second runner builds beside the first and ends with 23505 on
// pg_class_relname_nsp_index; with it, it waits and skips the file.
func TestMigrationRunnersBootingTogetherBuildMigration184Once(t *testing.T) {
	admin, dsn := testdb.SetupTestDBUpToWithDSN(t, 183)
	a := instancePool(t, dsn, "runner-a", nil)
	b := instancePool(t, dsn, "runner-b", nil)
	ctx := context.Background()

	blocker, err := admin.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `LOCK TABLE context_access_tokens IN ROW EXCLUSIVE MODE`); err != nil {
		t.Fatalf("blocker lock: %v", err)
	}

	resA := startRunner(func(ctx context.Context) error { return store.RunMigrationsUpTo(ctx, a, 184) })
	if !waitForWaiters(t, admin, 1, reachWithin) {
		t.Fatal("runner a never reached the index build of 184")
	}
	resB := startRunner(func(ctx context.Context) error { return store.RunMigrationsUpTo(ctx, b, 184) })
	if !waitForWaiters(t, admin, 2, reachWithin) {
		t.Fatal("runner b never waited beside runner a")
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("release blocker: %v", err)
	}

	outA, outB := await(t, "runner a", resA), await(t, "runner b", resB)
	if outA.err != nil {
		t.Fatalf("runner a: %v", outA.err)
	}
	if outB.err != nil {
		t.Errorf("runner b, booting beside runner a, failed: %v", outB.err)
	}
	var built int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_class
		WHERE relkind = 'i' AND relname IN ('idx_access_tokens_parent', 'idx_web_sessions_access_token')`).Scan(&built); err != nil {
		t.Fatalf("count indexes: %v", err)
	}
	if built != 2 {
		t.Errorf("indexes of 184 after both runners: %d, want 2", built)
	}
	if !versionRecorded(t, admin, 184) {
		t.Error("184 is not recorded after both runners")
	}
}

// TestMigrationRunnersBootingTogetherRunAFileOnce: a file whose second run
// would not fail but double its effect — an INSERT. The first runner waits
// inside it on the gate, the second arrives; once the gate opens, exactly one
// row may exist.
//
// "past the files' 3 s": the first runner stays inside the file 3.5 s after
// the second has started to wait, and the second runner's own session carries
// lock_timeout = 1s — its wait for the first runner is bounded by the runner's
// bound alone, not by a lock_timeout of the session or of a file.
//
// "repeatable read": both sessions default to REPEATABLE READ. The second
// runner's check under the lock must see the first runner's commit, which a
// snapshot taken before the lock would not.
func TestMigrationRunnersBootingTogetherRunAFileOnce(t *testing.T) {
	cases := []struct {
		name             string
		paramsA, paramsB map[string]string
		hold             time.Duration
	}{
		{name: "past the files' 3 s", paramsB: map[string]string{"lock_timeout": "1s"}, hold: 3500 * time.Millisecond},
		{
			name:    "repeatable read",
			paramsA: map[string]string{"default_transaction_isolation": "repeatable read"},
			paramsB: map[string]string{"default_transaction_isolation": "repeatable read"},
		},
	}
	files := fstest.MapFS{"9001_insert_once.sql": {Data: []byte(gateInFile + "SET LOCAL lock_timeout = '3s';\n" + probeInsert)}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			admin, dsn := testdb.SetupTestDBWithDSN(t)
			if _, err := admin.Exec(context.Background(), probeTableSQL); err != nil {
				t.Fatalf("probe table: %v", err)
			}
			a := instancePool(t, dsn, "runner-a", tc.paramsA)
			b := instancePool(t, dsn, "runner-b", tc.paramsB)
			release := holdGate(t, admin)

			resA := startRunner(runFiles(a, files, store.MigrationLockWaitForTest))
			if !waitForWaiters(t, admin, 1, reachWithin) {
				t.Fatal("runner a never reached the gate inside the file")
			}
			startB := time.Now()
			resB := startRunner(runFiles(b, files, store.MigrationLockWaitForTest))
			if !waitForWaiters(t, admin, 2, reachWithin) {
				t.Error("runner b never waited beside runner a")
			}
			time.Sleep(tc.hold)
			release()

			outA, outB := await(t, "runner a", resA), await(t, "runner b", resB)
			if outA.err != nil {
				t.Fatalf("runner a: %v", outA.err)
			}
			if outB.err != nil {
				t.Errorf("runner b failed after %s: %v", outB.ended.Sub(startB).Round(time.Millisecond), outB.err)
			}
			if tc.hold > 0 && outB.err == nil && outB.ended.Sub(startB) < tc.hold {
				t.Errorf("runner b returned after %s, before runner a left the file (%s)", outB.ended.Sub(startB), tc.hold)
			}
			rows := probeRows(t, admin)
			if len(rows) != 1 || rows[0].who != "runner-a" {
				t.Errorf("rows of the file after both runners: %+v, want exactly one, written by runner-a", rows)
			}
			if len(rows) > 0 && rows[0].isolation != "read committed" {
				t.Errorf("the file ran under %q, want read committed", rows[0].isolation)
			}
			if !versionRecorded(t, admin, 9001) {
				t.Error("9001 is not recorded")
			}
		})
	}
}

// TestMigrationRunnerAfterTheFirstRunnerFailsInTheFile: the first runner holds
// the lock and its file fails. The second runner, waiting, must then decide on
// its own: run the file itself — and succeed where the first runner's failure
// was its own, or fail with the file's error, not with one of the lock.
func TestMigrationRunnerAfterTheFirstRunnerFailsInTheFile(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		wantB   string // "" = runner b succeeds
		wantRow bool
	}{
		{
			name: "only the first runner fails",
			file: `DO $$
BEGIN
    IF current_setting('application_name') = 'runner-a' THEN
        PERFORM pg_advisory_xact_lock(398, 1);
        RAISE EXCEPTION 'runner-a fails inside the file';
    END IF;
END
$$;
` + probeInsert,
			wantRow: true,
		},
		{
			name: "the file fails for every runner",
			file: `DO $$
BEGIN
    IF current_setting('application_name') = 'runner-a' THEN
        PERFORM pg_advisory_xact_lock(398, 1);
    END IF;
    RAISE EXCEPTION 'the file fails for every runner';
END
$$;
`,
			wantB: "the file fails for every runner",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			admin, dsn := testdb.SetupTestDBWithDSN(t)
			if _, err := admin.Exec(context.Background(), probeTableSQL); err != nil {
				t.Fatalf("probe table: %v", err)
			}
			files := fstest.MapFS{"9002_fails.sql": {Data: []byte(tc.file)}}
			a := instancePool(t, dsn, "runner-a", nil)
			// The second instance's session lock_timeout: the file it runs
			// after its wait must see this value, not the runner's bound.
			b := instancePool(t, dsn, "runner-b", map[string]string{"lock_timeout": "1234ms"})
			release := holdGate(t, admin)

			resA := startRunner(runFiles(a, files, store.MigrationLockWaitForTest))
			if !waitForWaiters(t, admin, 1, reachWithin) {
				t.Fatal("runner a never reached the gate inside the file")
			}
			resB := startRunner(runFiles(b, files, store.MigrationLockWaitForTest))
			// The causal proof that runner b waited for runner a: both wait
			// at once, runner a on the gate inside its file, runner b on
			// runner a. Wall-clock order of the two returns proves nothing —
			// runner a's locks go when its file fails, before its ROLLBACK
			// returns to it.
			if !waitForWaiters(t, admin, 2, reachWithin) {
				release()
				outB := await(t, "runner b", resB)
				t.Fatalf("runner b never waited for runner a; it returned %v", outB.err)
			}
			release()

			outA, outB := await(t, "runner a", resA), await(t, "runner b", resB)
			if outA.err == nil {
				t.Fatal("runner a succeeded; the test needs its file to fail")
			}
			if errors.Is(outA.err, store.ErrMigrationLockTimeout) {
				t.Fatalf("runner a failed at the lock, not in the file: %v", outA.err)
			}
			switch {
			case tc.wantB == "" && outB.err != nil:
				t.Errorf("runner b: %v", outB.err)
			case tc.wantB != "" && (outB.err == nil || !strings.Contains(outB.err.Error(), tc.wantB)):
				t.Errorf("runner b: %v, want the file's error %q", outB.err, tc.wantB)
			case errors.Is(outB.err, store.ErrMigrationLockTimeout):
				t.Errorf("runner b failed at the lock: %v", outB.err)
			}
			rows := probeRows(t, admin)
			switch {
			case !tc.wantRow && len(rows) != 0:
				t.Errorf("rows: %+v, want none", rows)
			case tc.wantRow && (len(rows) != 1 || rows[0].who != "runner-b"):
				t.Errorf("rows: %+v, want exactly one, written by runner-b", rows)
			case tc.wantRow && rows[0].lockTimeout != "1234ms":
				t.Errorf("the file of runner b saw lock_timeout %q, want its session's 1234ms", rows[0].lockTimeout)
			}
			if got := versionRecorded(t, admin, 9002); got != tc.wantRow {
				t.Errorf("9002 recorded = %v, want %v", got, tc.wantRow)
			}
		})
	}
}

// TestMigrationLockWaitIsBounded: a first runner that does not leave its file
// does not keep the second one waiting without end. The second gives up after
// its bound with ErrMigrationLockTimeout, records nothing, and the first one
// is untouched by it.
func TestMigrationLockWaitIsBounded(t *testing.T) {
	const bound = 500 * time.Millisecond
	admin, dsn := testdb.SetupTestDBWithDSN(t)
	if _, err := admin.Exec(context.Background(), probeTableSQL); err != nil {
		t.Fatalf("probe table: %v", err)
	}
	files := fstest.MapFS{"9003_hangs.sql": {Data: []byte(gateInFile + probeInsert)}}
	a := instancePool(t, dsn, "runner-a", nil)
	b := instancePool(t, dsn, "runner-b", nil)
	release := holdGate(t, admin)

	resA := startRunner(runFiles(a, files, store.MigrationLockWaitForTest))
	if !waitForWaiters(t, admin, 1, reachWithin) {
		t.Fatal("runner a never reached the gate inside the file")
	}
	startB := time.Now()
	resB := startRunner(runFiles(b, files, bound))

	var outB runOutcome
	select {
	case outB = <-resB:
	case <-time.After(reachWithin):
		t.Errorf("runner b still waits %s after it started, with a bound of %s", reachWithin, bound)
		release()
		outB = await(t, "runner b", resB)
	}
	took := outB.ended.Sub(startB)
	if !errors.Is(outB.err, store.ErrMigrationLockTimeout) {
		t.Errorf("runner b: %v, want ErrMigrationLockTimeout", outB.err)
	} else if !strings.Contains(outB.err.Error(), "9003") {
		t.Errorf("runner b's error does not name the version: %v", outB.err)
	}
	if took < bound {
		t.Errorf("runner b gave up after %s, before its bound of %s", took, bound)
	}
	if versionRecorded(t, admin, 9003) {
		t.Error("9003 is recorded while runner a still holds its file")
	}

	release()
	if outA := await(t, "runner a", resA); outA.err != nil {
		t.Errorf("runner a: %v", outA.err)
	}
	if rows := probeRows(t, admin); len(rows) != 1 || rows[0].who != "runner-a" {
		t.Errorf("rows: %+v, want exactly one, written by runner-a", rows)
	}
}

// TestMigrationBootWithNothingPendingTakesNoLock: an instance whose embedded
// chain is applied in full boots while another runner holds the lock inside a
// file — the steady-state boot reads the recorded versions and nothing else.
func TestMigrationBootWithNothingPendingTakesNoLock(t *testing.T) {
	admin, dsn := testdb.SetupTestDBWithDSN(t)
	if _, err := admin.Exec(context.Background(), probeTableSQL); err != nil {
		t.Fatalf("probe table: %v", err)
	}
	files := fstest.MapFS{"9004_holds.sql": {Data: []byte(gateInFile + probeInsert)}}
	a := instancePool(t, dsn, "runner-a", nil)
	c := instancePool(t, dsn, "instance-c", nil)
	release := holdGate(t, admin)

	resA := startRunner(runFiles(a, files, store.MigrationLockWaitForTest))
	if !waitForWaiters(t, admin, 1, reachWithin) {
		t.Fatal("runner a never reached the gate inside the file")
	}
	resC := startRunner(func(ctx context.Context) error { return store.RunMigrations(ctx, c) })
	select {
	case outC := <-resC:
		if outC.err != nil {
			t.Errorf("instance c: %v", outC.err)
		}
	case <-time.After(reachWithin):
		t.Errorf("instance c, with nothing pending, still boots %s after it started", reachWithin)
	}
	release()
	if outA := await(t, "runner a", resA); outA.err != nil {
		t.Errorf("runner a: %v", outA.err)
	}
}

// TestMigrationRunnersBootingTogetherOnAnEmptyDatabase: two instances of a
// fresh install, started at the same moment against an empty database. Both
// boot; the chain is applied once.
func TestMigrationRunnersBootingTogetherOnAnEmptyDatabase(t *testing.T) {
	admin, dsn := testdb.SetupTestDBUpToWithDSN(t, 0)
	ctx := context.Background()
	if _, err := admin.Exec(ctx, `DROP TABLE _migrations`); err != nil {
		t.Fatalf("drop _migrations: %v", err)
	}
	a := instancePool(t, dsn, "runner-a", nil)
	b := instancePool(t, dsn, "runner-b", nil)

	start := make(chan struct{})
	run := func(pool *pgxpool.Pool) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			<-start
			return store.RunMigrations(ctx, pool)
		}
	}
	resA, resB := startRunner(run(a)), startRunner(run(b))
	close(start)

	outA, outB := await(t, "runner a", resA), await(t, "runner b", resB)
	if outA.err != nil {
		t.Errorf("runner a: %v", outA.err)
	}
	if outB.err != nil {
		t.Errorf("runner b: %v", outB.err)
	}
	last := lastEmbeddedVersion(t)
	var maxApplied int
	if err := admin.QueryRow(ctx, `SELECT COALESCE(max(version), 0) FROM _migrations`).Scan(&maxApplied); err != nil {
		t.Fatalf("read _migrations: %v", err)
	}
	if maxApplied != last {
		t.Errorf("highest recorded version %d, want %d", maxApplied, last)
	}
}

// TestMigrationsTableCreatedBesideAnotherRunner: a fresh database, and another
// runner whose CREATE TABLE of _migrations is in flight — created, not yet
// committed. The second CREATE TABLE IF NOT EXISTS does not see it, inserts
// the same catalog rows, waits for the other transaction and, on its commit,
// meets the duplicate. The runner must go on with the table that now stands.
func TestMigrationsTableCreatedBesideAnotherRunner(t *testing.T) {
	admin, dsn := testdb.SetupTestDBUpToWithDSN(t, 0)
	ctx := context.Background()
	if _, err := admin.Exec(ctx, `DROP TABLE _migrations`); err != nil {
		t.Fatalf("drop _migrations: %v", err)
	}
	b := instancePool(t, dsn, "runner-b", nil)

	other, err := admin.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = other.Rollback(ctx) }()
	if _, err := other.Exec(ctx, store.MigrationsTableSQLForTest); err != nil {
		t.Fatalf("create _migrations in the other transaction: %v", err)
	}

	resB := startRunner(func(ctx context.Context) error { return store.RunMigrationsUpTo(ctx, b, 0) })
	if !waitForWaiters(t, admin, 1, reachWithin) {
		t.Fatal("runner b never waited for the other creation")
	}
	if err := other.Commit(ctx); err != nil {
		t.Fatalf("commit the other creation: %v", err)
	}
	if outB := await(t, "runner b", resB); outB.err != nil {
		t.Errorf("runner b: %v", outB.err)
	}
}

// lastEmbeddedVersion is the highest version of the embedded chain.
func lastEmbeddedVersion(t *testing.T) int {
	t.Helper()
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	last := 0
	for _, e := range entries {
		v, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err == nil && strings.HasSuffix(e.Name(), ".sql") && v > last {
			last = v
		}
	}
	return last
}
