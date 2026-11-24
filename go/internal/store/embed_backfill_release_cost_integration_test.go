//go:build integration

// Issue #236, review of the release (F3): the dry run of a tenant-admin must
// not cost a scan of every memo of the class. Before, the count read the
// memo table whole and probed each row's block for the tenant (a sequential
// scan of context_embed_failures and of context_blocks: 10 963 buffers for 3
// own memos next to 200 000 of another tenant). The count of a tenant reader
// now starts at its own scopes and reads its pending blocks on their index
// ranges, so it grows with the tenant's own pending blocks, not with the
// other tenants' memos.
//
// The other tenant's memos are seeded with SQL (the scale of the fixture); the
// tenant's own blocks and memos go through the production writers. The
// statement is captured from the store call by a tracer and EXPLAINed on the
// same connection settings, at both deployment values of random_page_cost and
// with the custom and the generic plan.
//
//	CTX_I236_FOREIGN_MEMOS=200000 go test -tags=integration -p 1 ./internal/store/ -run TestBackfillReleaseTenantDryRun -count=1 -v
package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// relCapture keeps the last statement and arguments that start with prefix.
type relCapture struct {
	mu     sync.Mutex
	prefix string
	sql    string
	args   []any
}

func (c *relCapture) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.TrimSpace(d.SQL), c.prefix) {
		c.mu.Lock()
		c.sql, c.args = d.SQL, d.Args
		c.mu.Unlock()
	}
	return ctx
}

func (c *relCapture) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// relPlan walks a JSON plan and sums what the assertions read.
type relPlan struct {
	buffers int
	seqOn   []string
}

func relWalk(n map[string]any, p *relPlan) {
	if n["Node Type"] == "Seq Scan" {
		if rel, _ := n["Relation Name"].(string); rel != "" {
			p.seqOn = append(p.seqOn, rel)
		}
	}
	if kids, ok := n["Plans"].([]any); ok {
		for _, k := range kids {
			if m, ok := k.(map[string]any); ok {
				relWalk(m, p)
			}
		}
	}
}

func TestBackfillReleaseTenantDryRunReadsItsOwnMemos(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	foreign := 50000
	if v := os.Getenv("CTX_I236_FOREIGN_MEMOS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("CTX_I236_FOREIGN_MEMOS: %v", err)
		}
		foreign = n
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	tenant := func(slug string, scopes int) (string, []string) {
		tn, err := store.CreateTenant(ctx, pool, slug, slug)
		if err != nil {
			t.Fatalf("tenant %s: %v", slug, err)
		}
		var out []string
		for i := range scopes {
			sc := fmt.Sprintf("%s:s%02d", slug, i)
			if _, err := store.AssignTenantScope(ctx, pool, tn.ID, sc, nil); err != nil {
				t.Fatalf("scope %s: %v", sc, err)
			}
			out = append(out, sc)
		}
		return tn.ID, out
	}
	own, ownScopes := tenant("relc-own", 10)
	_, otherScopes := tenant("relc-other", 40)

	// The other tenant: foreign parked blocks with their memos, over its 40 scopes.
	if _, err := pool.Exec(ctx, `
		WITH b AS (
		  INSERT INTO context_blocks (category, title, content, scope)
		  SELECT 'learnings', 'relc other ' || g, 'parked text ' || g, ($2::text[])[1 + g % array_length($2::text[], 1)]
		    FROM generate_series(1, $1::int) g
		  RETURNING id)
		INSERT INTO context_embed_failures (block_id, migration_id, attempts, last_error, last_class, next_attempt_at)
		SELECT id, NULL, 1, 'oversize: seeded', 'oversize', 'infinity' FROM b`, foreign, otherScopes); err != nil {
		t.Fatalf("seed the other tenant: %v", err)
	}
	// The tenant itself: 30 pending blocks, three of them parked.
	var parked []string
	for i := range 30 {
		b, err := store.UpsertBlock(ctx, pool, "learnings", fmt.Sprintf("relc own %02d", i), "own text", nil, nil, ownScopes[i%len(ownScopes)], true, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatal(err)
		}
		if i < 3 {
			parked = append(parked, b.ID)
			if err := store.RecordEmbedFailure(ctx, pool, b.ID, store.EmbedFailureOversize,
				store.NormalizeEmbedError(store.EmbedFailureOversize, store.OversizeEstimateMessage(99, 50)), time.Minute, time.Hour); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks, context_embed_failures, context_tenant_scopes`); err != nil {
		t.Fatal(err)
	}

	for _, rpc := range testdb.RandomPageCosts {
		for _, mode := range []string{"auto", "force_generic_plan"} {
			t.Run(fmt.Sprintf("random_page_cost=%s plan_cache_mode=%s", rpc, mode), func(t *testing.T) {
				capture := &relCapture{prefix: "SELECT count(*)"}
				cfg, err := pgxpool.ParseConfig(dsn)
				if err != nil {
					t.Fatal(err)
				}
				cfg.MaxConns = 1
				cfg.ConnConfig.RuntimeParams["random_page_cost"] = rpc
				cfg.ConnConfig.RuntimeParams["plan_cache_mode"] = mode
				cfg.ConnConfig.Tracer = capture
				actor, err := pgxpool.NewWithConfig(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer actor.Close()
				dry, err := store.ReleaseBackfillMemos(ctx, actor, relDryRun(own))
				if err != nil || dry.Counted != int64(len(parked)) {
					t.Fatalf("tenant dry run: %+v %v, want counted %d", dry, err, len(parked))
				}
				capture.mu.Lock()
				sql, args := capture.sql, capture.args
				capture.mu.Unlock()
				if sql == "" {
					t.Fatal("no count statement captured")
				}
				var raw string
				if err := actor.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+sql, args...).Scan(&raw); err != nil {
					t.Fatalf("explain: %v", err)
				}
				var plans []struct {
					Plan map[string]any `json:"Plan"`
				}
				if err := json.Unmarshal([]byte(raw), &plans); err != nil || len(plans) != 1 {
					t.Fatalf("parse plan: %v", err)
				}
				var p relPlan
				hit, _ := plans[0].Plan["Shared Hit Blocks"].(float64)
				read, _ := plans[0].Plan["Shared Read Blocks"].(float64)
				p.buffers = int(hit + read)
				relWalk(plans[0].Plan, &p)
				t.Logf("tenant dry run, %d own pending blocks (3 parked) next to %d memos of another tenant: %d buffer accesses, sequential scans on %v", 30, foreign, p.buffers, p.seqOn)
				for _, rel := range p.seqOn {
					if rel == "context_embed_failures" || rel == "context_blocks" {
						t.Errorf("the tenant's count scans %s sequentially — its cost grows with the other tenants' memos", rel)
					}
				}
				if p.buffers > 600 {
					t.Errorf("the tenant's count read %d buffers, want at most 600 for 30 own pending blocks", p.buffers)
				}
			})
		}
	}
}

// relDryRun is the dry run of a tenant-admin over the oversize class.
func relDryRun(tenantID string) store.BackfillRelease {
	return store.BackfillRelease{Class: store.EmbedFailureOversize, TenantID: tenantID, Hold: time.Hour}
}
