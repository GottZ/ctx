//go:build integration

// Issue #555, point 4: a guard run that defers verdicts (the matched
// neighbour of its blocks held by another transaction, as a purge or a
// tenant-delete batch holds it) left the guard not pending when it decided
// nothing else, and no tick brought it back: the deferred blocks kept no
// verdict until the next block write. A run that deferred anything now
// leaves the guard pending, so the next tick decides the blocks once the
// holder is gone.
//
//	CTX_P555_GUARD_LOOP=1 go test -tags=integration -p 1 ./internal/events/ -run TestGuardRunsPerMinuteBesideAHolder -v
//
// runs the scheduler's own loop beside a holder of 90 s (opt-in: it takes
// two guard ticks).
package events

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func gpVec(i int, a float32, j int, b float32) []float32 {
	v := make([]float32, 1024)
	v[i], v[j] = a, b
	return v
}

// guardDeferralFixture is a scheduler over a fresh database with a
// neighbour block whose verdict is in, and two blocks to review against it
// that wait for theirs.
type guardDeferralFixture struct {
	pool      *pgxpool.Pool
	s         *Scheduler
	neighbour string
	review    []string
}

func newGuardDeferralFixture(t *testing.T) *guardDeferralFixture {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	s := NewScheduler(pool, config.NewStore(config.Defaults()), backends.NewPool(nil, nil), StartupConfig{})
	s.SetBlocktypeRegistry(reg)
	write := func(title string, vec []float32) string {
		b, err := store.UpsertBlock(ctx, pool, "q555", title, title+" content", nil, nil, "private", true, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.StoreEmbedding(ctx, pool, b.ID, "q555", vec); err != nil {
			t.Fatal(err)
		}
		return b.ID
	}
	f := &guardDeferralFixture{pool: pool, s: s}
	f.neighbour = write("q555 neighbour", gpVec(5, 1, 6, 0))
	if _, err := guard.RunGuardBatch(ctx, pool, reg.Snapshot(), 100, config.Defaults().GuardSearch()); err != nil {
		t.Fatal(err)
	}
	f.review = []string{write("q555 review", gpVec(5, 0.96, 6, 0.28)), write("q555 review two", gpVec(5, 0.96, 7, 0.28))}
	return f
}

// hold holds the neighbour FOR UPDATE, as a purge or prune batch does; the
// returned func ends the hold.
func (f *guardDeferralFixture) hold(t *testing.T) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1 FOR UPDATE`, f.neighbour); err != nil {
		t.Fatal(err)
	}
	return func() { _ = tx.Rollback(ctx) }
}

func (f *guardDeferralFixture) undecided(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM context_blocks WHERE id = ANY($1::uuid[]) AND metadata->>'guard_checked_at' IS NULL`, f.review).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *guardDeferralFixture) pending() bool {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	return f.s.guardPending
}

// tick is what the scheduler loop does on a guard tick (Run).
func (f *guardDeferralFixture) tick() {
	if f.pending() {
		f.s.runGuard(context.Background())
	}
}

func TestGuardDeferredVerdictsKeepTheGuardPending(t *testing.T) {
	f := newGuardDeferralFixture(t)
	f.s.NotifyBlockWrite(f.review[1])
	release := f.hold(t)
	f.tick()
	if n := f.undecided(t); n != 2 {
		release()
		t.Fatalf("the run beside the held neighbour decided %d of the two blocks, want it to defer both", 2-n)
	}
	pendingAfterDefer := f.pending()
	release()
	// No block write from here on: only ticks.
	f.tick()
	left := f.undecided(t)
	t.Logf("pending after the deferring run: %v; blocks without a verdict after the next tick: %d", pendingAfterDefer, left)
	if !pendingAfterDefer {
		t.Errorf("a run that deferred two verdicts left the guard not pending: no tick runs it again until the next write")
	}
	if left != 0 {
		t.Errorf("%d deferred blocks have no verdict after the holder ended and a tick passed, without a write", left)
	}
	if f.tick(); f.pending() {
		t.Errorf("the guard stays pending after a quiet run decided everything")
	}
}

// TestGuardRunsPerMinuteBesideAHolder runs the scheduler's loop (Run, with
// its 60 s guard ticker) beside a holder of 90 s — the first tick falls
// inside the hold — and counts the guard runs: the deferring run comes back
// once per tick, not in a loop, and the tick after the holder ended decides
// the blocks without a write.
func TestGuardRunsPerMinuteBesideAHolder(t *testing.T) {
	if os.Getenv("CTX_P555_GUARD_LOOP") == "" {
		t.Skip("measurement of three guard ticks: set CTX_P555_GUARD_LOOP=1")
	}
	f := newGuardDeferralFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.s.NotifyBlockWrite(f.review[1])
	release := f.hold(t)
	start := time.Now()
	go f.s.Run(ctx)
	runs := map[int64]bool{}
	sample := func(until time.Time) {
		for time.Now().Before(until) {
			if g, _, _ := f.s.LastArmRuns(); !g.IsZero() {
				runs[g.UnixNano()] = true
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	sample(start.Add(90 * time.Second))
	during := len(runs)
	release()
	held := time.Since(start)
	sample(start.Add(125 * time.Second))
	left := f.undecided(t)
	t.Logf("guard runs while the neighbour was held for %s: %d (%.1f per minute); after the hold: %d more; blocks without a verdict at %s, no write since the hold began: %d",
		held.Round(time.Second), during, float64(during)/held.Minutes(), len(runs)-during, time.Since(start).Round(time.Second), left)
	if during > 2 {
		t.Errorf("%d guard runs beside a 60 s holder: a loop, not one run per tick", during)
	}
	if left != 0 {
		t.Errorf("%d deferred blocks without a verdict after the holder ended", left)
	}
}
