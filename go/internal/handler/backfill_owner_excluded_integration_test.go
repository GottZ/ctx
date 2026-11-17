//go:build integration

// Issue #213: a block of a type that only tenant A's registry knows, excluded
// there, took an embed slot on every pick path of the query path: A's own
// request with the arm on, any request with the arm off (the old end is not
// bound to the caller), and a request of another tenant that holds a grant
// on A's scope. No reader ranks such a vector. Since migration 176 the block
// is marked (embed_excluded), out of the pick indexes, and no request embeds
// it.
//
// The cost pin: every pick statement of a request costs the same number of
// buffers whether 10 000 or 100 000 such rows are pending in A's scope, half
// older and half younger than the one servable block S, on the generic and
// the custom plan, and every pick returns S.
//
//	go test -tags=integration ./internal/handler/ -run 'TestBackfillPending_OwnerExcludedType_Integration|TestBackfillPickOwnerExcludedTypeCost' -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// ownerExclTenants creates tenant A (scope oxq-a) with its own type oxq-x,
// retrieval excluded, and tenant B (scope oxq-b); it returns B's tenant id.
func ownerExclTenants(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	ids := map[string]string{}
	for _, s := range []string{"oxq-a", "oxq-b"} {
		tn, err := store.CreateTenant(ctx, pool, s, s)
		if err != nil {
			t.Fatalf("tenant %s: %v", s, err)
		}
		if _, err := store.AssignTenantScope(ctx, pool, tn.ID, s, nil); err != nil {
			t.Fatalf("assign %s: %v", s, err)
		}
		ids[s] = tn.ID
	}
	cfg := json.RawMessage(`{"v":1,"retrieval":{"policy":"excluded"},"guard":{"check":false,"candidate":false},"dream":{"linkable":false},"digest":{"include":false},"overview":{"include":false},"parent":{"mode":"none"},"classify":{"priority":100}}`)
	if _, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{Name: "oxq-x", Scope: "oxq-a", DisplayName: "oxq-x", Config: cfg}, nil, "oxq-test"); err != nil {
		t.Fatalf("type: %v", err)
	}
	if _, err := store.ReconcileEmbedExcluded(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return ids["oxq-b"]
}

func TestBackfillPending_OwnerExcludedType_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	tenantB := ownerExclTenants(t, pool)
	for _, b := range []struct{ scope, title, typ string }{
		{"oxq-a", "oxq a x 1", "oxq-x"},
		{"oxq-a", "oxq a x 2", "oxq-x"},
		{"oxq-a", "oxq a knowledge", "knowledge"},
		{"oxq-b", "oxq b knowledge", "knowledge"},
	} {
		if _, err := store.UpsertBlock(ctx, pool, "oxq-cat", b.title, "body of "+b.title, nil, nil, b.scope, true, store.SensitivityWrite{}, b.typ); err != nil {
			t.Fatalf("upsert %s: %v", b.title, err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	srv, hits := fakeEmbedServer(t)
	st := &countingStore{}
	st.cfg.Store(snapshotTestConfig())
	h := NewQueryHandler(pool, st, embedPool(srv.URL), nil, blocktype.NewRegistry(), snapshotTestAdmitter(t))
	embedded := func() []string {
		rows, err := pool.Query(ctx, `SELECT title FROM context_blocks WHERE category = 'oxq-cat' AND embedding IS NOT NULL ORDER BY title`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	run := func(name string, readScopes []string, interval time.Duration, want []string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET embedding = NULL, embed_model = NULL WHERE category = 'oxq-cat'`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM context_embed_failures`); err != nil {
			t.Fatal(err)
		}
		cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
			SyncCap: 0, Interval: interval, MaxTokens: 1_000_000, BackoffBase: time.Minute, BackoffCap: time.Hour,
		}}
		before := hits.Load()
		n := h.backfillPending(ctx, nil, readScopes, h.embedAdmission(), cfg)
		got := embedded()
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s: embedded %v, want %v", name, got, want)
		}
		if n != len(want) || int(hits.Load()-before) != len(want) {
			t.Errorf("%s: backfilled %d with %d wire calls, want %d (a block of A's excluded own type costs no call)", name, n, hits.Load()-before, len(want))
		}
	}
	run("A, arm on", []string{"oxq-a"}, 15*time.Second, []string{"oxq a knowledge"})
	run("A, arm off", []string{"oxq-a"}, 0, []string{"oxq a knowledge", "oxq b knowledge"})
	run("B, arm off (old end unbound)", []string{"oxq-b"}, 0, []string{"oxq a knowledge", "oxq b knowledge"})
	if _, err := store.CreateTenantGrant(ctx, pool, tenantB, "oxq-a", ""); err != nil {
		t.Fatalf("grant: %v", err)
	}
	run("B with a grant on A's scope, arm on", []string{"oxq-b", "oxq-a"}, 15*time.Second, []string{"oxq a knowledge", "oxq b knowledge"})
}

// seedOwnerExclPickCost grows the fixture from its current size to n: S once,
// then rows from..n of A's own excluded type, at distances of a millisecond
// from S, through the row triggers.
func seedOwnerExclPickCost(t *testing.T, pool *pgxpool.Pool, from, n int) {
	t.Helper()
	ctx := context.Background()
	if from == 0 {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope, sensitivity, created_at, updated_at)
			 VALUES ('learnings', 'bpc servable', 'the one block every pick returns', 'oxq-a', 'public', '2026-01-01', now())`); err != nil {
			t.Fatalf("seed S: %v", err)
		}
	}
	for _, sign := range []int{-1, +1} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, created_at, updated_at)
			 SELECT 'oxq-cost', 'oxq x ' || $1::int || ' ' || i, 'never embedded ' || i, 'oxq-x', 'oxq-a', 'public',
			        timestamptz '2026-01-01' + $1::int * make_interval(secs => i / 1000.0), now()
			   FROM generate_series($2::int + 1, $3::int) i`,
			sign, from/2, n/2); err != nil {
			t.Fatalf("seed rows (%+d): %v", sign, err)
		}
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

func TestBackfillPickOwnerExcludedTypeCost(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ownerExclTenants(t, pool)
	variants := []struct {
		name     string
		armRuns  bool
		attempts int
		read     []string
	}{
		{"A_arm_on_caller_end", true, 1, []string{"oxq-a"}},
		{"A_arm_off_both_ends", false, 2, []string{"oxq-a"}},
		{"B_arm_off_both_ends", false, 2, []string{"oxq-b"}},
	}
	sizes := []int{10_000, 100_000}
	got := make(map[string][][]testdb.PlanCost)
	from := 0
	for _, n := range sizes {
		seedOwnerExclPickCost(t, pool, from, n)
		from = n
		for _, v := range variants {
			ceilings, _, _ := pickCostFixture()
			rec := &costRecorder{pool: pool}
			p := newBackfillPicker(rec, v.armRuns, ceilings, nil, v.read)
			p.backoffBase, p.backoffCap = time.Hour, 24*time.Hour
			for attempt := 0; attempt < v.attempts; attempt++ {
				row, err := p.next(context.Background(), attempt)
				if err != nil {
					t.Fatalf("%s at %d, pick %d: %v", v.name, n, attempt, err)
				}
				if row.title != "bpc servable" {
					t.Fatalf("%s at %d, pick %d returned %q, want the servable block", v.name, n, attempt, row.title)
				}
			}
			costs := make([]testdb.PlanCost, 0, 2*len(rec.stmts))
			for i, s := range rec.stmts {
				generic := testdb.ExplainGeneric(t, pool, s.sql, s.args...)
				custom := testdb.ExplainCustom(t, pool, s.sql, s.args...)
				t.Logf("%s at %d rows, statement %d: generic %v, custom %v", v.name, n, i+1, generic, custom)
				costs = append(costs, generic, custom)
			}
			got[v.name] = append(got[v.name], costs)
		}
	}
	for _, v := range variants {
		small, large := got[v.name][0], got[v.name][1]
		if len(small) != len(large) {
			t.Fatalf("%s: %d statements at %d rows, %d at %d", v.name, len(small), sizes[0], len(large), sizes[1])
		}
		for i := range small {
			for _, c := range []testdb.PlanCost{small[i], large[i]} {
				if c.Buffers > pickBufferCap {
					t.Errorf("%s, statement %d (%s plan): %d buffers, more than %d — the pick reads rows it never returns",
						v.name, i/2+1, [2]string{"generic", "custom"}[i%2], c.Buffers, pickBufferCap)
				}
			}
			if limit := small[i].Buffers + small[i].Buffers/2 + 8; large[i].Buffers > limit {
				t.Errorf("%s, statement %d (%s plan): %d buffers at %d rows, %d at %d — the pick grows with the rows it never returns (limit %d)",
					v.name, i/2+1, [2]string{"generic", "custom"}[i%2], small[i].Buffers, sizes[0], large[i].Buffers, sizes[1], limit)
			}
		}
	}
}
