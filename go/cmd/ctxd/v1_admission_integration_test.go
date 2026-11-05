//go:build integration

// A03-W8 over the model group itself: the herald measurement B11 (design/03
// §4.8a, design/02 B11 TestV1DoesNotRaiseHerald) on a fixture route in a
// mountModelGroup group with the real door and a real ctxm_ key, and the
// admission census over the routes W02-05 mounted on the production router
// (GET /v1/models, /v1/models/*: 0 acquires).
//
//	go test -tags=integration -p 1 -count=1 -run 'V1Admission|V1DoesNotRaiseHerald' -v ./cmd/ctxd/
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/handler/v1census"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	heraldGPU   = "http://herald-gpu.invalid:30000" // the /v1 fixture's target: S = 3, R = 1
	heraldOther = "http://herald-bg.invalid:8081"   // a second target for ctx's background
)

// heraldRig mounts one fixture route on the admission contract in a model
// group; wrap is identity in production and handler.WithScheduler in the red
// control.
type heraldRig struct {
	router  chi.Router
	d       *dispatch.Dispatcher
	key     string
	entered chan struct{}
	hold    chan struct{}
}

func newHeraldRig(t *testing.T, wrap func(http.HandlerFunc) http.HandlerFunc) *heraldRig {
	t.Helper()
	seed, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	admin, _, err := store.CreateApiKey(ctx, seed, "a03-w8-herald", "private", nil, "")
	if err != nil {
		t.Fatalf("admin key: %v", err)
	}
	_, _, model, err := store.CreateConsumerWithKey(ctx, seed, store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "herald", AdmissionLane: "urgent"},
		nil, store.APIStamp(admin.ID, "a03-w8"))
	if err != nil {
		t.Fatalf("model key: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	dispatch.SetPrincipalHook(handler.RequestPrincipal) // as cmd/ctxd/main.go
	t.Cleanup(func() { dispatch.SetPrincipalHook(nil) })
	d := dispatch.New(slog.New(slog.DiscardHandler), dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{
		heraldGPU:   {Slots: 3, InternalReserved: 1},
		heraldOther: {Slots: 1},
	}})

	base := config.Defaults().GatewaySettings()
	base.Enabled = true
	get := func() oai.Settings { return base }
	door := handler.ModelAuth(handler.ModelAuthConfig{
		Pool: pool, Settings: get, Usage: groupUsage{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Reject: func(w http.ResponseWriter, _ *http.Request, rej handler.ModelAuthRejection) {
			w.WriteHeader(rej.Status)
			_, _ = io.WriteString(w, `{"error":{"code":"`+rej.Code+`"}}`)
		},
	})
	adm := handler.NewV1Admission(handler.V1AdmissionConfig{Dispatch: d, Settings: get, Logger: slog.New(slog.DiscardHandler)})
	rig := &heraldRig{d: d, key: model.Plaintext(), entered: make(chan struct{}, 1), hold: make(chan struct{})}
	chain := []backends.Backend{{ID: "gpu", Name: "gpu", Host: heraldGPU, Protocol: backends.ProtocolOpenAI, Scope: backends.GlobalScope,
		Roles: []string{"chat"}, ModelMap: map[string]backends.ModelSpec{"chat": {Model: "m"}}}}
	fixture := func(w http.ResponseWriter, r *http.Request) {
		plan, err := adm.Plan(r.Context(), handler.ConsumerAuthFromContext(r.Context()), oai.AdmissionInput{Chain: chain})
		if err == nil {
			_, _, err = llm.WalkChain(r.Context(), plan.Chain, "chat", plan.Acquirer, nil, llm.WalkRules{AllowedClasses: adm.AllowedClasses()},
				func(ctx context.Context, _ *backends.Backend, _ *dispatch.Lease, _ time.Duration) (dispatch.Usage, bool, error) {
					rig.entered <- struct{}{}
					select { // the fake upstream holds
					case <-rig.hold:
					case <-ctx.Done():
					}
					return dispatch.Usage{PromptTokens: 1}, true, nil
				})
		}
		switch {
		case err == nil:
			w.WriteHeader(http.StatusOK)
		case llm.IsAdmissionError(err), errors.Is(err, llm.ErrWalkClass),
			errors.Is(err, handler.ErrLaneNotOffered), errors.Is(err, handler.ErrChainNotOffered):
			adm.WriteAdmissionError(r.Context(), w, err)
		default:
			oai.WriteCode(w, oai.CodeUpstreamUnavailable, "The upstream failed.", true)
		}
	}
	ident := fedhop.NewIdentity("00000000-0000-4000-8000-0000000000a8", bytes.Repeat([]byte{0xa8}, 32))
	r := chi.NewRouter()
	r.Use(fedhop.IdentityMiddleware(ident))
	mountModelGroup(r, "/v1", modelGroupDeps{access: testAccessLog(), gate: oai.EnabledGate(get), door: door, ingress: &fedhop.Ingress{},
		maxDepth: func() int { return 3 }, routes: []modelRoute{{http.MethodPost, "/chat/completions", wrap(fixture)}}})
	rig.router = r
	return rig
}

// probe sends one request and, while the fake upstream holds it, reads the
// herald and tries a background acquire on the other target.
func (h *heraldRig) probe(t *testing.T) (demand, status int, bgErr error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.RemoteAddr = "192.0.2.1:40000"
	req.TLS = &tls.ConnectionState{}
	req.Header.Set("Authorization", "Bearer "+h.key)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.router.ServeHTTP(rec, req); close(done) }()
	select {
	case <-h.entered:
	case <-done:
		t.Fatalf("handler returned before the upstream held: %d %s", rec.Code, rec.Body)
	case <-time.After(10 * time.Second):
		t.Fatal("the fixture never reached its upstream")
	}
	demand = h.d.InteractiveDemand()
	bgCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	l, _, bgErr := h.d.Acquire(bgCtx, dispatch.Request{Target: dispatch.Target{Origin: heraldOther}, Class: dispatch.ClassBackground, Role: "dream"})
	cancel()
	if l != nil {
		l.Release()
	}
	close(h.hold)
	<-done
	return demand, rec.Code, bgErr
}

// TestV1DoesNotRaiseHerald is gate (a), B11: WHILE a /v1 handler blocks
// (the fake upstream holds its lease) the herald stays down — demand 0 —
// and a background acquire on a second target is admitted at once (within
// 50 ms). The control mounts the same route in WithScheduler: demand 1, the
// background acquire blocks until its deadline — the probe can see the
// herald.
func TestV1DoesNotRaiseHerald(t *testing.T) {
	t.Run("model group", func(t *testing.T) {
		h := newHeraldRig(t, func(f http.HandlerFunc) http.HandlerFunc { return f })
		demand, status, bgErr := h.probe(t)
		if demand != 0 || bgErr != nil || status != http.StatusOK {
			t.Fatalf("during the /v1 handler: demand %d, background acquire %v, status %d — want 0, admitted, 200", demand, bgErr, status)
		}
	})
	t.Run("control: WithScheduler mount", func(t *testing.T) {
		var d atomic.Pointer[dispatch.Dispatcher]
		h := newHeraldRig(t, func(f http.HandlerFunc) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) { handler.WithScheduler(d.Load(), f)(w, r) }
		})
		d.Store(h.d)
		demand, _, bgErr := h.probe(t)
		if demand != 1 || !errors.Is(bgErr, context.DeadlineExceeded) {
			t.Fatalf("WithScheduler control: demand %d, background acquire %v — want 1 and blocked (the probe must see a raised herald)", demand, bgErr)
		}
	})
}

// modelRouteCensus runs the admission census over the /v1 group of a
// production-router fixture with a live model key: the recorded doors of
// rec and the targets of the router's own dispatcher, counted BEFORE the
// first request (PR #142 bot review 4477068967: sampled after the
// reachability GET, a target that GET created would hide every later
// acquire on it). The model routes hold no dispatcher, so rec sees only
// what a route hands it — the router's dispatcher is the evidence for the
// real routes, rec the evidence for an acquiring control route.
func modelRouteCensus(t *testing.T, f *v1Fixture, rec *v1census.Recorder) (violations []string, targetsBefore, targetsAfter int) {
	t.Helper()
	targetsBefore = len(f.disp.Snapshot().Targets)
	// The census requests reach the handlers (not a vacuous run over 401s).
	if a := f.do(http.MethodGet, "/v1/models", withBearer(f.allKey)); a.status != http.StatusOK {
		t.Fatalf("GET /v1/models with the census key = %d %s, want 200", a.status, a.body)
	}
	// The planner's own class source (RC-C-INDEX §4: no second class list;
	// A03-W9 adds XB there): the planner the router itself builds.
	allowed := gatewayDeps(context.Background(), f.pool, f.cfgStore, f.aliases, nil, nil, f.disp).Planner.AllowedClasses()
	violations = v1census.Check(f.router, rec, v1census.Options{
		Allowed: allowed,
		Prepare: func(r *http.Request) *http.Request {
			r.RemoteAddr = "192.0.2.1:40000"
			r.TLS = &tls.ConnectionState{}
			r.Header.Set("Authorization", "Bearer "+f.allKey)
			return r
		},
	})
	return violations, targetsBefore, len(f.disp.Snapshot().Targets)
}

// TestV1AdmissionCensusModelRoutes: the census over the /v1 routes W02-05
// mounted on the production router (buildRouter) with a live model key —
// GET /v1/models and /v1/models/* acquire nothing: no violation and no
// target created on the router's own dispatcher. The negative control
// replaces GET /v1/models by a route that acquires — through the recorder
// and on the router's dispatcher — and must fail both checks.
func TestV1AdmissionCensusModelRoutes(t *testing.T) {
	t.Run("real model routes", func(t *testing.T) {
		f := newV1Fixture(t, nil, nil)
		f.setEnabled(t, true)
		violations, before, after := modelRouteCensus(t, f, v1census.NewRecorder(f.disp))
		if len(violations) != 0 {
			t.Fatalf("census violations %q", violations)
		}
		if after != before {
			t.Fatalf("the router's dispatcher has %d targets after the census, %d before — a model route acquired", after, before)
		}
	})
	t.Run("control: an acquiring model route", func(t *testing.T) {
		var rec atomic.Pointer[v1census.Recorder]
		var disp atomic.Pointer[dispatch.Dispatcher]
		f := newV1Fixture(t, nil, func(routes []modelRoute) []modelRoute {
			out := slices.Clone(routes)
			for i := range out {
				if out[i].pattern != "/models" {
					continue
				}
				out[i].handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if l, _, err := disp.Load().Acquire(r.Context(), dispatch.Request{Target: dispatch.Target{Origin: "http://census-probe.invalid:8000"},
						Class: dispatch.ClassBackground, Role: "probe"}); err == nil {
						l.Release()
					}
					if l, _, err := rec.Load().Acquire(r.Context(), dispatch.Request{Target: dispatch.Target{Origin: "http://census-probe.invalid:8000"},
						Class: dispatch.ClassExternal, Role: "probe"}); err == nil {
						l.Release()
					}
					w.WriteHeader(http.StatusOK)
				})
			}
			return out
		})
		disp.Store(f.disp)
		rec.Store(v1census.NewRecorder(f.disp))
		f.setEnabled(t, true)
		violations, before, after := modelRouteCensus(t, f, rec.Load())
		if !slices.ContainsFunc(violations, func(v string) bool { return strings.Contains(v, "on a route that must not admit") }) {
			t.Fatalf("census violations %q, want the acquiring model route caught", violations)
		}
		if after == before {
			t.Fatalf("targets %d before and after — the target count missed an acquiring model route", before)
		}
	})
}

// classSamples reads the admitted-wait samples of host's two external
// classes from d's snapshot (XU, XB).
func classSamples(d *dispatch.Dispatcher, host string) (xu, xb int) {
	for _, ts := range d.Snapshot().Targets {
		if ts.Origin == host {
			return ts.External.Samples, ts.ExternalBulk.Samples
		}
	}
	return 0, 0
}

// TestV1AdmissionCensusChatRoute is gate (m) over the real mount (FOLGE-M
// M3, A03-W9 review 2 deviation 4): the census over the production router's
// /v1 group with POST /v1/chat/completions admitting, driven with a valid
// chat body and the census key, whose consumer has the default lane (bulk ⇒
// XB). The chat route is the one gatewayDeps builds for buildRouter — the
// router's alias registry, config store and dispatcher — with two seams:
// the admitting limiter (production has none yet, L11: the route would
// answer 503 before its planner) and the planner, built like gatewayDeps
// builds it but over a census recorder that wraps the router's own
// dispatcher (gatewayDeps takes the concrete dispatcher). Allowed is
// AllowedClasses() of that mounted planner: census green, and every XB
// acquire the census made is an admission of the router's dispatcher (not
// of a copy). The allowance trimmed to {XU} is census red.
func TestV1AdmissionCensusChatRoute(t *testing.T) {
	eng := newV1ChatEngine(t, http.StatusOK)
	var chat atomic.Pointer[http.Handler]
	f := newV1Fixture(t, nil, func(routes []modelRoute) []modelRoute {
		out := slices.Clone(routes)
		for i := range out {
			if out[i].method == http.MethodPost && out[i].pattern == "/chat/completions" {
				out[i].handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { (*chat.Load()).ServeHTTP(w, r) })
			}
		}
		return out
	})
	dispatch.SetPrincipalHook(handler.RequestPrincipal) // as cmd/ctxd/main.go
	t.Cleanup(func() { dispatch.SetPrincipalHook(nil) })
	var lane string
	if err := f.seed.QueryRow(context.Background(),
		`SELECT admission_lane FROM context_model_consumers WHERE label = 'w02-05-all'`).Scan(&lane); err != nil || lane != handler.LaneBulk {
		t.Fatalf("census key lane %q (%v), want the default %q", lane, err, handler.LaneBulk)
	}
	life, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	backendPool := backends.NewPool(f.pool, nil)
	backendPool.SeedSnapshotForTest([]backends.Backend{eng.row("census", 1)})
	f.disp.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{eng.srv.URL: {Slots: 4, InternalReserved: 1}}})
	rec := v1census.NewRecorder(f.disp)
	d := gatewayDeps(life, f.pool, f.cfgStore, f.aliases, backendPool, backends.NewQuotaAccountant(f.pool, 30*time.Second), f.disp)
	d.Limiter = &v1ChatLimiter{}
	d.Planner = handler.NewV1Admission(handler.V1AdmissionConfig{Dispatch: rec, Settings: d.Settings})
	h := oai.ChatCompletions(d)
	chat.Store(&h)
	f.setEnabled(t, true)
	opts := func(allowed []dispatch.Class) v1census.Options {
		return v1census.Options{
			Allowed:   allowed,
			Hosts:     []string{eng.srv.URL},
			Admitting: []string{"POST /v1/chat/completions"},
			Body: func(method, _ string) string {
				if method == http.MethodPost {
					return `{"model":"other","messages":[{"role":"user","content":"census"}]}`
				}
				return "{}"
			},
			Prepare: func(r *http.Request) *http.Request {
				r.RemoteAddr = "192.0.2.1:40000"
				r.TLS = &tls.ConnectionState{}
				r.Header.Set("Authorization", "Bearer "+f.allKey)
				r.Header.Set("Content-Type", "application/json")
				return r
			},
		}
	}
	allowed := d.Planner.AllowedClasses()
	if v := v1census.Check(f.router, rec, opts(allowed)); len(v) != 0 {
		t.Fatalf("census with the mounted planner's AllowedClasses() %v: violations %q", allowed, v)
	}
	if xu, xb := classSamples(f.disp, eng.srv.URL); xu != 0 || xb != 1 || eng.hits.Load() != 1 {
		t.Fatalf("after the census: the router's dispatcher admitted XU %d, XB %d; engine hits %d — want 0, 1 and 1 (the census acquire did not reach the router's dispatcher in class XB)",
			xu, xb, eng.hits.Load())
	}
	trimmed := v1census.Check(f.router, rec, opts([]dispatch.Class{dispatch.ClassExternal}))
	if !slices.ContainsFunc(trimmed, func(v string) bool {
		return strings.HasPrefix(v, "POST /v1/chat/completions: acquire in class "+dispatch.ClassExternalBulk.String())
	}) {
		t.Fatalf("census with the allowance trimmed to {XU}: violations %q, want the XB acquire of the chat route", trimmed)
	}
}
