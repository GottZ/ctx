//go:build integration

// F-W9 (design/04 §4.6, masterplan K14): the PeerIngress sits in every model
// group between the LoopGuard and the inner stages — EnabledGate → ModelAuth
// → LoopGuard → PeerIngress → body cap — mounted over mountModelGroup with
// the real door and real ctxm_ keys (an app and a ctx-peer consumer). G8's
// mount half: a group with the LoopGuard but without the PeerIngress is red
// here and in the route inventory below.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestModelGroupPeerIngress|TestPeerIngressRouteInventory' -v ./cmd/ctxd/
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// peerIngressSeen is what an inner stage of the group observed.
type peerIngressSeen struct {
	mu  sync.Mutex
	in  fedhop.Inbound
	pol federation.Policy
	ran bool
}

func (s *peerIngressSeen) stage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.in, s.pol, s.ran = fedhop.InboundFrom(r.Context()), federation.PolicyFrom(r.Context()), true
		s.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (s *peerIngressSeen) take() (fedhop.Inbound, federation.Policy, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, pol, ran := s.in, s.pol, s.ran
	s.in, s.pol, s.ran = fedhop.Inbound{}, federation.Policy{}, false
	return in, pol, ran
}

// modelRoutesOf lists the routes of r under /v1 and /v2.
func modelRoutesOf(t *testing.T, r chi.Routes) []string {
	t.Helper()
	var out []string
	if err := chi.Walk(r, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasPrefix(pattern, "/v1/") || strings.HasPrefix(pattern, "/v2/") || pattern == "/v1" || pattern == "/v2" {
			out = append(out, method+" "+pattern)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(out)
	return out
}

// peerIngressInventory is the PeerIngress stage of the route inventory: every
// route under /v1 and /v2 gets a request with a ctx-peer key and no
// Ctx-Path, declaring a body above every cap. Only the PeerIngress counts
// federation_peer_without_path, and it must do so before the body cap
// answers — a route whose request leaves the counter untouched has no
// PeerIngress in front of its inner stages. Dynamic like the LoopGuard
// inventory: what the stack does, not which middleware pointers it holds.
func peerIngressInventory(t *testing.T, r http.Handler, routes []string, peerKey string) (missing []string) {
	t.Helper()
	for _, name := range routes {
		method, pattern, _ := strings.Cut(name, " ")
		rctx, cancel := context.WithTimeout(context.Background(), inventoryRequestTimeout)
		req := httptest.NewRequestWithContext(rctx, method, placeholderPath(pattern), strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = inventoryDeclaredBody
		req.TLS = &tls.ConnectionState{}
		req.Header.Set("Authorization", "Bearer "+peerKey)
		before := federation.CounterSnapshot().PeerWithoutPath
		r.ServeHTTP(httptest.NewRecorder(), req)
		cancel()
		if federation.CounterSnapshot().PeerWithoutPath == before {
			missing = append(missing, name)
		}
	}
	return missing
}

func TestModelGroupPeerIngressOrder(t *testing.T) {
	seed, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	admin, _, err := store.CreateApiKey(ctx, seed, "group-peeringress", "private", nil, "")
	if err != nil {
		t.Fatalf("admin key: %v", err)
	}
	_, _, app, err := store.CreateConsumerWithKey(ctx, seed, store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "peeringress-app"}, nil, store.APIStamp(admin.ID, "peeringress"))
	if err != nil {
		t.Fatalf("app key: %v", err)
	}
	_, _, peer, err := store.CreateConsumerWithKey(ctx, seed, store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "peeringress-peer", Kind: "ctx-peer"}, nil, store.APIStamp(admin.ID, "peeringress"))
	if err != nil {
		t.Fatalf("peer key: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	base := config.Defaults().GatewaySettings()
	get := func() oai.Settings { s := base; s.Enabled = true; return s }
	door := handler.ModelAuth(handler.ModelAuthConfig{
		Pool: pool, Settings: get, Usage: groupUsage{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Reject: func(w http.ResponseWriter, _ *http.Request, rej handler.ModelAuthRejection) {
			w.WriteHeader(rej.Status)
			_, _ = io.WriteString(w, `{"error":{"code":"`+rej.Code+`"}}`)
		},
	})
	const bodyCap = 1024
	ident := fedhop.NewIdentity("00000000-0000-4000-8000-0000000000c9", bytes.Repeat([]byte{0xc9}, 32))
	other := fedhop.NewIdentity("00000000-0000-4000-8000-0000000000d9", bytes.Repeat([]byte{0xd9}, 32))
	ingress := &fedhop.Ingress{}
	seen := &peerIngressSeen{}
	var handled atomic.Int64
	r := chi.NewRouter()
	r.Use(fedhop.IdentityMiddleware(ident))
	probe := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { handled.Add(1); w.WriteHeader(http.StatusOK) })
	for prefix, routes := range map[string][]modelRoute{
		"/v1": {{http.MethodPost, "/chat/completions", probe}, {http.MethodGet, "/models", probe}, {http.MethodPost, "/embeddings", probe}},
		"/v2": {{http.MethodPost, "/rerank", probe}},
	} {
		mountModelGroup(r, prefix, modelGroupDeps{
			access: testAccessLog(), gate: oai.EnabledGate(get), door: door, ingress: ingress, maxDepth: func() int { return 3 },
			inner:  []func(http.Handler) http.Handler{seen.stage, handler.MaxBodySizeStrictWith(func() int64 { return bodyCap }, oai.RefuseRequestTooLarge)},
			routes: routes,
		})
	}
	if !ingress.Registered() {
		t.Fatal("mountModelGroup did not register the router's ingress (LoopGuard and PeerIngress)")
	}

	do := func(key string, hdr map[string]string, bodyLen int) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Repeat("x", bodyLen)))
		req.TLS = &tls.ConnectionState{}
		req.Header.Set("Authorization", "Bearer "+key)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}
	chain := map[string]string{fedhop.HeaderHop: other.MintToken(), fedhop.HeaderPath: other.ID}

	// A peer with Ctx-Path: the inner stages see the full, cleaned Inbound —
	// the class survives (the LoopGuard alone would have reduced it to the
	// chain) — and the policy PeerIngress resolved.
	hdr := map[string]string{fedhop.HeaderClass: "interactive", fedhop.HeaderPipeline: "dream", fedhop.HeaderSensitivity: "personal"}
	for k, v := range chain {
		hdr[k] = v
	}
	if st := do(peer.Plaintext(), hdr, 2); st != http.StatusOK {
		t.Fatalf("peer with path: status %d, want 200", st)
	}
	in, pol, ran := seen.take()
	if !ran || in.Class != "interactive" || in.Pipeline != "dream" || len(in.Path) != 1 {
		t.Errorf("peer with path: inner stage saw %+v (ran %v) — want class interactive, pipeline dream, the path (PeerIngress before inner, after LoopGuard)", in, ran)
	}
	if pol != (federation.Policy{Required: backends.SensCredentials, EgressLocal: false}) {
		t.Errorf("peer with path: policy %+v, want {credentials false} (consumer floor credentials, header personal cannot lower)", pol)
	}

	// An app: class dropped, policy resolved — EgressLocal false proves the
	// PeerIngress set it (the default without policy is true).
	if st := do(app.Plaintext(), map[string]string{fedhop.HeaderClass: "interactive"}, 2); st != http.StatusOK {
		t.Fatalf("app: status %d, want 200", st)
	}
	in, pol, ran = seen.take()
	if !ran || in.Class != "" || pol.EgressLocal || pol.Required != backends.SensCredentials {
		t.Errorf("app: inner stage saw %+v / %+v (ran %v), want no class and {credentials false}", in, pol, ran)
	}

	// A peer without Ctx-Path and a body over the cap: counted by the
	// PeerIngress, then refused by the cap (PeerIngress before body cap).
	before := federation.CounterSnapshot().PeerWithoutPath
	if st := do(peer.Plaintext(), nil, bodyCap+1); st != http.StatusRequestEntityTooLarge {
		t.Errorf("peer without path + oversize body: %d, want 413", st)
	}
	if got := federation.CounterSnapshot().PeerWithoutPath - before; got != 1 {
		t.Errorf("peer without path: counter +%d, want +1 before the body cap", got)
	}

	// The LoopGuard comes first: an own-token request is refused before the
	// PeerIngress counts anything.
	before = federation.CounterSnapshot().PeerWithoutPath
	if st := do(peer.Plaintext(), map[string]string{fedhop.HeaderHop: ident.MintToken()}, 2); st != http.StatusLoopDetected {
		t.Errorf("peer + own token: %d, want 508", st)
	}
	if got := federation.CounterSnapshot().PeerWithoutPath - before; got != 0 {
		t.Errorf("peer + own token: PeerIngress counted (+%d) — it runs before the LoopGuard", got)
	}
	if n := handled.Load(); n != 2 {
		t.Errorf("handler ran %d times, want 2 (the two 200s)", n)
	}

	// The inventory stage over every route of both groups.
	routes := modelRoutesOf(t, r)
	if missing := peerIngressInventory(t, r, routes, peer.Plaintext()); len(missing) > 0 {
		t.Errorf("%d of %d model routes without PeerIngress in front of the inner stages:\n  %s", len(missing), len(routes), strings.Join(missing, "\n  "))
	}
	t.Logf("peer ingress inventory over the test group: %d routes %v", len(routes), routes)
}

// TestPeerIngressRouteInventory is the PeerIngress stage of the inventory over
// the PRODUCTION router (F-W9, lead decision 4): every /v1 and /v2 route
// newRouter mounts counts a pathless peer request before its body cap. The
// model groups are mounted from W02-05/W02-10 on; until then the walk finds
// none and the test says so — it runs unchanged in the mount PR.
func TestPeerIngressRouteInventory(t *testing.T) {
	fx := inventoryRouter(t)
	routes := modelRoutesOf(t, fx.router)
	if len(routes) == 0 {
		t.Logf("peer ingress inventory: no /v1 or /v2 route mounted yet (W02-05, W02-10) — nothing to check; the registration test pins that the ingress is then unregistered")
		if fx.ingress.Registered() {
			t.Fatal("ingress registered without any model route")
		}
		return
	}
	if missing := peerIngressInventory(t, fx.router, routes, fx.peerKey); len(missing) > 0 {
		t.Errorf("%d of %d model routes without PeerIngress in front of the inner stages:\n  %s", len(missing), len(routes), strings.Join(missing, "\n  "))
	}
	t.Logf("peer ingress inventory: %d model routes checked", len(routes))
}
