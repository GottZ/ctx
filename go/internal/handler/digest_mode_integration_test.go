//go:build integration

// Wave W-E gate 3 (Cluster-Topic-Map, design/02 §7 "W-E"): `stub` does not read
// the corpus — measured over the WHOLE request, not just over RunDigest.
//
// The distinction is the point of the gate. A counter wrapped around RunDigest
// alone goes green while POST /api/digest still runs two unbounded O(corpus)
// counts in its response envelope — `count(*)` and, more expensively,
// `count(DISTINCT category)`, which at 10M is the single most expensive
// statement of this axis. So the counter here sits in the connection pool and
// sees every statement the request issues.
//
//	go test -tags=integration ./internal/handler/ -run TestDigestStubQueryCost -count=1 -v
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

// sqlTracer records every statement the pool issues. pgx hands the tracer the
// SQL verbatim, which is what makes an assertion about SHAPE possible at all.
type sqlTracer struct {
	mu   sync.Mutex
	on   bool
	sqls []string
}

func (t *sqlTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.on {
		t.sqls = append(t.sqls, d.SQL)
	}
	return ctx
}

func (t *sqlTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (t *sqlTracer) record(on bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.on = on
	if on {
		t.sqls = nil
	}
}

func (t *sqlTracer) statements() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.sqls...)
}

func sqlTracedPool(t *testing.T, dsn string) (*pgxpool.Pool, *sqlTracer) {
	t.Helper()
	tr := &sqlTracer{}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, tr
}

func TestDigestStubQueryCost(t *testing.T) {
	_, dsn := testdb.SetupTestDBWithDSN(t)
	pool, tr := sqlTracedPool(t, dsn)
	ctx := context.Background()

	for _, title := range []string{"cost-a", "cost-b", "cost-c"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope)
			 VALUES ('learnings', $1, 'cost fixture', 'private')`, title); err != nil {
			t.Fatalf("seed %s: %v", title, err)
		}
	}

	reg := blocktype.NewRegistry()
	bctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	reg.Boot(bctx, pool)

	cfg := &config.Config{}
	cfg.Digest.Mode = "stub"
	cfg.RootMap.CountTimeout = 5 * time.Second
	cfg.RootMap.BudgetBytes = 15360
	cfg.RootMap.FooterReserveBytes = 512
	cfg.RootMap.SmallClusterMax = 2

	h := NewDigestHandler(pool, reg, staticConfigStore{cfg: cfg})
	ar := &auth.AuthResult{IsValid: true, ApiKeyID: "00000000-0000-7000-8000-0000000000e1",
		HomeScope: "private", ReadScopes: []string{"private"}}

	req := httptest.NewRequest(http.MethodPost, "/api/digest", strings.NewReader(`{}`))
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()

	tr.record(true)
	h.HandleDigest(rec, req)
	tr.record(false)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	stmts := tr.statements()
	if len(stmts) == 0 {
		t.Fatal("the tracer saw nothing — the gate would be vacuously green")
	}

	var corpusCounts, caps int
	for _, sql := range stmts {
		norm := strings.Join(strings.Fields(sql), " ")
		// (a) The source scan of the linear map: every block's metadata, no
		// LIMIT, no cursor. Its signature is the ORDER BY it needs for grouping.
		if strings.Contains(norm, "FROM context_blocks") && strings.Contains(norm, "ORDER BY category, title") {
			t.Errorf("stub mode still ran the corpus scan:\n%s", norm)
		}
		if strings.Contains(norm, "count(") && strings.Contains(norm, "FROM context_blocks") {
			corpusCounts++
		}
		if strings.Contains(norm, "set_config('statement_timeout'") {
			caps++
		}
	}
	// (b) Whatever corpus counting is left — the envelope's block and category
	// numbers — runs under a cap. Before this wave both ran unbounded in the
	// request path, right next to a coverage count that the design caps to five
	// seconds.
	if corpusCounts > caps {
		t.Errorf("%d corpus counts against %d statement_timeout caps — at least one runs unbounded:\n%s",
			corpusCounts, caps, strings.Join(stmts, "\n"))
	}

	// The stub itself is there, and the envelope still answers.
	var length int
	if err := pool.QueryRow(ctx,
		`SELECT length(content)::int FROM context_blocks
		  WHERE category = 'index' AND title = 'topic-map-private' AND scope = 'private'`).Scan(&length); err != nil {
		t.Fatalf("no stub written: %v", err)
	}
	if length > 512 {
		t.Errorf("stub is %d B, over the gate", length)
	}
	if !strings.Contains(rec.Body.String(), `"contentLength"`) {
		t.Errorf("envelope lost contentLength: %s", rec.Body.String())
	}
}
