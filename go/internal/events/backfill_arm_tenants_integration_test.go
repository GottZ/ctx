//go:build integration

// Issue #228: the embed-backfill arm serves the background tenants one pick
// per turn, and an empty pick made the arm wait embed_backfill.interval
// before the next turn, whatever the other tenants had pending. With N
// tenants of which one has a backlog, that tenant got one block per round
// and the round cost (N-1) intervals. A wire-class memo (backfillFailed)
// paced the same way, so a tenant whose embed backend is down held every
// other tenant's backlog for an interval per failed block.
//
// Grün: the wait is per tenant. A tenant whose pick came back empty, or
// with a wire-class memo of its own backend, is held back for the interval;
// the arm goes on with the tenants that are due and sleeps only when none
// is. A wire failure on a shared backend row still holds the whole arm.
//
// Run: `go test -tags=integration ./internal/events/ -run TestEmbedBackfillArmTenants -count=1 -v`.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/testdb"
)

// seedScopedPending inserts one pending block (no embedding) in scope.
func seedScopedPending(t *testing.T, pool *pgxpool.Pool, scope, title string, age time.Duration) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, created_at, updated_at)
		 VALUES ('learnings', $1, $2, $3, now() - $4::interval, now())`,
		title, "content of "+title, scope, age.String()); err != nil {
		t.Fatalf("seed %s in %s: %v", title, scope, err)
	}
}

// pendingInScope counts the blocks of scope that have no embedding.
func pendingInScope(t *testing.T, pool *pgxpool.Pool, scope string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_blocks WHERE scope = $1 AND embedding IS NULL AND NOT is_archived`,
		scope).Scan(&n); err != nil {
		t.Fatalf("pending in %s: %v", scope, err)
	}
	return n
}

// countedTenants is a backgroundTenantsFn over a fixed list of single-scope
// tenants that counts its calls: one call is one read of the tenant list.
func countedTenants(calls *atomic.Int64, scopes ...string) func(context.Context) []backgroundTenant {
	return func(context.Context) []backgroundTenant {
		calls.Add(1)
		out := make([]backgroundTenant, len(scopes))
		for i, sc := range scopes {
			out[i] = backgroundTenant{scope: sc, owned: []string{sc}}
		}
		return out
	}
}

// arm228Title finds the block title in an embed input (document prefix,
// title, blank line, content).
var arm228Title = regexp.MustCompile(`arm228-[a-z-]+-[0-9]+`)

// recordingEmbedServer serves the ollama /api/embed wire shape after delay
// (the backend's embed time) and records the title of every input, in the
// order the calls arrived.
func recordingEmbedServer(t *testing.T, delay time.Duration) (*httptest.Server, func() []string) {
	t.Helper()
	vec := make([]float64, embed.TargetDims)
	for i := range vec {
		vec[i] = float64((i % 2) * 2)
	}
	var mu sync.Mutex
	var titles []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		time.Sleep(delay)
		mu.Lock()
		titles = append(titles, arm228Title.FindString(req.Input))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{vec}, "prompt_eval_count": 3})
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), titles...)
	}
}

func armTenantsCfg(interval time.Duration) *config.Config {
	return &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
		Interval: interval, MaxTokens: 0, BackoffBase: time.Hour, BackoffCap: 24 * time.Hour,
	}}
}

func TestEmbedBackfillArmTenants_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)

	// The issue's shape: four tenants, only the second has pending blocks.
	// Interval = 1 h, so any wait of the arm outlasts the test by far: a
	// backlog drained within seconds was drained without an interval wait
	// per idle tenant.
	t.Run("idle_tenants_do_not_pace_a_busy_one", func(t *testing.T) {
		defer clearBlocks(t, pool)
		const busy = "arm228-busy"
		const blocks = 6
		for i := range blocks {
			seedScopedPending(t, pool, busy, fmt.Sprintf("arm228-busy-%d", i), time.Duration(blocks-i)*time.Hour)
		}
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedPoolRow("embed-228", headOfLineEmbedServer(t).URL, 100)})
		s := NewScheduler(pool, config.NewStore(armTenantsCfg(time.Hour)), bpool, StartupConfig{DreamEnabled: false})
		s.SetDispatcher(d)
		var reads atomic.Int64
		s.backgroundTenantsFn = countedTenants(&reads, "arm228-idle-a", busy, "arm228-idle-c", "arm228-idle-d")

		armCtx, stop := context.WithCancel(ctx)
		defer stop()
		start := time.Now()
		go s.runEmbedBackfillLoop(armCtx)

		deadline := start.Add(3 * time.Second)
		for pendingInScope(t, pool, busy) > 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if got := pendingInScope(t, pool, busy); got > 0 {
			t.Fatalf("busy tenant still has %d of %d blocks pending after 3 s behind three idle tenants (interval 1 h): an empty pick of one tenant made the arm wait before the next tenant's turn", got, blocks)
		}
		t.Logf("drained %d blocks of the busy tenant in %v with %d tenant-list reads", blocks, time.Since(start).Round(time.Millisecond), reads.Load())
		// The tenant list is read again only when it is older than
		// min(interval, backfillArmPoll): one read for the whole drain, not
		// one per block (1+N queries per embed at N tenants).
		if got := reads.Load(); got > 2 {
			t.Fatalf("tenant list read %d times while draining %d blocks in well under 15 s, want at most 2", got, blocks)
		}
	})

	// A tenant whose embed backend cannot be reached: its wire-class memo
	// holds that tenant for the interval, not the other tenants. The failing
	// tenant's blocks are credentials, which only its own private row may
	// receive; that row is a tenant row, so the egress guard refuses its
	// loopback address — a wire failure, as for a backend that is down. The
	// healthy tenant's blocks are internal and go to the shared
	// no-credentials row.
	t.Run("a_failing_tenant_does_not_hold_the_others", func(t *testing.T) {
		defer clearBlocks(t, pool)
		const failing, healthy = "arm228-down", "arm228-up"
		var failingIDs []string
		for i := range 2 {
			failingIDs = append(failingIDs, seedSensitiveBlock(t, pool,
				fmt.Sprintf("arm228-down-%d", i), "credentials", failing, time.Duration(10-i)*time.Hour))
		}
		for i := range 3 {
			seedSensitiveBlock(t, pool, fmt.Sprintf("arm228-up-%d", i), "internal", healthy, time.Duration(5-i)*time.Hour)
		}
		shared := embedPoolRow("embed-228-shared", headOfLineEmbedServer(t).URL, 10)
		shared.Trust = backends.TrustNoCredentials
		down := embedPoolRow("embed-228-down", "http://127.0.0.1:9", 100)
		down.Scope = failing
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{shared, down})
		s := NewScheduler(pool, config.NewStore(armTenantsCfg(time.Hour)), bpool, StartupConfig{DreamEnabled: false})
		s.SetDispatcher(d)
		var reads atomic.Int64
		s.backgroundTenantsFn = countedTenants(&reads, failing, healthy)

		armCtx, stop := context.WithCancel(ctx)
		defer stop()
		go s.runEmbedBackfillLoop(armCtx)

		deadline := time.Now().Add(3 * time.Second)
		for pendingInScope(t, pool, healthy) > 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if got := pendingInScope(t, pool, healthy); got > 0 {
			t.Fatalf("healthy tenant still has %d of 3 blocks pending after 3 s (interval 1 h): a wire failure of another tenant's backend made the arm wait before the healthy tenant's turn", got)
		}
		// The failing tenant itself stays paced: one probe, so one memo, not
		// its whole backlog walked into memos.
		time.Sleep(500 * time.Millisecond)
		probed := 0
		for _, id := range failingIDs {
			probed += memoCount(t, pool, id)
		}
		if probed != 1 {
			t.Fatalf("failing tenant has %d memoized blocks of 2, want 1 (a wire memo holds its tenant for the interval)", probed)
		}
		if got := pendingInScope(t, pool, failing); got != 2 {
			t.Errorf("failing tenant pending = %d, want 2 (nothing of it was embedded)", got)
		}
	})

	// Every tenant idle: the idle tenants fall due together with each round,
	// and the list is read once per round, not once per tenant per interval.
	t.Run("idle_tenants_share_one_list_read_per_interval", func(t *testing.T) {
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedPoolRow("embed-228-idle", headOfLineEmbedServer(t).URL, 100)})
		s := NewScheduler(pool, config.NewStore(armTenantsCfg(300*time.Millisecond)), bpool, StartupConfig{DreamEnabled: false})
		s.SetDispatcher(d)
		var reads atomic.Int64
		s.backgroundTenantsFn = countedTenants(&reads, "arm228-empty-a", "arm228-empty-b", "arm228-empty-c")

		armCtx, stop := context.WithCancel(ctx)
		go s.runEmbedBackfillLoop(armCtx)
		time.Sleep(1 * time.Second)
		stop()
		// Turns at about 0, 300, 600 and 900 ms; one more for timing slack.
		if got := reads.Load(); got > 5 {
			t.Fatalf("tenant list read %d times in 1 s with three idle tenants at interval 300 ms, want at most 5 (one read per interval for all idle tenants)", got)
		}
	})

	// Two tenants with a backlog each take turns: one block of one, then one
	// of the other, as before the per-tenant pacing.
	t.Run("busy_tenants_take_turns", func(t *testing.T) {
		defer clearBlocks(t, pool)
		const busyA, busyB = "arm228-turn-a", "arm228-turn-b"
		for i := range 3 {
			seedScopedPending(t, pool, busyA, fmt.Sprintf("arm228-turn-a-%d", i), time.Duration(10-i)*time.Hour)
			seedScopedPending(t, pool, busyB, fmt.Sprintf("arm228-turn-b-%d", i), time.Duration(10-i)*time.Hour)
		}
		srv, embeddedTitles := recordingEmbedServer(t, 0)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedPoolRow("embed-228-turns", srv.URL, 100)})
		s := NewScheduler(pool, config.NewStore(armTenantsCfg(time.Hour)), bpool, StartupConfig{DreamEnabled: false})
		s.SetDispatcher(d)
		var reads atomic.Int64
		s.backgroundTenantsFn = countedTenants(&reads, busyA, busyB)

		armCtx, stop := context.WithCancel(ctx)
		defer stop()
		go s.runEmbedBackfillLoop(armCtx)
		deadline := time.Now().Add(3 * time.Second)
		for pendingInScope(t, pool, busyA)+pendingInScope(t, pool, busyB) > 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		got := embeddedTitles()
		want := []string{"arm228-turn-a-0", "arm228-turn-b-0", "arm228-turn-a-1", "arm228-turn-b-1", "arm228-turn-a-2", "arm228-turn-b-2"}
		if !slices.Equal(got, want) {
			t.Fatalf("embed order = %v, want %v (busy tenants take turns)", got, want)
		}
	})

	// A shared backend row (no tenant scope) every tenant routes to is down:
	// the wire memo makes the whole arm wait the interval, so the backend is
	// probed once per interval and not once per tenant with pending blocks.
	t.Run("a_failing_shared_backend_holds_the_whole_arm", func(t *testing.T) {
		defer clearBlocks(t, pool)
		scopes := []string{"arm228-shared-a", "arm228-shared-b", "arm228-shared-c"}
		for _, sc := range scopes {
			for i := range 2 {
				seedScopedPending(t, pool, sc, fmt.Sprintf("%s-%d", sc, i), time.Duration(5-i)*time.Hour)
			}
		}
		var hits atomic.Int64
		down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(down.Close)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedPoolRow("embed-228-shared-down", down.URL, 100)})
		s := NewScheduler(pool, config.NewStore(armTenantsCfg(time.Hour)), bpool, StartupConfig{DreamEnabled: false})
		s.SetDispatcher(d)
		var reads atomic.Int64
		s.backgroundTenantsFn = countedTenants(&reads, scopes...)

		armCtx, stop := context.WithCancel(ctx)
		defer stop()
		go s.runEmbedBackfillLoop(armCtx)
		time.Sleep(1 * time.Second)
		if got := hits.Load(); got != 1 {
			t.Fatalf("shared backend saw %d embed calls in 1 s from three tenants with pending blocks (interval 1 h), want 1 (a shared backend suspect holds the whole arm)", got)
		}
	})

	// A tenant whose backlog ran dry is looked at again within one interval:
	// its empty pick holds it until the next round, not for one interval
	// from the pick, which would land just after that round and cost a
	// second interval. Embed 100 ms, interval 1 s; the new block is written
	// 200 ms after the first one was embedded.
	t.Run("a_drained_tenant_is_looked_at_again_within_one_interval", func(t *testing.T) {
		defer clearBlocks(t, pool)
		const scope = "arm228-trickle"
		seedScopedPending(t, pool, scope, "arm228-trickle-0", time.Hour)
		srv, _ := recordingEmbedServer(t, 100*time.Millisecond)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedPoolRow("embed-228-trickle", srv.URL, 100)})
		s := NewScheduler(pool, config.NewStore(armTenantsCfg(time.Second)), bpool, StartupConfig{DreamEnabled: false})
		s.SetDispatcher(d)
		var reads atomic.Int64
		s.backgroundTenantsFn = countedTenants(&reads, scope)

		armCtx, stop := context.WithCancel(ctx)
		defer stop()
		go s.runEmbedBackfillLoop(armCtx)
		deadline := time.Now().Add(3 * time.Second)
		for pendingInScope(t, pool, scope) > 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if got := pendingInScope(t, pool, scope); got > 0 {
			t.Fatalf("first block still pending after 3 s")
		}
		time.Sleep(200 * time.Millisecond)
		written := time.Now()
		seedScopedPending(t, pool, scope, "arm228-trickle-1", 0)
		deadline = time.Now().Add(5 * time.Second)
		for pendingInScope(t, pool, scope) > 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		took := time.Since(written)
		t.Logf("block written after the drain embedded %v later (interval 1 s)", took.Round(time.Millisecond))
		if took > 1400*time.Millisecond {
			t.Fatalf("block written 200 ms after the drain embedded %v later at interval 1 s, want under 1.4 s (the drained tenant must be due with the next round)", took.Round(time.Millisecond))
		}
	})
}
