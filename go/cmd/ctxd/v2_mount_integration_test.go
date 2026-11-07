//go:build integration

// W02-10: the /v2 group on the PRODUCTION router (buildRouter, the
// constructor newRouter and main use) — the second model group, mounted
// behind the same stack and the same process-wide parts as /v1 (masterplan
// K23, design/02 §4.2, §5 B7): every model route and every unregistered path
// under both prefixes needs a model key, an HTML navigation under /v2 is the
// group's JSON 404 (never the root's SPA fallback), and the failure lock and
// the accepted counter of the ONE ModelDoor span both groups.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestEveryModelRouteRequiresAuth|TestV2' -v ./cmd/ctxd/
package main

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/modelkey"
	"github.com/GottZ/ctx/internal/oai"
)

// modelPrefixes are the model groups of the production router (K23).
var modelPrefixes = []string{"/v1", "/v2"}

// anonymousModelRouteViolations walks r — chi.Walk, so a route registered
// outside a group is found as well — and sends every route under the model
// prefixes, plus per prefix an unregistered path (GET and POST), one
// anonymous request over TLS declaring a JSON body: each must be the door's
// 401 invalid_api_key with WWW-Authenticate: Bearer realm="ctx". A prefix
// without a route is a violation too (a census over nothing proves nothing).
func anonymousModelRouteViolations(r *chi.Mux) (violations []string, targets, routes int) {
	type target struct{ method, path, why string }
	var list []target
	perPrefix := map[string]int{}
	if err := chi.Walk(r, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		for _, p := range modelPrefixes {
			if pattern == p || strings.HasPrefix(pattern, p+"/") {
				perPrefix[p]++
				routes++
				path := strings.ReplaceAll(routeParam.ReplaceAllString(pattern, "probe"), "*", "probe")
				list = append(list, target{method, path, "route " + method + " " + pattern})
			}
		}
		return nil
	}); err != nil {
		return []string{"chi.Walk: " + err.Error()}, 0, 0
	}
	for _, p := range modelPrefixes {
		if perPrefix[p] == 0 {
			violations = append(violations, "prefix "+p+" carries no route — a census over nothing proves nothing")
		}
		for _, m := range []string{http.MethodGet, http.MethodPost} {
			list = append(list, target{m, p + "/every-model-route-unregistered", "unregistered path " + m + " under " + p})
		}
	}
	for _, tg := range list {
		req := httptest.NewRequest(tg.method, tg.path, strings.NewReader("{}"))
		req.RemoteAddr = "192.0.2.1:40000"
		req.TLS = &tls.ConnectionState{} // the door takes a model key over TLS only
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		a := v1Answer{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
		if a.status != http.StatusUnauthorized || a.code() != oai.CodeInvalidAPIKey || a.header.Get("WWW-Authenticate") != `Bearer realm="ctx"` {
			violations = append(violations, fmt.Sprintf("%s (%s %s), anonymous: %d %q %.80q — want 401 invalid_api_key, Bearer realm=\"ctx\"",
				tg.why, tg.method, tg.path, a.status, a.header.Get("Content-Type"), a.body))
		}
	}
	return violations, len(list), routes
}

// isJSONNotFound reports whether a is the model groups' JSON 404
// not_found — not the SPA's index.html, its 503 placeholder or the plain-text
// 404 of the root's fallback.
func isJSONNotFound(a v1Answer) bool {
	return a.status == http.StatusNotFound && a.code() == oai.CodeNotFound &&
		strings.HasPrefix(a.header.Get("Content-Type"), "application/json") &&
		!bytes.Contains(bytes.ToLower(a.body), []byte("<html")) && !bytes.Contains(bytes.ToLower(a.body), []byte("<!doctype"))
}

// TestEveryModelRouteRequiresAuth is G2 of W02-10 (design/02 §5 B7,
// masterplan K23): over the production router with gateway.enabled on, every
// route chi.Walk finds under /v1 AND /v2, and an unregistered path per
// group, answers an anonymous request with the door's 401. An HTML
// navigation under /v2 is the group's JSON 404 with the flag off and the
// door's 401 with the flag on — the root's SPA fallback never sees a /v2
// path. The control registers a /v2 route on the root, outside the group,
// and the walk must catch it.
func TestEveryModelRouteRequiresAuth(t *testing.T) {
	f := newV1Fixture(t, nil, nil)
	f.setEnabled(t, true)
	violations, targets, routes := anonymousModelRouteViolations(f.router)
	t.Logf("flag on, anonymous: %d targets over %d route(s) under %v, %d violation(s)", targets, routes, modelPrefixes, len(violations))
	for _, v := range violations {
		t.Error(v)
	}

	html := func(r *http.Request) { r.Header.Set("Accept", "text/html,application/xhtml+xml") }
	for _, path := range []string{"/v2/x", "/v2", "/v2/rerank/x"} {
		f.setEnabled(t, false)
		if a := f.do(http.MethodGet, path, html); !isJSONNotFound(a) {
			t.Errorf("flag off, GET %s with Accept text/html: %d %q %.80q, want the JSON 404 not_found of the group", path, a.status, a.header.Get("Content-Type"), a.body)
		}
		f.setEnabled(t, true)
		if a := f.do(http.MethodGet, path, html); a.status != http.StatusUnauthorized || a.code() != oai.CodeInvalidAPIKey {
			t.Errorf("flag on, anonymous GET %s with Accept text/html: %d %q %.80q, want 401 invalid_api_key", path, a.status, a.header.Get("Content-Type"), a.body)
		}
	}

	t.Run("control: a /v2 route registered outside the group", func(t *testing.T) {
		const outside = "/v2/registered-outside-the-group"
		f.router.Get(outside, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		got, _, _ := anonymousModelRouteViolations(f.router)
		if !slices.ContainsFunc(got, func(v string) bool { return strings.Contains(v, "GET "+outside) }) {
			t.Fatalf("violations %q, want the route outside the group caught", got)
		}
	})
}

// TestV2SharesTheModelDoor is G3 of W02-10 (W01-6 handover "one door for
// /v1 and /v2", masterplan K23): the production router's two model groups
// stand behind ONE ModelDoor. gateway.auth_failure_limit failures of one
// well-formed but unknown key over one group lock that key, from that
// client, on the other group as well — 429 rate_limit_exceeded with
// Retry-After and without a ctx_model_auth statement — in both directions;
// and the door's accepted counter counts the successes of both groups. A
// second door for /v2 (the counterproof overlay) keeps a lock of its own and
// a counter of its own, and fails both halves.
func TestV2SharesTheModelDoor(t *testing.T) {
	// The probe bucket stays out of this gate: every failure below must reach
	// the database or the lock, not the bucket's 503.
	f := newV1Fixture(t, map[string]string{"CTX_GATEWAY_AUTH_PROBE_RATE": "1000000"}, nil)
	f.setEnabled(t, true)
	limit := f.cfgStore.Snapshot().GatewaySettings().Auth.FailureLimit //nolint:forbidigo // test: the gateway keys are global-only
	routes := map[string][2]string{
		"/v1": {http.MethodGet, "/v1/models"},
		"/v2": {http.MethodPost, "/v2/rerank"},
	}
	for _, dir := range [][2]string{{"/v1", "/v2"}, {"/v2", "/v1"}} {
		key, err := modelkey.Generate()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		failOn, probeOn := routes[dir[0]], routes[dir[1]]
		before := f.tracer.modelAuth.Load()
		for i := range limit {
			if a := f.do(failOn[0], failOn[1], withBearer(key.Plaintext())); a.status != http.StatusUnauthorized || a.code() != oai.CodeInvalidAPIKey {
				t.Fatalf("%s %s, unknown key, attempt %d: %d %s, want 401 invalid_api_key", failOn[0], failOn[1], i+1, a.status, a.body)
			}
		}
		if n := f.tracer.modelAuth.Load() - before; n != int64(limit) {
			t.Fatalf("%d failures over %s issued %d ctx_model_auth statements, want %d (each failure a database check)", limit, dir[0], n, limit)
		}
		before = f.tracer.modelAuth.Load()
		a := f.do(probeOn[0], probeOn[1], withBearer(key.Plaintext()))
		stmts := f.tracer.modelAuth.Load() - before
		t.Logf("after %d failures over %s: %s %s → %d %s Retry-After=%q, %d ctx_model_auth statement(s)",
			limit, dir[0], probeOn[0], probeOn[1], a.status, strings.TrimSpace(string(a.body)), a.header.Get("Retry-After"), stmts)
		if a.status != http.StatusTooManyRequests || a.code() != oai.CodeRateLimitExceeded || a.header.Get("Retry-After") == "" || stmts != 0 {
			t.Errorf("%d failures over %s, then %s %s with the same key: %d %s Retry-After=%q, %d statement(s) — want 429 rate_limit_exceeded with Retry-After and no database check (one failure lock for both groups)",
				limit, dir[0], probeOn[0], probeOn[1], a.status, a.body, a.header.Get("Retry-After"), stmts)
		}
	}

	accepted := f.gateway.door.Stats().Accepted
	if a := f.do(http.MethodGet, "/v1/models", withBearer(f.modelKey)); a.status != http.StatusOK {
		t.Errorf("GET /v1/models with a live key: %d %s, want 200", a.status, a.body)
	}
	// The rerank handler answers the empty body itself (400): the door let it
	// through.
	if a := f.do(http.MethodPost, "/v2/rerank", withBearer(f.modelKey)); a.status == http.StatusUnauthorized || a.status == http.StatusTooManyRequests || a.code() == oai.CodeNotFound {
		t.Errorf("POST /v2/rerank with a live key: %d %s, want the handler's answer behind the door", a.status, a.body)
	}
	if n := f.gateway.door.Stats().Accepted - accepted; n != 2 {
		t.Errorf("the gateway door accepted %d request(s) for one success per group, want 2 (one door counts /v1 and /v2)", n)
	}
}
