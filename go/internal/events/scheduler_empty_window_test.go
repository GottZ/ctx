package events

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
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

// Empty-clamp gate of the background loops: a BOUND tenant (owned != nil)
// whose clamped window is empty — read_scopes ∩ owned = ∅ for the dream loop,
// owned = ∅ for the owned-keyed arms — gets no work at all. Several consumers
// add their scope conjunct only for a non-empty list, so an empty but bound
// set that reached them would turn into an unfiltered, cross-tenant pass.
//
// The tests drive the production loop through its seams (backgroundTenantsFn,
// runCycle, a dead pool) and only use API that predates the gate, so they
// compile against the unfixed tree and fail there on behaviour.

// lockedBuf is a goroutine-safe log sink: the dream loop logs from its own
// goroutine while the test reads.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

const emptyWindowNotice = "read window is empty"

// TestDreamLoopSkipsBoundTenantWithEmptyWindow: tenant "ew-a:main" owns only
// its own scope, read_scopes stays at the default set, so its clamp is empty.
// The loop must never hand RunDreamCycle that empty window (on the unfixed
// tree it does, and PickBlock then picks across all tenants), must keep
// serving the default tenant, and must log the notice once per tenant and
// value — here twice, because the tenant's owned set changes mid-run while
// its window stays empty.
func TestDreamLoopSkipsBoundTenantWithEmptyWindow(t *testing.T) {
	logs := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	defer slog.SetDefault(prev)

	base := captureTestConfig(t, 12)
	base.Scheduler.ReadScopes = []string{"private", "shared", "work"}
	base.Dream.IdleWait = 2 * time.Millisecond

	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{dreamPoolRow("http://dream.example", "dream-model")})
	s := NewScheduler(deadPool(t), config.NewStore(base), bpool, StartupConfig{})

	const switchAt, stopAt = 8, 20
	var calls atomic.Int64
	reached := make(chan struct{})
	var once sync.Once
	s.backgroundTenantsFn = func(context.Context) []backgroundTenant {
		n := calls.Add(1)
		if n >= stopAt {
			once.Do(func() { close(reached) })
		}
		owned := []string{"ew-a:main"}
		if n > switchAt {
			owned = []string{"ew-a:main", "ew-a:dept"} // value change, window still empty
		}
		return []backgroundTenant{
			{scope: "ew-a:main", owned: owned},
			{scope: store.GlobalScope, owned: []string{"private", "shared", "work"}},
		}
	}

	var mu sync.Mutex
	var windows [][]string
	s.runCycle = func(_ context.Context, _ *pgxpool.Pool, _ *dream.Router, _ llm.Options, _ dream.BackoffConfig, readScopes []string, _ dream.Throttle) (int, error) {
		mu.Lock()
		windows = append(windows, slices.Clone(readScopes))
		mu.Unlock()
		return 0, nil // nothing linked: the loop takes the (2 ms) idle wait
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
		t.Fatalf("dream loop iterated only %d times within 20s", calls.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("dream loop did not exit after cancel")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(windows) == 0 {
		t.Fatal("no dream cycle ran — the default tenant must keep dreaming next to the gated one")
	}
	for i, w := range windows {
		if len(w) == 0 {
			t.Fatalf("cycle %d got an empty read window %#v — a bound tenant with an empty clamp reached RunDreamCycle (PickBlock would pick across all tenants)", i, w)
		}
		if !slices.Equal(w, []string{"private", "shared", "work"}) {
			t.Fatalf("cycle %d got window %v, want only the default tenant's [private shared work]", i, w)
		}
	}

	out := logs.String()
	var notices []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, emptyWindowNotice) {
			notices = append(notices, line)
		}
	}
	if len(notices) != 2 {
		t.Fatalf("empty-window notices = %d, want exactly 2 (once per tenant and value; the owned set changed once):\n%s", len(notices), strings.Join(notices, "\n"))
	}
	for _, n := range notices {
		if !strings.Contains(n, "tenant_scope=ew-a:main") || !strings.Contains(n, "scheduler.read_scopes") {
			t.Fatalf("notice must name the tenant scope and point at scheduler.read_scopes: %s", n)
		}
	}
	if !strings.Contains(notices[1], "ew-a:dept") {
		t.Fatalf("second notice must carry the changed owned set: %s", notices[1])
	}
}

// TestBackfillBoundEmptyOwnedIsIdle: the embed-backfill pick (backfillStep)
// adds its scope conjunct only for a non-empty owned set. Both of its callers
// must answer a bound tenant owning nothing without a query — on the unfixed
// tree the pick runs unfiltered (here: against the dead pool, which surfaces
// as an error).
func TestBackfillBoundEmptyOwnedIsIdle(t *testing.T) {
	cfg := captureTestConfig(t, 12)
	s := NewScheduler(deadPool(t), config.NewStore(cfg), backends.NewPool(nil, nil), StartupConfig{})
	ctx := context.Background()

	embedded, err := s.backfillOneEmbedding(ctx, s.newRouter(cfg, "ew-b:main"), cfg, []string{})
	if err != nil || embedded {
		t.Fatalf("backfillOneEmbedding(owned = []) = %v, %v; want false, nil — the pick ran although the tenant owns nothing", embedded, err)
	}
	outcome, err := s.backfillArmOnce(ctx, cfg, backgroundTenant{scope: "ew-b:main", owned: []string{}})
	if err != nil {
		t.Fatalf("backfillArmOnce(owned = []) error = %v — the pick ran although the tenant owns nothing", err)
	}
	if outcome != backfillIdle {
		t.Fatalf("backfillArmOnce(owned = []) = %v, want backfillIdle", outcome)
	}
}

// TestRebuildOverviewBoundEmptyOwnedIsNoop: the overview package reads an
// empty scope filter as the GLOBAL run (global load, TRUNCATE teardown, global
// attempt stamp). A bound tenant owning nothing must not reach it — not even
// the attempt stamp, which on the unfixed tree runs (and counts a failure).
func TestRebuildOverviewBoundEmptyOwnedIsNoop(t *testing.T) {
	cfg := captureTestConfig(t, 12) // graph_overview disabled: the first exit stamps an attempt
	s := NewScheduler(deadPool(t), config.NewStore(cfg), backends.NewPool(nil, nil), StartupConfig{})
	s.rebuildOverviewOnce(context.Background(), backgroundTenant{scope: "ew-c:main", owned: []string{}})
	if n := s.consecutiveOverviewFails.Load(); n != 0 {
		t.Fatalf("overview attempt stamped %d time(s) for a tenant owning nothing — an empty filter is the global run", n)
	}
}
