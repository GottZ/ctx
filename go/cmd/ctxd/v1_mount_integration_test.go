//go:build integration

// W02-05: the /v1 mount over the PRODUCTION router (buildRouter, the
// constructor newRouter and main use) against a throwaway database — the
// stack access log → oai.Recover → EnabledGate → ModelAuth → LoopGuard →
// body cap → in-flight budget → GET /v1/models, GET /v1/models/* (design/02
// §4.2, masterplan K14), the door census, the shutdown flush of the usage
// stamps and the boot assert.
//
//	go test -tags=integration -p 1 -count=1 -run 'V1|ModelGroup|DarkLaunch|ModelKeyUsage' -v ./cmd/ctxd/
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
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/handler/v1door"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/modelkey"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// v1Tracer counts the statements on the router's pool: all of them, the
// knowledge-auth ones (v1door's filter), the door's ctx_model_auth and the
// grant reads.
type v1Tracer struct {
	knowledge                v1door.KnowledgeAuthTracer
	all, modelAuth, grantSQL atomic.Int64
}

func (v *v1Tracer) TraceQueryStart(ctx context.Context, c *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	v.knowledge.TraceQueryStart(ctx, c, d)
	v.all.Add(1)
	if strings.Contains(d.SQL, "ctx_model_auth") {
		v.modelAuth.Add(1)
	}
	if strings.Contains(d.SQL, "context_model_consumer_grants") {
		v.grantSQL.Add(1)
	}
	return ctx
}

func (v *v1Tracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

const v1AdminKey = "02050000000000000000000000000000feedfacefeedfacefeedfacefeedface"

// v1Fixture is the production router over a throwaway database: three
// _global aliases ("granted", "other", "qwen/x"), an app consumer holding a
// grant on "granted" only, one with all_models, the bootstrap admin key.
type v1Fixture struct {
	router   *chi.Mux
	ingress  *fedhop.Ingress
	gateway  *modelGateway
	pool     *pgxpool.Pool
	seed     *pgxpool.Pool
	tracer   *v1Tracer
	cfgStore *config.Store
	aliases  *modelalias.Registry
	disp     *dispatch.Dispatcher // the router's dispatcher (A03-W8 census: no /v1 model route acquires)

	adminID          string
	modelKey, allKey string
	modelKeyID       string
	grantedID        string
}

func insertAlias(t *testing.T, pool *pgxpool.Pool, scope, name, kind, policy string, enabled bool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_model_aliases (scope, name, kind, role, sensitivity, policy, enabled)
		 VALUES ($1, $2, $3, $3, 'public', $4::jsonb, $5) RETURNING id::text`,
		scope, name, kind, policy, enabled).Scan(&id); err != nil {
		t.Fatalf("insert alias %s/%s: %v", scope, name, err)
	}
	return id
}

// newV1Fixture builds the router; wrap (nil in production) replaces the
// group's route handlers — the door census puts its probe behind them.
func newV1Fixture(t *testing.T, env map[string]string, wrap func([]modelRoute) []modelRoute) *v1Fixture {
	t.Helper()
	seed, dsn := testdb.SetupTestDBWithDSN(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if env == nil {
		env = map[string]string{}
	}
	applyEnv(t, env)
	cc, issues := loadFromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	cfgStore := config.NewStore(cc)

	f := &v1Fixture{seed: seed, cfgStore: cfgStore, tracer: &v1Tracer{}}
	f.grantedID = insertAlias(t, seed, "_global", "granted", "chat", `{"context_length":4096}`, true)
	insertAlias(t, seed, "_global", "other", "chat", `{}`, true)
	insertAlias(t, seed, "_global", "qwen/x", "chat", `{}`, true)
	created, adminID, err := store.BootstrapAdminKey(ctx, seed, v1AdminKey, "w02-05")
	if err != nil || !created {
		t.Fatalf("bootstrap admin key: created=%v err=%v", created, err)
	}
	f.adminID = adminID
	_, mk, model, err := store.CreateConsumerWithKey(ctx, seed,
		store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "w02-05-app", Models: []string{f.grantedID}}, nil, store.APIStamp(adminID, "w02-05"))
	if err != nil {
		t.Fatalf("model key: %v", err)
	}
	f.modelKey, f.modelKeyID = model.Plaintext(), mk.ID
	_, _, all, err := store.CreateConsumerWithKey(ctx, seed,
		store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "w02-05-all", AllModels: true}, nil, store.APIStamp(adminID, "w02-05"))
	if err != nil {
		t.Fatalf("all-models key: %v", err)
	}
	f.allKey = all.Plaintext()

	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	pcfg.ConnConfig.Tracer = f.tracer
	f.pool, err = pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(f.pool.Close)

	backendPool := backends.NewPool(f.pool, nil)
	scheduler := events.NewScheduler(f.pool, cfgStore, backendPool, events.StartupConfig{})
	f.aliases = bootModelAliases(ctx, f.pool, modelAliasCeilings(cfgStore))
	scheduler.SetModelAliasRegistry(f.aliases)
	projectHub := events.NewProjectHub(ctx, f.pool, cfgStore)
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	f.disp = d
	scheduler.SetDispatcher(d)
	ident := fedhop.NewIdentity("00000000-0000-4000-8000-000000000205", bytes.Repeat([]byte{0x25}, 32))
	f.router, f.ingress, f.gateway = buildRouter(fedhop.WithIdentity(ctx, ident), f.pool, cfgStore, scheduler, backendPool, blocktype.NewRegistry(), projectHub, d, wrap)
	return f
}

// setEnabled switches gateway.enabled on the live config store — the hot
// read the gate and the door make per request.
func (f *v1Fixture) setEnabled(t *testing.T, on bool) {
	t.Helper()
	next := *f.cfgStore.Snapshot()
	next.Gateway.Enabled = on
	if err := f.cfgStore.Replace(&next); err != nil {
		t.Fatalf("set gateway.enabled=%v: %v", on, err)
	}
}

type v1Answer struct {
	status int
	header http.Header
	body   []byte
}

// code is the envelope's error.code ("" for a non-envelope body).
func (a v1Answer) code() string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(a.body, &e)
	return e.Error.Code
}

// do sends one request over TLS from a fixed client address (the door takes
// a model key over TLS only) and returns the answer without X-Request-ID.
func (f *v1Fixture) do(method, path string, set func(*http.Request)) v1Answer {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "192.0.2.1:40000"
	req.TLS = &tls.ConnectionState{}
	if set != nil {
		set(req)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	h := rec.Header().Clone()
	h.Del("X-Request-ID")
	return v1Answer{status: rec.Code, header: h, body: rec.Body.Bytes()}
}

func withBearer(v string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+v) }
}

func sameAnswer(a, b v1Answer) bool {
	if a.status != b.status || !bytes.Equal(a.body, b.body) || len(a.header) != len(b.header) {
		return false
	}
	for k, v := range a.header {
		if !slices.Equal(v, b.header[k]) {
			return false
		}
	}
	return true
}

// TestV1EnabledGateNoDBHit is G7 (K4, design/02 §4.2 stage 1): with
// gateway.enabled=false every request under /v1 — anonymous, a knowledge
// key, a valid model key, a malformed one; the routes, an unknown path, a
// wrong method, OPTIONS — gets the byte-identical 404 and costs the pool
// not one statement.
func TestV1EnabledGateNoDBHit(t *testing.T) {
	f := newV1Fixture(t, nil, nil)
	f.setEnabled(t, false)
	creds := map[string]func(*http.Request){
		"anonymous":           nil,
		"knowledge key":       withBearer(v1AdminKey),
		"valid model key":     withBearer(f.modelKey),
		"malformed model key": withBearer(modelkey.Prefix + "not-a-key"),
	}
	targets := [][2]string{
		{http.MethodGet, "/v1/models"}, {http.MethodGet, "/v1/models/x"}, {http.MethodGet, "/v1/unbekannt"},
		{http.MethodDelete, "/v1/models"}, {http.MethodOptions, "/v1/models"},
	}
	var ref *v1Answer
	before := f.tracer.all.Load()
	for _, tg := range targets {
		for name, set := range creds {
			a := f.do(tg[0], tg[1], set)
			if a.status != http.StatusNotFound || a.code() != oai.CodeNotFound {
				t.Errorf("%s %s, %s: %d %s, want 404 not_found", tg[0], tg[1], name, a.status, a.body)
			}
			if ref == nil {
				ref = &a
			} else if !sameAnswer(a, *ref) {
				t.Errorf("%s %s, %s: answer differs from the first 404:\n  %d %v %s\n  %d %v %s", tg[0], tg[1], name, a.status, a.header, a.body, ref.status, ref.header, ref.body)
			}
		}
	}
	n := f.tracer.all.Load() - before
	t.Logf("flag off: %d requests, %d statements on the router's pool (ctx_model_auth %d, knowledge auth %d)",
		len(targets)*len(creds), n, f.tracer.modelAuth.Load(), f.tracer.knowledge.Count())
	if n != 0 {
		t.Errorf("flag off: %d statements on the pool, want 0 (the gate runs before the door)", n)
	}
}

// TestV1RejectsKnowledgeCredentials is B15a (design/02 §5, G6): a 64-hex
// knowledge key as Bearer and as X-Context-Key, a live ctxt_ token with the
// /mcp audience and a live session cookie each get 401 invalid_api_key with
// WWW-Authenticate: Bearer realm="ctx" under /v1 — without one knowledge-auth
// statement and without reaching a handler dependency. The same credentials
// authenticate on /api (positive control); the counterproof mounts the
// knowledge Auth in place of the door and must see the opposite.
func TestV1RejectsKnowledgeCredentials(t *testing.T) {
	// With a canonical issuer the knowledge side enforces the token's /mcp
	// audience (RFC 8707), so the positive control proves an /mcp token.
	f := newV1Fixture(t, map[string]string{handler.EnvCanonicalIssuer: "https://ctx.example"}, nil)
	f.setEnabled(t, true)
	ctx := context.Background()
	var principal string
	if err := f.seed.QueryRow(ctx, `INSERT INTO context_principals (display_name) VALUES ('w02-05-person') RETURNING id::text`).Scan(&principal); err != nil {
		t.Fatalf("principal: %v", err)
	}
	if _, err := f.seed.Exec(ctx, `UPDATE context_api_keys SET principal_id = $1::uuid WHERE id = $2::uuid`, principal, f.adminID); err != nil {
		t.Fatalf("bind principal: %v", err)
	}
	pair, err := store.MintTokenPair(ctx, f.seed, store.OAuthToken{APIKeyID: f.adminID, PrincipalID: principal,
		Audiences: []string{"https://ctx.example/mcp", "https://ctx.example/web"}, IssuedVia: "login"}, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	var tokenRow, family, session string
	if err := f.seed.QueryRow(ctx, `SELECT id::text, refresh_family::text FROM context_access_tokens WHERE token_hash = $1`,
		store.TokenHash(pair.AccessToken)).Scan(&tokenRow, &family); err != nil {
		t.Fatalf("token row: %v", err)
	}
	if err := f.seed.QueryRow(ctx, `INSERT INTO context_web_sessions (principal_id, access_token_id, refresh_family, csrf_secret)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, 'w02-05-csrf') RETURNING id::text`, principal, tokenRow, family).Scan(&session); err != nil {
		t.Fatalf("session: %v", err)
	}
	creds := map[string]func(*http.Request){
		"64-hex key as Bearer":        withBearer(v1AdminKey),
		"64-hex key as X-Context-Key": func(r *http.Request) { r.Header.Set("X-Context-Key", v1AdminKey) },
		"ctxt_ token (/mcp audience)": withBearer(pair.AccessToken),
		"session cookie":              func(r *http.Request) { r.Header.Set("Cookie", "ctx_session="+session) },
	}
	for name, set := range creds {
		if a := f.do(http.MethodGet, "/api/whoami", set); a.status != http.StatusOK {
			t.Fatalf("positive control %s on /api/whoami: %d %s — the credential is not live", name, a.status, a.body)
		}
	}

	knowledge, grants := f.tracer.knowledge.Count(), f.tracer.grantSQL.Load()
	for name, set := range creds {
		for _, path := range []string{"/v1/models", "/v1/models/granted", "/v1/unbekannt"} {
			a := f.do(http.MethodGet, path, set)
			if a.status != http.StatusUnauthorized || a.code() != oai.CodeInvalidAPIKey || a.header.Get("WWW-Authenticate") != `Bearer realm="ctx"` {
				t.Errorf("%s on %s: %d WWW-Authenticate=%q %s, want 401 invalid_api_key, Bearer realm=\"ctx\"", name, path, a.status, a.header.Get("WWW-Authenticate"), a.body)
			}
		}
	}
	if n := f.tracer.knowledge.Count() - knowledge; n != 0 {
		t.Errorf("%d knowledge-auth statements under /v1, want 0", n)
	}
	if n := f.tracer.grantSQL.Load() - grants; n != 0 {
		t.Errorf("%d grant reads under /v1 for knowledge credentials, want 0", n)
	}

	// The same stack with counting dependencies: not one call.
	var calls atomic.Int64
	count := oai.Deps{
		Consumer:      func(c context.Context) *auth.ConsumerAuth { calls.Add(1); return handler.ConsumerAuthFromContext(c) },
		KnowledgeAuth: func(context.Context) bool { calls.Add(1); return false },
		Aliases:       f.aliases,
		GrantIDs:      func(context.Context, string) ([]string, error) { calls.Add(1); return nil, nil },
		HasGrant:      func(context.Context, string, string) (bool, error) { calls.Add(1); return false, nil },
	}
	r := chi.NewRouter()
	mountModelGroup(r, "/v1", f.gateway.group(&fedhop.Ingress{}, func() int { return 3 }, v1Routes(count)))
	for name, set := range creds {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.TLS = &tls.ConnectionState{}
		set(req)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("production stack, %s: %d, want 401", name, rec.Code)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("knowledge credentials reached the handler dependencies %d times, want 0", calls.Load())
	}

	t.Run("counterproof: knowledge Auth in place of the door", func(t *testing.T) {
		before := f.tracer.knowledge.Count()
		cr := chi.NewRouter()
		cr.Route("/v1", func(g chi.Router) {
			g.Use(handler.Auth(f.pool))
			g.Get("/models", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		})
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		withBearer(v1AdminKey)(req)
		rec := httptest.NewRecorder()
		cr.ServeHTTP(rec, req)
		if rec.Code == http.StatusUnauthorized || f.tracer.knowledge.Count() == before {
			t.Errorf("counterproof: knowledge Auth answered %d with %d knowledge statements — the probe would not see a wrong door", rec.Code, f.tracer.knowledge.Count()-before)
		}
	})
}

// wrapProbe replaces every route handler of a model group with probe.
func wrapProbe(probe http.Handler) func([]modelRoute) []modelRoute {
	return func(rs []modelRoute) []modelRoute {
		out := slices.Clone(rs)
		for i := range out {
			out[i].handler = probe
		}
		return out
	}
}

// TestV1DoorCensus is G8 (design/01 §4.2 "Tür-Gate v1door", K23): the
// census over the production router with the probe behind the real /v1
// routes — flag off 404 everywhere, flag on anonymous 401, knowledge
// credentials 401 without a knowledge statement, a model key reaches every
// route with a ConsumerAuth and without an AuthResult, the unregistered
// path, the wrong method and OPTIONS reach no handler. Then, on the real
// handlers: an unregistered /v1 path with Accept: text/html gets the JSON
// 404 of the group, not the SPA's index.html (the root carries the SPA
// fallback).
func TestV1DoorCensus(t *testing.T) {
	// The fixture exists before the census mounts it (Assert needs its keys
	// and tracer); the probe the census hands over is reached through a
	// forwarder the router's routes were built with.
	var probe atomic.Pointer[http.Handler]
	forward := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { (*probe.Load()).ServeHTTP(w, r) })
	f := newV1Fixture(t, nil, wrapProbe(forward))
	v1door.Assert(t, func(r chi.Router, p http.Handler) {
		probe.Store(&p)
		r.Mount("/", f.router)
	}, v1door.Options{
		Prefixes: []string{"/v1"}, SetEnabled: func(on bool) { f.setEnabled(t, on) },
		KnowledgeKey: v1AdminKey, KnowledgeAuth: &f.tracer.knowledge, ModelKey: f.modelKey,
	})

	// On the real handlers: an HTML navigation under /v1 — flag off and on,
	// an unknown path, the bare prefix, a model name that is no alias — is
	// the group's JSON 404, never the SPA (index.html or its placeholder).
	real := newV1Fixture(t, nil, nil)
	html := func(r *http.Request) { r.Header.Set("Accept", "text/html,application/xhtml+xml") }
	for _, on := range []bool{false, true} {
		real.setEnabled(t, on)
		for _, path := range []string{"/v1/unbekannt", "/v1", "/v1/models/x/y/z"} {
			a := real.do(http.MethodGet, path, func(r *http.Request) { html(r); withBearer(real.modelKey)(r) })
			if a.status != http.StatusNotFound || !strings.HasPrefix(a.header.Get("Content-Type"), "application/json") || bytes.Contains(a.body, []byte("<html")) || bytes.Contains(a.body, []byte("<!doctype")) {
				t.Errorf("flag %v, GET %s with Accept text/html: %d %q %.80s, want the JSON 404 of the group", on, path, a.status, a.header.Get("Content-Type"), a.body)
			}
		}
	}
}

// TestV1PanicIsInternal is the panic half of G9 (masterplan K5/K29,
// R-02-14): a panic behind the door — here in a handler dependency of the
// real /v1 handlers, under the house chain of the production router
// (Recovery outside) — answers 503 internal_error in the envelope with
// x-should-retry: false, never the plain-text 500 of the house Recovery.
func TestV1PanicIsInternal(t *testing.T) {
	f := newV1Fixture(t, nil, nil)
	f.setEnabled(t, true)
	boom := oai.Deps{
		Consumer:      func(context.Context) *auth.ConsumerAuth { panic("w02-05 probe panic") },
		KnowledgeAuth: func(context.Context) bool { return false },
		Aliases:       f.aliases,
		GrantIDs:      func(context.Context, string) ([]string, error) { return nil, nil },
		HasGrant:      func(context.Context, string, string) (bool, error) { return false, nil },
	}
	r := chi.NewRouter()
	r.Use(handler.SecurityHeaders, handler.RequestID, handler.Logger, handler.Recovery)
	mountModelGroup(r, "/v1", f.gateway.group(&fedhop.Ingress{}, func() int { return 3 }, v1Routes(boom)))
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	for _, path := range []string{"/v1/models", "/v1/models/granted"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.TLS = &tls.ConnectionState{}
		withBearer(f.modelKey)(req)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		a := v1Answer{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
		if a.status != http.StatusServiceUnavailable || a.code() != oai.CodeInternalError || a.header.Get("x-should-retry") != "false" ||
			!strings.Contains(string(a.body), `"type":"service_unavailable_error"`) {
			t.Errorf("panic on %s: %d x-should-retry=%q %s, want 503 internal_error (service_unavailable_error), x-should-retry false", path, a.status, a.header.Get("x-should-retry"), a.body)
		}
	}
}

// TestV1MethodNotAllowedAllow is G11 (RFC 9110 §15.5.6): a wrong method on
// a /v1 route with a valid key ⇒ 405 method_not_allowed with the Allow the
// group computes from its registered methods (chi sets none for a custom
// handler).
func TestV1MethodNotAllowedAllow(t *testing.T) {
	f := newV1Fixture(t, nil, nil)
	f.setEnabled(t, true)
	for _, tg := range [][2]string{{http.MethodDelete, "/v1/models"}, {http.MethodPost, "/v1/models/granted"}, {http.MethodOptions, "/v1/models"}, {http.MethodPut, "/v1/models/qwen/x"}} {
		a := f.do(tg[0], tg[1], withBearer(f.modelKey))
		if a.status != http.StatusMethodNotAllowed || a.code() != oai.CodeMethodNotAllowed || a.header.Get("Allow") != http.MethodGet {
			t.Errorf("%s %s: %d Allow=%q %s, want 405 method_not_allowed, Allow: GET", tg[0], tg[1], a.status, a.header.Get("Allow"), a.body)
		}
	}
	if a := f.do(http.MethodDelete, "/v1/models", nil); a.status != http.StatusUnauthorized {
		t.Errorf("DELETE /v1/models anonymous: %d, want 401 (the 405 is behind the door)", a.status)
	}
}

// TestV1BodyCapAfterDoor is G12 (design/02 §4.2 stage 3): a declared body
// above gateway.max_request_bytes — anonymous ⇒ 401 (no cap oracle before
// the door), with a valid key ⇒ 413 request_too_large.
func TestV1BodyCapAfterDoor(t *testing.T) {
	f := newV1Fixture(t, nil, nil)
	f.setEnabled(t, true)
	over := f.cfgStore.Snapshot().GatewaySettings().MaxRequestBytes + 1 //nolint:forbidigo // test: the gateway keys are global-only
	big := func(r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader("{}"))
		r.ContentLength = over
		r.Header.Set("Content-Length", strconv.FormatInt(over, 10))
	}
	if a := f.do(http.MethodGet, "/v1/models", big); a.status != http.StatusUnauthorized || a.code() != oai.CodeInvalidAPIKey {
		t.Errorf("anonymous, body over the cap: %d %s, want 401 invalid_api_key", a.status, a.body)
	}
	a := f.do(http.MethodGet, "/v1/models", func(r *http.Request) { big(r); withBearer(f.modelKey)(r) })
	if a.status != http.StatusRequestEntityTooLarge || a.code() != oai.CodeRequestTooLarge {
		t.Errorf("valid key, body over the cap: %d %s, want 413 request_too_large", a.status, a.body)
	}
}

// TestV1ModelKeyUsageFlushedAtShutdown is G13 (W01-3 F-7): a request the
// door accepted stamps its key in memory (Touch); the shutdown sequence
// ends the gateway's loops after the HTTP drain, and their final flush
// writes last_used_at. The duration of that step is logged (shutdown
// budget, docker-compose stop_grace_period).
func TestV1ModelKeyUsageFlushedAtShutdown(t *testing.T) {
	f := newV1Fixture(t, nil, nil)
	f.setEnabled(t, true)
	stop := startModelGateway(f.gateway)
	if a := f.do(http.MethodGet, "/v1/models", withBearer(f.modelKey)); a.status != http.StatusOK {
		t.Fatalf("accepted request: %d %s", a.status, a.body)
	}
	lastUsed := func() *time.Time {
		var at *time.Time
		if err := f.seed.QueryRow(context.Background(), `SELECT last_used_at FROM context_model_keys WHERE id = $1::uuid`, f.modelKeyID).Scan(&at); err != nil {
			t.Fatalf("read last_used_at: %v", err)
		}
		return at
	}
	if at := lastUsed(); at != nil {
		t.Fatalf("last_used_at already %v before the shutdown — the flush interval is %s, nothing should have run", at, store.ModelKeyUsageFlushInterval)
	}
	var stopTook time.Duration
	timed := func() {
		b := time.Now()
		stop()
		stopTook = time.Since(b)
	}
	begin := time.Now()
	shutdownSequence(&http.Server{}, noDrain{}, f.pool, timed)
	t.Logf("shutdown sequence %s, of it the gateway stop (final usage flush + access-log flush) %s — budget %s",
		time.Since(begin).Round(time.Millisecond), stopTook.Round(time.Microsecond), gatewayStopTimeout)
	if at := lastUsed(); at == nil {
		t.Error("last_used_at still NULL after the shutdown sequence — the final usage flush did not run")
	}
}

type noDrain struct{}

func (noDrain) Wait() {}

// TestV1BootAssertPassesWithMountedGroup: with gateway.enabled=true the boot
// assert of F-W8 lets the production router boot — only because the /v1
// group is mounted and registered the router's federation ingress.
func TestV1BootAssertPassesWithMountedGroup(t *testing.T) {
	f := newV1Fixture(t, map[string]string{"CTX_GATEWAY_ENABLED": "true"}, nil)
	if !f.ingress.Registered() {
		t.Fatal("the production router did not register its federation ingress")
	}
	if err := gatewayIngressAssert(f.cfgStore.Snapshot(), f.ingress); err != nil { //nolint:forbidigo // test: gateway.enabled is global-only
		t.Fatalf("boot assert with the /v1 group mounted: %v", err)
	}
}

// TestV1ModelsEndToEnd: the real handlers over the real grant reads and
// registry — the consumer with a grant on "granted" lists and retrieves
// exactly it, the all-models consumer lists all three; the slash id
// resolves (escaped as the SDKs send it); a granted name the consumer does
// not hold is the one 404. On the production router the request is one
// access-log line and no "http request" line (W06-9 gate (d)).
func TestV1ModelsEndToEnd(t *testing.T) {
	// The access log takes slog.Default at construction: the sink goes in
	// before the router is built.
	sink := &probeSink{}
	prev := slog.Default()
	slog.SetDefault(slog.New(sink))
	t.Cleanup(func() { slog.SetDefault(prev) })
	f := newV1Fixture(t, nil, nil)
	f.setEnabled(t, true)

	ids := func(key string) []string {
		a := f.do(http.MethodGet, "/v1/models", withBearer(key))
		var l struct {
			Object string `json:"object"`
			Data   []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if a.status != http.StatusOK || json.Unmarshal(a.body, &l) != nil || l.Object != "list" {
			t.Fatalf("GET /v1/models: %d %s", a.status, a.body)
		}
		var out []string
		for _, m := range l.Data {
			out = append(out, m.ID)
		}
		return out
	}
	sink.take()
	if got := strings.Join(ids(f.modelKey), ","); got != "granted" {
		t.Errorf("grant on granted: list %q, want granted", got)
	}
	recs := sink.take()
	if n := countMsg(recs, "http request"); n != 0 {
		t.Errorf("/v1 request on the production router: %d \"http request\" lines, want 0 (the group's access log owns it)", n)
	}
	if n := countMsg(recs, "gateway request"); n != 1 {
		t.Errorf("/v1 request on the production router: %d \"gateway request\" lines, want 1", n)
	}
	if got := strings.Join(ids(f.allKey), ","); got != "granted,other,qwen/x" {
		t.Errorf("all models: list %q, want granted,other,qwen/x", got)
	}
	for _, path := range []string{"/v1/models/qwen/x", "/v1/models/qwen%2Fx"} {
		if a := f.do(http.MethodGet, path, withBearer(f.allKey)); a.status != http.StatusOK || !strings.Contains(string(a.body), `"id":"qwen/x"`) {
			t.Errorf("all models, GET %s: %d %s", path, a.status, a.body)
		}
	}
	if a := f.do(http.MethodGet, "/v1/models/granted", withBearer(f.modelKey)); a.status != http.StatusOK {
		t.Errorf("GET granted with its grant: %d %s", a.status, a.body)
	}
	// The same database work for both 404s (Fremd-Review 1 L-1, review 2
	// L-a): one grant statement each — a name that is no alias probes the
	// grants too, so the time does not tell it from an alias not released.
	probes := func(path string) (v1Answer, int64) {
		before := f.tracer.grantSQL.Load()
		a := f.do(http.MethodGet, path, withBearer(f.modelKey))
		return a, f.tracer.grantSQL.Load() - before
	}
	notGranted, nG := probes("/v1/models/other")
	missing, nM := probes("/v1/models/nope")
	if nG != 1 || nM != 1 {
		t.Errorf("grant statements per 404: not granted %d, missing %d — want 1 each (no timing oracle beside the identical bytes)", nG, nM)
	}
	if notGranted.status != http.StatusNotFound || notGranted.code() != oai.CodeModelNotFound || !sameAnswer(notGranted, missing) {
		t.Errorf("not granted vs missing: %d %s / %d %s, want the same 404 model_not_found", notGranted.status, notGranted.body, missing.status, missing.body)
	}
}
