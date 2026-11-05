//go:build integration

// W01-4 gate (design/01 §4.3, masterplan I-K0): a model key (ctxm_) presented
// on any knowledge surface of the PRODUCTION router is refused before the
// database is asked. Surfaces: header auth (GET /api/whoami as Bearer and as
// X-Context-Key, POST /api/query, POST /mcp), POST /auth/login, POST /authorize
// (registered client, consent set, valid PKCE S256 — otherwise the request
// never reaches the key check) and POST /register in DCR mode admin. The count
// is filtered on knowledge-auth statements (ctx_auth, ctx_auth_by_id,
// context_api_keys) because /authorize loads its client before the key check.
// The positive control in the same matrix: a 64-hex knowledge key still takes
// exactly one ctx_auth query per surface and succeeds.
//
//	go test -tags=integration -p 1 -run TestModelKeyShapeNeverReachesKnowledgeAuth -count=1 -v ./cmd/ctxd/
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/modelkey"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// probeTagKey tags one probe request's context. The tracer counts only
// statements issued under a tag, so a background worker of the router that
// reads context_api_keys at the same moment cannot leak into a surface's count.
type probeTagKey struct{}

// knowledgeAuthTracer records, per probe tag, the knowledge-auth family of
// every statement: ctx_auth (raw key), ctx_auth_by_id (token, session) and any
// statement naming context_api_keys.
type knowledgeAuthTracer struct {
	mu   sync.Mutex
	hits map[string][]string
}

func (k *knowledgeAuthTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	tag, ok := ctx.Value(probeTagKey{}).(string)
	if !ok {
		return ctx
	}
	var family string
	switch {
	case strings.Contains(data.SQL, "ctx_auth_by_id("):
		family = "ctx_auth_by_id"
	case strings.Contains(data.SQL, "ctx_auth("):
		family = "ctx_auth"
	case strings.Contains(data.SQL, "context_api_keys"):
		family = "context_api_keys"
	default:
		return ctx
	}
	k.mu.Lock()
	k.hits[tag] = append(k.hits[tag], family)
	k.mu.Unlock()
	return ctx
}

func (k *knowledgeAuthTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (k *knowledgeAuthTracer) of(tag string) []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.hits[tag]...)
}

// knowledgeSurface is one knowledge entry point of the production router.
type knowledgeSurface struct {
	name    string
	request func(cred string) *http.Request
	refused func(rec *httptest.ResponseRecorder) bool // the surface's documented refusal
	ok      func(rec *httptest.ResponseRecorder) bool // positive control with a valid key
}

func bearer(req *http.Request, cred string) *http.Request {
	req.Header.Set("Authorization", "Bearer "+cred)
	return req
}

func status(code int) func(*httptest.ResponseRecorder) bool {
	return func(rec *httptest.ResponseRecorder) bool { return rec.Code == code }
}

func statusWith(code int, body string) func(*httptest.ResponseRecorder) bool {
	return func(rec *httptest.ResponseRecorder) bool {
		return rec.Code == code && strings.Contains(rec.Body.String(), body)
	}
}

func TestModelKeyShapeNeverReachesKnowledgeAuth(t *testing.T) {
	seed, dsn := testdb.SetupTestDBWithDSN(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	applyEnv(t, map[string]string{})
	t.Setenv(handler.EnvDCRMode, "admin") // read per request (oauth_register.go dcrMode)
	cc, issues := loadFromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	cfgStore := config.NewStore(cc)

	// Hex-only admin key: the positive control of every surface (DCR admin
	// needs server-admin).
	const hexKey = "a4112026110300000000000000000000cafebabecafebabecafebabecafebabe"
	if created, _, err := store.BootstrapAdminKey(ctx, seed, hexKey, "w01-4-knowledge-auth-probe"); err != nil || !created {
		t.Fatalf("bootstrap admin key: created=%v err=%v", created, err)
	}
	const callback = "https://probe.example/cb"
	client, _, err := store.RegisterOAuthClient(ctx, seed, store.RegisterOAuthClientSpec{
		Label: "w01-4-probe", RedirectURIs: []string{callback}, TokenEndpointAuthMethod: "none", Source: "admin",
	})
	if err != nil {
		t.Fatalf("register oauth client: %v", err)
	}
	verifier := strings.Repeat("v", 64)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	tracer := &knowledgeAuthTracer{hits: map[string][]string{}}
	pcfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(pool.Close)

	backendPool := backends.NewPool(pool, nil)
	blocktypeReg := blocktype.NewRegistry()
	scheduler := events.NewScheduler(pool, cfgStore, backendPool, events.StartupConfig{})
	projectHub := events.NewProjectHub(ctx, pool, cfgStore)
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	scheduler.SetDispatcher(d)
	router := NewRouter(ctx, pool, cfgStore, scheduler, backendPool, blocktypeReg, projectHub, d)

	jsonBody := func(v any) *strings.Reader {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return strings.NewReader(string(b))
	}
	surfaces := []knowledgeSurface{
		{
			name:    "GET /api/whoami (Bearer)",
			request: func(c string) *http.Request { return bearer(httptest.NewRequest(http.MethodGet, "/api/whoami", nil), c) },
			refused: status(http.StatusUnauthorized),
			ok:      status(http.StatusOK),
		},
		{
			name: "GET /api/whoami (X-Context-Key)",
			request: func(c string) *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
				req.Header.Set("X-Context-Key", c)
				return req
			},
			refused: status(http.StatusUnauthorized),
			ok:      status(http.StatusOK),
		},
		{
			name: "POST /api/query",
			request: func(c string) *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(`{}`))
				req.Header.Set("Content-Type", "application/json")
				return bearer(req, c)
			},
			refused: status(http.StatusUnauthorized),
			// Past auth, the empty query is the handler's own 400 — no LLM needed.
			ok: statusWith(http.StatusBadRequest, "query is required"),
		},
		{
			name: "POST /mcp",
			request: func(c string) *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(
					`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"w01-4","version":"0"}}}`))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				return bearer(req, c)
			},
			refused: status(http.StatusUnauthorized),
			ok:      status(http.StatusOK),
		},
		{
			name: "POST /auth/login",
			request: func(c string) *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/auth/login", jsonBody(map[string]string{"api_key": c}))
				req.Header.Set("Content-Type", "application/json")
				return req
			},
			refused: statusWith(http.StatusUnauthorized, "authentication failed"),
			ok:      statusWith(http.StatusOK, `"success":true`),
		},
		{
			name: "POST /authorize",
			request: func(c string) *http.Request {
				form := url.Values{
					"response_type": {"code"}, "client_id": {client.ClientID}, "redirect_uri": {callback},
					"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"st-w01-4"},
					// handler/oauth_consent.go consentFieldValue: the checkbox
					// is checked before the key, so it must be set.
					"consent": {"yes"},
					"api_key": {c},
				}
				req := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(form.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				return req
			},
			refused: statusWith(http.StatusUnauthorized, "Invalid API key."),
			ok: func(rec *httptest.ResponseRecorder) bool {
				loc, err := url.Parse(rec.Header().Get("Location"))
				return rec.Code == http.StatusFound && err == nil && loc.Query().Get("code") != "" &&
					strings.HasPrefix(loc.String(), callback)
			},
		},
		{
			name: "POST /register (DCR admin)",
			request: func(c string) *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(
					`{"redirect_uris":["https://claude.ai/api/mcp/auth_callback"],"client_name":"w01-4 probe"}`))
				req.Header.Set("Content-Type", "application/json")
				return bearer(req, c)
			},
			refused: statusWith(http.StatusUnauthorized, "unauthorized"),
			ok:      status(http.StatusCreated),
		},
	}

	mk, err := modelkey.Generate()
	if err != nil {
		t.Fatalf("modelkey.Generate: %v", err)
	}
	key := mk.Plaintext()
	shapes := []struct{ name, cred string }{
		{"canonical", key},
		{"leading-space", " " + key},
		{"byte-order-mark", "\ufeff" + key},
		{"zero-width-space", "\u200b" + key},
		{"prefix-glued-hex", modelkey.Prefix + hexKey},
	}

	addr := 0
	serve := func(tag string, req *http.Request) *httptest.ResponseRecorder {
		addr++ // a fresh client address per request: no per-IP limiter interferes
		req.RemoteAddr = fmt.Sprintf("198.51.100.%d:40000", addr)
		req = req.WithContext(context.WithValue(req.Context(), probeTagKey{}, tag))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	for _, s := range surfaces {
		for _, sh := range shapes {
			tag := s.name + " / " + sh.name
			rec := serve(tag, s.request(sh.cred))
			hits := tracer.of(tag)
			t.Logf("surface=%q shape=%s status=%d knowledge_auth=%d %v", s.name, sh.name, rec.Code, len(hits), hits)
			if !s.refused(rec) {
				t.Errorf("%s: status %d body %.200q, want the surface's refusal", tag, rec.Code, rec.Body.String())
			}
			if len(hits) != 0 {
				t.Errorf("%s: %d knowledge-auth statement(s) %v, want 0", tag, len(hits), hits)
			}
		}

		tag := s.name + " / hex-key"
		rec := serve(tag, s.request(hexKey))
		hits := tracer.of(tag)
		t.Logf("surface=%q shape=hex-key status=%d knowledge_auth=%d %v", s.name, rec.Code, len(hits), hits)
		if !s.ok(rec) {
			t.Errorf("%s: status %d body %.200q, want success", tag, rec.Code, rec.Body.String())
		}
		rawKeyChecks := 0
		for _, h := range hits {
			if h == "ctx_auth" {
				rawKeyChecks++
			}
		}
		if rawKeyChecks != 1 {
			t.Errorf("%s: knowledge-auth statements %v, want exactly one ctx_auth", tag, hits)
		}
	}
}
