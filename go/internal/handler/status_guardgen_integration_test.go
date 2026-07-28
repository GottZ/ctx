//go:build integration

// Integration probes for RC-1 wave S1 (guard_review generation): the flagged-
// block section must come from ONE per-tick generation shared by every reader,
// not from a per-request query on the tenant path.
//
//	go test -tags=integration ./internal/handler/ -run TestGuardReviewGeneration -count=1 -v
package handler

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

// guardQueryCounter is the injected pool seam for the cadence probe: a pgx
// QueryTracer that counts the guard aggregate specifically. The fingerprint
// (context_blocks + needs_review) matches BOTH the per-request shape and the
// ROLLUP generation shape, so the same counter reads red before and green
// after the change — it measures the CADENCE, not the SQL text.
type guardQueryCounter struct {
	aggregates atomic.Int64 // guard flagged-block aggregate over context_blocks
	scopeList  atomic.Int64 // context_tenant_scopes scope-universe read
}

func (g *guardQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	sql := data.SQL
	switch {
	case strings.Contains(sql, "context_blocks") && strings.Contains(sql, "needs_review"):
		g.aggregates.Add(1)
	case strings.Contains(sql, "context_tenant_scopes") && !strings.Contains(sql, "context_llm_log"):
		g.scopeList.Add(1)
	}
	return ctx
}

func (g *guardQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// tracedPool opens a SECOND pool against the same testcontainer DSN, wired with
// the query counter. The collector under probe gets this pool, so every query it
// issues passes the seam.
func tracedPool(t *testing.T, dsn string) (*pgxpool.Pool, *guardQueryCounter) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	ctr := &guardQueryCounter{}
	cfg.ConnConfig.Tracer = ctr
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctr
}

// seedGuardScope registers a tenant scope and (optionally) flagged blocks in it.
func seedGuardScope(t *testing.T, pool *pgxpool.Pool, scope string, flagged map[string]int) {
	t.Helper()
	ctx := context.Background()
	var tid string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_tenants (slug, display_name) VALUES ($1,$2) RETURNING id::text`, scope, scope).Scan(&tid); err != nil {
		t.Fatalf("tenant %s: %v", scope, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1,$2::uuid)`, scope, tid); err != nil {
		t.Fatalf("tenant scope %s: %v", scope, err)
	}
	for status, n := range flagged {
		for i := 0; i < n; i++ {
			if _, err := pool.Exec(ctx,
				`INSERT INTO context_blocks (category, title, content, scope, guard_status)
				 VALUES ('reference', $1, 'c', $2, $3)`,
				fmt.Sprintf("%s-%s-%d", scope, status, i), scope, status); err != nil {
				t.Fatalf("block %s/%s: %v", scope, status, err)
			}
		}
	}
}

// TestGuardReviewGenerationCadence is probe S1(a): N tenant pulls inside ONE
// tick must cost exactly ONE guard aggregate. Against the per-request Ist-Stand
// (SnapshotForTenant → buildGuardReviewStatus per call) the counter reads N+1
// (one global aggregate from the cold buildCheap, one per request).
func TestGuardReviewGenerationCadence(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	seedPool, dsn := testdb.SetupTestDBWithDSN(t)
	seedGuardScope(t, seedPool, "gg-a", map[string]int{"needs_review": 3, "near_duplicate": 1})
	seedGuardScope(t, seedPool, "gg-b", map[string]int{"possible_duplicate": 2})

	pool, ctr := tracedPool(t, dsn)
	col := NewStatusCollector(pool, backends.NewPool(nil, nil), fakeDreamMode{},
		config.NewStore(&config.Config{}), nil, nil)
	ctx := context.Background()

	const pulls = 10
	for i := 0; i < pulls; i++ {
		snap := col.SnapshotForTenant(ctx, "gg-a", nil)
		if snap.GuardReview == nil {
			t.Fatalf("pull %d: guard_review section missing", i)
		}
		if snap.GuardReview.NeedsReview != 3 || snap.GuardReview.NearDuplicate != 1 {
			t.Fatalf("pull %d: wrong counts: %+v", i, *snap.GuardReview)
		}
	}
	if got := ctr.aggregates.Load(); got != 1 {
		t.Errorf("guard aggregate queries within one tick: got %d, want 1 (%d tenant pulls)", got, pulls)
	}
	if got := ctr.scopeList.Load(); got > 1 {
		t.Errorf("scope-universe reads within one tick: got %d, want <= 1", got)
	}

	// The server-admin path shares the SAME generation — its global slot costs
	// no additional aggregate.
	if srv := col.Snapshot(ctx); srv.GuardReview == nil || srv.GuardReview.NeedsReview != 3 {
		t.Errorf("server-admin global slot: %+v, want needs_review 3", srv.GuardReview)
	}
	if got := ctr.aggregates.Load(); got != 1 {
		t.Errorf("server-admin pull inside the same tick added an aggregate: got %d, want 1", got)
	}
}

// TestGuardReviewGenerationRollup covers probes S1(b) ROLLUP consistency and
// S1(d) materialized empty scopes, plus the fail-closed lookup, against a real
// ROLLUP plan.
func TestGuardReviewGenerationRollup(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedGuardScope(t, pool, "gr-a", map[string]int{"needs_review": 4, "possible_duplicate": 1})
	seedGuardScope(t, pool, "gr-b", map[string]int{"near_duplicate": 2})
	// gr-empty exists as a tenant scope but carries no flagged block at all.
	seedGuardScope(t, pool, "gr-empty", nil)
	// An archived flagged block must not count anywhere (WHERE NOT is_archived).
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope, guard_status, is_archived)
		 VALUES ('reference','gr-a-archived','c','gr-a','needs_review', true)`); err != nil {
		t.Fatalf("archived block: %v", err)
	}

	gen := buildGuardReviewGeneration(ctx, pool)
	if gen == nil {
		t.Fatal("buildGuardReviewGeneration returned nil")
	}
	if gen.builtAt.IsZero() {
		t.Error("successful build left the SUCCESS stamp unset")
	}

	// Probe S1(b): the grand total equals the sum over the per-scope rows — one
	// plan, no second query that could disagree with the first.
	var sumNeeds, sumNear, sumPossible int
	for scope, row := range gen.byScope {
		if scope == "" {
			t.Error("the generation materialized an EMPTY scope key")
		}
		sumNeeds += row.NeedsReview
		sumNear += row.NearDuplicate
		sumPossible += row.PossibleDuplicate
	}
	g := gen.globalRow()
	if g == nil {
		t.Fatal("generation carries no global row")
	}
	if g.NeedsReview != sumNeeds || g.NearDuplicate != sumNear || g.PossibleDuplicate != sumPossible {
		t.Errorf("ROLLUP inconsistent: global {%d,%d,%d} vs scope sum {%d,%d,%d}",
			g.NeedsReview, g.NearDuplicate, g.PossibleDuplicate, sumNeeds, sumNear, sumPossible)
	}
	if g.NeedsReview != 4 || g.NearDuplicate != 2 || g.PossibleDuplicate != 1 {
		t.Errorf("global counts %+v — want {4,2,1} (the archived block must not count)", *g)
	}

	// Probe S1(d): an existing scope WITHOUT flagged blocks is materialized as
	// {0,0,0,null} — present, not missing. A missing map key would be
	// indistinguishable from a degraded read on the tenant's dashboard.
	empty := gen.forScope("gr-empty")
	if empty == nil {
		t.Fatal("a tenant scope with zero flagged blocks resolved to nil — empty scopes must be materialized")
	}
	if empty.NeedsReview != 0 || empty.NearDuplicate != 0 || empty.PossibleDuplicate != 0 {
		t.Errorf("empty scope row = %+v, want all zeros", *empty)
	}
	if empty.OldestUpdatedAt != nil {
		t.Errorf("empty scope oldest_updated_at = %v, want null", *empty.OldestUpdatedAt)
	}
	if empty.BuiltAt == nil || !empty.BuiltAt.Equal(gen.builtAt) {
		t.Errorf("empty scope built_at = %v, want the generation stamp %v", empty.BuiltAt, gen.builtAt)
	}

	// A scope that is neither a tenant scope nor carries flagged blocks stays
	// nil — fail closed, never the grand total (Verschärfung 1 end to end).
	col := NewStatusCollector(pool, backends.NewPool(nil, nil), fakeDreamMode{},
		config.NewStore(&config.Config{}), nil, nil)
	if s := col.SnapshotForTenant(ctx, "gr-nonexistent", nil).GuardReview; s != nil {
		t.Errorf("unknown scope produced a guard_review section %+v (must fail closed)", *s)
	}
	// The EMPTY scope is the live leak path this closes: HandleStatus passes
	// scope="" when the request carries no AuthResult, and the pre-S1 builder
	// read "" as "aggregate globally" — a tenant-path response with every
	// tenant's flagged counts in it. It must now be a section-less response.
	if s := col.SnapshotForTenant(ctx, "", nil).GuardReview; s != nil {
		t.Errorf("empty scope produced a guard_review section %+v — the tenant path must never serve the global total", *s)
	}
	if s := col.SnapshotForTenant(ctx, "gr-empty", nil).GuardReview; s == nil || s.NeedsReview != 0 {
		t.Errorf("empty scope through the tenant path: %+v, want a zero section", s)
	}
	if s := col.SnapshotForTenant(ctx, "gr-a", nil).GuardReview; s == nil || s.NeedsReview != 4 || s.PossibleDuplicate != 1 {
		t.Errorf("gr-a through the tenant path: %+v, want {4,0,1}", s)
	}
}

// TestGuardReviewGenerationEmptyQueue pins that a system with NO flagged block
// keeps the global section (zeros), instead of losing it because the ROLLUP
// produced no rows — the pre-S1 aggregate always returned a row, and that
// visibility must not change.
func TestGuardReviewGenerationEmptyQueue(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedGuardScope(t, pool, "gq-a", nil)

	gen := buildGuardReviewGeneration(ctx, pool)
	if gen == nil {
		t.Fatal("buildGuardReviewGeneration returned nil on an empty queue")
	}
	g := gen.globalRow()
	if g == nil {
		t.Fatal("empty queue lost the global section")
	}
	if g.NeedsReview != 0 || g.NearDuplicate != 0 || g.PossibleDuplicate != 0 || g.OldestUpdatedAt != nil {
		t.Errorf("empty-queue global row = %+v, want {0,0,0,null}", *g)
	}
	if row := gen.forScope("gq-a"); row == nil {
		t.Error("empty queue lost the materialized scope row")
	}
}
