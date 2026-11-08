//go:build integration

// W01-6 (K23): the door gate over mountModelGroup itself, for /v1 and /v2
// with one shared door — the form W02-05's mount PR runs with the real
// handlers.
//
//	go test -tags=integration -p 1 -count=1 -run TestModelGroup -v ./cmd/ctxd/
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/handler/v1door"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

type groupUsage struct{}

func (groupUsage) Touch(string) {}

func TestModelGroupDoor_Integration(t *testing.T) {
	seed, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	admin, knowledge, err := store.CreateApiKey(ctx, seed, "group-knowledge", "private", nil, "")
	if err != nil {
		t.Fatalf("knowledge key: %v", err)
	}
	_, _, model, err := store.CreateConsumerWithKey(ctx, seed, store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "group"}, nil, store.APIStamp(admin.ID, "group"))
	if err != nil {
		t.Fatalf("model key: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	tracer := &v1door.KnowledgeAuthTracer{}
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	var enabled atomic.Bool
	base := config.Defaults().GatewaySettings()
	get := func() oai.Settings { s := base; s.Enabled = enabled.Load(); return s }
	door := handler.ModelAuth(handler.ModelAuthConfig{
		Pool: pool, Settings: get, Usage: groupUsage{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Reject: func(w http.ResponseWriter, _ *http.Request, rej handler.ModelAuthRejection) {
			w.WriteHeader(rej.Status)
			_, _ = io.WriteString(w, `{"error":{"code":"`+rej.Code+`"}}`)
		},
	})
	gate := oai.EnabledGate(get)
	access := handler.NewGatewayAccessLog(handler.GatewayAccessLogConfig{Settings: get, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	ingress := &fedhop.Ingress{}
	depth := func() int { return 3 }
	mount := func(r chi.Router, probe http.Handler) {
		mountModelGroup(r, "/v1", modelGroupDeps{access: access, gate: gate, door: door, ingress: ingress, maxDepth: depth, binder: acceptPeerBinder{}, requirePeerHop: noPeerHop, routes: []modelRoute{
			{http.MethodGet, "/models", probe},
			{http.MethodGet, "/models/{model}", probe},
			{http.MethodPost, "/chat/completions", probe},
		}})
		mountModelGroup(r, "/v2", modelGroupDeps{access: access, gate: gate, door: door, ingress: ingress, maxDepth: depth, binder: acceptPeerBinder{}, requirePeerHop: noPeerHop, routes: []modelRoute{
			{http.MethodPost, "/rerank", probe},
		}})
	}
	v1door.Assert(t, mount, v1door.Options{
		SetEnabled: enabled.Store, KnowledgeKey: knowledge, KnowledgeAuth: tracer, ModelKey: model.Plaintext(),
	})
}

// TestModelGroupRefusesMissingDeps: a group without access log, gate, door,
// ingress registration or depth getter is a wiring bug and panics at mount
// time instead of serving an open (or unlogged) group (F-W8: the LoopGuard is
// fixed in the group, so its two inputs are as mandatory as the door; W06-9:
// so is the access log; F-W12: so are the PeerIngress's peer binder and the
// federation.require_peer_hop getter).
func TestModelGroupRefusesMissingDeps(t *testing.T) {
	access := handler.NewGatewayAccessLog(handler.GatewayAccessLogConfig{Settings: func() oai.Settings { return oai.Settings{} }})
	pass := func(h http.Handler) http.Handler { return h }
	depth := func() int { return 3 }
	for name, deps := range map[string]modelGroupDeps{
		"no gate":             {access: access, door: &handler.ModelDoor{}, ingress: &fedhop.Ingress{}, maxDepth: depth, binder: acceptPeerBinder{}, requirePeerHop: noPeerHop},
		"no door":             {access: access, gate: pass, ingress: &fedhop.Ingress{}, maxDepth: depth, binder: acceptPeerBinder{}, requirePeerHop: noPeerHop},
		"no access log":       {gate: pass, door: &handler.ModelDoor{}, ingress: &fedhop.Ingress{}, maxDepth: depth, binder: acceptPeerBinder{}, requirePeerHop: noPeerHop},
		"no ingress":          {access: access, gate: pass, door: &handler.ModelDoor{}, maxDepth: depth, binder: acceptPeerBinder{}, requirePeerHop: noPeerHop},
		"no max depth":        {access: access, gate: pass, door: &handler.ModelDoor{}, ingress: &fedhop.Ingress{}, binder: acceptPeerBinder{}, requirePeerHop: noPeerHop},
		"no peer binder":      {access: access, gate: pass, door: &handler.ModelDoor{}, ingress: &fedhop.Ingress{}, maxDepth: depth, requirePeerHop: noPeerHop},
		"no require_peer_hop": {access: access, gate: pass, door: &handler.ModelDoor{}, ingress: &fedhop.Ingress{}, maxDepth: depth, binder: acceptPeerBinder{}},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: mountModelGroup did not panic", name)
				}
			}()
			mountModelGroup(chi.NewRouter(), "/v1", deps)
		}()
	}
}

// TestModelGroupLoopGuardOrder is G2 (F-W8, masterplan K14): the federation
// guard sits in the model group between the door and the inner stages —
// EnabledGate → ModelAuth → LoopGuard → body cap. Mounted over
// mountModelGroup with the real door, a real ctxm_ key and the router's
// identity:
//   - flag off + own token ⇒ 404 (no 508 oracle on a switched-off API);
//   - anonymous + own token ⇒ 401 (auth before federation);
//   - valid key + own token ⇒ 508 federation_loop, on /v1 and /v2;
//   - valid key + garbage hop header + a body over the cap ⇒ 400
//     federation_header_invalid (guard before the cap), without the garbage
//     ⇒ 413.
func TestModelGroupLoopGuardOrder(t *testing.T) {
	seed, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	admin, _, err := store.CreateApiKey(ctx, seed, "group-loopguard", "private", nil, "")
	if err != nil {
		t.Fatalf("admin key: %v", err)
	}
	_, _, model, err := store.CreateConsumerWithKey(ctx, seed, store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "loopguard"}, nil, store.APIStamp(admin.ID, "loopguard"))
	if err != nil {
		t.Fatalf("model key: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	var enabled atomic.Bool
	base := config.Defaults().GatewaySettings()
	get := func() oai.Settings { s := base; s.Enabled = enabled.Load(); return s }
	door := handler.ModelAuth(handler.ModelAuthConfig{
		Pool: pool, Settings: get, Usage: groupUsage{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Reject: func(w http.ResponseWriter, _ *http.Request, rej handler.ModelAuthRejection) {
			w.WriteHeader(rej.Status)
			_, _ = io.WriteString(w, `{"error":{"code":"`+rej.Code+`"}}`)
		},
	})
	const bodyCap = 1024
	ident := fedhop.NewIdentity("00000000-0000-4000-8000-0000000000c3", bytes.Repeat([]byte{0xc3}, 32))
	ingress := &fedhop.Ingress{}
	r := chi.NewRouter()
	r.Use(fedhop.IdentityMiddleware(ident))
	probe := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	for prefix, pattern := range map[string]string{"/v1": "/chat/completions", "/v2": "/rerank"} {
		mountModelGroup(r, prefix, modelGroupDeps{
			access: testAccessLog(), gate: oai.EnabledGate(get), door: door, ingress: ingress, maxDepth: func() int { return 3 }, binder: acceptPeerBinder{}, requirePeerHop: noPeerHop,
			inner:  []func(http.Handler) http.Handler{handler.MaxBodySizeStrictWith(func() int64 { return bodyCap }, oai.RefuseRequestTooLarge)},
			routes: []modelRoute{{http.MethodPost, pattern, probe}},
		})
	}
	if !ingress.Registered() {
		t.Fatal("mountModelGroup did not register the router's ingress")
	}

	type req struct {
		path, key, hop string
		bodyLen        int
	}
	do := func(q req) (int, string) {
		hr := httptest.NewRequest(http.MethodPost, q.path, strings.NewReader(strings.Repeat("x", q.bodyLen)))
		hr.TLS = &tls.ConnectionState{} // the door accepts a model key over TLS only
		if q.key != "" {
			hr.Header.Set("Authorization", "Bearer "+q.key)
		}
		if q.hop != "" {
			hr.Header.Set(fedhop.HeaderHop, q.hop)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, hr)
		var e struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		return rec.Code, e.Error.Code
	}
	key := model.Plaintext()
	own := ident.MintToken()

	enabled.Store(false)
	if st, code := do(req{"/v1/chat/completions", key, own, 2}); st != http.StatusNotFound {
		t.Errorf("flag off + own token = %d %q, want 404 (no 508 oracle while the API is off)", st, code)
	}
	enabled.Store(true)
	if st, code := do(req{"/v1/chat/completions", "", own, 2}); st != http.StatusUnauthorized {
		t.Errorf("anonymous + own token = %d %q, want 401 (auth before federation)", st, code)
	}
	for _, path := range []string{"/v1/chat/completions", "/v2/rerank"} {
		if st, code := do(req{path, key, own, 2}); st != http.StatusLoopDetected || code != oai.CodeFederationLoop {
			t.Errorf("%s: valid key + own token = %d %q, want 508 %s", path, st, code, oai.CodeFederationLoop)
		}
	}
	if st, code := do(req{"/v1/chat/completions", key, "ZZ", bodyCap + 1}); st != http.StatusBadRequest || code != oai.CodeFederationHeaderInvalid {
		t.Errorf("valid key + garbage hop + oversize body = %d %q, want 400 %s (guard before the body cap)", st, code, oai.CodeFederationHeaderInvalid)
	}
	if st, code := do(req{"/v1/chat/completions", key, "", bodyCap + 1}); st != http.StatusRequestEntityTooLarge {
		t.Errorf("valid key + oversize body = %d %q, want 413", st, code)
	}
	if st, code := do(req{"/v1/chat/completions", key, "", 2}); st != http.StatusOK {
		t.Errorf("valid key, no chain = %d %q, want 200", st, code)
	}
}
