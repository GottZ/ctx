//go:build integration

// Issue #518, the background digest: a run that fails on a value the database
// cannot store is not retried on every tick. After issue #518's cap the topic
// map no longer reaches the database's limit, so the failure is injected — a
// trigger that refuses the map's row with the SQLSTATE under test and counts
// every attempt in a sequence (nextval survives the rollback), so each run
// that reached the write is counted at the database, not inferred from logs.
//
//	go test -tags=integration ./internal/events/ -run 'TestBackgroundDigest' -count=1 -v
package events

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

type holdLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *holdLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// count returns how many records contain all of the given parts.
func (l *holdLog) count(parts ...string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range strings.Split(l.buf.String(), "\n") {
		all := line != ""
		for _, p := range parts {
			all = all && strings.Contains(line, p)
		}
		if all {
			n++
		}
	}
	return n
}

// holdFixture is a scheduler over a test database with one block in
// "private" and a trigger that refuses the topic map's row with code.
// attempts reads how many writes of the map the trigger refused.
func holdFixture(t *testing.T, code string) (s *Scheduler, pool *pgxpool.Pool, attempts func() int64, logs *holdLog) {
	t.Helper()
	pool = testdb.SetupTestDB(t)
	ctx := context.Background()
	if _, err := store.UpsertBlock(ctx, pool, "decisions", "hold fixture", "body", nil, nil, "private", true,
		store.SensitivityWrite{}, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE SEQUENCE digest_attempts;
		CREATE FUNCTION digest_refuse() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM nextval('digest_attempts');
			RAISE EXCEPTION 'injected refusal of the topic map' USING ERRCODE = TG_ARGV[0];
		END $$;
		CREATE TRIGGER trg_digest_refuse BEFORE INSERT OR UPDATE ON context_blocks
			FOR EACH ROW WHEN (NEW.title LIKE 'topic-map-%') EXECUTE FUNCTION digest_refuse('`+code+`');`); err != nil {
		t.Fatalf("inject: %v", err)
	}
	attempts = func() int64 {
		var last int64
		var called bool
		if err := pool.QueryRow(ctx, `SELECT last_value, is_called FROM digest_attempts`).Scan(&last, &called); err != nil {
			t.Fatalf("read attempts: %v", err)
		}
		if !called {
			return 0
		}
		return last
	}

	reg := blocktype.NewRegistry()
	bctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	reg.Boot(bctx, pool)

	s = NewScheduler(pool, config.NewStore(captureTestConfig(t, 12)), backends.NewPool(nil, nil), StartupConfig{})
	s.SetBlocktypeRegistry(reg)
	s.backgroundTenantsFn = func(context.Context) []backgroundTenant {
		return []backgroundTenant{{scope: store.GlobalScope}}
	}

	logs = &holdLog{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return s, pool, attempts, logs
}

func digestPending(s *Scheduler) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.digestPending
}

// TestBackgroundDigestHoldsAFailureThatCannotHeal is the RED of the
// background half of issue #518. Before, a run refused with a data or limit
// error left digestPending set, so every tick after the debounce ran the
// whole digest again into the same refusal, with an ERROR line each time:
// three runs, three attempts at the database, three ERROR lines. Now the
// first run holds the tenant — one attempt, one ERROR line — until a block is
// written; that write buys one more run, which fails without a second ERROR;
// once the database takes the map, an INFO says so.
func TestBackgroundDigestHoldsAFailureThatCannotHeal(t *testing.T) {
	s, pool, attempts, logs := holdFixture(t, "54000")
	ctx := context.Background()

	s.NotifyWrite()
	for i := 0; i < 3; i++ { // the first run and the two ticks after it
		s.runDigest(ctx)
	}
	if got := attempts(); got != 1 {
		t.Fatalf("three runs without a write between them reached the database %d times, want 1", got)
	}
	if digestPending(s) {
		t.Error("digestPending still set after a failure that cannot heal — every tick runs the digest again")
	}
	if got := logs.count("level=ERROR"); got != 1 {
		t.Errorf("%d ERROR lines for one failure that cannot heal, want 1:\n%s", got, logs.buf.String())
	}
	if logs.count("level=ERROR", "sqlstate=54000", "not retried until a block is written or a setting changes") != 1 {
		t.Errorf("the ERROR line does not name the code and when the digest runs again:\n%s", logs.buf.String())
	}

	// A block is written: one more run, failing the same way — no new ERROR.
	s.NotifyWrite()
	s.runDigest(ctx)
	s.runDigest(ctx)
	if got := attempts(); got != 2 {
		t.Fatalf("after a write the digest reached the database %d times in all, want 2", got)
	}
	if got := logs.count("level=ERROR"); got != 1 {
		t.Errorf("the same failure after a write logged a second ERROR (%d in all)", got)
	}
	if logs.count("level=DEBUG", "failed again") != 1 {
		t.Errorf("the repeated failure has no DEBUG line:\n%s", logs.buf.String())
	}

	// The database takes the map again.
	if _, err := pool.Exec(ctx, `DROP TRIGGER trg_digest_refuse ON context_blocks`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	s.NotifyWrite()
	s.runDigest(ctx)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE title = 'topic-map-private' AND NOT is_archived`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("topic map after the refusal ended: %d rows (err %v), want 1", n, err)
	}
	if logs.count("level=INFO", "digest writes the topic map again") != 1 {
		t.Errorf("no INFO line for the recovery:\n%s", logs.buf.String())
	}
	if digestPending(s) {
		t.Error("digestPending still set after the recovery")
	}
}

// TestBackgroundDigestRetriesAFailureThatCanHeal: a failure that can go
// differently on the next tick — here a deadlock, 40P01 — keeps the retry it
// always had: digestPending stays set and each run reaches the database.
func TestBackgroundDigestRetriesAFailureThatCanHeal(t *testing.T) {
	s, _, attempts, logs := holdFixture(t, "40P01")
	ctx := context.Background()

	s.NotifyWrite()
	s.runDigest(ctx)
	s.runDigest(ctx)
	if got := attempts(); got != 2 {
		t.Fatalf("two runs after a deadlock reached the database %d times, want 2", got)
	}
	if !digestPending(s) {
		t.Error("digestPending cleared after a failure that can heal — the next tick would not retry")
	}
	if got := logs.count("level=ERROR", "scheduler: digest error"); got != 2 {
		t.Errorf("%d ERROR lines for two failed runs, want 2:\n%s", got, logs.buf.String())
	}
}

// TestBackgroundDigestHeldUntilASettingChanges drives the ticker's step: a
// held tenant is not run while nothing changes, and a new configuration
// generation — a setting written, no block — gives it one more run.
func TestBackgroundDigestHeldUntilASettingChanges(t *testing.T) {
	s, _, attempts, _ := holdFixture(t, "22021")
	ctx := context.Background()

	s.NotifyWrite()
	s.mu.Lock()
	s.lastWriteAt = time.Now().Add(-2 * digestDebounce) // the debounce has passed
	s.mu.Unlock()

	s.digestTick(ctx)
	s.digestTick(ctx)
	if got := attempts(); got != 1 {
		t.Fatalf("two ticks reached the database %d times, want 1", got)
	}

	next := *s.cfg.Snapshot()
	if err := s.cfg.Replace(&next); err != nil {
		t.Fatalf("replace config: %v", err)
	}
	s.digestTick(ctx)
	if got := attempts(); got != 2 {
		t.Fatalf("the tick after a setting changed reached the database %d times in all, want 2", got)
	}
	s.digestTick(ctx)
	if got := attempts(); got != 2 {
		t.Fatalf("the tick after that reached it again (%d in all) — the new generation is held too", got)
	}
}

// refuseWith re-points the injected refusal to code.
func refuseWith(t *testing.T, pool *pgxpool.Pool, code string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `CREATE OR REPLACE TRIGGER trg_digest_refuse BEFORE INSERT OR UPDATE ON context_blocks
		FOR EACH ROW WHEN (NEW.title LIKE 'topic-map-%') EXECUTE FUNCTION digest_refuse('`+code+`')`); err != nil {
		t.Fatalf("refuse with %s: %v", code, err)
	}
}

// TestBackgroundDigestWriteDuringAHeldRunEndsTheHold: a block written while
// a held run is under way (after it read the write generation) keeps the
// digest pending, and the next run tries again — a run in which no tenant
// succeeded wrote nothing itself, so the write came from elsewhere.
func TestBackgroundDigestWriteDuringAHeldRunEndsTheHold(t *testing.T) {
	s, _, attempts, _ := holdFixture(t, "54000")
	ctx := context.Background()

	s.NotifyWrite()
	s.runDigest(ctx) // held
	tenants := s.backgroundTenantsFn
	s.backgroundTenantsFn = func(c context.Context) []backgroundTenant {
		s.NotifyWrite() // a write while the run is under way
		return tenants(c)
	}
	s.runDigest(ctx) // skipped: held at the generation it read
	s.backgroundTenantsFn = tenants
	if !digestPending(s) {
		t.Fatal("a write during a held run left the digest not pending — the next tick would not run it")
	}
	s.runDigest(ctx)
	if got := attempts(); got != 2 {
		t.Fatalf("after a write during the held run the digest reached the database %d times in all, want 2", got)
	}
}

// TestBackgroundDigestFailureAfterASettingIsRetried: the run a setting gives
// a held tenant is pending for real — a failure of the other kind in it is
// retried on the next tick — and a failure of the first kind after that is
// not reported a second time: one ERROR for the whole episode.
func TestBackgroundDigestFailureAfterASettingIsRetried(t *testing.T) {
	s, pool, attempts, logs := holdFixture(t, "54000")
	ctx := context.Background()

	s.NotifyWrite()
	s.mu.Lock()
	s.lastWriteAt = time.Now().Add(-2 * digestDebounce)
	s.mu.Unlock()
	s.digestTick(ctx) // held
	refuseWith(t, pool, "40P01")
	next := *s.cfg.Snapshot()
	if err := s.cfg.Replace(&next); err != nil {
		t.Fatalf("replace config: %v", err)
	}
	s.digestTick(ctx) // the setting's run: a deadlock
	if got := attempts(); got != 2 || !digestPending(s) {
		t.Fatalf("after the setting's run failed on a deadlock: %d attempts, pending %v; want 2, true", got, digestPending(s))
	}
	s.digestTick(ctx) // retried
	if got := attempts(); got != 3 {
		t.Fatalf("the deadlock of the setting's run was not retried: %d attempts, want 3", got)
	}
	refuseWith(t, pool, "54000")
	s.digestTick(ctx) // the first kind again
	if got := attempts(); got != 4 {
		t.Fatalf("%d attempts, want 4", got)
	}
	if got := logs.count("level=ERROR", "cannot store"); got != 1 {
		t.Errorf("the failure that cannot heal was reported %d times without a success between, want 1:\n%s", got, logs.buf.String())
	}
	s.digestTick(ctx)
	if got := attempts(); got != 4 {
		t.Errorf("held again, the next tick still reached the database (%d attempts)", got)
	}
}

// TestBackgroundDigestForgetsATenantThatLeft: a held tenant that leaves the
// background register is forgotten, so its state cannot outlive it (a moving
// configuration generation of a tenant nobody runs would keep the ticker
// starting digests); back in the register it starts anew.
func TestBackgroundDigestForgetsATenantThatLeft(t *testing.T) {
	s, _, attempts, logs := holdFixture(t, "54000")
	ctx := context.Background()

	s.NotifyWrite()
	s.runDigest(ctx) // held
	tenants := s.backgroundTenantsFn
	s.backgroundTenantsFn = func(context.Context) []backgroundTenant { return nil }
	s.runDigest(ctx)
	s.digestHold.mu.Lock()
	left := len(s.digestHold.tenants)
	s.digestHold.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d tenants kept after the tenant left the register, want 0", left)
	}
	next := *s.cfg.Snapshot()
	if err := s.cfg.Replace(&next); err != nil {
		t.Fatalf("replace config: %v", err)
	}
	if s.digestHold.configChanged(ctx, s.cfg) {
		t.Error("a tenant that left still makes a setting change start the digest")
	}
	s.backgroundTenantsFn = tenants
	s.runDigest(ctx)
	if got, errs := attempts(), logs.count("level=ERROR", "cannot store"); got != 2 || errs != 2 {
		t.Errorf("back in the register: %d attempts, %d ERROR lines; want 2, 2", got, errs)
	}
}
