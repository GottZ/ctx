//go:build integration

// Package v1door is the door gate of the model groups /v1 and /v2 (design/01
// §4.2 "Tür-Gate v1door", B3, B4; masterplan K23): a behavioural census over
// every route a mount registers under the model prefixes — found with
// chi.Walk, so a route mounted outside the group is found too — plus an
// unregistered path, a wrong method and OPTIONS per prefix. It proves:
//
//  1. gateway.enabled off ⇒ 404 on every path, whatever the credential;
//     on ⇒ an anonymous request is a 401;
//  2. a knowledge credential (64-hex key as Bearer and as X-Context-Key, an
//     OAuth ctxt_ token, a session cookie) ⇒ 401 and not one knowledge-auth
//     statement (ctx_auth, ctx_auth_by_id, context_api_keys);
//  3. a live model key ⇒ the probe handler runs with a ConsumerAuth and
//     without an AuthResult; the unregistered path, the wrong method and
//     OPTIONS reach no handler.
//
// The mount under test puts the probe handler behind every route it
// registers (its handlers come from a struct, K-02-2). The package is a test
// helper: integration-tagged like internal/testdb, never linked into ctxd.
// Wave W01-6 delivers it; the mount PR (W02-05) runs it.
package v1door

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/handler"
)

// Options configure one census.
type Options struct {
	// Prefixes the mount claims; each must carry at least one route (a census
	// over nothing proves nothing). Routes under /v1 and /v2 are checked
	// whether claimed or not. Default {"/v1", "/v2"}.
	Prefixes []string
	// SetEnabled switches gateway.enabled for the gate in front of the door.
	SetEnabled func(on bool)
	// KnowledgeKey is a live 64-hex knowledge key of the database behind the
	// mount; KnowledgeAuth counts the knowledge-auth statements on its pool.
	KnowledgeKey  string
	KnowledgeAuth *KnowledgeAuthTracer
	// ModelKey is a live model key the door accepts over TLS from RemoteAddr.
	ModelKey string
	// RemoteAddr of every request; default "192.0.2.1:40000".
	RemoteAddr string
}

// KnowledgeAuthTracer counts knowledge-auth statements — ctx_auth,
// ctx_auth_by_id and anything naming context_api_keys (the W01-4 filter):
// install it as the pool's tracer. The needle is the bare function name
// "ctx_auth", which both functions contain and ctx_model_auth does not; it
// carries no call parenthesis, because a string literal with "ctx_auth("
// outside internal/auth is what the W01-4 census reads as a second way to
// the knowledge-auth SQL function.
type KnowledgeAuthTracer struct{ n atomic.Int64 }

// TraceQueryStart implements pgx.QueryTracer.
func (k *KnowledgeAuthTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "ctx_auth") || strings.Contains(d.SQL, "context_api_keys") {
		k.n.Add(1)
	}
	return ctx
}

// TraceQueryEnd implements pgx.QueryTracer.
func (k *KnowledgeAuthTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Count is the number of knowledge-auth statements so far.
func (k *KnowledgeAuthTracer) Count() int64 { return k.n.Load() }

// probe is the handler behind every route of the mount under test.
type probe struct {
	mu   sync.Mutex
	hits []hit
}

type hit struct{ consumer, knowledge bool }

func (p *probe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ca := handler.ConsumerAuthFromContext(r.Context())
	p.mu.Lock()
	p.hits = append(p.hits, hit{consumer: ca != nil && ca.IsValid, knowledge: handler.AuthResultFromContext(r.Context()) != nil})
	p.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (p *probe) take() []hit {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.hits
	p.hits = nil
	return h
}

// target is one request target of the census.
type target struct {
	method, path, why string
	route             bool // a registered route: the probe must run for a live model key
}

var paramSeg = regexp.MustCompile(`\{[^}]*\}`)

// concrete turns a chi pattern into a path that matches it.
func concrete(pattern string) string {
	p := paramSeg.ReplaceAllString(pattern, "probe")
	if strings.HasSuffix(p, "*") {
		p = strings.TrimSuffix(p, "*") + "probe"
	}
	return p
}

func underModelPrefix(route string) bool {
	for _, pfx := range []string{"/v1", "/v2"} {
		if route == pfx || strings.HasPrefix(route, pfx+"/") {
			return true
		}
	}
	return false
}

// Assert runs the census. Failures are reported through t; every target
// is checked, a broken invariant does not stop the others.
func Assert(t testing.TB, mount func(r chi.Router, probe http.Handler), opts Options) {
	t.Helper()
	if opts.SetEnabled == nil || opts.KnowledgeKey == "" || opts.KnowledgeAuth == nil || opts.ModelKey == "" {
		t.Fatalf("v1door: Options need SetEnabled, KnowledgeKey, KnowledgeAuth and ModelKey")
		return
	}
	if len(opts.Prefixes) == 0 {
		opts.Prefixes = []string{"/v1", "/v2"}
	}
	if opts.RemoteAddr == "" {
		opts.RemoteAddr = "192.0.2.1:40000"
	}
	c := &census{t: t, opts: opts, probe: &probe{}, router: chi.NewRouter()}
	mount(c.router, c.probe)
	targets, routes, ok := c.targets()
	if !ok {
		return
	}
	c.flag(targets)
	c.knowledge(targets)
	c.modelKey(targets)
	t.Logf("v1door: %d targets over %d route(s) under %v", len(targets), routes, opts.Prefixes)
}

// census is one run of Assert.
type census struct {
	t      testing.TB
	opts   Options
	probe  *probe
	router chi.Router
}

// targets walks the router: every route under /v1 or /v2, plus per prefix
// an unregistered path and OPTIONS, per route OPTIONS and one wrong method.
func (c *census) targets() ([]target, int, bool) {
	methods := map[string][]string{}
	if err := chi.Walk(c.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if underModelPrefix(route) {
			methods[route] = append(methods[route], method)
		}
		return nil
	}); err != nil {
		c.t.Fatalf("v1door: chi.Walk: %v", err)
		return nil, 0, false
	}
	var out []target
	for _, pfx := range c.opts.Prefixes {
		n := 0
		for route := range methods {
			if route == pfx || strings.HasPrefix(route, pfx+"/") {
				n++
			}
		}
		if n == 0 {
			c.t.Fatalf("v1door: the mount registers no route under %s — a census over nothing proves nothing", pfx)
			return nil, 0, false
		}
		out = append(out,
			target{http.MethodGet, pfx + "/v1door-unregistered", "unregistered path", false},
			target{http.MethodOptions, pfx + "/v1door-unregistered", "OPTIONS", false})
	}
	routes := make([]string, 0, len(methods))
	for route := range methods {
		routes = append(routes, route)
	}
	slices.Sort(routes)
	for _, route := range routes {
		out = append(out, routeTargets(route, methods[route])...)
	}
	return out, len(routes), true
}

// routeTargets are the registered methods of route, OPTIONS (unless
// registered) and the first unregistered method of a fixed list.
func routeTargets(route string, registered []string) []target {
	path := concrete(route)
	var out []target
	for _, m := range registered {
		out = append(out, target{m, path, "route " + m + " " + route, true})
	}
	if !slices.Contains(registered, http.MethodOptions) {
		out = append(out, target{http.MethodOptions, path, "OPTIONS " + route, false})
	}
	for _, wrong := range []string{http.MethodPatch, http.MethodDelete, http.MethodPut, http.MethodPost, http.MethodGet} {
		if !slices.Contains(registered, wrong) {
			return append(out, target{wrong, path, "wrong method " + wrong + " " + route, false})
		}
	}
	return out
}

func (c *census) do(tg target, set func(*http.Request)) (int, []hit) {
	req := httptest.NewRequest(tg.method, tg.path, nil)
	req.RemoteAddr = c.opts.RemoteAddr
	req.TLS = &tls.ConnectionState{}
	if set != nil {
		set(req)
	}
	rec := httptest.NewRecorder()
	c.router.ServeHTTP(rec, req)
	return rec.Code, c.probe.take()
}

func bearer(v string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+v) }
}

type credential struct {
	name string
	set  func(*http.Request)
}

// knowledgeCredentials are the knowledge-side carriers of check (2).
func (c *census) knowledgeCredentials() []credential {
	return []credential{
		{"knowledge key as Bearer", bearer(c.opts.KnowledgeKey)},
		{"knowledge key as X-Context-Key", func(r *http.Request) { r.Header.Set("X-Context-Key", c.opts.KnowledgeKey) }},
		{"OAuth token as Bearer", bearer("ctxt_" + strings.Repeat("A", 43))},
		{"session cookie", func(r *http.Request) { r.Header.Set("Cookie", "ctx_session="+c.opts.KnowledgeKey) }},
	}
}

// flag is check (1): off ⇒ 404 everywhere whatever the credential; on ⇒
// anonymous is a 401.
func (c *census) flag(targets []target) {
	all := append([]credential{{"anonymous", nil}, {"model key", bearer(c.opts.ModelKey)}}, c.knowledgeCredentials()...)
	c.opts.SetEnabled(false)
	for _, tg := range targets {
		for _, cr := range all {
			if code, hits := c.do(tg, cr.set); code != http.StatusNotFound || len(hits) != 0 {
				c.t.Errorf("v1door (1) flag off, %s, %s: %d (handler ran %d×), want 404 and no handler", tg.why, cr.name, code, len(hits))
			}
		}
	}
	c.opts.SetEnabled(true)
	for _, tg := range targets {
		if code, hits := c.do(tg, nil); code != http.StatusUnauthorized || len(hits) != 0 {
			c.t.Errorf("v1door (1) flag on, %s, anonymous: %d (handler ran %d×), want 401", tg.why, code, len(hits))
		}
	}
}

// knowledge is check (2): every knowledge credential ⇒ 401 without one
// knowledge-auth statement.
func (c *census) knowledge(targets []target) {
	c.opts.SetEnabled(true)
	for _, tg := range targets {
		for _, cr := range c.knowledgeCredentials() {
			before := c.opts.KnowledgeAuth.Count()
			code, hits := c.do(tg, cr.set)
			if n := c.opts.KnowledgeAuth.Count() - before; code != http.StatusUnauthorized || len(hits) != 0 || n != 0 {
				c.t.Errorf("v1door (2) %s, %s: %d (handler ran %d×, %d knowledge-auth statement(s)), want 401 and 0", tg.why, cr.name, code, len(hits), n)
			}
		}
	}
}

// modelKey is check (3): the probe runs on every route with a consumer and
// without a knowledge AuthResult; the extra targets reach no handler.
func (c *census) modelKey(targets []target) {
	c.opts.SetEnabled(true)
	for _, tg := range targets {
		code, hits := c.do(tg, bearer(c.opts.ModelKey))
		switch {
		case tg.route && (code != http.StatusOK || len(hits) != 1):
			c.t.Errorf("v1door (3) %s, model key: %d (handler ran %d×), want the probe handler once", tg.why, code, len(hits))
		case tg.route && (!hits[0].consumer || hits[0].knowledge):
			c.t.Errorf("v1door (3) %s, model key: ConsumerAuth=%v AuthResult=%v, want a consumer and no AuthResult", tg.why, hits[0].consumer, hits[0].knowledge)
		case !tg.route && (len(hits) != 0 || (code != http.StatusNotFound && code != http.StatusMethodNotAllowed)):
			c.t.Errorf("v1door (3) %s, model key: %d (handler ran %d×), want 404/405 and no handler", tg.why, code, len(hits))
		}
	}
}
