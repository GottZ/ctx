//go:build integration

package handler

// Issue #505 (points 7 and 8 of #504/#505): can the error of a session
// timeout answer a statement or a COMMIT whose transaction committed? Read in
// the PostgreSQL 18.6 source (tag REL_18_6) and measured on the container
// fatalContainer starts (fsync on, so commit_delay sleeps), every session
// logging its statements' durations (log_min_duration_statement=0: the
// interrupt check after the CommandComplete where #500 found a held terminate
// or cancel) and sleeping 100 ms in commit_delay.
//
//   - idle_in_transaction_session_timeout (FATAL 25P03) and
//     idle_session_timeout (FATAL 57P05) run only while the backend waits for
//     its next command: PostgresMain disarms them after it read one and checks
//     for interrupts before it runs it, so a timer that fired in between ends
//     the session with the command unrun. Measured: the FATAL after a long
//     idle, and the edge (idle about the timeout) over many runs. Not
//     measured, and not closed in the source: when a held statement timeout
//     raises its ERROR first in that check, the idle timer's pending flag
//     outlives the error and fires at any later interrupt check.
//   - statement_timeout (ERROR 57014) is disarmed before the commit record,
//     but disable_statement_timeout skips a timer that already fired, so its
//     indicator survives: a timer that fired after the statement's last
//     interrupt check is reported after the commit, with the timeout's own
//     message — at the duration log (simple protocol) or, without one, as the
//     answer to the connection's next command (a bracket's COMMIT among them,
//     which then never runs). Measured deterministically with
//     a statement-level AFTER trigger whose last expression runs without an
//     interrupt check while the timer fires, and at the edge with a plain
//     INSERT. In the extended protocol the implicit commit waits for Sync and
//     the held cancel is reported before it.
//   - Of a 57014's PgError fields only Message (localized) and Line (the
//     server's source line, which moves between releases) tell a statement
//     timeout from a pg_cancel_backend; Routine and File are the same.
//
// The tests pin what writeOutcome and statementOutcome say today — unknown for
// every FATAL and every 57014 — next to the row each run left behind.
//
//	go test -tags=integration ./internal/handler/ -run 'TestWriteOutcomeTimeout' -count=1 -v
//	CTX_I505_RUNS=500 go test ... (runs at each edge; default i505DefaultRuns)

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// i505DefaultRuns is the number of runs at each edge without CTX_I505_RUNS:
// enough to straddle each edge, few enough for the file's 120 s budget.
const i505DefaultRuns = 40

// i505Schema: i505_timeout takes the plain writes. i505_held carries a
// statement-level AFTER trigger, i505_hold, that holds a statement timer
// across the commit: it builds 64 MiB of text (repeat checks for interrupts),
// sleeps until i505.hold_at seconds after the statement began, and returns
// through md5 over that text — the RETURN's interrupt check comes first, md5
// runs without one, and nothing checks again before the commit (trigger.c and
// execMain.c have no CHECK_FOR_INTERRUPTS). i505_hold_cost measures, on the
// server, how long the trigger takes to build the text and to run md5 over it
// (seconds). i505_spin sleeps, then spins n loop iterations, each with an
// interrupt check; i505_spin_cost measures n of them.
const i505Schema = `
CREATE FUNCTION i505_hold_cost(OUT build float8, OUT hash float8) LANGUAGE plpgsql AS $f$
DECLARE
	t0 timestamptz := clock_timestamp();
	s text := repeat(repeat('x', 1024), 65536);
	t1 timestamptz := clock_timestamp();
BEGIN
	PERFORM md5(s);
	build := extract(epoch FROM t1 - t0)::float8;
	hash := extract(epoch FROM clock_timestamp() - t1)::float8;
END
$f$;
CREATE FUNCTION i505_spin_cost(n int) RETURNS float8 LANGUAGE plpgsql AS $f$
DECLARE
	t0 timestamptz := clock_timestamp();
BEGIN
	FOR i IN 1..n LOOP
	END LOOP;
	RETURN extract(epoch FROM clock_timestamp() - t0)::float8;
END
$f$;
CREATE TABLE i505_timeout (id int PRIMARY KEY);
CREATE TABLE i505_held (id int PRIMARY KEY);
CREATE FUNCTION i505_hold() RETURNS trigger LANGUAGE plpgsql AS $f$
DECLARE
	s text := repeat(repeat('x', 1024), 65536);
BEGIN
	PERFORM pg_sleep(greatest(0, current_setting('i505.hold_at')::float8
		- extract(epoch FROM clock_timestamp() - statement_timestamp())::float8));
	RETURN CASE WHEN md5(s) IS NULL THEN NULL END;
END
$f$;
CREATE TRIGGER i505_hold AFTER INSERT ON i505_held FOR EACH STATEMENT EXECUTE FUNCTION i505_hold();
CREATE FUNCTION i505_spin(pause float8, n int) RETURNS int LANGUAGE plpgsql AS $f$
BEGIN
	PERFORM pg_sleep(pause);
	FOR i IN 1..n LOOP
	END LOOP;
	RETURN 1;
END
$f$;
`

// i505Runs returns the number of runs at each edge.
func i505Runs(t *testing.T) int {
	t.Helper()
	v := os.Getenv("CTX_I505_RUNS")
	if v == "" {
		return i505DefaultRuns
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		t.Fatalf("CTX_I505_RUNS=%q: want a positive integer", v)
	}
	return n
}

// i505Config is a connection config on dsn with fatalPool's settings — every
// statement's duration logged, 100 ms commit_delay — plus params.
func i505Config(t *testing.T, dsn string, params map[string]string) *pgx.ConnConfig {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"application_name":           "i505-timeout",
		"log_min_duration_statement": "0",
		"commit_delay":               "100000", // µs, the maximum
		"commit_siblings":            "0",
	} {
		cfg.RuntimeParams[k] = v
	}
	for k, v := range params {
		cfg.RuntimeParams[k] = v
	}
	return cfg
}

// i505Connect opens a connection the test closes at its end.
func i505Connect(t *testing.T, cfg *pgx.ConnConfig) *pgx.Conn {
	t.Helper()
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// i505Exec runs statements on conn and fails the test on an error.
func i505Exec(t *testing.T, conn *pgx.Conn, statements ...string) {
	t.Helper()
	for _, s := range statements {
		if _, err := conn.Exec(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// i505Row reports whether table holds the row id.
func i505Row(t *testing.T, direct *pgxpool.Pool, table string, id int) bool {
	t.Helper()
	var n int
	if err := direct.QueryRow(context.Background(), `SELECT count(*)::int FROM `+table+` WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// i505PgError returns err's PgError and fails the test without one.
func i505PgError(t *testing.T, err error) *pgconn.PgError {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error = %v, want a PgError", err)
	}
	return pgErr
}

// i505Kind names an error for a tally: the PgError's severity, code, server
// routine and line and message, or the error text.
func i505Kind(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Sprintf("%s %s %s:%d %q", pgErr.SeverityUnlocalized, pgErr.Code, pgErr.Routine, pgErr.Line, pgErr.Message)
	}
	return fmt.Sprintf("%.160s (safe_to_retry=%t)", err.Error(), pgconn.SafeToRetry(err))
}

// i505Tally counts the runs at one edge.
type i505Tally struct {
	name                                           string
	runs, failed, failedWithRow, clean, cleanNoRow int
	kinds                                          map[string]int
}

func newI505Tally(name string) *i505Tally {
	return &i505Tally{name: name, kinds: map[string]int{}}
}

// add counts a run: err is what the client got, row whether the row is there,
// outcome the classification that applies (writeOutcome for a labeled
// bracket's COMMIT, statementOutcome for one statement). Every FATAL and every
// 57014 classifies unknown today.
func (c *i505Tally) add(t *testing.T, err error, row bool, outcome func(error) string) {
	t.Helper()
	c.runs++
	if err == nil {
		if row {
			c.clean++
		} else {
			c.cleanNoRow++
		}
		return
	}
	c.failed++
	if row {
		c.failedWithRow++
	}
	got := outcome(err)
	c.kinds[fmt.Sprintf("%s row=%t outcome=%s", i505Kind(err), row, got)]++
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && commitAmbiguous(pgErr) && got != writeUnknown {
		t.Errorf("%v classified %q, want unknown", err, got)
	}
}

func (c *i505Tally) log(t *testing.T) {
	t.Helper()
	t.Logf("TALLY %s: runs=%d failed=%d failed_with_row=%d without_error=%d without_error_without_row=%d",
		c.name, c.runs, c.failed, c.failedWithRow, c.clean, c.cleanNoRow)
	keys := make([]string, 0, len(c.kinds))
	for k := range c.kinds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("TALLY %s:   %d x %s", c.name, c.kinds[k], k)
	}
	if c.cleanNoRow != 0 {
		t.Errorf("%d runs returned no error and left no row", c.cleanNoRow)
	}
}

// i505Seconds is a duration the server measured, in seconds.
func i505Seconds(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}

// i505HoldTiming measures the trigger's two parts on the server three times
// (the host's load moves them) and returns the statement_timeout and
// i505.hold_at that start md5 after the slowest build and fire the timer
// half the fastest md5 into it.
func i505HoldTiming(t *testing.T, direct *pgxpool.Pool) (timeout time.Duration, holdAt float64) {
	t.Helper()
	var buildMax, hashMax time.Duration
	hashMin := time.Duration(1<<63 - 1)
	for range 3 {
		var build, hash float64
		if err := direct.QueryRow(context.Background(), `SELECT build, hash FROM i505_hold_cost()`).Scan(&build, &hash); err != nil {
			t.Fatal(err)
		}
		buildMax = max(buildMax, i505Seconds(build))
		hashMax = max(hashMax, i505Seconds(hash))
		if d := i505Seconds(hash); d < hashMin { // the package's min (armsweep_bw5) takes ints
			hashMin = d
		}
	}
	if hashMin < 20*time.Millisecond {
		t.Fatalf("md5 over 64 MiB took %v: too short to hold a timer", hashMin)
	}
	timeout = (buildMax + hashMax + 300*time.Millisecond).Truncate(time.Millisecond)
	holdAt = (timeout - hashMin/2).Seconds()
	t.Logf("hold: build up to %v, md5 %v to %v, statement_timeout %v, the trigger sleeps until %.3f s",
		buildMax, hashMin, hashMax, timeout, holdAt)
	return timeout, holdAt
}

// i505HeldAttempts bounds the attempts of a held-timer case (i505Held).
const i505HeldAttempts = 5

// i505Held runs try with fresh row ids until it reports the probe inside its
// window, at most i505HeldAttempts times. try returns why an attempt missed
// the window ("" when it was inside and its checks ran): a host under load
// wakes the trigger late or runs md5 fast, which says nothing about the
// server. Every attempt missed ⇒ skip, not fail.
func i505Held(t *testing.T, base int, try func(id int) (missed string)) {
	t.Helper()
	for a := range i505HeldAttempts {
		missed := try(base + a)
		if missed == "" {
			return
		}
		t.Logf("attempt %d missed the held window: %s", a+1, missed)
	}
	t.Skipf("all %d attempts missed the held window under the host's load", i505HeldAttempts)
}

func TestWriteOutcomeTimeout(t *testing.T) {
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
	if _, err := direct.Exec(ctx, i505Schema); err != nil {
		t.Fatal(err)
	}
	var version string
	if err := direct.QueryRow(ctx, `SELECT version()`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Logf("server: %s", version)
	runs := i505Runs(t)

	holdTimeout, holdAt := i505HoldTiming(t, direct)
	holdSettings := []string{
		fmt.Sprintf(`SET statement_timeout = '%dms'`, holdTimeout.Milliseconds()),
		fmt.Sprintf(`SET i505.hold_at = '%.6f'`, holdAt),
	}

	var heldTimeout *pgconn.PgError // the 57014 of a timeout held across a commit

	t.Run("statement_timeout held across the implicit commit", func(t *testing.T) {
		i505Held(t, 10, func(id int) string {
			conn := i505Connect(t, i505Config(t, dsn, nil))
			i505Exec(t, conn, holdSettings...)
			// No arguments: the simple protocol, which commits before the
			// CommandComplete and logs the duration after it.
			_, err := conn.Exec(ctx, fmt.Sprintf(`INSERT INTO i505_held VALUES (%d)`, id))
			switch row := i505Row(t, direct, "i505_held", id); {
			case err == nil:
				return "no timeout: md5 ended before the timer"
			case !row:
				return fmt.Sprintf("the timeout cut the statement before its commit (%v)", err)
			}
			wantPgError(t, err, "ERROR", "57014")
			heldTimeout = i505PgError(t, err)
			t.Logf("FIELDS statement timeout held across the implicit commit: %+v", *heldTimeout)
			if got := statementOutcome(err); got != writeUnknown {
				t.Errorf("statementOutcome = %q, want unknown for a timeout held across the implicit commit", got)
			}
			return ""
		})
	})

	t.Run("statement_timeout held, extended protocol", func(t *testing.T) {
		// With an argument pgx sends Bind/Execute/Sync: Execute logs its
		// duration before the Sync that commits.
		i505Held(t, 20, func(id int) string {
			conn := i505Connect(t, i505Config(t, dsn, nil))
			i505Exec(t, conn, holdSettings...)
			_, err := conn.Exec(ctx, `INSERT INTO i505_held VALUES ($1)`, id)
			if err == nil {
				return "no timeout: md5 ended before the timer"
			}
			wantPgError(t, err, "ERROR", "57014")
			if i505Row(t, direct, "i505_held", id) {
				t.Fatal("the row is present: the held timeout was reported after the commit")
			}
			if got := statementOutcome(err); got != writeUnknown {
				t.Errorf("statementOutcome = %q, want unknown (it cannot tell the protocol)", got)
			}
			return ""
		})
	})

	t.Run("statement_timeout held into the next command, no duration log", func(t *testing.T) {
		i505Held(t, 30, func(id int) string {
			conn := i505Connect(t, i505Config(t, dsn, map[string]string{"log_min_duration_statement": "-1"}))
			i505Exec(t, conn, holdSettings...)
			if _, err := conn.Exec(ctx, fmt.Sprintf(`INSERT INTO i505_held VALUES (%d)`, id)); err != nil {
				return fmt.Sprintf("the timeout cut the INSERT (%v)", err)
			}
			if !i505Row(t, direct, "i505_held", id) {
				t.Fatal("the row is absent")
			}
			_, err := conn.Exec(ctx, `SELECT 1`)
			if err == nil {
				return "no timeout: md5 ended before the timer"
			}
			wantPgError(t, err, "ERROR", "57014")
			next := i505PgError(t, err)
			t.Logf("FIELDS statement timeout held into the next command: %+v", *next)
			if heldTimeout != nil && next.Line != heldTimeout.Line {
				t.Errorf("line %d, want the statement timeout's %d", next.Line, heldTimeout.Line)
			}
			if _, err := conn.Exec(ctx, `SELECT 1`); err != nil {
				t.Fatalf("the command after: %v", err)
			}
			return ""
		})
	})

	t.Run("statement_timeout held into a bracket's COMMIT, no duration log", func(t *testing.T) {
		i505Held(t, 40, func(id int) string {
			conn := i505Connect(t, i505Config(t, dsn, map[string]string{"log_min_duration_statement": "-1"}))
			err := pgxdb.Write(ctx, conn, pgxdb.At("probe: write"), func(tx pgx.Tx) error {
				for _, s := range append(holdSettings, fmt.Sprintf(`INSERT INTO i505_held VALUES (%d)`, id)) {
					if _, err := tx.Exec(ctx, s); err != nil {
						return err
					}
				}
				return nil
			})
			switch {
			case err == nil:
				return "no timeout: md5 ended before the timer"
			case !pgxdb.FailedAtCommit(err):
				return fmt.Sprintf("the timeout cut the INSERT, not the COMMIT (%v)", err)
			}
			wantPgError(t, err, "ERROR", "57014")
			if i505Row(t, direct, "i505_held", id) {
				t.Fatal("the row is present: the COMMIT ran")
			}
			if got := writeOutcome(err); got != writeUnknown {
				t.Errorf("writeOutcome = %q, want unknown (today: every 57014 at the COMMIT)", got)
			}
			return ""
		})
	})

	t.Run("57014 fields: statement timeout and pg_cancel_backend", func(t *testing.T) {
		tconn := i505Connect(t, i505Config(t, dsn, map[string]string{"statement_timeout": "100"}))
		_, err := tconn.Exec(ctx, `SELECT pg_sleep(5)`)
		wantPgError(t, err, "ERROR", "57014")
		timeoutCut := i505PgError(t, err)

		cconn := i505Connect(t, i505Config(t, dsn, map[string]string{"application_name": fatalAppName}))
		err = signalAt(t, direct, "pg_cancel_backend", `wait_event = 'PgSleep'`, `SELECT pg_sleep(30)`, func() error {
			_, err := cconn.Exec(ctx, `SELECT pg_sleep(30)`)
			return err
		})
		wantPgError(t, err, "ERROR", "57014")
		cancelCut := i505PgError(t, err)

		err = signalAt(t, direct, "pg_cancel_backend", inCommitDelay, `INSERT INTO i505_timeout VALUES (5)`, func() error {
			_, err := cconn.Exec(ctx, `INSERT INTO i505_timeout VALUES (5)`)
			return err
		})
		wantPgError(t, err, "ERROR", "57014")
		cancelHeld := i505PgError(t, err)
		if !i505Row(t, direct, "i505_timeout", 5) {
			t.Fatal("the row is absent: the cancel came before the commit record")
		}

		all := map[string]*pgconn.PgError{
			"statement timeout before the commit": timeoutCut,
			"cancel before the commit":            cancelCut,
			"cancel held across the commit":       cancelHeld,
		}
		if heldTimeout != nil {
			all["statement timeout held across the commit"] = heldTimeout
		}
		for name, e := range all {
			t.Logf("FIELDS %s: %+v", name, *e)
			if e.Routine != timeoutCut.Routine || e.File != timeoutCut.File {
				t.Errorf("%s: raised at %s (%s), the timeout at %s (%s)", name, e.Routine, e.File, timeoutCut.Routine, timeoutCut.File)
			}
		}
		if cancelHeld.Line != cancelCut.Line || cancelHeld.Message != cancelCut.Message {
			t.Errorf("a held cancel reads %d %q, a cut one %d %q", cancelHeld.Line, cancelHeld.Message, cancelCut.Line, cancelCut.Message)
		}
		if heldTimeout != nil && (heldTimeout.Line != timeoutCut.Line || heldTimeout.Message != timeoutCut.Message) {
			t.Errorf("a held timeout reads %d %q, a cut one %d %q", heldTimeout.Line, heldTimeout.Message, timeoutCut.Line, timeoutCut.Message)
		}
		if timeoutCut.Line == cancelCut.Line {
			t.Errorf("timeout and cancel share line %d", timeoutCut.Line)
		}

		// The localized half: lc_messages changes Message and Severity, not
		// Line, Routine, File, SeverityUnlocalized or Code. The candidates are
		// the operating system's locales initdb found, English and C aside.
		rows, err := direct.Query(ctx, `SELECT collname FROM pg_collation WHERE collprovider = 'c'
			AND collname !~ '^(C|POSIX|ucs_basic|en_|default)' ORDER BY collname LIKE 'de%' DESC, collname`)
		if err != nil {
			t.Fatal(err)
		}
		locales, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("non-English locales in the image: %v", locales)
		for _, loc := range locales {
			if _, err := tconn.Exec(ctx, `SET lc_messages = '`+loc+`'`); err != nil {
				t.Logf("lc_messages %s: %v", loc, err)
				continue
			}
			_, err := tconn.Exec(ctx, `SELECT pg_sleep(5)`)
			wantPgError(t, err, "ERROR", "57014")
			e := i505PgError(t, err)
			t.Logf("FIELDS statement timeout, lc_messages %s: %+v", loc, *e)
			if e.Line != timeoutCut.Line || e.Routine != timeoutCut.Routine || e.File != timeoutCut.File {
				t.Errorf("lc_messages %s moved the raise site: %s %s:%d", loc, e.Routine, e.File, e.Line)
			}
			if e.Message == timeoutCut.Message {
				t.Logf("lc_messages %s: the message stays untranslated (no catalog in the image)", loc)
			}
			break
		}
	})

	t.Run("idle_in_transaction_session_timeout, COMMIT after the timeout", func(t *testing.T) {
		conn := i505Connect(t, i505Config(t, dsn, nil))
		err := pgxdb.Write(ctx, conn, pgxdb.At("probe: write"), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO i505_timeout VALUES (1)`); err != nil {
				return err
			}
			// ctx's own form (QueryHandler.armSweepSearch): SET LOCAL in the bracket.
			if _, err := tx.Exec(ctx, `SET LOCAL idle_in_transaction_session_timeout = '200ms'`); err != nil {
				return err
			}
			time.Sleep(400 * time.Millisecond)
			return nil
		})
		t.Logf("error: %v", err)
		wantPgError(t, err, "FATAL", "25P03")
		if !pgxdb.FailedAtCommit(err) {
			t.Fatalf("error %v is not the bracket's COMMIT", err)
		}
		if i505Row(t, direct, "i505_timeout", 1) {
			t.Fatal("the row is present: the COMMIT ran after the timeout")
		}
		if got := writeOutcome(err); got != writeUnknown {
			t.Errorf("writeOutcome = %q, want unknown (today: every FATAL)", got)
		}
		if got := statementOutcome(err); got != writeUnknown {
			t.Errorf("statementOutcome = %q, want unknown (today: every FATAL)", got)
		}
	})

	t.Run("idle_in_transaction_session_timeout at the edge", func(t *testing.T) {
		cfg := i505Config(t, dsn, map[string]string{"idle_in_transaction_session_timeout": "50"})
		tally := newI505Tally("25P03 edge (timeout 50 ms, idle 45-55 ms)")
		for i := range runs {
			id := 100000 + i
			idle := 45*time.Millisecond + rand.N(10*time.Millisecond)
			conn, err := pgx.ConnectConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			err = pgxdb.Write(ctx, conn, pgxdb.At("probe: write"), func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO i505_timeout VALUES (%d)`, id)); err != nil {
					return err
				}
				time.Sleep(idle)
				return nil
			})
			_ = conn.Close(ctx)
			if err != nil && !pgxdb.FailedAtCommit(err) {
				t.Fatalf("run %d: %v came before the COMMIT", i, err)
			}
			tally.add(t, err, i505Row(t, direct, "i505_timeout", id), writeOutcome)
		}
		tally.log(t)
		if tally.failedWithRow != 0 {
			t.Errorf("%d runs got an error for a COMMIT that committed", tally.failedWithRow)
		}
	})

	t.Run("idle_session_timeout, INSERT after the timeout", func(t *testing.T) {
		conn := i505Connect(t, i505Config(t, dsn, map[string]string{"idle_session_timeout": "200"}))
		time.Sleep(400 * time.Millisecond)
		_, err := conn.Exec(ctx, `INSERT INTO i505_timeout VALUES (2)`)
		t.Logf("error: %v", err)
		wantPgError(t, err, "FATAL", "57P05")
		if i505Row(t, direct, "i505_timeout", 2) {
			t.Fatal("the row is present: the INSERT ran after the timeout")
		}
		if got := statementOutcome(err); got != writeUnknown {
			t.Errorf("statementOutcome = %q, want unknown (today: every FATAL)", got)
		}
	})

	t.Run("idle_session_timeout at the edge", func(t *testing.T) {
		cfg := i505Config(t, dsn, map[string]string{"idle_session_timeout": "50"})
		tally := newI505Tally("57P05 edge (timeout 50 ms, idle 45-55 ms)")
		for i := range runs {
			id := 200000 + i
			conn, err := pgx.ConnectConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(45*time.Millisecond + rand.N(10*time.Millisecond))
			_, err = conn.Exec(ctx, fmt.Sprintf(`INSERT INTO i505_timeout VALUES (%d)`, id))
			_ = conn.Close(ctx)
			tally.add(t, err, i505Row(t, direct, "i505_timeout", id), statementOutcome)
		}
		tally.log(t)
		if tally.failedWithRow != 0 {
			t.Errorf("%d runs got an error for an INSERT that committed", tally.failedWithRow)
		}
	})

	t.Run("statement_timeout at the edge, pg_sleep", func(t *testing.T) {
		// As briefed: the statement sleeps 48.0-50.5 ms under a 50 ms timeout.
		// pg_sleep waits in whole milliseconds (ceil), so the end of the sleep
		// barely moves inside one millisecond.
		conn := i505Connect(t, i505Config(t, dsn, map[string]string{"statement_timeout": "50"}))
		tally := newI505Tally("57014 edge, pg_sleep (timeout 50 ms, sleep 48.0-50.5 ms)")
		for i := range runs {
			id := 300000 + i
			pause := 0.048 + rand.Float64()*0.0025
			_, err := conn.Exec(ctx, fmt.Sprintf(`INSERT INTO i505_timeout (id) SELECT %d FROM pg_sleep(%.6f)`, id, pause))
			tally.add(t, err, i505Row(t, direct, "i505_timeout", id), statementOutcome)
		}
		tally.log(t)
	})

	t.Run("statement_timeout at the edge, staircase", func(t *testing.T) {
		// The statement sleeps 25 ms, then spins n interrupt-checked
		// iterations under a 30 ms timeout; n walks up after a run that beat
		// the timer and down after one the timer cut, the step halving at each
		// turn down to about 2 µs, so the timer keeps landing about the end of
		// the statement — and now and then after its last interrupt check.
		const pause = 0.025
		var spin float64
		if err := direct.QueryRow(ctx, `SELECT i505_spin_cost(200000)`).Scan(&spin); err != nil {
			t.Fatal(err)
		}
		perIter := max(time.Nanosecond, i505Seconds(spin)/200000)
		n := int(4 * time.Millisecond / perIter)
		minStep := max(1, int(2*time.Microsecond/perIter))
		maxStep := max(minStep, n/20)
		step, up, same := maxStep, true, 0
		turn := func(goUp bool) {
			if goUp != up {
				step, up, same = max(minStep, step/2), goUp, 0
			} else if same++; same >= 3 {
				// The host's load moved the edge: follow it.
				step, same = min(maxStep, 2*step), 0
			}
			if goUp {
				n += step
			} else {
				n = max(1, n-step)
			}
		}
		t.Logf("spin: %v per iteration, start n=%d, step %d down to %d", perIter, n, step, minStep)
		conn := i505Connect(t, i505Config(t, dsn, map[string]string{"statement_timeout": "30"}))
		tally := newI505Tally("57014 edge, staircase (timeout 30 ms, sleep 25 ms + spin)")
		for i := range runs {
			id := 400000 + i
			_, err := conn.Exec(ctx, fmt.Sprintf(`INSERT INTO i505_timeout (id) SELECT %d WHERE i505_spin(%.3f, %d) = 1`, id, pause, n))
			row := i505Row(t, direct, "i505_timeout", id)
			tally.add(t, err, row, statementOutcome)
			switch {
			case err != nil && row:
				t.Logf("run %d (n=%d): %s after the commit", i, n, i505Kind(err))
				if e := i505PgError(t, err); heldTimeout != nil && e.Line != heldTimeout.Line {
					t.Errorf("run %d: line %d, want the statement timeout's %d", i, e.Line, heldTimeout.Line)
				}
			case err != nil:
				turn(false)
			default:
				turn(true)
			}
		}
		t.Logf("spin: end n=%d, step %d", n, step)
		tally.log(t)
	})
}
