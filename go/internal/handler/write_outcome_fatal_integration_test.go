//go:build integration

package handler

// Issue #483 (review of #490): PostgreSQL holds interrupts while it commits,
// from before the commit record until the transaction is done. A backend
// terminated in that window (pg_terminate_backend, a fast shutdown) commits
// first and dies at its next interrupt check. With a duration log (here
// log_min_duration_statement=0) that check is the log line after the COMMIT's
// CommandComplete, so the client reads "COMMIT" and then FATAL 57P01, and pgx
// returns the FATAL — for a write the database committed. The window is
// widened with commit_delay (the backend sleeps before it flushes its commit
// record — only with fsync on, so the test runs its own container: the shared
// one runs with fsync=off on its command line) and the test terminates the
// backend while it sleeps there (inCommitDelay). writeOutcome and
// statementOutcome must say unknown for it; a FATAL inside the transaction,
// before its COMMIT, rolled
// it back (writeOutcome: rolled_back; statementOutcome cannot see where the
// error came from and says unknown); a FATAL at connect time sent nothing
// (rolled_back).
//
//	go test -tags=integration ./internal/handler/ -run 'TestWriteOutcomeFatal' -count=1 -v

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// fatalContainer starts a PostgreSQL with fsync on — commit_delay sleeps only
// then — from the image the shared test container uses, and returns its dsn.
func fatalContainer(t *testing.T) string {
	t.Helper()
	image := "timescale/timescaledb-ha:pg18" // internal/testdb's default
	if v := os.Getenv("CTX_TEST_PG_IMAGE"); v != "" {
		image = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c, err := tcpostgres.Run(ctx, image,
		tcpostgres.WithDatabase("i483"), tcpostgres.WithUsername("i483"), tcpostgres.WithPassword("i483"),
		testcontainers.WithEnv(map[string]string{"TS_TUNE_MEMORY": "1GB", "TS_TUNE_NUM_CPUS": "1"}),
		testcontainers.WithCmd("postgres", "-c", "fsync=on", "-c", "timescaledb.max_background_workers=0"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(3*time.Minute)),
	)
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatalf("start the fsync container: %v", err)
	}
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	return dsn
}

// fatalAppName marks the connections the test terminates.
const fatalAppName = "i483-fatal"

// fatalPool is a pool on dsn whose connections log every statement's
// duration and sleep 100 ms before they flush a commit record.
func fatalPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"application_name":           fatalAppName,
		"log_min_duration_statement": "0",
		"commit_delay":               "100000", // µs, the maximum
		"commit_siblings":            "0",
	} {
		cfg.ConnConfig.RuntimeParams[k] = v
	}
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// terminateAt is signalAt with pg_terminate_backend.
func terminateAt(t *testing.T, direct *pgxpool.Pool, where, query string, call func() error) error {
	t.Helper()
	return signalAt(t, direct, "pg_terminate_backend", where, query, call)
}

// signalAt runs call and sends its backend signal (pg_terminate_backend or
// pg_cancel_backend) once that backend matches where (a condition on
// pg_stat_activity with $1 the statement it runs); it fails the test when
// the call returns before.
func signalAt(t *testing.T, direct *pgxpool.Pool, signal, where, query string, call func() error) error {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- call() }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		select {
		case err := <-errc:
			t.Fatalf("the call returned (%v) before its backend matched %s", err, where)
		default:
		}
		var pid int64
		err := direct.QueryRow(context.Background(),
			`SELECT pid FROM pg_stat_activity WHERE application_name = '`+fatalAppName+`' AND query = $1 AND `+where+` LIMIT 1`,
			query).Scan(&pid)
		if err == nil {
			var ok bool
			if err := direct.QueryRow(context.Background(), `SELECT `+signal+`($1::int)`, pid).Scan(&ok); err != nil || !ok {
				t.Fatalf("%s %d: %v %v", signal, pid, ok, err)
			}
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no backend matched %s", where)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-errc:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("the call did not return after its backend was signalled")
		return nil
	}
}

// inCommitDelay matches a backend that has run its statement for 20 ms: a
// COMMIT or a one-row INSERT that takes that long sleeps in commit_delay
// (100 ms; no wait event marks the sleep), after its commit record was
// written and with interrupts held.
const inCommitDelay = `state = 'active' AND clock_timestamp() - state_change > interval '20 ms'`

// wantFatal checks that err is the FATAL of a terminated backend.
func wantFatal(t *testing.T, err error) {
	t.Helper()
	wantPgError(t, err, "FATAL", "57P01")
}

// wantPgError checks that err carries a PgError of severity and code.
func wantPgError(t *testing.T, err error, severity, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code || pgErr.SeverityUnlocalized != severity {
		t.Fatalf("error = %v, want %s %s", err, severity, code)
	}
}

func fatalRows(t *testing.T, direct *pgxpool.Pool, id int) int {
	t.Helper()
	var n int
	if err := direct.QueryRow(context.Background(), `SELECT count(*)::int FROM i483_fatal WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWriteOutcomeFatalAfterTheCommitRecord(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	dsn := fatalContainer(t)
	ctx := context.Background()
	direct, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(direct.Close)
	if _, err := direct.Exec(ctx, `CREATE TABLE i483_fatal (id int PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	pool := fatalPool(t, dsn)

	t.Run("COMMIT of a labeled bracket", func(t *testing.T) {
		err := terminateAt(t, direct, inCommitDelay, "commit", func() error {
			return pgxdb.Write(ctx, pool, pgxdb.At("probe: write"), func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO i483_fatal VALUES (1)`)
				return err
			})
		})
		wantFatal(t, err)
		if !pgxdb.FailedAtCommit(err) {
			t.Fatalf("error %v is not the bracket's COMMIT", err)
		}
		if fatalRows(t, direct, 1) != 1 {
			t.Fatal("the row is absent: the backend died before its commit record, the probe tested nothing")
		}
		if got := writeOutcome(err); got != writeUnknown {
			t.Errorf("writeOutcome = %q, want unknown for a FATAL after a committed COMMIT", got)
		}
		if got := statementOutcome(err); got != writeUnknown {
			t.Errorf("statementOutcome = %q, want unknown", got)
		}
	})

	t.Run("implicit commit of a single statement", func(t *testing.T) {
		// No arguments: pgx sends it in the simple protocol, where the
		// statement commits before its CommandComplete.
		err := terminateAt(t, direct, inCommitDelay, `INSERT INTO i483_fatal VALUES (2)`, func() error {
			_, err := pool.Exec(ctx, `INSERT INTO i483_fatal VALUES (2)`)
			return err
		})
		wantFatal(t, err)
		if fatalRows(t, direct, 2) != 1 {
			t.Fatal("the row is absent: the backend died before its commit record, the probe tested nothing")
		}
		if got := statementOutcome(err); got != writeUnknown {
			t.Errorf("statementOutcome = %q, want unknown for a FATAL after the implicit commit", got)
		}
	})

	t.Run("inside the transaction, before its COMMIT", func(t *testing.T) {
		err := terminateAt(t, direct, `wait_event = 'PgSleep'`, `SELECT pg_sleep(30)`, func() error {
			return pgxdb.Write(ctx, pool, pgxdb.At("probe: write"), func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `INSERT INTO i483_fatal VALUES (3)`); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `SELECT pg_sleep(30)`)
				return err
			})
		})
		wantFatal(t, err)
		if fatalRows(t, direct, 3) != 0 {
			t.Fatal("the row is present: the transaction was not cut before its COMMIT")
		}
		if got := writeOutcome(err); got != writeRolledBack {
			t.Errorf("writeOutcome = %q, want rolled_back: the FATAL came before the COMMIT", got)
		}
		if got := statementOutcome(err); got != writeUnknown {
			t.Errorf("statementOutcome = %q, want unknown: it cannot tell a statement from a commit", got)
		}
	})

	// Issue #483, second review: PostgreSQL holds a query cancel while it
	// commits as it holds a terminate, and reports it at the duration log
	// after the COMMIT's CommandComplete — ERROR 57014 for a write that landed.
	t.Run("COMMIT of a labeled bracket, cancelled", func(t *testing.T) {
		err := signalAt(t, direct, "pg_cancel_backend", inCommitDelay, "commit", func() error {
			return pgxdb.Write(ctx, pool, pgxdb.At("probe: write"), func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO i483_fatal VALUES (5)`)
				return err
			})
		})
		wantPgError(t, err, "ERROR", "57014")
		if !pgxdb.FailedAtCommit(err) {
			t.Fatalf("error %v is not the bracket's COMMIT", err)
		}
		if fatalRows(t, direct, 5) != 1 {
			t.Fatal("the row is absent: the cancel came before the commit record, the probe tested nothing")
		}
		if got := writeOutcome(err); got != writeUnknown {
			t.Errorf("writeOutcome = %q, want unknown for a cancel held across a committed COMMIT", got)
		}
		if got := statementOutcome(err); got != writeUnknown {
			t.Errorf("statementOutcome = %q, want unknown", got)
		}
	})

	t.Run("implicit commit of a single statement, cancelled", func(t *testing.T) {
		err := signalAt(t, direct, "pg_cancel_backend", inCommitDelay, `INSERT INTO i483_fatal VALUES (6)`, func() error {
			_, err := pool.Exec(ctx, `INSERT INTO i483_fatal VALUES (6)`)
			return err
		})
		wantPgError(t, err, "ERROR", "57014")
		if fatalRows(t, direct, 6) != 1 {
			t.Fatal("the row is absent: the cancel came before the commit record, the probe tested nothing")
		}
		if got := statementOutcome(err); got != writeUnknown {
			t.Errorf("statementOutcome = %q, want unknown for a cancel held across the implicit commit", got)
		}
	})

	// The timeouts that end a statement waiting on a lock, before the COMMIT:
	// statement_timeout raises 57014 too — writeOutcome keeps rolled_back by
	// the stage, statementOutcome cannot tell it from a held cancel — and
	// lock_timeout raises 55P03, an ERROR both read as rolled_back.
	release := lockedTx(t, direct, `SELECT pg_advisory_xact_lock(483484)`)
	defer release()
	for _, tc := range []struct {
		name, set, code, statement string
		row                        int
	}{
		{"statement_timeout before the COMMIT", `SET LOCAL statement_timeout = '200ms'`, "57014", writeUnknown, 7},
		{"lock_timeout before the COMMIT", `SET LOCAL lock_timeout = '200ms'`, "55P03", writeRolledBack, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := pgxdb.Write(ctx, pool, pgxdb.At("probe: write"), func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO i483_fatal VALUES (%d)`, tc.row)); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, tc.set); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(483484)`)
				return err
			})
			wantPgError(t, err, "ERROR", tc.code)
			if fatalRows(t, direct, tc.row) != 0 {
				t.Fatal("the row is present: the timeout did not end the transaction")
			}
			if got := writeOutcome(err); got != writeRolledBack {
				t.Errorf("writeOutcome = %q, want rolled_back: the error came before the COMMIT", got)
			}
			if got := statementOutcome(err); got != tc.statement {
				t.Errorf("statementOutcome = %q, want %s", got, tc.statement)
			}
		})
	}

	t.Run("at connect time", func(t *testing.T) {
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ConnConfig.Password = "i483-wrong-password"
		bad, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer bad.Close()
		_, err = bad.Exec(ctx, `INSERT INTO i483_fatal VALUES (4)`)
		var pgErr *pgconn.PgError
		var connectErr *pgconn.ConnectError
		if !errors.As(err, &connectErr) || !errors.As(err, &pgErr) || pgErr.SeverityUnlocalized != "FATAL" {
			t.Fatalf("error = %v, want a connect error carrying the server's FATAL", err)
		}
		if got := statementOutcome(err); got != writeRolledBack {
			t.Errorf("statementOutcome = %q, want rolled_back: nothing was sent", got)
		}
	})
}
