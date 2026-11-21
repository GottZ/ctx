//go:build integration

package handler

// Issue #453: the cost of the MCP write tool's server line. One direct store
// call through the production chain (RequestID, Logger, Auth, the MCP
// handler) over a real database, with the line and — the same server with
// the store tool registered bare, mcp.AddTool without addMCPTool — without
// it, alternating, on one database. The log goes to a JSON handler on
// io.Discard: the formatting is paid, the sink is free. The line must not add
// a statement: both variants send the same number per call. The time per call
// is logged; the wrapper alone is BenchmarkToolWriteLine.
//
//	go test -tags=integration ./internal/handler/ -run TestMCPStoreCallCost -count=1 -v

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	pgxvec "github.com/pgvector/pgvector-go/pgx"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// stmtCounter counts the statements a pool sends.
type stmtCounter struct{ n atomic.Int64 }

func (c *stmtCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}
func (c *stmtCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestMCPStoreCallCost(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	_, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pcfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error { return pgxvec.RegisterTypes(ctx, conn) }
	counter := &stmtCounter{}
	pcfg.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	_, plain, err := store.CreateApiKey(ctx, pool, "i453-bench", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := MCPConfig{Pool: pool, Blocktypes: reg, Cfg: staticConfigStore{cfg: &config.Config{
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute, BatchMaxItems: 100},
	}}}
	serve := func(server *mcp.Server) string {
		sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
			&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
		srv := httptest.NewServer(RequestID(Logger(Auth(pool)(withMCPRequestContext(withMCPKeepalive(sdk))))))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	withLine := serve(newMCPServer(cfg))
	bareServer := newMCPServer(cfg)
	mcp.AddTool(bareServer, &mcp.Tool{Name: "store"}, mcpStoreHandler(cfg))
	bare := serve(bareServer)

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// A fixed number of calls per variant and round: the test runs in every
	// integration run, so it measures a few dozen writes, not a benchmark's
	// second per variant.
	const rounds, calls = 3, 30
	type sample struct{ ns, stmts float64 }
	measure := func(url, variant string, round int) sample {
		before, start := counter.n.Load(), time.Now()
		for i := range calls {
			storeCall(t, url, plain, fmt.Sprintf("i453 bench %s %d %d", variant, round, i))
		}
		return sample{ns: float64(time.Since(start).Nanoseconds()) / calls, stmts: float64(counter.n.Load()-before) / calls}
	}
	// Warm up both chains first: a connection the pool opens runs its own
	// setup statements (AfterConnect), which would land in one round.
	measure(withLine, "warm-line", -1)
	measure(bare, "warm-bare", -1)
	var a, z [rounds]sample
	for r := range rounds {
		a[r] = measure(withLine, "line", r)
		z[r] = measure(bare, "bare", r)
	}
	for r := range rounds {
		t.Logf("round %d: with line %.0f ns/call %.2f stmts/call | bare %.0f ns/call %.2f stmts/call",
			r, a[r].ns, a[r].stmts, z[r].ns, z[r].stmts)
		if a[r].stmts != z[r].stmts {
			t.Errorf("round %d: %.2f statements per call with the line, %.2f without — the line must not add a database round trip", r, a[r].stmts, z[r].stmts)
		}
	}
}

// storeCall makes one direct store call that writes a new block.
func storeCall(t *testing.T, url, plain, title string) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "store", "arguments": map[string]any{
			"category": "i453-bench", "title": title, "content": "bench content " + title}}})
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("X-Context-Key", plain)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Error(err)
		return
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), "Stored:") {
		t.Errorf("store: %s", raw)
	}
}
