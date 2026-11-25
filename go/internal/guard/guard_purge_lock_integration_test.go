//go:build integration

// Issue #547 point 1: a guard run beside a scope purge.
//
// The run holds its picked blocks FOR NO KEY UPDATE for its whole batch and,
// at each audit insert, takes the foreign key's key-share lock on the matched
// neighbour (context_write_log.matched_block_id). A purge batch holds the
// scope's supersedes lock and its blocks FOR UPDATE, level by level through
// parent_id. Two interleavings close a cycle:
//
//   - purge first: the purge holds a matched neighbour M (its pick) and waits
//     for M's child Y, which the run picked; the run's audit insert of X,
//     whose neighbour is M, waits for M;
//   - run first: the run holds the key-share lock on M (the audit insert of
//     an earlier block A); the purge holds P and M2 and waits for M, a child
//     of M2; the run's audit insert of B, whose neighbour is M2, waits for M2.
//
// Neither side may die (40P01): the run leaves a block whose neighbour it
// cannot key-share at once pending for its next run, and the purge waits for
// the run's picks. Every block goes through store.UpsertBlock / UpsertBlocks
// (the parent) and store.StoreEmbedding, every verdict through
// guard.RunGuardBatch, the purge through store.PurgeScope; the interleavings
// are held at the run's audit step for the held block, before it asks for any
// lock on the neighbour (a query tracer on its pool).
//
//	CTX_G547_ROUNDS=200 go test -tags=integration -p 1 ./internal/guard/ -run 'TestGuardRunBesideAScopePurge' -v
//	CTX_G547_ROUNDS=200 CTX_G547_FREE=1 go test … (the same shapes without a hold point)
package guard_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
)

// g547Tracer calls before (when set) right before each statement of its pool
// is sent, and counts the statements that ended in 40P01.
type g547Tracer struct {
	before    func(pid uint32, sql string, args []any)
	deadlocks atomic.Int64
}

func (tr *g547Tracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if tr.before != nil {
		tr.before(conn.PgConn().PID(), d.SQL, d.Args)
	}
	return ctx
}

func (tr *g547Tracer) TraceQueryEnd(_ context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	if deadlock(d.Err) {
		tr.deadlocks.Add(1)
	}
}

func (f *g284Fixture) traced(t *testing.T, tr *g547Tracer) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.Tracer = tr
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close) // Close is idempotent: a round closes its pools at its end
	return p
}

// tagged writes a block carrying tag through the production upsert.
func (f *g284Fixture) tagged(t *testing.T, title, tag string, vec []float32) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), f.pool, "g284", title, title+" content", []string{tag}, nil, "private", true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed %s: %v", title, err)
	}
	if vec != nil {
		f.embed(t, b.ID, vec)
	}
	return b.ID
}

// child writes a block under parent through the block batch (the /api/store
// path of a write that names a parent).
func (f *g284Fixture) child(t *testing.T, title, parent string, vec []float32) string {
	t.Helper()
	res, err := store.UpsertBlocks(context.Background(), f.pool, []store.BatchUpsert{{
		Category: "g284", Title: title, Content: title + " content", Scope: "private", ScopeExplicit: true, ParentID: parent,
	}}, "", f.set, nil)
	if err != nil {
		t.Fatalf("seed child %s: %v", title, err)
	}
	id := res[0].Block.ID
	if vec != nil {
		f.embed(t, id, vec)
	}
	return id
}

func (f *g284Fixture) purge(pool *pgxpool.Pool, tag string, n int64) (*store.PurgeResult, error) {
	return store.PurgeScope(context.Background(), pool, store.PurgeQuery{
		Scope: "private", AllTenants: true, Filter: store.PurgeFilter{Tags: []string{tag}}, ConfirmBlocks: &n,
	})
}

func g547Rounds(t *testing.T) int {
	t.Helper()
	n, err := strconv.Atoi(os.Getenv("CTX_G547_ROUNDS"))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// TestGuardRunBesideAScopePurge runs both interleavings of the issue, held at
// the run's audit step, g547Rounds times each: no 40P01 on either side, the
// purge removes what it confirmed, and a block the run left pending gets its
// verdict in the next quiet run.
func TestGuardRunBesideAScopePurge(t *testing.T) {
	f := newG284Fixture(t)
	ctx := context.Background()
	rounds := g547Rounds(t)
	var deferredSeen int

	modes := []string{"purge first", "run first"}
	if os.Getenv("CTX_G547_FREE") != "" {
		// Free rounds: the same shapes, the run and the purge started together
		// without a hold point.
		modes = []string{"free purge first", "free run first"}
	}
	for _, mode := range modes {
		free := strings.HasPrefix(mode, "free ")
		dir := strings.TrimPrefix(mode, "free ")
		t.Run(mode, func(t *testing.T) {
			var runDeadlocks, purgeDeadlocks int64
			for r := 0; r < rounds; r++ {
				p := fmt.Sprintf("g547 %s %d", dir, r)
				tag := strings.ReplaceAll(p, " ", "-")
				var held, purged, waited string // held: the block whose audit insert is the hold point
				var confirm int64
				var leftovers []string // archived after the round, so no later round matches them
				// Axes of this round: archived blocks of earlier rounds with
				// the same vectors would crowd the index search.
				ax := 6*(r%85) + 2
				if dir == "run first" {
					ax += 512
				}
				switch dir {
				case "purge first":
					m := f.tagged(t, p+" m", tag, g284Vec(ax, 1, 0, 0))
					f.run(t, f.pool)
					held = f.block(t, p+" x", g284Vec(ax, 0.96, ax+1, 0.28)) // needs_review against M
					leftovers = []string{held}
					waited = f.child(t, p+" y", m, g284Vec(ax+2, 1, 0, 0)) // picked by the run, in the purge's closure
					purged, confirm = m, 2
				case "run first":
					top := f.tagged(t, p+" p", tag, nil)
					m2 := f.child(t, p+" m2", top, g284Vec(ax+3, 1, 0, 0))
					m := f.child(t, p+" m", m2, g284Vec(ax, 1, 0, 0))
					f.run(t, f.pool)
					a := f.block(t, p+" a", g284Vec(ax, 0.96, ax+1, 0.28))     // its audit insert names M first
					held = f.block(t, p+" b", g284Vec(ax+3, 0.96, ax+4, 0.28)) // needs_review against M2
					leftovers = []string{a, held}
					waited, purged, confirm = m, m2, 3
				}

				purgeTr := &g547Tracer{}
				purgePool := f.traced(t, purgeTr)
				purgeDone := make(chan struct{})
				var purgeRes *store.PurgeResult
				var purgeErr error
				started := false
				var once sync.Once
				hold := func(pid uint32, sql string, args []any) {
					// The audit step's first statement (the block's meta read),
					// before any lock on the neighbour is asked for.
					if !strings.Contains(sql, "SELECT title, category, scope FROM context_blocks") || len(args) == 0 || fmt.Sprint(args[0]) != held {
						return
					}
					once.Do(func() {
						started = true
						go func() {
							defer close(purgeDone)
							purgeRes, purgeErr = f.purge(purgePool, tag, confirm)
						}()
						if !waitedOn(t, f.pool, pid, purgeDone) {
							t.Errorf("round %d: the purge did not wait for the run", r)
						}
					})
				}
				runTr := &g547Tracer{}
				if !free {
					runTr.before = hold
				}
				runPool := f.traced(t, runTr)
				if free {
					started = true
					go func() {
						defer close(purgeDone)
						purgeRes, purgeErr = f.purge(purgePool, tag, confirm)
					}()
				}
				_, runErr := guard.RunGuardBatch(ctx, runPool, f.set, 50, guard.SearchIndexForTest)
				_, deferredInRun := guard.DeferredVerdicts()
				if !started {
					t.Fatalf("round %d: the run never reached the audit insert of the held block", r)
				}
				<-purgeDone
				runPool.Close()
				purgePool.Close()
				runDeadlocks += runTr.deadlocks.Load()
				purgeDeadlocks += purgeTr.deadlocks.Load()
				if runErr != nil {
					t.Fatalf("round %d: guard run: %v", r, runErr)
				}
				if purgeErr != nil || purgeRes.Deleted == nil || purgeRes.Deleted.Blocks != confirm {
					t.Fatalf("round %d: purge = %+v, %v; want %d blocks deleted", r, purgeRes, purgeErr, confirm)
				}
				var gone int
				if err := f.pool.QueryRow(ctx, `SELECT count(*)::int FROM context_blocks WHERE id = ANY($1::uuid[])`,
					[]string{purged, waited}).Scan(&gone); err != nil || gone != 0 {
					t.Fatalf("round %d: %d purged blocks still stand (%v)", r, gone, err)
				}
				if s := f.state(t, held); s.auditRows == 0 {
					deferredSeen++
					if runErr == nil && deferredInRun < 1 {
						t.Errorf("round %d: the held block was left pending, but the run counted %d deferred verdicts", r, deferredInRun)
					}
					f.run(t, f.pool) // the next quiet run
					if s := f.state(t, held); s.auditRows != 1 || !s.checked {
						t.Fatalf("round %d: the held block after the next quiet run: %+v, want one verdict", r, s)
					}
				}
				for _, id := range leftovers {
					if _, err := store.DeleteBlock(ctx, f.pool, id, []string{"private"}); err != nil {
						t.Fatalf("round %d: archive %s: %v", r, id, err)
					}
				}
			}
			t.Logf("%s: %d rounds, 40P01 in the run %d, in the purge %d", mode, rounds, runDeadlocks, purgeDeadlocks)
			if runDeadlocks != 0 || purgeDeadlocks != 0 {
				t.Errorf("%s: 40P01 in %d run statements and %d purge statements over %d rounds, want none", mode, runDeadlocks, purgeDeadlocks, rounds)
			}
		})
	}
	t.Logf("held blocks left pending and decided by the next quiet run: %d", deferredSeen)
}
