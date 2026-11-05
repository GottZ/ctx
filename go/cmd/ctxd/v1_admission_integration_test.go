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

// TestV1AdmissionCensusModelRoutes: the census over the /v1 routes W02-05
// mounted on the production router (buildRouter) with a live model key —
// GET /v1/models and /v1/models/* acquire nothing: no recorded door and no
// target created on the router's own dispatcher.
func TestV1AdmissionCensusModelRoutes(t *testing.T) {
	f := newV1Fixture(t, nil, nil)
	f.setEnabled(t, true)
	// The census requests reach the handlers (not a vacuous run over 401s).
	if a := f.do(http.MethodGet, "/v1/models", withBearer(f.allKey)); a.status != http.StatusOK {
		t.Fatalf("GET /v1/models with the census key = %d %s, want 200", a.status, a.body)
	}
	before := len(f.disp.Snapshot().Targets)
	rec := v1census.NewRecorder(f.disp)
	v1census.Assert(t, f.router, rec, v1census.Options{
		Allowed: []dispatch.Class{dispatch.ClassExternal},
		Prepare: func(r *http.Request) *http.Request {
			r.RemoteAddr = "192.0.2.1:40000"
			r.TLS = &tls.ConnectionState{}
			r.Header.Set("Authorization", "Bearer "+f.allKey)
			return r
		},
	})
	if after := len(f.disp.Snapshot().Targets); after != before {
		t.Fatalf("the router's dispatcher has %d targets after the census, %d before — a model route acquired", after, before)
	}
}
