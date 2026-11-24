//go:build integration

// Issue #547 point 2: the tenant prune and the scopes' supersedes lock.
//
// Point 2: store.PruneTenant deleted the dream links and then the blocks of
// the tenant's scopes in batches by ctid, without the scopes' supersedes
// locks and in scan order. A writer of one of those scopes that holds block
// rows and waits for the next one — a declaration (source, then target), a
// block batch (its items' identity rows in order), a dream link batch (the
// key-share lock of each link's ends, link by link) — met the prune's delete
// holding the row it wanted next and waiting for one it held: 40P01. Each
// pair is held at the writer's statement that asks for the row the prune
// took (a query tracer on the writer's pool); the archive and the scope move
// lock their rows in one statement and run as free rounds
// (TestTenantPruneBesideAWriterFree).
//
//	CTX_P547_ROUNDS=200 go test -tags=integration -p 1 ./internal/store/ -run 'TestTenantPruneBeside' -v
package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// p547Tracer calls before (when set) right before each statement of its pool
// is sent and counts the statements that ended in 40P01.
type p547Tracer struct {
	before    func(pid uint32, sql string, args []any)
	deadlocks atomic.Int64
}

func (tr *p547Tracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if tr.before != nil {
		tr.before(conn.PgConn().PID(), d.SQL, d.Args)
	}
	return ctx
}

func (tr *p547Tracer) TraceQueryEnd(_ context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	var pgErr *pgconn.PgError
	if errors.As(d.Err, &pgErr) && pgErr.Code == "40P01" {
		tr.deadlocks.Add(1)
	}
}

func p547Pool(t *testing.T, dsn string, tr *p547Tracer) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.Tracer = tr
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func p547Rounds() int {
	n, err := strconv.Atoi(os.Getenv("CTX_P547_ROUNDS"))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// p547WaitOn polls until a backend waits on a lock pid holds (true) or done is
// closed (false).
func p547WaitOn(t *testing.T, pool *pgxpool.Pool, pid uint32, done <-chan struct{}) bool {
	t.Helper()
	deadline := time.Now().Add(testdb.CommitHoldBound)
	for {
		select {
		case <-done:
			return false
		default:
		}
		var waiting bool
		if err := pool.QueryRow(context.Background(),
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid)))`, int64(pid)).Scan(&waiting); err != nil {
			t.Fatalf("poll lock wait: %v", err)
		}
		if waiting {
			return true
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing waited on pid %d within %s", pid, testdb.CommitHoldBound)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// p547Fixture is one database with the default tenant's key scope and a
// block-type snapshot.
type p547Fixture struct {
	pool *pgxpool.Pool
	dsn  string
	set  *blocktype.Set
}

func newP547Fixture(t *testing.T) *p547Fixture {
	t.Helper()
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	reg := blocktype.NewRegistry()
	reg.Boot(context.Background(), pool)
	return &p547Fixture{pool: pool, dsn: dsn, set: reg.Snapshot()}
}

// tenant creates a tenant owning scope (and scope2 when not "").
func (f *p547Fixture) tenant(t *testing.T, slug, scope, scope2 string) string {
	t.Helper()
	ctx := context.Background()
	tn, err := store.CreateTenant(ctx, f.pool, slug, slug)
	if err != nil {
		t.Fatalf("tenant %s: %v", slug, err)
	}
	for _, s := range []string{scope, scope2} {
		if s == "" {
			continue
		}
		if _, err := store.AssignTenantScope(ctx, f.pool, tn.ID, s, nil); err != nil {
			t.Fatalf("scope %s: %v", s, err)
		}
	}
	return tn.ID
}

func (f *p547Fixture) block(t *testing.T, scope, title string) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), f.pool, "learnings", title, title+" content", nil, nil, scope, true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed %s: %v", title, err)
	}
	return b.ID
}

// p547Writer is one writer of a pair: it runs on pool; marker names the
// statement at which the prune is started and waited for (nil: free round).
type p547Writer struct {
	run    func(pool *pgxpool.Pool) error
	marker func(sql string, args []any) bool
}

// p547Case seeds a fresh tenant scope for round r and returns its writer.
type p547Case struct {
	name string
	seed func(t *testing.T, f *p547Fixture, scope, scope2, p string) p547Writer
}

func p547Cases() []p547Case {
	return []p547Case{
		{"declaration", func(t *testing.T, f *p547Fixture, scope, _, p string) p547Writer {
			tgt := f.block(t, scope, p+" target") // before the source in heap order: the prune deletes it first
			f.block(t, scope, p+" source")
			return p547Writer{
				run: func(pool *pgxpool.Pool) error {
					_, err := store.UpsertBlockSupersedes(context.Background(), pool, "learnings", p+" source", p+" source v2", nil, nil, scope, true,
						store.SensitivityWrite{}, "", store.SupersedesDeclaration{TargetID: tgt, APIKeyID: "p547", Types: f.set})
					return err
				},
				marker: func(sql string, args []any) bool {
					return strings.Contains(sql, "SELECT type_name, lifecycle_state = 'chunk'") && len(args) > 0 && fmt.Sprint(args[0]) == tgt
				},
			}
		}},
		{"block batch", func(t *testing.T, f *p547Fixture, scope, _, p string) p547Writer {
			f.block(t, scope, p+" b") // locked second, deleted first
			f.block(t, scope, p+" a")
			return p547Writer{
				run: func(pool *pgxpool.Pool) error {
					_, err := store.UpsertBlocks(context.Background(), pool, []store.BatchUpsert{
						{Category: "learnings", Title: p + " b", Content: "b changed", Scope: scope, ScopeExplicit: true},
						{Category: "learnings", Title: p + " a", Content: "a changed", Scope: scope, ScopeExplicit: true},
					}, "", f.set, nil)
					return err
				},
				marker: func(sql string, args []any) bool {
					return strings.Contains(sql, "content_hash = encode(digest(") && len(args) > 3 && fmt.Sprint(args[3]) == p+" b"
				},
			}
		}},
		{"dream link batch", func(t *testing.T, f *p547Fixture, scope, _, p string) p547Writer {
			u := f.block(t, scope, p+" u") // the second link's end, first in heap order
			src := f.block(t, scope, p+" s")
			tgt := f.block(t, scope, p+" t")
			return p547Writer{
				run: func(pool *pgxpool.Pool) error {
					_, err := dream.WriteLinks(context.Background(), pool, f.set, src, scope, 1.0, []dream.Link{
						{TargetID: tgt, Relationship: "topical", Confidence: 0.9},
						{TargetID: u, Relationship: "topical", Confidence: 0.9},
					})
					return err
				},
				marker: func(sql string, args []any) bool {
					return strings.Contains(sql, "INSERT INTO context_dream_links") && len(args) > 1 && fmt.Sprint(args[1]) == u
				},
			}
		}},
		{"archive", func(t *testing.T, f *p547Fixture, scope, _, p string) p547Writer {
			tgt := f.block(t, scope, p+" target")
			src, err := store.UpsertBlockSupersedes(context.Background(), f.pool, "learnings", p+" source", p+" source content", nil, nil, scope, true,
				store.SensitivityWrite{}, "", store.SupersedesDeclaration{TargetID: tgt, APIKeyID: "p547", Types: f.set})
			if err != nil {
				t.Fatalf("declare: %v", err)
			}
			return p547Writer{run: func(pool *pgxpool.Pool) error {
				_, err := store.DeleteBlock(context.Background(), pool, src.ID, []string{scope})
				return err
			}}
		}},
		{"scope move", func(t *testing.T, f *p547Fixture, scope, scope2, p string) p547Writer {
			tgt := f.block(t, scope, p+" target")
			src, err := store.UpsertBlockSupersedes(context.Background(), f.pool, "learnings", p+" source", p+" source content", nil, nil, scope, true,
				store.SensitivityWrite{}, "", store.SupersedesDeclaration{TargetID: tgt, APIKeyID: "p547", Types: f.set})
			if err != nil {
				t.Fatalf("declare: %v", err)
			}
			return p547Writer{run: func(pool *pgxpool.Pool) error {
				_, _, err := store.UpdateBlock(context.Background(), pool, src.ID, store.UpdateBlockData{Scope: &scope2}, []string{scope, scope2})
				return err
			}}
		}},
	}
}

// p547Round runs one round of a pair: a fresh tenant, the writer held at its
// marker while the prune runs up to its first wait (or, free, both started
// together), then both to their end. It returns the 40P01 counts of each
// side and the outcomes.
func p547Round(t *testing.T, f *p547Fixture, c p547Case, r int, free bool) (writerDeadlocks, pruneDeadlocks int64, writerErr, pruneErr error) {
	t.Helper()
	slug := fmt.Sprintf("p547-%s-%d", strings.ReplaceAll(c.name, " ", "-"), r)
	if free {
		slug = "f" + slug
	}
	scope, scope2 := slug+"-s", slug+"-s2"
	tenantID := f.tenant(t, slug, scope, scope2)
	w := c.seed(t, f, scope, scope2, slug)

	pruneTr := &p547Tracer{}
	prunePool := p547Pool(t, f.dsn, pruneTr)
	pruneDone := make(chan struct{})
	startPrune := func() {
		go func() {
			defer close(pruneDone)
			pruneErr = store.PruneTenant(context.Background(), prunePool, tenantID, store.AuditStamp{})
		}()
	}
	writerTr := &p547Tracer{}
	var once sync.Once
	started := free
	if !free && w.marker != nil {
		writerTr.before = func(pid uint32, sql string, args []any) {
			if !w.marker(sql, args) {
				return
			}
			once.Do(func() {
				started = true
				startPrune()
				p547WaitOn(t, f.pool, pid, pruneDone)
			})
		}
	}
	writerPool := p547Pool(t, f.dsn, writerTr)
	if free {
		startPrune()
	}
	writerErr = w.run(writerPool)
	if !started {
		t.Fatalf("%s round %d: the writer never reached its marker (writer: %v)", c.name, r, writerErr)
	}
	<-pruneDone
	writerPool.Close()
	prunePool.Close()
	return writerTr.deadlocks.Load(), pruneTr.deadlocks.Load(), writerErr, pruneErr
}

// TestTenantPruneBesideAWriter: a declaration, a block batch and a dream link
// batch of one of the tenant's scopes, each held at the statement that asks
// for a row the prune deletes before the one the writer holds. Neither side
// may end with 40P01; the prune removes the tenant whatever the writer did.
func TestTenantPruneBesideAWriter(t *testing.T) {
	f := newP547Fixture(t)
	rounds := p547Rounds()
	for _, c := range p547Cases() {
		if c.name == "archive" || c.name == "scope move" {
			continue // one statement locks their rows: free rounds only
		}
		t.Run(c.name, func(t *testing.T) {
			var wd, pd int64
			for r := 0; r < rounds; r++ {
				w, p, _, pruneErr := p547Round(t, f, c, r, false)
				wd, pd = wd+w, pd+p
				if pruneErr != nil && p == 0 {
					t.Fatalf("round %d: prune: %v", r, pruneErr)
				}
			}
			t.Logf("%s: %d rounds, 40P01 in the writer %d, in the prune %d", c.name, rounds, wd, pd)
			if wd != 0 || pd != 0 {
				t.Errorf("%s: 40P01 in %d writer and %d prune statements over %d rounds, want none", c.name, wd, pd, rounds)
			}
		})
	}
}

// TestTenantPruneBesideAWriterFree runs every pair, the archive and the scope
// move included, with the writer and the prune started together; it runs only
// with CTX_P547_FREE set (a measurement, not a pin).
func TestTenantPruneBesideAWriterFree(t *testing.T) {
	if os.Getenv("CTX_P547_FREE") == "" {
		t.Skip("set CTX_P547_FREE to run the free rounds")
	}
	f := newP547Fixture(t)
	rounds := p547Rounds()
	for _, c := range p547Cases() {
		t.Run(c.name, func(t *testing.T) {
			var wd, pd int64
			var pruneFails int
			for r := 0; r < rounds; r++ {
				w, p, _, pruneErr := p547Round(t, f, c, r, true)
				wd, pd = wd+w, pd+p
				if pruneErr != nil {
					pruneFails++
				}
			}
			t.Logf("%s free: %d rounds, 40P01 in the writer %d, in the prune %d, failed prunes %d", c.name, rounds, wd, pd, pruneFails)
			if wd != 0 || pd != 0 {
				t.Errorf("%s free: 40P01 in %d writer and %d prune statements over %d rounds, want none", c.name, wd, pd, rounds)
			}
		})
	}
}
