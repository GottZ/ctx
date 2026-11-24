//go:build integration

// Issue #547 point 2, the measurement: the prune of a tenant with
// CTX_P547_BIG blocks (100 000 for the issue's comment), with two writers
// beside it — one in the pruned tenant's scope (a block batch's check,
// store.CheckUpsertBlocks: the scope's supersedes lock and an identity row,
// rolled back, so it adds no row the prune would have to chase) and one in a
// scope of ANOTHER tenant (a real one-item block batch). It logs the longest
// transaction of the prune, the writers' longest and 99th-percentile call,
// and the other tenant's writer without a prune beside it. It runs only with
// CTX_P547_BIG set.
//
//	CTX_P547_BIG=100000 go test -tags=integration -p 1 ./internal/store/ -run 'TestTenantPruneAtScale' -v -timeout 30m
package store_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
)

// scaleTxTimer records the duration of every transaction of its pool, from
// its BEGIN to its COMMIT or ROLLBACK.
type scaleTxTimer struct {
	mu    sync.Mutex
	begun map[*pgx.Conn]time.Time
	durs  []time.Duration
}

func (s *scaleTxTimer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	sql := strings.ToLower(strings.TrimSpace(d.SQL))
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case strings.HasPrefix(sql, "begin"):
		s.begun[conn] = time.Now()
	case strings.HasPrefix(sql, "commit"), strings.HasPrefix(sql, "rollback"):
		if t0, ok := s.begun[conn]; ok {
			s.durs = append(s.durs, time.Since(t0))
			delete(s.begun, conn)
		}
	}
	return ctx
}

func (s *scaleTxTimer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func scaleStats(d []time.Duration) (maxD, p99 time.Duration) {
	if len(d) == 0 {
		return 0, 0
	}
	s := slices.Clone(d)
	slices.Sort(s)
	return s[len(s)-1], s[len(s)*99/100]
}

func TestTenantPruneAtScale(t *testing.T) {
	n, err := strconv.Atoi(os.Getenv("CTX_P547_BIG"))
	if err != nil || n <= 0 {
		t.Skip("set CTX_P547_BIG to the tenant's block count")
	}
	f := newP547Fixture(t)
	ctx := context.Background()
	pruned := f.tenant(t, "p547-big", "p547-big-s", "")
	f.tenant(t, "p547-other", "p547-other-s", "")
	t0 := time.Now()
	for done := 0; done < n; {
		k := min(1000, n-done)
		items := make([]store.BatchUpsert, k)
		for i := range items {
			items[i] = store.BatchUpsert{Category: "learnings", Title: fmt.Sprintf("big %d", done+i),
				Content: fmt.Sprintf("content of block %d of the pruned tenant", done+i), Scope: "p547-big-s", ScopeExplicit: true}
		}
		if _, err := store.UpsertBlocks(ctx, f.pool, items, "", f.set, nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
		done += k
	}
	t.Logf("seeded %d blocks in %s", n, time.Since(t0).Round(time.Second))

	writer := func(stop <-chan struct{}, scope string, check bool, out *[]time.Duration) {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			item := []store.BatchUpsert{{Category: "learnings", Title: fmt.Sprintf("w %s %d", scope, i), Content: "a writer's block", Scope: scope, ScopeExplicit: true}}
			start := time.Now()
			var werr error
			if check {
				werr = store.CheckUpsertBlocks(ctx, f.pool, item, "", f.set)
			} else {
				_, werr = store.UpsertBlocks(ctx, f.pool, item, "", f.set, nil)
			}
			if werr != nil {
				t.Errorf("writer %s: %v", scope, werr)
				return
			}
			*out = append(*out, time.Since(start))
			time.Sleep(5 * time.Millisecond)
		}
	}

	// The other tenant's writer alone, as long as the prune will take roughly.
	var alone []time.Duration
	stop := make(chan struct{})
	go func() { time.Sleep(10 * time.Second); close(stop) }()
	writer(stop, "p547-other-s", false, &alone)

	timer := &scaleTxTimer{begun: map[*pgx.Conn]time.Time{}}
	cfg, err := pgxpool.ParseConfig(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = timer
	prunePool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer prunePool.Close()

	var same, other []time.Duration
	stop = make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); writer(stop, "p547-big-s", true, &same) }()
	go func() { defer wg.Done(); writer(stop, "p547-other-s", false, &other) }()
	start := time.Now()
	pruneErr := store.PruneTenant(ctx, prunePool, pruned, store.AuditStamp{})
	took := time.Since(start)
	close(stop)
	wg.Wait()
	if pruneErr != nil {
		t.Fatalf("prune: %v", pruneErr)
	}
	txMax, txP99 := scaleStats(timer.durs)
	sameMax, sameP99 := scaleStats(same)
	otherMax, otherP99 := scaleStats(other)
	aloneMax, aloneP99 := scaleStats(alone)
	t.Logf("prune of %d blocks: %s, %d transactions, longest %s, p99 %s", n, took.Round(time.Millisecond), len(timer.durs), txMax.Round(time.Millisecond), txP99.Round(time.Millisecond))
	t.Logf("writer in the pruned tenant's scope: %d calls, longest %s, p99 %s", len(same), sameMax.Round(time.Millisecond), sameP99.Round(time.Millisecond))
	t.Logf("writer in another tenant's scope: %d calls beside the prune, longest %s, p99 %s; alone %d calls, longest %s, p99 %s",
		len(other), otherMax.Round(time.Millisecond), otherP99.Round(time.Millisecond), len(alone), aloneMax.Round(time.Millisecond), aloneP99.Round(time.Millisecond))
	var left int
	if err := f.pool.QueryRow(ctx, `SELECT count(*)::int FROM context_blocks WHERE scope = 'p547-big-s'`).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d blocks of the pruned tenant left (%v)", left, err)
	}
}
