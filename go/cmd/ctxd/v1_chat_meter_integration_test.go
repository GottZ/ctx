//go:build integration

package main

// W06-10 over the PRODUCTION router (buildRouter WITHOUT a route wrap): the
// chat route's limiter is the per-consumer meter gatewayDeps wires
// (design/06 §4.2, masterplan gate "Überschreitung ⇒ 429 zuerst rot"). The
// fixture is the one of the limiter-nil pin (W02-07 H-5): a throwaway
// database, an urgent-lane app consumer with all models, one fake engine with
// a hit counter, the real dispatcher and planner. The meter's limits come
// from the environment like in production (CTX_GATEWAY_CHAT_RPM_DEFAULT).
//
//	go test -tags=integration -p 1 -count=1 -run 'TestV1ChatMeter|TestV1ChatLimiterMissing|TestV1ChatLoop' -v ./cmd/ctxd/

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// v1MeterRig is the production router over a throwaway database. deps are
// the chat route's production dependencies as gatewayDeps builds them over
// the router's own meter (gw.meter) — for a test that rebuilds the route
// with one field changed (a route wrap reads them at request time).
type v1MeterRig struct {
	router *chi.Mux
	gw     *modelGateway
	deps   oai.Deps
	engine *v1ChatEngine
	seed   *pgxpool.Pool
	pool   *pgxpool.Pool
	disp   *dispatch.Dispatcher
	ident  fedhop.Identity
	key    string
}

// newV1MeterRig builds the router as production does (buildRouter) under
// env; wrap is the route seam (nil = production routes unchanged).
func newV1MeterRig(t *testing.T, env map[string]string, wrap func([]modelRoute) []modelRoute) *v1MeterRig {
	t.Helper()
	seed, dsn := testdb.SetupTestDBWithDSN(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	applyEnv(t, env)
	cc, issues := loadFromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("env: %v", issues)
	}
	cfgStore := config.NewStore(cc)
	insertAlias(t, seed, "_global", v1ChatAlias, "chat", `{}`, true)
	admin, _, err := store.CreateApiKey(ctx, seed, "w06-10-meter", "private", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	_, _, key, err := store.CreateConsumerWithKey(ctx, seed,
		store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "w06-10-meter", AllModels: true, AdmissionLane: handler.LaneUrgent},
		nil, store.APIStamp(admin.ID, "w06-10-meter"))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	dispatch.SetPrincipalHook(handler.RequestPrincipal)
	t.Cleanup(func() { dispatch.SetPrincipalHook(nil) })
	e := newV1ChatEngine(t, http.StatusOK)
	rows := []backends.Backend{e.row("gpu", 10)}
	bp := backends.NewPool(pool, nil)
	bp.SeedSnapshotForTest(rows)
	sched := events.NewScheduler(pool, cfgStore, bp, events.StartupConfig{})
	sched.SetModelAliasRegistry(bootModelAliases(ctx, pool, modelAliasCeilings(cfgStore)))
	hub := events.NewProjectHub(ctx, pool, cfgStore)
	disp := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(disp.Close)
	pol := dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{}}
	for _, b := range rows {
		pol.Targets[b.Host] = dispatch.TargetPolicy{Slots: 4, InternalReserved: 1}
	}
	disp.UpdatePolicy(pol)
	sched.SetDispatcher(disp)
	ident := fedhop.NewIdentity("00000000-0000-4000-8000-0000000f0610", bytes.Repeat([]byte{0x61}, 32))
	sctx := fedhop.WithIdentity(ctx, ident)
	router, _, gw := buildRouter(sctx, pool, cfgStore, sched, bp, blocktype.NewRegistry(), hub, disp, wrap)
	next := *cfgStore.Snapshot()
	next.Gateway.Enabled = true
	if err := cfgStore.Replace(&next); err != nil {
		t.Fatal(err)
	}
	// The quota accountant is a second instance built like buildRouter's.
	deps := gatewayDeps(sctx, pool, cfgStore, sched.ModelAliasRegistry(), bp, backends.NewQuotaAccountant(pool, 30*time.Second), disp, gw.meter)
	return &v1MeterRig{router: router, gw: gw, deps: deps, engine: e, seed: seed, pool: pool, disp: disp, ident: ident, key: key.Plaintext()}
}

// do sends one request with the rig's model key over TLS; hdr are extra
// headers (name, value, …).
func (r *v1MeterRig) do(method, path, body string, hdr ...string) (int, http.Header, []byte) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "192.0.2.1:40000"
	req.TLS = &tls.ConnectionState{}
	req.Header.Set("Authorization", "Bearer "+r.key)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	r.router.ServeHTTP(rec, req)
	return rec.Code, rec.Header(), rec.Body.Bytes()
}

// chatRows settles the asynchronous row writer and counts gateway-chat rows.
func (r *v1MeterRig) chatRows(t *testing.T) int {
	t.Helper()
	sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer scancel()
	if err := llmlog.Settle(sctx, r.pool); err != nil {
		t.Fatalf("llmlog settle: %v", err)
	}
	var n int
	if err := r.seed.QueryRow(context.Background(), `SELECT count(*) FROM context_llm_log WHERE pipeline = $1`, llmlog.PipelineGatewayChat).Scan(&n); err != nil {
		t.Fatalf("count gateway-chat rows: %v", err)
	}
	return n
}

// v1MeterAnswer is one logged answer.
type v1MeterAnswer struct {
	status int
	header http.Header
	body   []byte
}

func (a v1MeterAnswer) String() string {
	var h []string
	for _, k := range []string{"Retry-After", "Retry-After-Ms", "X-Should-Retry",
		"X-Ratelimit-Limit-Requests", "X-Ratelimit-Remaining-Requests", "X-Ratelimit-Reset-Requests",
		"X-Ratelimit-Limit-Tokens", "X-Ratelimit-Remaining-Tokens", "X-Ratelimit-Reset-Tokens"} {
		if v := a.header.Get(k); v != "" {
			h = append(h, k+": "+v)
		}
	}
	return strconv.Itoa(a.status) + " [" + strings.Join(h, "; ") + "] " + strings.TrimSpace(string(a.body))
}

// TestV1ChatMeterOnTheRealRouter is the masterplan gate "Überschreitung ⇒
// 429 zuerst rot" (design/06 §7 W06-10) and the turn-around of the W02-07
// limiter-nil pin (H-5): gateway.chat_rpm_default = 2 ⇒ requests 1 and 2 are
// served (x-ratelimit-remaining-requests 1, then 0), request 3 is 429
// rate_limit_exceeded with Retry-After in [1, 30], retry-after-ms and
// x-should-retry: true; the engine saw 2 requests, the log holds 2 rows.
func TestV1ChatMeterOnTheRealRouter(t *testing.T) {
	r := newV1MeterRig(t, map[string]string{"CTX_GATEWAY_CHAT_RPM_DEFAULT": "2"}, nil)
	body := `{"model":"` + v1ChatAlias + `","messages":[{"role":"user","content":"hi"}]}`
	var got []v1MeterAnswer
	for range 3 {
		st, h, b := r.do(http.MethodPost, "/v1/chat/completions", body)
		got = append(got, v1MeterAnswer{st, h.Clone(), b})
	}
	hits := r.engine.hits.Load()
	rows := r.chatRows(t)
	for i, a := range got {
		t.Logf("request %d: %s", i+1, a)
	}
	t.Logf("engine hits %d, gateway-chat rows %d", hits, rows)

	for i, want := range []string{"1", "0"} {
		a := got[i]
		if a.status != http.StatusOK {
			t.Errorf("request %d: status %d, want 200", i+1, a.status)
			continue
		}
		if v := a.header.Get("x-ratelimit-remaining-requests"); v != want {
			t.Errorf("request %d: x-ratelimit-remaining-requests %q, want %q", i+1, v, want)
		}
		if v := a.header.Get("x-ratelimit-limit-requests"); v != "2" {
			t.Errorf("request %d: x-ratelimit-limit-requests %q, want 2", i+1, v)
		}
		for _, k := range []string{"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens"} {
			if _, err := time.ParseDuration(a.header.Get(k)); err != nil {
				t.Errorf("request %d: %s %q is no Go duration: %v", i+1, k, a.header.Get(k), err)
			}
		}
		for _, k := range []string{"x-ratelimit-limit-tokens", "x-ratelimit-remaining-tokens"} {
			if _, err := strconv.Atoi(a.header.Get(k)); err != nil {
				t.Errorf("request %d: %s %q is no integer", i+1, k, a.header.Get(k))
			}
		}
	}
	third := got[2]
	var env struct {
		Error struct {
			Type    string          `json:"type"`
			Code    *string         `json:"code"`
			Param   json.RawMessage `json:"param"`
			Message string          `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(third.body, &env); err != nil || env.Error.Code == nil {
		t.Fatalf("request 3: %s — want the 429 rate_limit_exceeded envelope", third)
	}
	if third.status != http.StatusTooManyRequests || env.Error.Type != "rate_limit_error" || *env.Error.Code != "rate_limit_exceeded" ||
		string(env.Error.Param) != "null" || env.Error.Message == "" {
		t.Errorf("request 3: %s — want 429 {type rate_limit_error, code rate_limit_exceeded, param null, message}", third)
	}
	if ra, err := strconv.Atoi(third.header.Get("Retry-After")); err != nil || ra < 1 || ra > 30 {
		t.Errorf("request 3: Retry-After %q, want an integer in [1, 30]", third.header.Get("Retry-After"))
	}
	if ms, err := strconv.Atoi(third.header.Get("retry-after-ms")); err != nil || ms < 1 || ms > 30000 {
		t.Errorf("request 3: retry-after-ms %q, want an integer in [1, 30000]", third.header.Get("retry-after-ms"))
	}
	if v := third.header.Get("x-should-retry"); v != "true" {
		t.Errorf("request 3: x-should-retry %q, want true", v)
	}
	if hits != 2 {
		t.Errorf("engine hits %d, want 2 (the third request is refused before the planner)", hits)
	}
	if rows != 2 {
		t.Errorf("gateway-chat rows %d, want 2", rows)
	}
}

// TestV1ChatLoopNeverReachesTheMeter is B-06-17 (design/06 §4.1 step 3
// before step 8): a request that comes back to this instance (Ctx-Hop with
// its own token, A→B→A) is refused with 508 by the LoopGuard and leaves the
// meter untouched — with gateway.chat_rpm_default = 1 the next regular
// request is still served.
func TestV1ChatLoopNeverReachesTheMeter(t *testing.T) {
	r := newV1MeterRig(t, map[string]string{"CTX_GATEWAY_CHAT_RPM_DEFAULT": "1"}, nil)
	body := `{"model":"` + v1ChatAlias + `","messages":[{"role":"user","content":"hi"}]}`
	st, h, b := r.do(http.MethodPost, "/v1/chat/completions", body, fedhop.HeaderHop, r.ident.MintToken())
	loop := v1MeterAnswer{st, h.Clone(), b}
	st, h, b = r.do(http.MethodPost, "/v1/chat/completions", body)
	next := v1MeterAnswer{st, h.Clone(), b}
	t.Logf("loop request: %s", loop)
	t.Logf("next request: %s", next)
	if loop.status != http.StatusLoopDetected {
		t.Errorf("loop request: %s, want 508", loop)
	}
	if next.status != http.StatusOK || next.header.Get("x-ratelimit-remaining-requests") != "0" {
		t.Errorf("next request: %s, want 200 with x-ratelimit-remaining-requests 0 (the loop request booked nothing)", next)
	}
	if n := r.engine.hits.Load(); n != 1 {
		t.Errorf("engine hits %d, want 1", n)
	}
}
