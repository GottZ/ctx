//go:build integration

// Issue #547, review round 1 (F1): no row of the pruned tenant's scopes
// outlives a prune that reported success, whoever writes meanwhile.
//
// The prune deleted the dream links and the blocks scope by scope; the
// tenant's keys authenticated until its key step, and a write of one of them
// into a scope whose pass had ended stayed — with the tenant row and its
// scope mappings gone, owned by nobody. The writers here are what a request
// is: auth.Authenticate (ctx_auth, the gate every surface passes) and then
// the store write. TestTenantPruneRefusesTheTenantsKeysFromItsStart and
// TestTenantPruneClosingPassTakesAnInFlightWrite hold the prune at a
// statement; TestTenantPruneBesideAuthenticatedWriters runs it free under
// continuous load and counts.
//
//	CTX_P547_LOAD_BLOCKS=24000 go test -tags=integration -p 1 ./internal/store/ -run 'TestTenantPruneBesideAuthenticatedWriters' -v -count=3
package store_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// pruneWritersTenant creates a tenant owning n scopes named prefix-1 … and a
// key of it that may write all of them; it returns the tenant id, the scopes
// and the key's plaintext.
func pruneWritersTenant(t *testing.T, f *p547Fixture, prefix string, n int) (string, []string, string) {
	t.Helper()
	ctx := context.Background()
	tn, err := store.CreateTenant(ctx, f.pool, prefix, prefix)
	if err != nil {
		t.Fatalf("tenant %s: %v", prefix, err)
	}
	scopes := make([]string, n)
	for i := range scopes {
		scopes[i] = fmt.Sprintf("%s-%d", prefix, i+1)
		if _, err := store.AssignTenantScope(ctx, f.pool, tn.ID, scopes[i], nil); err != nil {
			t.Fatalf("scope %s: %v", scopes[i], err)
		}
	}
	_, key, err := store.CreateApiKey(ctx, f.pool, prefix+" key", scopes[0], scopes, tn.ID)
	if err != nil {
		t.Fatalf("key %s: %v", prefix, err)
	}
	return tn.ID, scopes, key
}

// pruneWritersSeed writes n blocks over the scopes through the block batch,
// in chains of ten (each block a child of the one before it, in one scope);
// the third block of every fifth chain is then moved into the next scope (a
// scope move keeps parent_id), so a chain crosses scopes of the tenant.
func pruneWritersSeed(t *testing.T, f *p547Fixture, scopes []string, n int) {
	t.Helper()
	ctx := context.Background()
	var movers []string
	for done := 0; done < n; {
		k := min(500, n-done)
		items := make([]store.BatchUpsert, k)
		for i := range items {
			g := done + i
			items[i] = store.BatchUpsert{Category: "learnings", Title: fmt.Sprintf("seed %d", g),
				Content: fmt.Sprintf("content of seed block %d", g), Scope: scopes[(g/10)%len(scopes)], ScopeExplicit: true}
			if i > 0 && g%10 != 0 {
				prev := i - 1
				items[i].ParentItem = &prev
			}
		}
		res, err := store.UpsertBlocks(ctx, f.pool, items, "", f.set, nil)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		for i, r := range res {
			if g := done + i; g%50 == 2 {
				movers = append(movers, r.Block.ID+"\x00"+scopes[(g/10+1)%len(scopes)])
			}
		}
		done += k
	}
	for _, m := range movers {
		id, to, _ := strings.Cut(m, "\x00")
		if _, _, err := store.UpdateBlock(ctx, f.pool, id, store.UpdateBlockData{Scope: &to}, scopes); err != nil {
			t.Fatalf("seed move: %v", err)
		}
	}
}

// pruneLeftovers counts the rows of the scopes in every scope-carried table
// the prune drains, and whether the tenant row is still there.
func pruneLeftovers(t *testing.T, pool *pgxpool.Pool, tenantID string, scopes []string) (blocks, links, other, tenant int) {
	t.Helper()
	ctx := context.Background()
	q := func(sql string, args ...any) int {
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", sql, err)
		}
		return n
	}
	blocks = q(`SELECT count(*)::int FROM context_blocks WHERE scope = ANY($1::text[])`, scopes)
	links = q(`SELECT count(*)::int FROM context_dream_links WHERE scope = ANY($1::text[])`, scopes)
	other = q(`SELECT (SELECT count(*) FROM context_blobs WHERE scope = ANY($1::text[]))
	                + (SELECT count(*) FROM context_sources WHERE scope = ANY($1::text[]))
	                + (SELECT count(*) FROM context_chat_sessions WHERE scope = ANY($1::text[]))
	                + (SELECT count(*) FROM context_secrets WHERE scope = ANY($1::text[]))`, scopes)
	tenant = q(`SELECT count(*)::int FROM context_tenants WHERE id = $1::uuid`, tenantID)
	return
}

// pruneHold stops the prune right before the first statement match accepts,
// until release is closed; held is closed once it stands there.
type pruneHold struct {
	match   func(sql string) bool
	once    sync.Once
	held    chan struct{}
	release chan struct{}
}

func newPruneHold(match func(sql string) bool) *pruneHold {
	return &pruneHold{match: match, held: make(chan struct{}), release: make(chan struct{})}
}

func (h *pruneHold) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if h.match(d.SQL) {
		h.once.Do(func() {
			close(h.held)
			<-h.release
		})
	}
	return ctx
}

func (h *pruneHold) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func pruneHoldPool(t *testing.T, dsn string, h *pruneHold) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.Tracer = h
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func waitPruneHeld(t *testing.T, h *pruneHold, done <-chan error) {
	t.Helper()
	select {
	case <-h.held:
	case err := <-done:
		t.Fatalf("the prune ended (%v) before it reached the held statement", err)
	case <-time.After(testdb.CommitHoldBound):
		t.Fatalf("the prune did not reach the held statement within %s", testdb.CommitHoldBound)
	}
}

// The prune closes the door first: once it has begun, a key of the tenant no
// longer authenticates, so no request that starts while the prune runs can
// write into one of its scopes.
func TestTenantPruneRefusesTheTenantsKeysFromItsStart(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	tenantID, scopes, key := pruneWritersTenant(t, f, "pw-door", 2)
	f.block(t, scopes[0], "pw door block")
	if ar, err := auth.Authenticate(ctx, f.pool, key); err != nil || !ar.IsValid {
		t.Fatalf("the key does not authenticate before the prune: %+v, %v", ar, err)
	}

	h := newPruneHold(func(sql string) bool { return strings.Contains(sql, "DELETE FROM context_dream_links") })
	done := make(chan error, 1)
	go func() { done <- store.PruneTenant(ctx, pruneHoldPool(t, f.dsn, h), tenantID, store.AuditStamp{}) }()
	waitPruneHeld(t, h, done)
	ar, err := auth.Authenticate(ctx, f.pool, key)
	close(h.release)
	if perr := <-done; perr != nil {
		t.Fatalf("prune: %v", perr)
	}
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if ar.IsValid {
		t.Fatalf("a key of the tenant authenticated while its prune ran (home scope %s): a request could write into a scope the prune has passed", ar.HomeScope)
	}
}

// A writer that holds no gate (an older instance beside this one in a rolling
// deploy; a request write holds its key's gate and is refused after the key
// step) writes into a scope whose pass has ended (the prune held at its last
// pass statement, the projects): the closing pass takes the row before the
// tenant row goes, and the prune reports success with nothing left. Both
// writer kinds: the single store (no scope lock) and the block batch (the
// scope lock).
func TestTenantPruneClosingPassTakesAnInFlightWrite(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	for _, kind := range []string{"single", "batch"} {
		t.Run(kind, func(t *testing.T) {
			tenantID, scopes, key := pruneWritersTenant(t, f, "pw-late-"+kind, 3)
			for i, s := range scopes {
				f.block(t, s, fmt.Sprintf("pw late %s %d", kind, i))
			}
			ar, err := auth.Authenticate(ctx, f.pool, key)
			if err != nil || !ar.IsValid {
				t.Fatalf("the key does not authenticate: %+v, %v", ar, err)
			}
			h := newPruneHold(func(sql string) bool { return strings.Contains(sql, "DELETE FROM context_projects WHERE tenant_id") })
			done := make(chan error, 1)
			go func() { done <- store.PruneTenant(ctx, pruneHoldPool(t, f.dsn, h), tenantID, store.AuditStamp{}) }()
			waitPruneHeld(t, h, done)
			if n, _, _, _ := pruneLeftovers(t, f.pool, tenantID, scopes); n != 0 {
				t.Fatalf("%d blocks left when the prune reached its last pass statement", n)
			}
			// The request that authenticated before the prune writes now.
			if kind == "single" {
				_, err = store.UpsertBlock(ctx, f.pool, "learnings", "pw late write", "written by a request in flight", nil, nil, scopes[0], true, store.SensitivityWrite{}, "")
			} else {
				_, err = store.UpsertBlocks(ctx, f.pool, []store.BatchUpsert{
					{Category: "learnings", Title: "pw late write a", Content: "in flight", Scope: scopes[0], ScopeExplicit: true},
					{Category: "learnings", Title: "pw late write b", Content: "in flight", Scope: scopes[2], ScopeExplicit: true},
				}, ar.ApiKeyID, f.set, nil)
			}
			if err != nil {
				t.Fatalf("in-flight write: %v", err)
			}
			close(h.release)
			if perr := <-done; perr != nil {
				t.Fatalf("prune: %v", perr)
			}
			blocks, links, other, tenant := pruneLeftovers(t, f.pool, tenantID, scopes)
			if blocks+links+other+tenant != 0 {
				t.Fatalf("after a successful prune: %d blocks, %d links, %d other rows in the tenant's scopes, tenant row %d — want none", blocks, links, other, tenant)
			}
		})
	}
}

func pruneLoadBlocks() int {
	n, err := strconv.Atoi(os.Getenv("CTX_P547_LOAD_BLOCKS"))
	if err != nil || n <= 0 {
		return 4000
	}
	return n
}

// pidTracer records the backend pids of its pool.
type pidTracer struct {
	mu   sync.Mutex
	pids map[uint32]struct{}
}

func (p *pidTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	p.mu.Lock()
	p.pids[conn.PgConn().PID()] = struct{}{}
	p.mu.Unlock()
	return ctx
}

func (p *pidTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (p *pidTracer) list() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]int64, 0, len(p.pids))
	for pid := range p.pids {
		out = append(out, int64(pid))
	}
	return out
}

func pidPool(t *testing.T, dsn string, tr *pidTracer, maxConns int32) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = maxConns
	cfg.ConnConfig.Tracer = tr
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// pruneWriterStats counts the calls of the writers that started while the
// prune ran.
type pruneWriterStats struct {
	during, accepted, refused atomic.Int64
}

// The prune under continuous load of the tenant's own requests (each one
// authenticates, then writes: four single stores, one block batch over two
// scopes) beside a block batch of ANOTHER tenant: nothing of the tenant's
// scopes is left after it reports success, and the other tenant's writer
// is never blocked by a prune backend (sampled every millisecond through
// pg_blocking_pids).
func TestTenantPruneBesideAuthenticatedWriters(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	tenantID, scopes, key := pruneWritersTenant(t, f, "pw-load", 8)
	f.tenant(t, "pw-other", "pw-other-s", "")
	pruneWritersSeed(t, f, scopes, pruneLoadBlocks())

	var stats pruneWriterStats
	var pruning atomic.Bool
	stop := make(chan struct{})
	var wg sync.WaitGroup
	tenantWriter := func(name string, write func(ar *auth.AuthResult, i int) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				during := pruning.Load()
				ar, err := auth.Authenticate(ctx, f.pool, key)
				switch {
				case err != nil:
					t.Errorf("%s: authenticate: %v", name, err)
					return
				case !ar.IsValid:
					if during {
						stats.during.Add(1)
						stats.refused.Add(1)
					}
				default:
					if err := write(ar, i); err != nil {
						t.Errorf("%s: write: %v", name, err)
						return
					}
					if during {
						stats.during.Add(1)
						stats.accepted.Add(1)
					}
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	for w := range 4 {
		tenantWriter(fmt.Sprintf("store %d", w), func(_ *auth.AuthResult, i int) error {
			_, err := store.UpsertBlock(ctx, f.pool, "learnings", fmt.Sprintf("pw store %d %d", w, i), "a request's block", nil, nil,
				scopes[(w+i)%len(scopes)], true, store.SensitivityWrite{}, "")
			return err
		})
	}
	tenantWriter("batch", func(ar *auth.AuthResult, i int) error {
		_, err := store.UpsertBlocks(ctx, f.pool, []store.BatchUpsert{
			{Category: "learnings", Title: fmt.Sprintf("pw batch a %d", i), Content: "a request's block", Scope: scopes[i%len(scopes)], ScopeExplicit: true},
			{Category: "learnings", Title: fmt.Sprintf("pw batch b %d", i), Content: "a request's block", Scope: scopes[(i+3)%len(scopes)], ScopeExplicit: true},
		}, ar.ApiKeyID, f.set, nil)
		return err
	})

	otherTr := &pidTracer{pids: map[uint32]struct{}{}}
	otherPool := pidPool(t, f.dsn, otherTr, 1)
	var otherCalls atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := store.UpsertBlocks(ctx, otherPool, []store.BatchUpsert{{Category: "learnings",
				Title: fmt.Sprintf("pw other %d", i), Content: "another tenant's block", Scope: "pw-other-s", ScopeExplicit: true}}, "", f.set, nil); err != nil {
				t.Errorf("other tenant's writer: %v", err)
				return
			}
			otherCalls.Add(1)
			time.Sleep(time.Millisecond)
		}
	}()

	pruneTr := &pidTracer{pids: map[uint32]struct{}{}}
	prunePool := pidPool(t, f.dsn, pruneTr, 2)
	var samples, blockedByPrune atomic.Int64
	sampled := make(chan struct{})
	sample := func() {
		defer close(sampled)
		for pruning.Load() {
			others, prunes := otherTr.list(), pruneTr.list()
			if len(others) > 0 && len(prunes) > 0 {
				var hit bool
				if err := f.pool.QueryRow(ctx, `SELECT pg_blocking_pids($1::int) && $2::int[]`, others[0], prunes).Scan(&hit); err != nil {
					t.Errorf("sample: %v", err)
					return
				}
				samples.Add(1)
				if hit {
					blockedByPrune.Add(1)
				}
			}
			time.Sleep(time.Millisecond)
		}
	}

	time.Sleep(200 * time.Millisecond)
	pruning.Store(true)
	go sample()
	start := time.Now()
	perr := store.PruneTenant(ctx, prunePool, tenantID, store.AuditStamp{})
	took := time.Since(start)
	pruning.Store(false)
	<-sampled
	close(stop)
	wg.Wait()
	if perr != nil {
		t.Fatalf("prune: %v", perr)
	}
	blocks, links, other, tenant := pruneLeftovers(t, f.pool, tenantID, scopes)
	t.Logf("prune of %d seeded blocks over %d scopes took %s; tenant's writer calls during it: %d (accepted %d, refused %d); other tenant's writer calls %d, blocked by a prune backend in %d of %d samples",
		pruneLoadBlocks(), len(scopes), took.Round(time.Millisecond), stats.during.Load(), stats.accepted.Load(), stats.refused.Load(),
		otherCalls.Load(), blockedByPrune.Load(), samples.Load())
	t.Logf("left in the tenant's scopes: %d blocks, %d links, %d other rows; tenant row %d", blocks, links, other, tenant)
	if blocks+links+other+tenant != 0 {
		var per []string
		rows, err := f.pool.Query(ctx, `SELECT scope || '=' || count(*) FROM context_blocks WHERE scope = ANY($1::text[]) GROUP BY scope`, scopes)
		if err == nil {
			per, _ = pgx.CollectRows(rows, pgx.RowTo[string])
		}
		slices.Sort(per)
		t.Fatalf("rows outlived a successful prune (blocks per scope %v)", per)
	}
	if blockedByPrune.Load() != 0 {
		t.Fatalf("another tenant's writer was blocked by a prune backend in %d samples", blockedByPrune.Load())
	}
}
