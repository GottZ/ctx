//go:build integration

package handler

// #194, the database half: a query pipeline that panics while it holds a pool
// connection inside a transaction gives the connection back, and the same
// client session then runs a real tool against the same pool. The child runs
// in its own process (mcp_recover_test.go) against the parent's test database.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// mcpPanicDSNEnv hands the parent's test database to the pool child.
const mcpPanicDSNEnv = "CTX_TEST_MCP_PANIC_DSN"

func init() { mcpPanicScenarios["pool"] = mcpPanicPoolScenario }

// connHoldingDelegate stands in for /api/query: it opens a transaction on the
// pool, runs a statement in it and panics with the connection still held.
type connHoldingDelegate struct{ pool *pgxpool.Pool }

func (d connHoldingDelegate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tx, err := d.pool.Begin(r.Context())
	if err != nil {
		http.Error(w, `{"error":"begin failed"}`, http.StatusInternalServerError)
		return
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(r.Context(), "SELECT 1"); err != nil {
		http.Error(w, `{"error":"exec failed"}`, http.StatusInternalServerError)
		return
	}
	panic(mcpPanicNeedle + " while holding a pool connection")
}

func mcpPanicPoolScenario(t *testing.T) mcpPanicReport {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv(mcpPanicDSNEnv))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	h := NewMCPHandler(MCPConfig{Pool: pool, QueryHandler: connHoldingDelegate{pool: pool}})
	srv := httptest.NewServer(RequestID(Logger(Recovery(withTestAuth(h)))))
	defer srv.Close()
	session := mcpPanicConnect(t, ctx, srv.URL)
	defer func() { _ = session.Close() }()

	var rep mcpPanicReport
	// Twenty panics, each with a connection held: one kept per panic would
	// show in the acquired count below (and, past the pool's default bound of
	// max(4, NumCPU) connections, starve the later calls).
	for range mcpPanicCalls {
		rep.Calls = append(rep.Calls, mcpPanicCallTool(ctx, session, "query", map[string]any{"question": "panic"}))
	}
	stat := pool.Stat()
	rep.PoolAcquired, rep.PoolTotal = stat.AcquiredConns(), stat.TotalConns()
	rep.FollowUp = mcpPanicCallTool(ctx, session, "recent", map[string]any{})
	return rep
}

// TestMCPToolPanicReleasesPoolConnections: after twenty panics with a held
// connection no connection is acquired any more, and a real tool on the same
// pool answers in the same client session.
func TestMCPToolPanicReleasesPoolConnections(t *testing.T) {
	_, dsn := testdb.SetupTestDBWithDSN(t)
	child := runMCPPanicChild(t, "pool", mcpPanicDSNEnv+"="+dsn)
	rep := child.report
	if len(rep.Calls) != mcpPanicCalls {
		t.Fatalf("child made %d panicking calls, want %d", len(rep.Calls), mcpPanicCalls)
	}
	for i, c := range rep.Calls {
		assertInternalToolError(t, i, c)
	}
	t.Logf("pool after %d panics: %d acquired, %d open", len(rep.Calls), rep.PoolAcquired, rep.PoolTotal)
	if rep.PoolAcquired != 0 || rep.PoolTotal == 0 {
		t.Errorf("pool after the panics: %d connections acquired (want 0), %d open (want > 0: the delegate used the pool)",
			rep.PoolAcquired, rep.PoolTotal)
	}
	if f := rep.FollowUp; f.Err != "" || f.IsError {
		t.Errorf("follow-up recent call on the same pool and session: %+v, want an answer", f)
	}
	if n := len(child.panicLogs("mcp: panic recovered")); n != mcpPanicCalls {
		t.Errorf("%d panic log lines, want %d", n, mcpPanicCalls)
	}
}
