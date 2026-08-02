//go:build integration

// Gap-C6-b (welle B6): the PRODUCTION /mcp mount must cap the JSON-RPC request
// body like every other authenticated write surface — and it must answer in the
// house envelope, not in the MCP SDK's plain-text prose.
//
// The probes observe cmd/ctxd.NewRouter, never a re-built fixture chain: what
// is tested is the mount ORDER (Auth first, cap second) and the mounted cap
// value, both of which live only in server.go.
//
// RED (pre-B6, /mcp group without any body middleware): oversize_authenticated
// answered 200 — the 2 MB JSON-RPC body was accepted AND the block was stored.
//
// RED of the two rejected variants, both run before the mount was fixed:
// mounting the WRAPPING handler.MaxBodySize on /mcp instead answers 400 "failed
// to read body" (go-sdk streamable.go:433-435) — the MaxBytesError surfaces
// inside the SDK, so the caller never sees the house envelope. Mounting
// MaxBodySizeStrict BEFORE Auth answers the credential-less oversize probe with
// 413 instead of 401 — the cap becomes an unauthenticated oracle for "route
// exists, cap is N".
//
//	CTX_TEST_PG_IMAGE=pgvector-timescaledb:pg18 go test -tags=integration ./cmd/ctxd/ -run TestMCPBodyCap -count=1 -v
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// mcpBodyCapRouter boots the production router against a throwaway DB and
// returns it together with the plaintext admin key. Hex-only: auth.SanitizeKey
// strips every non-hex character before hashing, so the fixture must survive
// sanitization unchanged.
func mcpBodyCapRouter(t *testing.T) (http.Handler, *pgxpool.Pool, string) {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	applyEnv(t, map[string]string{})
	cc, issues := loadFromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	cfgStore := config.NewStore(cc)

	backendPool := backends.NewPool(pool, nil)
	blocktypeReg := blocktype.NewRegistry()
	scheduler := events.NewScheduler(pool, cfgStore, backendPool, events.StartupConfig{})
	projectHub := events.NewProjectHub(ctx, pool, cfgStore)
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	scheduler.SetDispatcher(d)

	const plaintext = "b6112026072800000000000000000000deadbeefdeadbeefdeadbeefdeadbeef"
	created, _, err := store.BootstrapAdminKey(ctx, pool, plaintext, "b6-mcp-bodycap-probe")
	if err != nil || !created {
		t.Fatalf("bootstrap admin key: created=%v err=%v", created, err)
	}
	return NewRouter(ctx, pool, cfgStore, scheduler, backendPool, blocktypeReg, projectHub, d), pool, plaintext
}

// mcpStoreBody builds a tools/call JSON-RPC envelope whose `content` argument is
// padded to contentBytes ASCII chars (no JSON escaping, so the wire size is
// predictable).
func mcpStoreBody(title string, contentBytes int) string {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "store",
			"arguments": map[string]any{
				"category":    "learnings",
				"title":       title,
				"content":     strings.Repeat("a", contentBytes),
				"sensitivity": "internal",
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("marshal mcp store body: %v", err))
	}
	return string(raw)
}

func postMCP(t *testing.T, router http.Handler, body, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestMCPBodyCapOnProductionMount_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	router, pool, key := mcpBodyCapRouter(t)

	// (a) Over-cap WITH a valid credential ⇒ 413 in the house envelope.
	t.Run("oversize_authenticated", func(t *testing.T) {
		body := mcpStoreBody("b6-oversize", 2<<20)
		if len(body) <= 1<<20 {
			t.Fatalf("fixture body is %d bytes, want > 1 MB", len(body))
		}
		rec := postMCP(t, router, body, key)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413; body=%.200q", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("Content-Type = %q, want application/json (house envelope)", ct)
		}
		var env struct {
			Success bool   `json:"success"`
			Error   string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("response is not the house envelope: %v (body=%.200q)", err, rec.Body.String())
		}
		if env.Success || env.Error == "" {
			t.Fatalf("envelope = %+v, want success=false with a non-empty error", env)
		}
		// The over-cap request must not have reached the tool.
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM context_blocks WHERE title = 'b6-oversize'`).Scan(&n); err != nil {
			t.Fatalf("verify no write: %v", err)
		}
		if n != 0 {
			t.Fatalf("rejected request still wrote %d block(s)", n)
		}
	})

	// (b) Under-cap store ⇒ 200 and an actual write: the strict cap must not
	// turn a legitimate large-but-allowed call into a false positive. The
	// upper bound of "legitimate" is NOT the transport cap: since B5
	// (8c597e6) the direct MCP store arm runs the full REST write-gate
	// chain, whose blockSizeLimit rejects content > 50 KiB — so the largest
	// allowed call is a hair under that content gate, well below the 1 MiB
	// body cap this test guards. (The original 900 KiB fixture predates the
	// B5 gate parity and became a size_cap reject once both waves merged.)
	t.Run("undersize_authenticated_stores", func(t *testing.T) {
		body := mcpStoreBody("b6-undersize", 45<<10)
		if len(body) >= 1<<20 {
			t.Fatalf("fixture body is %d bytes, want < 1 MB", len(body))
		}
		rec := postMCP(t, router, body, key)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%.200q", rec.Code, rec.Body.String())
		}
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM context_blocks WHERE title = 'b6-undersize'`).Scan(&n); err != nil {
			t.Fatalf("verify write: %v", err)
		}
		if n != 1 {
			t.Fatalf("under-cap store wrote %d block(s), want 1; body=%.400q", n, rec.Body.String())
		}
	})

	// (c) Order probe: over-cap WITHOUT a credential ⇒ 401, never 413. A cap
	// mounted before Auth would confirm route + limit to an anonymous caller.
	t.Run("oversize_unauthenticated_is_401", func(t *testing.T) {
		rec := postMCP(t, router, mcpStoreBody("b6-anon", 2<<20), "")
		if rec.Code == http.StatusRequestEntityTooLarge {
			t.Fatalf("status = 413 for an unauthenticated caller — cap is mounted BEFORE Auth and leaks route+limit")
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body=%.200q", rec.Code, rec.Body.String())
		}
	})
}
