//go:build integration

package handler

// Issue #519 (the comment "store: with log_lock_waits on, a lock_timeout that
// fired at the grant answers the next, unrelated command on the same pooled
// connection"): ctx sets lock_timeout on connections of its pool — SET LOCAL
// in the embedding cutover's transactions (5 s, events/embed_cutover.go), the
// migration files' SET LOCAL and the migration runner's lock wait. A lock
// timer that fires at the grant of a wait at the commit (the notify lock
// every transaction takes that ran a NOTIFY, a deferred constraint's check)
// leaves its 55P03 held; with no duration log after the COMMIT's answer (the
// server default) the commit succeeds and the held 55P03 is raised by the
// connection's next command — on a pool, the statement of whoever acquires
// it next. TestLockTimeoutHeldPastCommitOnThePool drives that through the
// production pool (store.NewPool): a transaction with SET LOCAL lock_timeout
// at the edge of the notify lock, then another acquirer's statement on the
// same connection. log_lock_waits with a deadlock_timeout below the lock
// timeout — the cutover's 5 s against the default 1 s — widens the window in
// which the timer fires after the wait loop's last interrupt check (the
// comment's measurement: 355 and 380 of 2,000 runs); the message is filtered
// out of the server log, as in that measurement.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
)

func TestLockTimeoutHeldPastCommitOnThePool(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	dsn := fatalContainer(t)
	direct, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(direct.Close)
	// store.NewPool registers pgvector's types on each connection.
	if _, err := direct.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		t.Fatal(err)
	}
	if _, err := direct.Exec(ctx, i519Schema); err != nil {
		t.Fatal(err)
	}
	// The server's own defaults, which the shipped compose and image keep.
	var llw, dlt, lmds string
	if err := direct.QueryRow(ctx, `SELECT current_setting('log_lock_waits'), current_setting('deadlock_timeout'), current_setting('log_min_duration_statement')`).Scan(&llw, &dlt, &lmds); err != nil {
		t.Fatal(err)
	}
	t.Logf("server defaults: log_lock_waits=%s deadlock_timeout=%s log_min_duration_statement=%s", llw, dlt, lmds)
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	// The production pool over the probe's session settings.
	sep := "?"
	if strings.Contains(cfg.ConnString(), "?") {
		sep = "&"
	}
	pool, err := store.NewPool(ctx, cfg.ConnString()+sep+
		"application_name=i519-pool&commit_delay=0&synchronous_commit=off&log_lock_waits=on&deadlock_timeout=1&log_min_messages=fatal")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	env := &i519Env{direct: direct,
		notifyHolder: i505Config(t, dsn, map[string]string{"application_name": "i519-holder", "commit_delay": "0",
			"log_min_duration_statement": "-1", "synchronous_commit": "on", "client_min_messages": "error", "log_min_messages": "error"}),
		canceler: i505Config(t, dsn, map[string]string{"application_name": "i519-canceler", "log_min_duration_statement": "-1"}),
	}
	i519SyncStandby(t, direct)
	h := &i519Holder{kind: "notify", conn: i505Connect(t, env.notifyHolder), canceler: i505Connect(t, env.canceler)}

	runs := i519Runs(t)
	stair := &i519Stair{at: -400e-6, step: 100e-6, minStep: 1e-6, maxStep: 400e-6}
	var committed, timerWon, foreignFailed, otherConn int
	var spills []*pgconn.PgError
	for i := range runs {
		k := 3_000_000 + i
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		pid := c.Conn().PgConn().PID()
		done := h.start(t, direct, k, pid, i519LockTimeout.Seconds()+stair.at)
		// The transaction sets its lock_timeout as the cutover does.
		werr := pgxdb.Write(ctx, c, pgxdb.At("probe: write"), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, i519LockTimeout.Milliseconds())); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO i519_notify VALUES (%d)`, k))
			return err
		})
		report, herr := done()
		if herr != nil {
			t.Fatalf("run %d: %v; the probe returned %v", i, herr, werr)
		}
		c.Release()
		// Whoever acquires the connection next: an unrelated statement.
		next, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if next.Conn().PgConn().PID() != pid {
			otherConn++
		}
		_, ferr := next.Exec(ctx, `SELECT 1`)
		next.Release()
		if werr == nil {
			committed++
			if ferr != nil {
				foreignFailed++
				if pgErr := new(pgconn.PgError); errors.As(ferr, &pgErr) {
					spills = append(spills, pgErr)
				}
			}
		} else {
			timerWon++
		}
		if report != nil && !*report {
			stair.next(werr != nil)
		}
	}
	t.Logf("%d runs at the notify lock's edge: %d committed, %d cut by the timer, %d times the next acquirer got another connection; "+
		"an unrelated statement after a committed write failed %d times", runs, committed, timerWon, otherConn, foreignFailed)
	for i, e := range spills {
		if i < 3 {
			t.Logf("the next acquirer's error: %s %s (%s:%d %s)", e.Code, e.Message, e.File, e.Line, e.Routine)
		}
	}
	if committed == 0 || timerWon == 0 {
		t.Fatalf("the runs did not straddle the edge: %d committed, %d cut", committed, timerWon)
	}
	if foreignFailed != 0 {
		t.Errorf("an unrelated statement on a pooled connection failed %d times after a committed transaction whose lock timer fired at the grant", foreignFailed)
	}
}
