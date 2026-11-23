//go:build integration

package handler

// Issue #519 (point 8 of the #504/#505 follow-ups): can lock_timeout's 55P03
// answer a statement or a COMMIT whose transaction committed? Read in the
// PostgreSQL 18.6 source (tag REL_18_6) and measured on the server
// fatalContainer starts, every probe session logging its statements'
// durations (log_min_duration_statement=0: the interrupt check after the
// CommandComplete where #500 found a held terminate or cancel).
//
//   - ProcSleep (proc.c) arms LOCK_TIMEOUT with the deadlock timer, checks for
//     interrupts in its wait loop and, once the lock is granted, disarms both
//     with keep_indicator for LOCK_TIMEOUT. A timer that fires after the
//     loop's last interrupt check and before disable_timeouts leaves
//     QueryCancelPending set with the lock timeout's indicator, and
//     ProcessInterrupts reports it as 55P03 at the next check, whatever runs
//     then. The timer never outlives ProcSleep, so the window is the few
//     instructions of its tail; no wide, deterministic form exists like
//     statement_timeout's held trigger.
//   - Inside a statement a check follows the wait before the commit
//     (ExecResult, ExecScanFetch before the next row), so the held cancel
//     ends the statement: 55P03, rolled back.
//   - At the commit it does not: a deferred constraint's check
//     (AfterTriggerFireDeferred) and PreCommit_Notify's lock — every
//     transaction that ran a NOTIFY takes it, as ctx's link, block-type and
//     tenant triggers do — wait before HOLD_INTERRUPTS with no interrupt check
//     in between (xact.c CommitTransaction), so the held cancel is reported at
//     the duration log after the CommandComplete: ERROR 55P03 for a
//     transaction that committed, as the answer to a bracket's COMMIT or to a
//     single statement. Its fields are those of a 55P03 that cut the wait
//     before the grant (ProcessInterrupts, the same line and message).
//   - With the window widened (log_lock_waits builds its text in the tail
//     while the message itself is filtered out, and the duration log with
//     it), the held cancel answers the connection's next command after the
//     commit succeeded.
//
// A holder session takes the lock first; the probe waits for it under
// lock_timeout; the holder releases it at an offset from the probe's own lock
// timer (pg_locks.waitstart is the instant ProcSleep armed it), and a
// staircase walks that offset about the edge where the timer and the grant
// meet. The holder releases row and unique locks by aborting a
// subtransaction; for the notify lock a third session cancels the holder
// COMMIT's wait for a synchronous standby that never connects.
//
//	go test -tags=integration ./internal/handler/ -run 'TestWriteOutcomeLockTimeout' -count=1 -v
//	CTX_I519_RUNS=2000 go test ... (runs at each edge; default i519DefaultRuns)

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// i519DefaultRuns is the number of runs at each edge without CTX_I519_RUNS:
// enough to straddle each edge, few enough for the file's 60 s budget.
const i519DefaultRuns = 100

// i519LockTimeout is the probe's lock_timeout.
const i519LockTimeout = 10 * time.Millisecond

// i519Schema: i519_row takes the row-lock probes (the holder locks the row,
// the probe updates it), i519_unique the unique-conflict probes (the holder
// inserts the key first), i519_deferred the probes whose wait is the deferred
// unique check at the commit, i519_notify the probes whose wait is the notify
// lock at the commit (a row trigger NOTIFYs with a constant payload, as ctx's
// triggers do). i519_release_at waits until the probe waits for a lock and
// returns `after` seconds after the probe's lock timer started: false on
// time, true when it saw the wait only after that instant, NULL when it never
// saw it (a wait that fell between two polls). i519_hold takes the holder's
// lock in a subtransaction and aborts it then; i519_cancel_at cancels the
// holder's backend instead.
const i519Schema = `
CREATE TABLE i519_row (id int PRIMARY KEY, v int NOT NULL DEFAULT 0);
CREATE TABLE i519_unique (id int PRIMARY KEY);
CREATE TABLE i519_deferred (id int CONSTRAINT i519_deferred_id UNIQUE DEFERRABLE INITIALLY DEFERRED);
CREATE TABLE i519_notify (id int PRIMARY KEY);
CREATE FUNCTION i519_notify() RETURNS trigger LANGUAGE plpgsql AS $f$
BEGIN
	PERFORM pg_notify('i519', TG_TABLE_NAME);
	RETURN NULL;
END
$f$;
CREATE TRIGGER i519_notify AFTER INSERT ON i519_notify FOR EACH ROW EXECUTE FUNCTION i519_notify();
CREATE FUNCTION i519_release_at(probe int, after float8) RETURNS boolean LANGUAGE plpgsql AS $f$
DECLARE
	t0 timestamptz := clock_timestamp();
	ws timestamptz;
	target timestamptz;
BEGIN
	LOOP
		SELECT l.waitstart INTO ws FROM pg_locks l
		WHERE l.pid = probe AND NOT l.granted AND l.waitstart IS NOT NULL LIMIT 1;
		EXIT WHEN ws IS NOT NULL;
		IF clock_timestamp() - t0 > interval '2 s' THEN
			RETURN NULL;
		END IF;
		PERFORM pg_sleep(0.0001);
	END LOOP;
	target := ws + make_interval(secs => after);
	IF clock_timestamp() > target THEN
		RETURN true;
	END IF;
	PERFORM pg_sleep(greatest(0, extract(epoch FROM target - clock_timestamp()) - 0.003));
	WHILE clock_timestamp() < target LOOP
	END LOOP;
	RETURN false;
END
$f$;
CREATE FUNCTION i519_hold(kind text, k int, probe int, after float8) RETURNS boolean LANGUAGE plpgsql AS $f$
DECLARE
	missed boolean;
BEGIN
	BEGIN
		CASE kind
		WHEN 'row' THEN PERFORM 1 FROM i519_row WHERE id = k FOR UPDATE;
		WHEN 'unique' THEN INSERT INTO i519_unique VALUES (k);
		WHEN 'deferred' THEN INSERT INTO i519_deferred VALUES (k);
		END CASE;
		missed := i519_release_at(probe, after);
		RAISE EXCEPTION USING ERRCODE = 'P0001';
	EXCEPTION WHEN raise_exception THEN
		-- the subtransaction's abort released its lock
	END;
	RETURN missed;
END
$f$;
CREATE FUNCTION i519_cancel_at(holder int, probe int, after float8) RETURNS boolean LANGUAGE plpgsql AS $f$
DECLARE
	missed boolean := i519_release_at(probe, after);
BEGIN
	IF NOT pg_cancel_backend(holder) THEN
		RAISE EXCEPTION 'i519: cancel of % failed', holder;
	END IF;
	RETURN missed;
END
$f$;
`

// i519Runs returns the number of runs at each edge.
func i519Runs(t *testing.T) int {
	t.Helper()
	v := os.Getenv("CTX_I519_RUNS")
	if v == "" {
		return i519DefaultRuns
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		t.Fatalf("CTX_I519_RUNS=%q: want a positive integer", v)
	}
	return n
}

// i519Env is what every edge shares: a pool to look with and the connection
// configs of the roles.
type i519Env struct {
	direct       *pgxpool.Pool
	probe        *pgx.ConnConfig // lock_timeout and the duration log
	wideProbe    *pgx.ConnConfig // probe with the widened window (no duration log)
	holder       *pgx.ConnConfig
	notifyHolder *pgx.ConnConfig // synchronous_commit on: its COMMIT waits for the standby
	canceler     *pgx.ConnConfig
}

// i519Case is one probe form: the lock the holder takes (i519_hold's kind, or
// "notify" for the notify lock), the probe's write of row k, how to tell the
// write landed, and the classification that applies to the probe's error.
type i519Case struct {
	kind    string
	probe   func(ctx context.Context, conn *pgx.Conn, k int) error
	written func(t *testing.T, direct *pgxpool.Pool, k int) bool
	outcome func(error) string
}

// i519Stair walks the release offset (seconds after the probe's lock timer
// started): down after a run the timer won (55P03), up after one the grant
// won, the step halving at each turn and doubling after three steps the same
// way (the host's load moved the edge).
type i519Stair struct {
	at, step, minStep, maxStep float64
	up                         bool
	same                       int
}

func (s *i519Stair) next(timerWon bool) {
	goUp := !timerWon
	if goUp != s.up {
		s.step, s.up, s.same = math.Max(s.minStep, s.step/2), goUp, 0
	} else if s.same++; s.same >= 3 {
		s.step, s.same = math.Min(s.maxStep, 2*s.step), 0
	}
	if goUp {
		s.at += s.step
	} else {
		s.at -= s.step
	}
}

// i519Tally counts the runs at one edge.
type i519Tally struct {
	name                      string
	runs, clean, cleanNoRow   int
	timeouts, timeoutsWithRow int // 55P03, and 55P03 with the write landed
	timeoutsAtCommit          int // 55P03 as the answer to a bracket's COMMIT
	misclassified             int // 55P03 the case's classification does not call unknown
	late, unseen              int // the holder saw the probe wait too late, or never
	spill, spillAfterWrite    int // the next command failed; after a write that landed without an error
	kinds                     map[string]int
	offsets, hitOffsets       []float64 // µs after the lock timer
	hits, spills              []*pgconn.PgError
}

func newI519Tally(name string) *i519Tally {
	return &i519Tally{name: name, kinds: map[string]int{}}
}

func (c *i519Tally) log(t *testing.T) {
	t.Helper()
	t.Logf("TALLY %s: runs=%d without_error=%d without_error_without_row=%d lock_timeout=%d lock_timeout_with_row=%d lock_timeout_at_commit=%d not_unknown=%d holder_late=%d holder_unseen=%d next_command_failed=%d of_them_after_a_landed_write=%d",
		c.name, c.runs, c.clean, c.cleanNoRow, c.timeouts, c.timeoutsWithRow, c.timeoutsAtCommit, c.misclassified, c.late, c.unseen, c.spill, c.spillAfterWrite)
	keys := make([]string, 0, len(c.kinds))
	for k := range c.kinds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("TALLY %s:   %d x %s", c.name, c.kinds[k], k)
	}
	if len(c.offsets) > 0 {
		o := slices.Clone(c.offsets)
		slices.Sort(o)
		t.Logf("TALLY %s: release offset µs min %.1f p10 %.1f p50 %.1f p90 %.1f max %.1f; runs with 55P03 and the row at %.1f",
			c.name, o[0], o[len(o)/10], o[len(o)/2], o[len(o)*9/10], o[len(o)-1], c.hitOffsets)
	}
}

// add counts a run: err is what the probe got, row whether its write landed,
// report the holder's (nil: never saw the wait; true: saw it late).
func (c *i519Tally) add(t *testing.T, cs i519Case, err error, row bool, report *bool, at float64) {
	t.Helper()
	c.runs++
	switch {
	case report == nil:
		c.unseen++
	case *report:
		c.late++
	default:
		c.offsets = append(c.offsets, at*1e6)
	}
	if err == nil {
		if row {
			c.clean++
		} else {
			c.cleanNoRow++
		}
		return
	}
	atCommit := pgxdb.FailedAtCommit(err)
	got := cs.outcome(err)
	c.kinds[fmt.Sprintf("%s row=%t at_commit=%t outcome=%s", i505Kind(err), row, atCommit, got)]++
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		return
	}
	c.timeouts++
	if atCommit {
		c.timeoutsAtCommit++
	}
	if got != writeUnknown {
		c.misclassified++
	}
	if row {
		c.timeoutsWithRow++
		c.hitOffsets = append(c.hitOffsets, at*1e6)
		c.hits = append(c.hits, pgErr)
		t.Logf("55P03 with the write landed (offset %.1f µs, at_commit=%t, outcome=%s): %+v", at*1e6, atCommit, got, *pgErr)
	}
}

// i519AwaitWait waits until pid's backend shows waitEvent.
func i519AwaitWait(t *testing.T, direct *pgxpool.Pool, pid uint32, waitEvent string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := direct.QueryRow(context.Background(),
			`SELECT count(*)::int FROM pg_stat_activity WHERE pid = $1 AND wait_event = $2`, int(pid), waitEvent).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend %d never waited on %s", pid, waitEvent)
		}
		time.Sleep(100 * time.Microsecond)
	}
}

// i519Holder is the holder's side of a run.
type i519Holder struct {
	conn, canceler *pgx.Conn
	kind           string
}

// i519Report is what the holder (or the canceler) says about its release.
type i519Report struct {
	missed *bool
	err    error
}

// start takes the holder's lock for row k and arms its release at `after`
// seconds past the probe's lock timer; done waits for the release.
func (h *i519Holder) start(t *testing.T, direct *pgxpool.Pool, k int, probePID uint32, after float64) (done func() (*bool, error)) {
	t.Helper()
	ctx := context.Background()
	pid := h.conn.PgConn().PID()
	if h.kind != "notify" {
		holdc := make(chan i519Report, 1)
		go func() {
			var r i519Report
			r.err = h.conn.QueryRow(ctx, fmt.Sprintf(`SELECT i519_hold('%s', %d, %d, %.9f)`, h.kind, k, probePID, after)).Scan(&r.missed)
			holdc <- r
		}()
		i519AwaitWait(t, direct, pid, "PgSleep")
		return func() (*bool, error) {
			r := <-holdc
			if r.err != nil {
				return nil, fmt.Errorf("holder: %w", r.err)
			}
			return r.missed, nil
		}
	}
	// The notify lock: the holder's COMMIT takes it in PreCommit_Notify and
	// waits for the standby after its commit record; the canceler ends that.
	if _, err := h.conn.Exec(ctx, fmt.Sprintf(`BEGIN; INSERT INTO i519_notify VALUES (%d)`, -k)); err != nil {
		t.Fatal(err)
	}
	commitc := make(chan error, 1)
	go func() {
		_, err := h.conn.Exec(ctx, `COMMIT`)
		commitc <- err
	}()
	i519AwaitWait(t, direct, pid, "SyncRep")
	cancelc := make(chan i519Report, 1)
	go func() {
		var r i519Report
		r.err = h.canceler.QueryRow(ctx, fmt.Sprintf(`SELECT i519_cancel_at(%d, %d, %.9f)`, pid, probePID, after)).Scan(&r.missed)
		cancelc <- r
	}()
	return func() (*bool, error) {
		r := <-cancelc
		if err := <-commitc; err != nil {
			return nil, fmt.Errorf("holder COMMIT: %w", err)
		}
		if r.err != nil {
			return nil, fmt.Errorf("canceler: %w", r.err)
		}
		return r.missed, nil
	}
}

// i519Rig is one probe connection with its holder.
type i519Rig struct {
	env   *i519Env
	c     i519Case
	probe *pgx.Conn
	h     *i519Holder
}

func newI519Rig(t *testing.T, env *i519Env, probeCfg *pgx.ConnConfig, c i519Case) *i519Rig {
	t.Helper()
	r := &i519Rig{env: env, c: c, probe: i505Connect(t, probeCfg), h: &i519Holder{kind: c.kind}}
	if c.kind == "notify" {
		i519SyncStandby(t, env.direct)
		r.h.conn, r.h.canceler = i505Connect(t, env.notifyHolder), i505Connect(t, env.canceler)
	} else {
		r.h.conn = i505Connect(t, env.holder)
	}
	return r
}

// i519Run is one run's result: the probe's error, whether its write landed,
// the holder's report, and the error of the probe connection's next command.
type i519Run struct {
	err, next error
	row       bool
	report    *bool
}

// run is one run on row k with the release `offset` seconds after the
// probe's lock timer fires.
func (r *i519Rig) run(t *testing.T, k int, offset float64) i519Run {
	t.Helper()
	ctx := context.Background()
	if r.c.kind == "row" {
		if _, err := r.env.direct.Exec(ctx, `INSERT INTO i519_row (id) VALUES ($1)`, k); err != nil {
			t.Fatal(err)
		}
	}
	done := r.h.start(t, r.env.direct, k, r.probe.PgConn().PID(), i519LockTimeout.Seconds()+offset)
	var res i519Run
	res.err = r.c.probe(ctx, r.probe, k)
	report, err := done()
	if err != nil {
		t.Fatalf("row %d (offset %.1f µs): %v; the probe returned %v", k, offset*1e6, err, res.err)
	}
	res.report = report
	_, res.next = r.probe.Exec(ctx, `SELECT 1`)
	res.row = r.c.written(t, r.env.direct, k)
	return res
}

// i519Edge runs c `runs` times about the edge and returns the tally. Row ids
// are base+i.
func i519Edge(t *testing.T, env *i519Env, probeCfg *pgx.ConnConfig, name string, c i519Case, runs, base int) *i519Tally {
	t.Helper()
	rig := newI519Rig(t, env, probeCfg, c)
	stair := &i519Stair{at: -400e-6, step: 100e-6, minStep: 1e-6, maxStep: 400e-6}
	tally := newI519Tally(name)
	for i := range runs {
		res := rig.run(t, base+i, stair.at)
		tally.add(t, c, res.err, res.row, res.report, stair.at)
		if res.next != nil {
			tally.spill++
			if res.err == nil && res.row {
				tally.spillAfterWrite++
			}
			if pgErr := new(pgconn.PgError); errors.As(res.next, &pgErr) {
				tally.spills = append(tally.spills, pgErr)
			}
		}
		if res.report != nil && !*res.report {
			stair.next(res.err != nil)
		}
	}
	tally.log(t)
	return tally
}

// i519Written reports whether row k landed: in table, or for i519_row the
// probe's update of it.
func i519Written(table string) func(t *testing.T, direct *pgxpool.Pool, k int) bool {
	q := `SELECT count(*)::int FROM ` + table + ` WHERE id = $1`
	if table == "i519_row" {
		q = `SELECT count(*)::int FROM i519_row WHERE id = $1 AND v = 1`
	}
	return func(t *testing.T, direct *pgxpool.Pool, k int) bool {
		t.Helper()
		var n int
		if err := direct.QueryRow(context.Background(), q, k).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
}

// i519Statement is a probe that runs sql (%d: the row) as one statement in
// the simple protocol: autocommit, the commit before the CommandComplete and
// the duration log after it.
func i519Statement(sql string) func(ctx context.Context, conn *pgx.Conn, k int) error {
	return func(ctx context.Context, conn *pgx.Conn, k int) error {
		_, err := conn.Exec(ctx, fmt.Sprintf(sql, k))
		return err
	}
}

// i519Bracket is a probe that runs sql inside a labeled write bracket, so a
// failed COMMIT is recognisable (pgxdb.FailedAtCommit).
func i519Bracket(sql string) func(ctx context.Context, conn *pgx.Conn, k int) error {
	return func(ctx context.Context, conn *pgx.Conn, k int) error {
		return pgxdb.Write(ctx, conn, pgxdb.At("probe: write"), func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, fmt.Sprintf(sql, k))
			return err
		})
	}
}

// i519SyncStandby names a synchronous standby that never connects, so a
// commit with synchronous_commit=on waits after its commit record, its locks
// held, until it is cancelled; reset when the (sub)test ends.
func i519SyncStandby(t *testing.T, direct *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, s := range []string{`ALTER SYSTEM SET synchronous_standby_names = 'i519_never'`, `SELECT pg_reload_conf()`} {
		if _, err := direct.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, s := range []string{`ALTER SYSTEM RESET synchronous_standby_names`, `SELECT pg_reload_conf()`} {
			if _, err := direct.Exec(ctx, s); err != nil {
				t.Errorf("%s: %v", s, err)
			}
		}
	})
	time.Sleep(200 * time.Millisecond) // the postmaster passes the reload on asynchronously
}

func i519Setup(t *testing.T) *i519Env {
	t.Helper()
	dsn := fatalContainer(t)
	ctx := context.Background()
	direct, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(direct.Close)
	if _, err := direct.Exec(ctx, i519Schema); err != nil {
		t.Fatal(err)
	}
	var version string
	if err := direct.QueryRow(ctx, `SELECT version()`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Logf("server: %s", version)
	probe := map[string]string{
		"application_name":   "i519-probe",
		"commit_delay":       "0",
		"lock_timeout":       strconv.FormatInt(i519LockTimeout.Milliseconds(), 10),
		"synchronous_commit": "off",
	}
	// The widened window: after the grant ProcSleep builds the log_lock_waits
	// text (the lock's holders, under its partition lock) before it disarms
	// the timers; with the message filtered out no errfinish checks for
	// interrupts after it — and the duration log is filtered out with it.
	wide := map[string]string{"log_lock_waits": "on", "deadlock_timeout": "1", "log_min_messages": "fatal"}
	for k, v := range probe {
		wide[k] = v
	}
	return &i519Env{
		direct:    direct,
		probe:     i505Config(t, dsn, probe),
		wideProbe: i505Config(t, dsn, wide),
		holder: i505Config(t, dsn, map[string]string{
			"application_name":           "i519-holder",
			"commit_delay":               "0",
			"log_min_duration_statement": "-1",
			"synchronous_commit":         "off",
		}),
		notifyHolder: i505Config(t, dsn, map[string]string{
			"application_name":           "i519-holder",
			"commit_delay":               "0",
			"log_min_duration_statement": "-1",
			"synchronous_commit":         "on",
			"client_min_messages":        "error",
			"log_min_messages":           "error",
		}),
		canceler: i505Config(t, dsn, map[string]string{
			"application_name":           "i519-canceler",
			"log_min_duration_statement": "-1",
		}),
	}
}

// i519SameError reports whether two errors carry the same code, severity,
// message and raise site.
func i519SameError(a, b *pgconn.PgError) bool {
	return a.Code == b.Code && a.SeverityUnlocalized == b.SeverityUnlocalized && a.Message == b.Message &&
		a.Routine == b.Routine && a.File == b.File && a.Line == b.Line
}

// The probe forms. Statement probes are classified by statementOutcome, bracket
// probes by writeOutcome.
var (
	i519InStatement = map[string]i519Case{
		"row lock, autocommit UPDATE": {kind: "row", probe: i519Statement(`UPDATE i519_row SET v = v + 1 WHERE id = %d`),
			written: i519Written("i519_row"), outcome: statementOutcome},
		"unique conflict, autocommit INSERT": {kind: "unique", probe: i519Statement(`INSERT INTO i519_unique VALUES (%d)`),
			written: i519Written("i519_unique"), outcome: statementOutcome},
		"row lock, UPDATE in a bracket": {kind: "row", probe: i519Bracket(`UPDATE i519_row SET v = v + 1 WHERE id = %d`),
			written: i519Written("i519_row"), outcome: writeOutcome},
	}
	i519AtCommit = map[string]i519Case{
		"deferred unique check, autocommit INSERT": {kind: "deferred", probe: i519Statement(`INSERT INTO i519_deferred VALUES (%d)`),
			written: i519Written("i519_deferred"), outcome: statementOutcome},
		"deferred unique check, bracket COMMIT": {kind: "deferred", probe: i519Bracket(`INSERT INTO i519_deferred VALUES (%d)`),
			written: i519Written("i519_deferred"), outcome: writeOutcome},
		"notify lock, autocommit INSERT": {kind: "notify", probe: i519Statement(`INSERT INTO i519_notify VALUES (%d)`),
			written: i519Written("i519_notify"), outcome: statementOutcome},
		"notify lock, bracket COMMIT": {kind: "notify", probe: i519Bracket(`INSERT INTO i519_notify VALUES (%d)`),
			written: i519Written("i519_notify"), outcome: writeOutcome},
	}
)

func i519Names(cases map[string]i519Case) []string {
	names := make([]string, 0, len(cases))
	for n := range cases {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func TestWriteOutcomeLockTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	env := i519Setup(t)
	runs := i519Runs(t)
	base := 0
	nextBase := func() int { base += 1_000_000; return base }

	// The 55P03 of a wait the timer cut before the grant, at each kind of
	// commit-time wait: what a 55P03 after the commit has to be told from.
	cut := map[string]*pgconn.PgError{}
	for _, name := range i519Names(i519AtCommit) {
		c := i519AtCommit[name]
		t.Run(name+", the timer before the grant", func(t *testing.T) {
			rig := newI519Rig(t, env, env.probe, c)
			res := rig.run(t, nextBase(), 50e-3)
			wantPgError(t, res.err, "ERROR", "55P03")
			if res.row || res.next != nil {
				t.Fatalf("row=%t, next command %v: want no write and a clean connection", res.row, res.next)
			}
			cut[c.kind] = i505PgError(t, res.err)
			if got := c.outcome(res.err); got != writeUnknown {
				t.Errorf("outcome = %q, want unknown: the same 55P03 follows a commit at the edge", got)
			}
		})
	}

	for _, name := range i519Names(i519InStatement) {
		c := i519InStatement[name]
		t.Run(name, func(t *testing.T) {
			tally := i519Edge(t, env, env.probe, name, c, runs, nextBase())
			if tally.cleanNoRow != 0 || tally.timeoutsWithRow != 0 || tally.spill != 0 {
				t.Errorf("%d runs without error left no write, %d with 55P03 left one, %d failed the next command: a check follows the wait inside a statement",
					tally.cleanNoRow, tally.timeoutsWithRow, tally.spill)
			}
		})
	}

	for _, name := range i519Names(i519AtCommit) {
		c := i519AtCommit[name]
		t.Run(name, func(t *testing.T) {
			tally := i519Edge(t, env, env.probe, name, c, runs, nextBase())
			if tally.cleanNoRow != 0 {
				t.Errorf("%d runs returned no error and left no write", tally.cleanNoRow)
			}
			if tally.misclassified != 0 {
				t.Errorf("%d of %d 55P03 classified other than unknown (%d of them after the write landed)",
					tally.misclassified, tally.timeouts, tally.timeoutsWithRow)
			}
			for _, e := range tally.hits {
				if cut[c.kind] != nil && !i519SameError(e, cut[c.kind]) {
					t.Errorf("55P03 after the commit %+v differs from the cut one %+v", *e, *cut[c.kind])
				}
			}
		})
	}

	// The widened window, for the deferred check: the held cancel survives the
	// commit and answers the connection's next command. A run of no such
	// survivor says the host's timing missed the window, not that it is shut.
	for _, name := range []string{"deferred unique check, autocommit INSERT", "deferred unique check, bracket COMMIT"} {
		c := i519AtCommit[name]
		t.Run(name+", widened window", func(t *testing.T) {
			tally := i519Edge(t, env, env.wideProbe, name+", widened window", c, runs, nextBase())
			if tally.spillAfterWrite == 0 {
				t.Skipf("no 55P03 survived a commit in %d runs under the host's load", runs)
			}
			for _, e := range tally.spills {
				if cut[c.kind] != nil && !i519SameError(e, cut[c.kind]) {
					t.Errorf("the next command's error %+v differs from the cut 55P03 %+v", *e, *cut[c.kind])
				}
			}
		})
	}
}
