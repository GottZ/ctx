//go:build integration

// W02-05 dark-launch witness (Bestands-Pin, not a gate): the router with
// gateway.enabled=false (the production state until RC-D) is unchanged
// outside /v1 and /v2 — the same route inventory but the model-group
// entries, the same bytes for a fixed set of requests. The tests use only
// what the base of the wave already has (NewRouter), so the same file runs
// on the base and on the head and the two -v outputs are compared (reports
// W02-05, W02-10). The model-group rows are listed separately: there the
// answer changes on purpose, from the plain-text 404 / the SPA's index.html
// (or its 503 placeholder) to the JSON 404 of the group.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestDarkLaunchRouterInventory|TestDarkLaunchGoldenBytes' -v ./cmd/ctxd/
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/testdb"
)

// darkLaunchRouter is NewRouter with the default environment (gateway off).
func darkLaunchRouter(t *testing.T) *chi.Mux {
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
	scheduler := events.NewScheduler(pool, cfgStore, backendPool, events.StartupConfig{})
	projectHub := events.NewProjectHub(ctx, pool, cfgStore)
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	scheduler.SetDispatcher(d)
	return NewRouter(ctx, pool, cfgStore, scheduler, backendPool, blocktype.NewRegistry(), projectHub, d)
}

// TestDarkLaunchRouterInventory logs every "METHOD pattern" of the router,
// sorted, the model-group entries marked; it requires that every route under
// /v1 or /v2 is one the model groups register (W02-05; W02-07 adds the chat
// route, W02-09 the embeddings route, W02-10 the rerank routes of /v1 and
// /v2).
func TestDarkLaunchRouterInventory(t *testing.T) {
	r := darkLaunchRouter(t)
	var lines []string
	model := 0
	if err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		mark := ""
		if route == "/v1" || route == "/v2" || strings.HasPrefix(route, "/v1/") || strings.HasPrefix(route, "/v2/") {
			mark = "  [model group]"
			model++
			switch name := method + " " + route; name {
			case "GET /v1/models", "GET /v1/models/*", "POST /v1/chat/completions", // W02-05, W02-07
				"POST /v1/embeddings",                // W02-09
				"POST /v1/rerank", "POST /v2/rerank": // W02-10
			default:
				t.Errorf("unexpected model-group route %s", name)
			}
		}
		lines = append(lines, method+" "+route+mark)
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(lines)
	t.Logf("inventory: %d routes, %d under /v1 or /v2", len(lines), model)
	for _, l := range lines {
		t.Log("  " + l)
	}
}

// goldenRequests are the requests outside the model groups whose answer
// must not change, then the model-group rows (/v1, and /v2 since W02-10)
// whose answer changes on purpose.
var goldenRequests = []struct {
	name, method, path, accept string
	model                      bool
}{
	{name: "GET /health", method: http.MethodGet, path: "/health"},
	{name: "GET /api/unbekannt", method: http.MethodGet, path: "/api/unbekannt"},
	{name: "POST /", method: http.MethodPost, path: "/"},
	{name: "GET /blocks (HTML)", method: http.MethodGet, path: "/blocks", accept: "text/html"},
	{name: "GET /.well-known/oauth-authorization-server", method: http.MethodGet, path: "/.well-known/oauth-authorization-server"},
	{name: "GET /v1/models", method: http.MethodGet, path: "/v1/models", model: true},
	{name: "GET /v1/models (HTML)", method: http.MethodGet, path: "/v1/models", accept: "text/html", model: true},
	{name: "DELETE /v1/models", method: http.MethodDelete, path: "/v1/models", model: true},
	{name: "POST /v1/chat/completions", method: http.MethodPost, path: "/v1/chat/completions", model: true},
	{name: "POST /v1/embeddings", method: http.MethodPost, path: "/v1/embeddings", model: true},
	{name: "POST /v1/rerank", method: http.MethodPost, path: "/v1/rerank", model: true},
	{name: "POST /v2/rerank", method: http.MethodPost, path: "/v2/rerank", model: true},
	{name: "GET /v2/x (HTML)", method: http.MethodGet, path: "/v2/x", accept: "text/html", model: true},
}

// goldenVolatile are the headers that vary per request by design.
var goldenVolatile = map[string]bool{"X-Request-Id": true, "Date": true}

// TestDarkLaunchGoldenBytes logs, per request, the status, the headers
// without X-Request-ID and Date, and the SHA-256 of status + headers + body
// (the body itself for the model-group rows and short bodies) — compared between
// base and head in the report.
func TestDarkLaunchGoldenBytes(t *testing.T) {
	r := darkLaunchRouter(t)
	for _, g := range goldenRequests {
		req := httptest.NewRequest(g.method, g.path, nil)
		if g.accept != "" {
			req.Header.Set("Accept", g.accept)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var hs []string
		for k, v := range rec.Header() {
			if !goldenVolatile[http.CanonicalHeaderKey(k)] {
				hs = append(hs, k+": "+strings.Join(v, ", "))
			}
		}
		sort.Strings(hs)
		sum := sha256.Sum256([]byte(http.StatusText(rec.Code) + "\n" + strings.Join(hs, "\n") + "\n\n" + rec.Body.String()))
		body := ""
		if g.model || rec.Body.Len() <= 160 {
			body = " body=" + strings.TrimSpace(rec.Body.String())
		}
		prefix := "golden"
		if g.model {
			prefix = "model group (changes on purpose)"
		}
		t.Logf("%s %s: %d sha256=%s headers=[%s]%s", prefix, g.name, rec.Code, hex.EncodeToString(sum[:8]), strings.Join(hs, "; "), body)
	}
}
