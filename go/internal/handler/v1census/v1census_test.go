package v1census_test

// The census over a fixture mount (wave A03-W8): green on the planner of the
// admission contract (handler.V1Admission), and red on every planner defect
// it exists to catch — each hand-built here against the same Recorder.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/handler/v1census"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/oai"
)

const (
	hostA   = "http://census-a.invalid:8000"
	hostLAN = "http://census-lan.invalid:8080" // no slots: not offered
)

func row(name, host string) backends.Backend {
	return backends.Backend{
		ID: name, Name: name, Host: host, Protocol: backends.ProtocolOpenAI, Scope: backends.GlobalScope,
		Roles:    []string{"chat", "embed"},
		ModelMap: map[string]backends.ModelSpec{"chat": {Model: "m"}, "embed": {Model: "e"}},
	}
}

func consumer() *auth.ConsumerAuth {
	return &auth.ConsumerAuth{ConsumerID: "census-consumer", TenantID: "census-tenant", Kind: auth.KindApp,
		FairWeight: 1, IsValid: true, AdmissionLane: handler.LaneUrgent}
}

// bulkConsumer is the census consumer on the lane bulk (A03-W9: the external
// bulk class XB).
func bulkConsumer() *auth.ConsumerAuth {
	ca := consumer()
	ca.AdmissionLane = handler.LaneBulk
	return ca
}

// rig is a real dispatcher behind a Recorder, the principal hook resolving
// every context to the census consumer (the door's part in production).
func rig(t *testing.T) (*v1census.Recorder, *handler.V1Admission) {
	t.Helper()
	ca := consumer()
	dispatch.SetPrincipalHook(func(context.Context) dispatch.Principal {
		return dispatch.Principal{TenantID: ca.TenantID, ConsumerID: ca.ConsumerID, ConsumerKind: string(ca.Kind), FairWeight: ca.FairWeight}
	})
	t.Cleanup(func() { dispatch.SetPrincipalHook(nil) })
	d := dispatch.New(slog.New(slog.DiscardHandler), dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{hostA: {Slots: 5, InternalReserved: 1}}})
	rec := v1census.NewRecorder(d)
	adm := handler.NewV1Admission(handler.V1AdmissionConfig{Dispatch: rec, Settings: func() oai.Settings { return oai.Settings{} },
		Logger: slog.New(slog.DiscardHandler)})
	return rec, adm
}

func served(context.Context, *backends.Backend, *dispatch.Lease, time.Duration) (dispatch.Usage, bool, error) {
	return dispatch.Usage{PromptTokens: 1}, true, nil
}

// chatHandler is the non-stream route shape on the contract: plan, walk,
// answer — an admission error goes to the planner's writer. who is the
// consumer the door resolved.
func chatHandler(adm *handler.V1Admission, chain []backends.Backend, who func() *auth.ConsumerAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		plan, err := adm.Plan(r.Context(), who(), oai.AdmissionInput{Chain: chain})
		if err == nil {
			_, _, err = llm.WalkChain(r.Context(), plan.Chain, "chat", plan.Acquirer, nil, llm.WalkRules{AllowedClasses: adm.AllowedClasses()}, served)
		}
		answer(w, r, adm, err)
	}
}

// embedHandler walks a sub-block request: First, then one continuation per
// further block of the plan, all on the attempt's row, each after the
// previous block's lease went back.
func embedHandler(adm *handler.V1Admission, chain []backends.Backend, who func() *auth.ConsumerAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		plan, err := adm.Plan(r.Context(), who(), oai.AdmissionInput{Chain: chain, Units: 8, SubBatch: 4})
		if err == nil {
			_, _, err = llm.WalkChain(r.Context(), plan.Chain, "embed", plan.Acquirer, nil, llm.WalkRules{AllowedClasses: adm.AllowedClasses()},
				func(_ context.Context, b *backends.Backend, first *dispatch.Lease, timeout time.Duration) (dispatch.Usage, bool, error) {
					// Block one's wire call is done: its lease goes back
					// before the next block asks (one lease per sub-block,
					// design/02 §4.8; Release is idempotent, the walker's
					// deferred one is then a no-op). Release cancels that
					// lease's run context, so the continuations ask on the
					// request's context.
					first.Release()
					for range plan.Acquirer.Blocks()[1:] {
						l, _, cerr := plan.Acquirer.Continue(r.Context(), b, "embed", timeout)
						if cerr != nil {
							return dispatch.Usage{}, true, cerr
						}
						l.Release()
					}
					return dispatch.Usage{PromptTokens: 1}, true, nil
				})
		}
		answer(w, r, adm, err)
	}
}

// answer is the route's error split: an admission failure (Plan, an acquire
// error of the walk, its class assert) goes to the planner's writer, an
// attempt error to the route's own mapping (W02-07), success is a 200.
func answer(w http.ResponseWriter, r *http.Request, adm *handler.V1Admission, err error) {
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

func models(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
}

// TestCensusFixtureMount: the census holds on a mount whose inference
// routes plan with V1Admission — every acquire external, continuations after
// the First, the filter and clamp read with b.Host, and the model list
// acquires nothing.
func TestCensusFixtureMount(t *testing.T) {
	rec, adm := rig(t)
	chain := []backends.Backend{row("lan", hostLAN), row("a", hostA)}
	r := chi.NewRouter()
	r.Route("/v1", func(g chi.Router) {
		g.Post("/chat/completions", chatHandler(adm, chain, consumer))
		g.Post("/embeddings", embedHandler(adm, chain, consumer))
		g.Get("/models", models)
		g.Get("/models/{model}", models)
	})
	v1census.Assert(t, r, rec, v1census.Options{
		Allowed:   adm.AllowedClasses(),
		Hosts:     []string{hostA, hostLAN},
		Admitting: []string{"POST /v1/chat/completions", "POST /v1/embeddings"},
	})
	// The fixture really served (a census over refused requests would hold
	// rule 6 only through the refusal).
	for _, path := range []string{"/v1/chat/completions", "/v1/embeddings"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200 (body %s)", path, w.Code, w.Body)
		}
	}
}

// TestCensusFixtureMountBulk is gate (m) of A03-W9: the same fixture mount
// for a consumer on the lane bulk — the census holds with Allowed =
// AllowedClasses() (0 violations), both admitting routes acquire, every
// acquire is in the external bulk class XB ("external_bulk", compared by
// name so the gate compiles against the tree before A03-W9, where bulk
// planned no acquire: rule 6 "no acquire on an admitting route", 503), and
// both routes answer 200. An AllowedClasses without XB fails the walk's
// class assert (llm.ErrWalkClass) before any acquire — red here too.
func TestCensusFixtureMountBulk(t *testing.T) {
	rec, adm := rig(t)
	chain := []backends.Backend{row("lan", hostLAN), row("a", hostA)}
	r := chi.NewRouter()
	r.Route("/v1", func(g chi.Router) {
		g.Post("/chat/completions", chatHandler(adm, chain, bulkConsumer))
		g.Post("/embeddings", embedHandler(adm, chain, bulkConsumer))
		g.Get("/models", models)
		g.Get("/models/{model}", models)
	})
	v1census.Assert(t, r, rec, v1census.Options{
		Allowed:   adm.AllowedClasses(),
		Hosts:     []string{hostA, hostLAN},
		Admitting: []string{"POST /v1/chat/completions", "POST /v1/embeddings"},
	})
	for _, path := range []string{"/v1/chat/completions", "/v1/embeddings"} {
		rec.Take()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		if w.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200 (body %s)", path, w.Code, w.Body)
		}
		acquires := 0
		for _, e := range rec.Take() {
			if e.Kind != v1census.KindAcquire && e.Kind != v1census.KindTry {
				continue
			}
			acquires++
			if e.Class.String() != "external_bulk" {
				t.Errorf("%s: %s (continuation %v) in class %s, want external_bulk", path, e.Kind, e.Continuation, e.Class)
			}
		}
		if acquires == 0 {
			t.Errorf("%s: no acquire for a bulk consumer", path)
		}
	}
}

// TestCensusCatchesDefects: one fixture route per defect the census exists
// for; each must produce its violation.
func TestCensusCatchesDefects(t *testing.T) {
	rec, adm := rig(t)
	a := row("a", hostA)
	req := func(cont bool, class dispatch.Class) dispatch.Request {
		return dispatch.Request{Target: dispatch.Target{Origin: hostA}, Class: class, Role: "chat", Continuation: cont}
	}
	release := func(l *dispatch.Lease, _ context.Context, _ error) {
		if l != nil {
			l.Release()
		}
	}
	reads := func() { rec.ExternalAdmissible(hostA); rec.ExternalWeightCap(hostA) }
	for _, tc := range []struct {
		name      string
		handler   http.HandlerFunc
		admitting bool
		want      string
	}{
		{"planner binds interactive", func(_ http.ResponseWriter, r *http.Request) {
			reads()
			release(llm.Admission{Admitter: rec, Class: dispatch.ClassInteractive}.AcquireWith(r.Context(), &a, "chat", time.Minute, llm.AdmitOptions{}))
		}, true, "outside the external classes"},
		{"planner binds background", func(_ http.ResponseWriter, r *http.Request) {
			reads()
			release(rec.Acquire(r.Context(), req(false, dispatch.ClassBackground)))
		}, true, "outside the external classes"},
		{"continue before first", func(_ http.ResponseWriter, r *http.Request) {
			reads()
			release(rec.Acquire(r.Context(), req(true, dispatch.ClassExternal)))
		}, true, "before the first acquire"},
		{"next before first", func(_ http.ResponseWriter, r *http.Request) {
			reads()
			release(rec.TryAcquire(r.Context(), req(false, dispatch.ClassExternal)))
		}, true, "before the first acquire"},
		{"second first", func(_ http.ResponseWriter, r *http.Request) {
			reads()
			release(rec.Acquire(r.Context(), req(false, dispatch.ClassExternal)))
			release(rec.Acquire(r.Context(), req(false, dispatch.ClassExternal)))
		}, true, "second wait point"},
		{"request-derived spelling", func(_ http.ResponseWriter, r *http.Request) {
			rec.ExternalAdmissible("http://" + r.Host)
			rec.ExternalWeightCap(hostA)
			release(rec.Acquire(r.Context(), req(false, dispatch.ClassExternal)))
		}, true, "not the b.Host of a chain row"},
		{"request-derived spelling at the door", func(_ http.ResponseWriter, r *http.Request) {
			reads()
			release(rec.Acquire(r.Context(), dispatch.Request{Target: dispatch.Target{Origin: "http://" + r.Host}, Class: dispatch.ClassExternal, Role: "chat"}))
		}, true, "not the b.Host of a chain row"},
		{"request-derived spelling at the try door", func(_ http.ResponseWriter, r *http.Request) {
			reads()
			release(rec.Acquire(r.Context(), req(false, dispatch.ClassExternal)))
			release(rec.TryAcquire(r.Context(), dispatch.Request{Target: dispatch.Target{Origin: hostA + "/"}, Class: dispatch.ClassExternal, Role: "chat"}))
		}, true, "not the b.Host of a chain row"},
		{"request-derived spelling at the clamp", func(_ http.ResponseWriter, r *http.Request) {
			rec.ExternalAdmissible(hostA)
			rec.ExternalWeightCap(strings.ToUpper(hostA))
			release(rec.Acquire(r.Context(), req(false, dispatch.ClassExternal)))
		}, true, "not the b.Host of a chain row"},
		{"class hint for external", func(_ http.ResponseWriter, r *http.Request) {
			reads()
			release(rec.Acquire(r.Context(), req(false, dispatch.ClassExternal)))
			rec.RetryAfterHint(hostA, dispatch.ClassExternal)
		}, true, "RetryAfterHint read for an external class"},
		{"acquire without the filter and clamp reads", func(_ http.ResponseWriter, r *http.Request) {
			release(rec.Acquire(r.Context(), req(false, dispatch.ClassExternal)))
		}, true, "without reading ExternalAdmissible"},
		{"admitting route without acquire", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }, true, "no acquire on an admitting route"},
		// Review 2 F-2 / H-3: a continuation asked while the request still
		// holds its previous block's lease waits for its own units — on a
		// full E a self-deadlock, else the class head held against others.
		{"continue while holding the block lease", func(_ http.ResponseWriter, r *http.Request) {
			reads()
			first, _, err := rec.Acquire(r.Context(), req(false, dispatch.ClassExternal))
			if err != nil {
				return
			}
			release(rec.Acquire(r.Context(), req(true, dispatch.ClassExternal)))
			first.Release()
		}, true, "while the request holds"},
		// Review 3 FR3-2: after a failover the held lease is the Next
		// (TryAcquire) one — the same rule.
		{"continue while holding the failover lease", func(_ http.ResponseWriter, r *http.Request) {
			reads()
			release(rec.Acquire(r.Context(), req(false, dispatch.ClassExternal))) // the first row failed over
			next, _, err := rec.TryAcquire(r.Context(), req(false, dispatch.ClassExternal))
			if err != nil {
				return
			}
			release(rec.Acquire(r.Context(), req(true, dispatch.ClassExternal)))
			next.Release()
		}, true, "while the request holds"},
		{"model list acquires", func(_ http.ResponseWriter, r *http.Request) {
			reads()
			release(rec.Acquire(r.Context(), req(false, dispatch.ClassExternal)))
		}, false, "on a route that must not admit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chi.NewRouter()
			r.Route("/v1", func(g chi.Router) { g.Post("/x", tc.handler) })
			opts := v1census.Options{Allowed: adm.AllowedClasses(), Hosts: []string{hostA}}
			if tc.admitting {
				opts.Admitting = []string{"POST /v1/x"}
			}
			got := v1census.Check(r, rec, opts)
			found := false
			for _, v := range got {
				found = found || strings.Contains(v, tc.want)
			}
			if !found {
				t.Fatalf("violations %q, want one containing %q", got, tc.want)
			}
		})
	}
	// PR #142 bot review 4477069089: the held block fills the partition, so
	// the continuation would wait forever for its own units — the census
	// must report rule 7 at once, not hang in the request.
	t.Run("continue while holding a full partition", func(t *testing.T) {
		r := chi.NewRouter()
		r.Route("/v1", func(g chi.Router) {
			g.Post("/x", func(_ http.ResponseWriter, hr *http.Request) {
				reads()
				full := dispatch.Request{Target: dispatch.Target{Origin: hostA}, Class: dispatch.ClassExternal, Role: "embed", Weight: 4} // E = 4
				first, _, err := rec.Acquire(hr.Context(), full)
				if err != nil {
					return
				}
				defer first.Release()
				full.Continuation = true
				release(rec.Acquire(hr.Context(), full))
			})
		})
		done := make(chan []string, 1)
		go func() {
			done <- v1census.Check(r, rec, v1census.Options{Allowed: adm.AllowedClasses(), Hosts: []string{hostA}, Admitting: []string{"POST /v1/x"}})
		}()
		select {
		case got := <-done:
			if !slices.ContainsFunc(got, func(v string) bool { return strings.Contains(v, "while the request holds") }) {
				t.Fatalf("violations %q, want rule 7", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Check hung in a continuation that waits for its own units — rule 7 must refuse it before the dispatcher")
		}
	})
	if got := v1census.Check(chi.NewRouter(), rec, v1census.Options{}); len(got) == 0 {
		t.Fatal("a census over an empty /v1 passed — it must refuse to prove nothing")
	}
}
