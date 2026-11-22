package events

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Issue #495: the dream loop of a new installation before the seed. Each
// skipped cycle used to log a WARN (Chain's "backends: empty chain", for the
// dream role) and two INFO lines ("dream: no eligible dream backend — cycle
// skipped before pick", "scheduler: dream idle, waiting") — three of each a
// minute and worker at the default dream.idle_wait. An operator gets one
// line per change of state instead: a WARN when the backend goes missing
// that says what to do, DEBUG for each skip after it, an INFO when a backend
// serves again.
//
// The test drives the production loop (runDreamLoop) through its seams: the
// pool snapshot changes between iterations (backgroundTenantsFn runs once per
// iteration, in the loop's goroutine, before the backend gate), runCycle
// stands in for the pipeline, the log handler records DEBUG too.
func TestDreamLoopReportsAMissingDreamBackendOncePerChange(t *testing.T) {
	logs := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	const idle = 20 * time.Millisecond
	base := captureTestConfig(t, 12)
	base.Scheduler.ReadScopes = []string{"private", "shared", "work"}
	base.Dream.IdleWait = idle

	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{}) // a new installation: no backend at all
	s := NewScheduler(deadPool(t), config.NewStore(base), bpool, StartupConfig{})

	// Iterations 1–6: no backend; 7–10: a dream backend serves; 11–16: gone
	// again (a disable profile, a deleted row — the same empty chain).
	const missingUntil, servedUntil, stopAt = 6, 10, 16
	var iter atomic.Int64
	var mu sync.Mutex
	stamps := map[int64]time.Time{}
	reached := make(chan struct{})
	var once sync.Once
	s.backgroundTenantsFn = func(context.Context) []backgroundTenant {
		n := iter.Add(1)
		mu.Lock()
		stamps[n] = time.Now()
		mu.Unlock()
		switch n {
		case missingUntil + 1:
			bpool.SeedSnapshotForTest([]backends.Backend{dreamPoolRow("http://dream.example", "dream-model")})
		case servedUntil + 1:
			bpool.SeedSnapshotForTest([]backends.Backend{})
		}
		if n >= stopAt {
			once.Do(func() { close(reached) })
		}
		return []backgroundTenant{{scope: store.GlobalScope}}
	}
	var cycles atomic.Int64
	var cyclesWhileMissing atomic.Int64
	s.runCycle = func(_ context.Context, _ *pgxpool.Pool, _ *dream.Router, _ llm.Options, _ dream.BackoffConfig, _ []string, _ dream.Throttle) (int, error) {
		cycles.Add(1)
		if n := iter.Load(); n <= missingUntil || n > servedUntil {
			cyclesWhileMissing.Add(1)
		}
		return 0, nil // nothing linked: the loop takes its idle wait
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.runDreamLoop(ctx)
		close(done)
	}()
	select {
	case <-reached:
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatalf("dream loop iterated only %d times within 20s", iter.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("dream loop did not exit after cancel")
	}

	out := logs.String()
	count := func(sub string) int { return strings.Count(out, sub) }
	warns := count(`level=WARN msg="dream: no backend of role dream`)
	recovered := count(`level=INFO msg="dream: a backend of role dream serves again`)
	emptyChain := count(`backends: empty chain`)
	idleLines := count(`scheduler: dream idle, waiting`)
	skips := count(`level=DEBUG msg="dream: no eligible dream backend`)
	t.Logf("%d iterations: %d WARN missing, %d INFO serves again, %d DEBUG skips, %d empty-chain lines, %d idle lines, %d cycles",
		iter.Load(), warns, recovered, skips, emptyChain, idleLines, cycles.Load())

	if warns != 2 {
		t.Errorf("WARN \"no backend of role dream\" logged %d times, want 2 — once when it went missing, once when it went missing again", warns)
	}
	if recovered != 1 {
		t.Errorf("INFO \"serves again\" logged %d times, want 1 — when the seeded backend served", recovered)
	}
	if emptyChain != 0 {
		t.Errorf("%d \"backends: empty chain\" line(s) — the skip asks the pool through Chain, which logs a WARN per cycle", emptyChain)
	}
	if want := (missingUntil - 1) + (stopAt - servedUntil - 1); skips < want {
		t.Errorf("%d DEBUG skip lines, want at least %d — every skip after the first is a DEBUG line", skips, want)
	}
	if n := cyclesWhileMissing.Load(); n != 0 {
		t.Errorf("%d dream cycle(s) ran while no backend served — the loop must skip before the cycle", n)
	}
	if c := cycles.Load(); c != int64(servedUntil-missingUntil) || int64(idleLines) != c {
		t.Errorf("%d cycles and %d idle lines, want %d of each — a skipped iteration logs no idle line, a served empty one does",
			c, idleLines, servedUntil-missingUntil)
	}
	// The skip takes the idle wait: six missing iterations span at least five
	// waits (a loop without it would run them back to back).
	mu.Lock()
	span := stamps[missingUntil].Sub(stamps[1])
	mu.Unlock()
	if span < (missingUntil-1)*idle {
		t.Errorf("iterations 1–%d took %s, want at least %s — the skip does not take its idle wait", missingUntil, span, (missingUntil-1)*idle)
	}
}

// runDreamLoopUntil drives runDreamLoop until backgroundTenantsFn was asked
// stopAt times, then cancels it; list(n) is the tenant list of iteration n.
func runDreamLoopUntil(t *testing.T, s *Scheduler, stopAt int64, list func(n int64) []backgroundTenant) {
	t.Helper()
	var iter atomic.Int64
	reached := make(chan struct{})
	var once sync.Once
	s.backgroundTenantsFn = func(context.Context) []backgroundTenant {
		n := iter.Add(1)
		if n >= stopAt {
			once.Do(func() { close(reached) })
		}
		return list(n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.runDreamLoop(ctx)
		close(done)
	}()
	select {
	case <-reached:
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatalf("dream loop iterated only %d times within 20s", iter.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("dream loop did not exit after cancel")
	}
}

// TestDreamLoopForgetsTenantsThatLeftTheList: the loop hands the tenant list
// to the backend notice at the start of each round. Tenant "gone" skips on
// its own disabled row (one WARN), leaves the list for a while, comes back
// still skipping — a change of state after it was forgotten, so a WARN
// again. Without the hand-over its old entry would keep it silent and stay
// in the notice for good. The default tenant skips for the shared reason
// throughout: one WARN, never repeated.
func TestDreamLoopForgetsTenantsThatLeftTheList(t *testing.T) {
	logs := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	defer slog.SetDefault(prev)

	base := captureTestConfig(t, 12)
	base.Scheduler.ReadScopes = []string{"private", "shared", "work"}
	base.Dream.IdleWait = 2 * time.Millisecond
	bpool := backends.NewPool(nil, nil)
	// No shared dream row (a shared one would serve "gone" too): "gone" skips
	// on its own disabled row, the default tenant for the shared reason.
	bpool.SeedSnapshotForTest([]backends.Backend{
		{ID: "gone-dream", Name: "gone-dream", Host: "http://gone.example", Protocol: backends.ProtocolOllama,
			Trust: backends.TrustFull, Locality: "lan", Scope: "gone", Roles: []string{backends.RoleDream},
			ModelMap: map[string]backends.ModelSpec{"default": {Model: "m"}}, Priority: 100, Enabled: false},
	})
	s := NewScheduler(deadPool(t), config.NewStore(base), bpool, StartupConfig{})
	s.runCycle = func(_ context.Context, _ *pgxpool.Pool, _ *dream.Router, _ llm.Options, _ dream.BackoffConfig, _ []string, _ dream.Throttle) (int, error) {
		return 0, nil
	}
	both := []backgroundTenant{{scope: "gone"}, {scope: store.GlobalScope}}
	runDreamLoopUntil(t, s, 12, func(n int64) []backgroundTenant {
		if n > 4 && n <= 8 {
			return []backgroundTenant{{scope: store.GlobalScope}} // "gone" left the list
		}
		return both
	})

	if n := strings.Count(logs.String(), `msg="dream: no backend of role dream of this tenant serves`); n != 2 {
		t.Errorf("tenant WARN logged %d times, want 2 — once before it left the list, once after it came back:\n%s", n, logs.String())
	}
	if n := strings.Count(logs.String(), `msg="dream: no backend of role dream —`); n != 1 {
		t.Errorf("shared WARN logged %d times, want once — the default tenant stayed in the list", n)
	}
}
