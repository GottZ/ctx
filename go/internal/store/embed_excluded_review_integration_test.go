//go:build integration

// Issue #213, review of migration 176: the cost of a tenant's queue entry,
// stale vectors of a type that becomes never-embedded, a '*' reconcile batch
// against a scope assignment, and two scope moves while a block write stays
// open.
//
//	go test -tags=integration ./internal/store/ -run 'TestEmbedExcludedReview' -count=1 -v
package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// reviewTracer keeps the statements a pool runs.
type reviewTracer struct {
	mu    sync.Mutex
	stmts []reviewStmt
}

type reviewStmt struct {
	sql  string
	args []any
}

func (r *reviewTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	r.stmts = append(r.stmts, reviewStmt{d.SQL, d.Args})
	r.mu.Unlock()
	return ctx
}

func (r *reviewTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *reviewTracer) take() []reviewStmt {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stmts
	r.stmts = nil
	return s
}

// TestEmbedExcludedReviewTenantEntryCost: tenant A writes an excluded overlay
// of checkpoint (the only overlay the narrowing gate admits), which queues
// A's group. The reconcile of that entry reads A's scopes only: the buffers
// of all its statements, read again on their custom plans, stay the same
// whether tenant B holds 10 000 or 50 000 marked checkpoint blocks.
func TestEmbedExcludedReviewTenantEntryCost(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	tracer := &reviewTracer{}
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pcfg.ConnConfig.Tracer = tracer
	traced, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	reconcile(t, pool)
	ownerTenant(t, pool, "ec-a", "ec-a")
	ownerTenant(t, pool, "ec-b", "ec-b")
	ownerSeed(t, pool, "ec-a", "checkpoint", 3)
	var sums []int64
	from := 0
	for i, n := range []int{10_000, 50_000} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
			 SELECT 'ec', 'ec ' || i, 'x', 'checkpoint', 'ec-b', 'public', now() FROM generate_series($1::int + 1, $2::int) i`, from, n); err != nil {
			t.Fatal(err)
		}
		from = n
		if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
			t.Fatal(err)
		}
		reconcile(t, pool)
		if i == 0 {
			createType(t, pool, "checkpoint", "ec-a", "excluded")
		} else {
			// The same entry once more, as the overlay's next write queues it.
			if _, err := pool.Exec(ctx, `INSERT INTO context_embed_excluded_queue (name, grp) SELECT 'checkpoint', ctx_embed_excluded_group('ec-a') ON CONFLICT DO NOTHING`); err != nil {
				t.Fatal(err)
			}
		}
		var grp string
		if err := pool.QueryRow(ctx, `SELECT grp FROM context_embed_excluded_queue WHERE name = 'checkpoint'`).Scan(&grp); err != nil {
			t.Fatalf("A's entry: %v", err)
		}
		tracer.take()
		if _, err := store.ReconcileEmbedExcluded(ctx, traced); err != nil {
			t.Fatal(err)
		}
		var sum int64
		for _, st := range tracer.take() {
			if !strings.Contains(st.sql, "context_blocks") {
				continue
			}
			c := testdb.ExplainCustom(t, pool, st.sql, st.args...)
			sum += c.Buffers
		}
		t.Logf("entry (checkpoint, %s) with %d marked checkpoint blocks of B: %d buffers over its statements", grp, n, sum)
		sums = append(sums, sum)
	}
	if sums[1] > sums[0]+sums[0]/2+50 {
		t.Errorf("the reconcile of A's entry reads %d buffers with 10 000 marked blocks of B and %d with 50 000: it walks B's rows", sums[0], sums[1])
	}
}

// TestEmbedExcludedReviewStaleBecomesExcluded: a block that holds a stale
// vector (a content change after its embedding) of a type that then becomes
// never-embedded — through a registry change and the reconcile, or through a
// re-type — drops the vector and carries the marker; it does not stay in the
// stale-pick indexes.
func TestEmbedExcludedReviewStaleBecomesExcluded(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reconcile(t, pool)
	ownerTenant(t, pool, "se-a", "se-a")
	row := createType(t, pool, "se-x", "se-a", "full-pass")
	createType(t, pool, "se-z", "se-a", "full-pass")
	createType(t, pool, "se-y", "se-a", "excluded")
	reconcile(t, pool)
	staleBlock := func(title, typ string) *store.Block {
		b := storeTyped(t, pool, "se-a", title, typ)
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET embedding = array_fill(0.1::real, ARRAY[1024])::vector, embed_model = 'test' WHERE id = $1`, b.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.UpsertBlock(ctx, pool, "ee-cat", title, "changed body", nil, nil, "se-a", true, store.SensitivityWrite{}, typ); err != nil {
			t.Fatal(err)
		}
		return b
	}
	state := func(id string) (vec, stale, marked bool) {
		if err := pool.QueryRow(ctx, `SELECT embedding IS NOT NULL, embedding_stale, embed_excluded FROM context_blocks WHERE id = $1`, id).
			Scan(&vec, &stale, &marked); err != nil {
			t.Fatal(err)
		}
		return
	}
	viaRegistry := staleBlock("se registry", "se-x")
	viaRetype := staleBlock("se retype", "se-z")
	for _, b := range []*store.Block{viaRegistry, viaRetype} {
		if v, s, _ := state(b.ID); !v || !s {
			t.Fatalf("fixture %s: vector %v stale %v", b.Title, v, s)
		}
	}
	updateType(t, pool, row.ID, "se-a", "excluded")
	reconcile(t, pool)
	if v, s, m := state(viaRegistry.ID); v || s || !m {
		t.Errorf("stale block after its type became excluded and the reconcile: vector %v, stale %v, marker %v; want false, false, true", v, s, m)
	}
	if v, s, _ := state(viaRetype.ID); !v || !s {
		t.Fatalf("fixture: the block of se-z lost its stale vector before the re-type (vector %v stale %v)", v, s)
	}
	// viaRetype is stale under the embeddable se-z; a re-type into the
	// excluded se-y marks it and drops the vector at once.
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET type_name = 'se-y' WHERE id = $1`, viaRetype.ID); err != nil {
		t.Fatal(err)
	}
	if v, s, m := state(viaRetype.ID); v || s || !m {
		t.Errorf("stale block re-typed into an excluded type: vector %v, stale %v, marker %v; want false, false, true", v, s, m)
	}
}

// TestEmbedExcludedReviewStarVsAssignment: a reconcile of a '*' entry whose
// mark batch waits on a held block of a tenantless scope, while that scope
// is assigned to the tenant that owns another scope of the batch's blocks.
// A batch over blocks of several groups took the groups' shared locks in row
// order, the assignment takes them exclusive and sorted: 40P01. A batch now
// stays within one scope.
func TestEmbedExcludedReviewStarVsAssignment(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reconcile(t, pool)
	ta := ownerTenant(t, pool, "dl-a", "dl-a")
	reconcile(t, pool)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, created_at, updated_at, embed_excluded)
		VALUES ('dl', 'dl 1', 'x', 'checkpoint', 'dl-a', 'public', now() - interval '2 hours', now(), false),
		       ('dl', 'dl 2', 'x', 'checkpoint', 'dl-x', 'public', now() - interval '1 hour', now(), false)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO context_embed_excluded_queue (name, grp) VALUES ('checkpoint', '*')`); err != nil {
		t.Fatal(err)
	}
	hold, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec(ctx, `SELECT 1 FROM context_blocks WHERE title = 'dl 2' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	rdone := make(chan error, 1)
	go func() {
		_, err := store.ReconcileEmbedExcluded(ctx, pool)
		rdone <- err
	}()
	if !lockWaitSeen(t, pool, func() bool { return len(rdone) == 0 }) {
		t.Fatalf("the reconcile did not wait for the held block")
	}
	adone := make(chan error, 1)
	go func() {
		_, err := store.AssignTenantScope(ctx, pool, ta, "dl-x", nil)
		adone <- err
	}()
	time.Sleep(500 * time.Millisecond)
	_ = hold.Rollback(ctx)
	rerr, aerr := <-rdone, <-adone
	if rerr != nil && strings.Contains(rerr.Error(), "40P01") || aerr != nil && strings.Contains(aerr.Error(), "40P01") {
		t.Errorf("deadlock between a '*' reconcile batch and a scope assignment: reconcile %v, assignment %v", rerr, aerr)
	}
	reconcile(t, pool)
	if got := markedCount(t, pool, "dl-a", "checkpoint") + markedCount(t, pool, "dl-x", "checkpoint"); got != 2 {
		t.Errorf("%d/2 checkpoint blocks marked after the assignment and a reconcile", got)
	}
}

// TestEmbedExcludedReviewDoubleMove: a scope moves from tenant B to C while a
// block write in it stays open, then from C to D. The block trigger waited
// for the first move and decided under C; it now also holds C's group lock,
// so the second move waits for the block's commit, and the block carries the
// answer under D after the reconcile.
func TestEmbedExcludedReviewDoubleMove(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reconcile(t, pool)
	tb := ownerTenant(t, pool, "dm-b", "dm-b")
	tc := ownerTenant(t, pool, "dm-c", "dm-c")
	td := ownerTenant(t, pool, "dm-d", "dm-d")
	createType(t, pool, "dm-x", "dm-b", "excluded")
	createType(t, pool, "dm-x", "dm-c", "excluded")
	if _, err := store.AssignTenantScope(ctx, pool, tb, "dm-s", nil); err != nil {
		t.Fatal(err)
	}
	reconcile(t, pool)

	t1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = t1.Rollback(ctx) }()
	if _, err := t1.Exec(ctx, `UPDATE context_tenant_scopes SET tenant_id = $1 WHERE scope = 'dm-s'`, tc); err != nil {
		t.Fatal(err)
	}
	type res struct {
		id  string
		err error
	}
	wrote := make(chan res, 1)
	release := make(chan struct{})
	committed := make(chan error, 1)
	go func() {
		tx3, err := pool.Begin(ctx)
		if err != nil {
			wrote <- res{err: err}
			return
		}
		var id string
		err = tx3.QueryRow(ctx, `INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
			VALUES ('own-cat', 'dm open', 'x', 'dm-x', 'dm-s', 'public', now()) RETURNING id`).Scan(&id)
		wrote <- res{id: id, err: err}
		<-release
		committed <- tx3.Commit(ctx)
	}()
	if !lockWaitSeen(t, pool, func() bool { return len(wrote) == 0 }) {
		t.Errorf("the block write did not wait for the first move")
	}
	if err := t1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-wrote
	if r.err != nil {
		t.Fatal(r.err)
	}
	moved := make(chan error, 1)
	go func() {
		_, err := pool.Exec(ctx, `UPDATE context_tenant_scopes SET tenant_id = $1 WHERE scope = 'dm-s'`, td)
		moved <- err
	}()
	if !lockWaitSeen(t, pool, func() bool { return len(moved) == 0 }) {
		t.Errorf("the second move did not wait for the open block write")
	}
	close(release)
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	if err := <-moved; err != nil {
		t.Fatal(err)
	}
	reconcile(t, pool)
	var marked, never bool
	if err := pool.QueryRow(ctx, `SELECT embed_excluded, ctx_block_type_never_embedded(type_name, scope) FROM context_blocks WHERE id = $1`, r.id).
		Scan(&marked, &never); err != nil {
		t.Fatal(err)
	}
	if marked != never {
		t.Errorf("block after two moves and the reconcile: marker %v, answer under D %v", marked, never)
	}
}
