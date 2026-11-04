//go:build integration

// F-W8 G3 (design/04 §4.6 "Routen-Inventar-Gate", Finding 4): chi.Walk over
// the PRODUCTION router. Every route under /api, /mcp, /v1 and /v2 gets an
// authenticated request whose Ctx-Hop carries the router's own token and
// must answer 508 — the proof is dynamic (what the route answers), not a
// comparison of middleware pointers. Every request declares a body above
// all caps, so a strict cap in front of the guard (or of the
// authentication) is found as well. The same request without credential
// must answer 401: authentication runs before the federation check, so an
// anonymous probe gets no instance oracle. Named exceptions carry their
// reason; an exception that no longer exists is an error too.
//
// /v1 and /v2 are not mounted in this wave (W02-05, W02-10). The inventory
// covers them already: their routes take the model key (ctxm_), the gateway
// flag is on, so the mount PR runs this gate unchanged.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestLoopGuardRouteInventory|TestLoopGuardMaxDepthIsHot' -v ./cmd/ctxd/
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// inventoryExceptions are the routes under the inventoried prefixes that
// carry no LoopGuard, by name and with the reason.
var inventoryExceptions = map[string]string{
	"GET /api/img/{sig}": "camo fetch half: no authentication (the HMAC signature is the capability, a browser <img> carries no key) and no model egress",
}

// inventoryPrefixes are the authenticated groups with model egress (K14).
var inventoryPrefixes = []string{"/api", "/mcp", "/v1", "/v2"}

// inventoryRequestTimeout bounds one inventory request: a guarded route
// answers 508 at once, an unguarded stream would otherwise never return.
const inventoryRequestTimeout = 3 * time.Second

// inventoryDeclaredBody is the Content-Length every inventory request
// declares: above the largest group cap (blob store, 75 MiB) and the
// gateway's request cap. The body itself stays two bytes.
const inventoryDeclaredBody = 1 << 30

const inventoryAdminKey = "f8f82026110400000000000000000000deadbeefdeadbeefdeadbeefdeadbeef"

type inventoryFixture struct {
	router   *chi.Mux
	ingress  *fedhop.Ingress
	pool     *pgxpool.Pool
	ident    fedhop.Identity
	adminKey string
	modelKey string
}

// inventoryRouter boots the production router against a throwaway DB, with
// an identity of its own and the gateway flag on (the model groups answer
// once mounted).
func inventoryRouter(t *testing.T) inventoryFixture {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	applyEnv(t, map[string]string{"CTX_GATEWAY_ENABLED": "true"})
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

	created, adminID, err := store.BootstrapAdminKey(ctx, pool, inventoryAdminKey, "f-w8-inventory")
	if err != nil || !created {
		t.Fatalf("bootstrap admin key: created=%v err=%v", created, err)
	}
	_, _, model, err := store.CreateConsumerWithKey(ctx, pool, store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "f-w8-inventory"}, nil, store.APIStamp(adminID, "f-w8-inventory"))
	if err != nil {
		t.Fatalf("model key: %v", err)
	}
	ident := fedhop.NewIdentity("00000000-0000-4000-8000-0000000000f8", bytes.Repeat([]byte{0xf8}, 32))
	// newRouter, the constructor main uses: the fixture also carries the
	// router's own ingress registration (FR2-2 pins it).
	router, ingress := newRouter(fedhop.WithIdentity(ctx, ident), pool, cfgStore, scheduler, backendPool, blocktype.NewRegistry(), projectHub, d)
	return inventoryFixture{router: router, ingress: ingress, pool: pool, ident: ident, adminKey: inventoryAdminKey, modelKey: model.Plaintext()}
}

var routeParam = regexp.MustCompile(`\{[^}]*\}`)

// placeholderPath fills every path parameter; a UUID matches every
// unconstrained parameter the routes use.
func placeholderPath(route string) string {
	p := routeParam.ReplaceAllString(route, "00000000-0000-4000-8000-000000000001")
	return strings.TrimSuffix(p, "/*")
}

func inventoried(route string) bool {
	for _, p := range inventoryPrefixes {
		if route == p || strings.HasPrefix(route, p+"/") {
			return true
		}
	}
	return false
}

func TestLoopGuardRouteInventory(t *testing.T) {
	fx := inventoryRouter(t)
	type route struct{ method, pattern string }
	var routes []route
	if err := chi.Walk(fx.router, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if inventoried(pattern) {
			routes = append(routes, route{method, pattern})
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].pattern != routes[j].pattern {
			return routes[i].pattern < routes[j].pattern
		}
		return routes[i].method < routes[j].method
	})

	seenException := map[string]bool{}
	var missing, oracle []string
	checked := 0
	for _, rt := range routes {
		name := rt.method + " " + rt.pattern
		if _, ok := inventoryExceptions[name]; ok {
			seenException[name] = true
			continue
		}
		checked++
		key := fx.adminKey
		if strings.HasPrefix(rt.pattern, "/v1") || strings.HasPrefix(rt.pattern, "/v2") {
			key = fx.modelKey
		}
		path := placeholderPath(rt.pattern)
		for _, anonymous := range []bool{false, true} {
			// A route without guard may be a stream (GET /api/events, the MCP
			// GET stream): the deadline ends it, the status it wrote is the
			// finding.
			rctx, cancel := context.WithTimeout(context.Background(), inventoryRequestTimeout)
			req := httptest.NewRequestWithContext(rctx, rt.method, path, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			// A declared body above every group cap (Fremd-Review FR-1): the
			// guard and the authentication answer before any cap does, so a
			// strict cap (/mcp, the model groups' inner stages) moved in
			// front of them shows up as a 413 here. The plain MaxBodySize of
			// the /api groups only wraps the body, which the guard never
			// reads — its position relative to the guard has no effect.
			req.ContentLength = inventoryDeclaredBody
			req.TLS = &tls.ConnectionState{} // the model-key door (/v1, /v2) accepts a key over TLS only
			if !anonymous {
				req.Header.Set("Authorization", "Bearer "+key)
			}
			req.Header.Set(fedhop.HeaderHop, fx.ident.MintToken())
			rec := httptest.NewRecorder()
			fx.router.ServeHTTP(rec, req)
			cancel()
			switch {
			case !anonymous && rec.Code != http.StatusLoopDetected:
				missing = append(missing, name+" → "+http.StatusText(rec.Code))
			case anonymous && rec.Code != http.StatusUnauthorized:
				oracle = append(oracle, name+" (anonymous) → "+http.StatusText(rec.Code))
			}
		}
	}
	t.Logf("inventory: %d routes under %v checked, %d named exceptions", checked, inventoryPrefixes, len(seenException))
	for _, rt := range routes {
		t.Logf("  %s %s", rt.method, rt.pattern)
	}
	for name, why := range inventoryExceptions {
		if !seenException[name] {
			t.Errorf("exception %q (%s) is no longer a route — drop it", name, why)
		} else {
			t.Logf("exception %s: %s", name, why)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d routes answer an authenticated own-token request without 508 (no LoopGuard in front):\n  %s", len(missing), strings.Join(missing, "\n  "))
	}
	if len(oracle) > 0 {
		t.Errorf("%d routes answer an anonymous own-token request without 401 (federation before auth, or no auth):\n  %s", len(oracle), strings.Join(oracle, "\n  "))
	}
	// A walk that finds next to nothing proves nothing: the production router
	// has well over forty routes under /api alone.
	if checked < 40 {
		t.Errorf("only %d routes inventoried — the walk did not see the production router", checked)
	}
}

// TestLoopGuardMaxDepthIsHot is the hot half of G6 over the production
// router: a settings write of federation.max_depth applies to the next
// request without a restart, and V43 refuses an unreachable limit with 422.
func TestLoopGuardMaxDepthIsHot(t *testing.T) {
	fx := inventoryRouter(t)
	other := fedhop.NewIdentity("00000000-0000-4000-8000-0000000000e9", bytes.Repeat([]byte{0xe9}, 32))
	chain := other.MintToken() + ", " + other.MintToken()
	search := func() (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/api/search", strings.NewReader(`{"query":"x"}`))
		req.Header.Set("Authorization", "Bearer "+fx.adminKey)
		req.Header.Set(fedhop.HeaderHop, chain)
		rec := httptest.NewRecorder()
		fx.router.ServeHTTP(rec, req)
		return rec.Code, rec.Header().Get(fedhop.HeaderReason)
	}
	put := func(v string) int {
		req := httptest.NewRequest(http.MethodPut, "/api/settings/federation.max_depth", strings.NewReader(`{"value":`+v+`}`))
		req.Header.Set("Authorization", "Bearer "+fx.adminKey)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		fx.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			var e map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &e)
			t.Logf("PUT max_depth=%s: %d %v", v, rec.Code, e["error"])
		}
		return rec.Code
	}
	if st, reason := search(); st == http.StatusLoopDetected {
		t.Fatalf("default depth 3, chain 2: 508 (%s) — want the request served", reason)
	}
	if st := put("2"); st != http.StatusOK {
		t.Fatalf("PUT federation.max_depth=2 = %d, want 200", st)
	}
	if st, reason := search(); st != http.StatusLoopDetected || reason != string(fedhop.ReasonDepth) {
		t.Fatalf("after the write, chain 2: %d Ctx-Reason %q, want 508 depth without a restart", st, reason)
	}
	for _, bad := range []string{"0", "17"} {
		if st := put(bad); st != http.StatusUnprocessableEntity {
			t.Errorf("PUT federation.max_depth=%s = %d, want 422 (V43)", bad, st)
		}
	}
}

// TestNewRouterRegistersIngressOnlyWithModelGroup (Fremd-Review 2, FR2-2)
// pins the production wiring of the registration: the router main serves is
// registered exactly when it mounts a model group (/v1 or /v2 — only
// mountModelGroup marks, and it puts the LoopGuard in front). Without one,
// PUT /api/settings/gateway.enabled {"value":true} through that same router
// is a 409 and leaves no override row; with one (W02-05 onwards) the write
// is accepted. A registration that newRouter set on its own (fail-open) or a
// settings handler wired to another router's marker would break the first
// half before any model route exists.
func TestNewRouterRegistersIngressOnlyWithModelGroup(t *testing.T) {
	fx := inventoryRouter(t)
	modelRoutes := 0
	if err := chi.Walk(fx.router, func(_, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasPrefix(pattern, "/v1/") || strings.HasPrefix(pattern, "/v2/") || pattern == "/v1" || pattern == "/v2" {
			modelRoutes++
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if got, want := fx.ingress.Registered(), modelRoutes > 0; got != want {
		t.Fatalf("ingress registered = %v with %d model routes — want registered exactly when a model group is mounted", got, modelRoutes)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/settings/gateway.enabled", strings.NewReader(`{"value":true}`))
	req.Header.Set("Authorization", "Bearer "+fx.adminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	fx.router.ServeHTTP(rec, req)
	var rows int
	if err := fx.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_settings WHERE key = 'gateway.enabled'`).Scan(&rows); err != nil {
		t.Fatalf("row probe: %v", err)
	}
	if modelRoutes == 0 {
		if rec.Code != http.StatusConflict || rows != 0 {
			t.Fatalf("no model group: PUT gateway.enabled=true = %d (%s), %d override rows — want 409 and none", rec.Code, rec.Body.Bytes(), rows)
		}
		return
	}
	if rec.Code != http.StatusOK || rows != 1 {
		t.Fatalf("model group mounted: PUT gateway.enabled=true = %d (%s), %d override rows — want 200 and one", rec.Code, rec.Body.Bytes(), rows)
	}
}
