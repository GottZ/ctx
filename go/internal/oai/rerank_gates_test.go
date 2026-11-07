package oai_test

// The heap gates of the rerank routes (W02-10, after FOLGE-E's G1 and the
// class gate over the route): over the real route, a lean engine (it reads
// the upstream body through a small buffer and answers from a count — no
// copy of the request in this process), the production in-flight budget of
// 256 MiB and GOMEMLIMIT=200MiB (the image's limit), the live heap a run of
// requests holds stays within what their reservations held plus a fixed
// ground load per request: the route holds on the heap only what it booked.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/oai"
)

// rerankGroundLoad is the heap one request may hold beside its reservation:
// the router, the middleware, the dispatcher's lease, the lean engine's side
// of the call — FOLGE-E's ground load per request.
const rerankGroundLoad = 256 << 10

// leanRerankEngine answers llama.cpp's rerank form without holding the
// request: it reads the body through a 4 KiB buffer, counts the documents
// (plain test documents carry no quote: every two quotes past the frame's
// ten are one document) and answers one result per document, two prompt
// tokens each.
func leanRerankEngine(t testing.TB) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		br := bufio.NewReaderSize(r.Body, 4<<10)
		quotes := 0
		for {
			b, err := br.ReadByte()
			if err != nil {
				break
			}
			if b == '"' {
				quotes++
			}
		}
		n := max((quotes-10)/2, 0)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"usage":{"prompt_tokens":%d},"results":[`, 2*n)
		for i := range n {
			if i > 0 {
				_, _ = io.WriteString(w, ",")
			}
			fmt.Fprintf(w, `{"index":%d,"relevance_score":%d.5}`, i, i%7)
		}
		_, _ = io.WriteString(w, "]}")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// rerankMemMount mounts POST /v1/rerank with the production stack's body cap
// and budget, and adds the reservation each request held at its end to held.
func rerankMemMount(r *chatRig, h http.Handler, held *heldSum) chi.Router {
	rt := chi.NewRouter()
	rt.Route("/v1", func(g chi.Router) {
		g.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				ctx := oai.WithAccessRecord(fedhop.WithIdentity(req.Context(), rigIdentity), &oai.AccessRecord{})
				if ca := r.ca.Load(); ca != nil {
					if other := req.Header.Get(foreignTenantHeader); other != "" {
						ca = foreignConsumer(ca, other)
					}
					ctx = context.WithValue(ctx, consumerKey{}, ca)
				}
				next.ServeHTTP(w, req.WithContext(ctx))
			})
		})
		g.Use(oai.Recover)
		g.Use(federation.PeerIngress(federation.PeerIngressConfig{Consumer: r.deps.Consumer, Logger: slog.New(slog.DiscardHandler)}))
		g.Use(handler.MaxBodySizeStrictWith(func() int64 { return r.settings.Load().MaxRequestBytes }, oai.RefuseRequestTooLarge))
		g.Use(oai.InflightBudget(&r.budget, func() oai.Settings { return *r.settings.Load() }))
		g.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req)
				if res := oai.ReservationFrom(req.Context()); res != nil {
					held.add(oai.ReservationHeld(res))
				}
			})
		})
		g.Method(http.MethodPost, "/rerank", h)
	})
	return rt
}

// liveSampler samples the live heap (HeapAlloc right after a forced
// collection) every few milliseconds until stop; peak is the largest, busy
// the requests in flight (the caller counts them), seen the samples taken
// while one was (lead review 1 delta D-3: a run between two samples
// measures nothing).
func liveSampler(busy *atomic.Int64) (stop func() (peak uint64, seen int64)) {
	var peak atomic.Uint64
	var seen atomic.Int64
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		for {
			inFlight := busy.Load() > 0
			runtime.GC()
			runtime.ReadMemStats(&ms)
			if inFlight && busy.Load() > 0 {
				seen.Add(1)
			}
			if ms.HeapAlloc > peak.Load() {
				peak.Store(ms.HeapAlloc)
			}
			select {
			case <-quit:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	return func() (uint64, int64) { close(quit); <-done; return peak.Load(), seen.Load() }
}

// TestRerankHeapGate: 1, 128 and 1000 documents filling a body, one request
// and ten at once (each its own consumer), to an alias at credentials
// (rrwide: no scan; sub_batch 128) at the 8-MiB body cap and one below it
// (rrpub: the scan booked; one block) at 4 MiB — the live heap peak over the
// baseline stays within the reservations the requests held (Σ, read at each
// request's end) plus rerankGroundLoad per request. At the cap a request to
// rrpub books more than the budget of 256 MiB (the scan's 23 × the body and
// the wire's 29/4 × the body on top of the request share): it is 503
// server_overloaded before its allocations — the named limit of the
// budget floor (bau report W02-10, stop finding S-V39), pinned as such.
// Without -short only (eight-MiB bodies, a collection every 2 ms); never
// under the race detector, which changes what allocates.
func TestRerankHeapGate(t *testing.T) {
	if testing.Short() {
		t.Skip("heap gate: eight-MiB bodies under a collection every 2 ms — runs without -short")
	}
	skipUnderRace(t, "the live heap of the rerank route")
	old := debug.SetMemoryLimit(200 << 20)
	defer debug.SetMemoryLimit(old)
	const bodyCap = 8 << 20
	srv := leanRerankEngine(t)
	row := backends.Backend{
		ID: "id-lean", Name: "lean", Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "RERANK-MODEL",
		Roles: []string{"rerank"}, Trust: backends.TrustFull, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
		Priority: 10, Timeouts: map[string]int{"rerank": 60},
	}
	t.Run("rrpub at the cap: the named limit", func(t *testing.T) {
		rig := newRerankRig(t, []backends.Backend{row}, 200, 1, withoutCensus)
		rig.setSettings(func(s *oai.Settings) { s.InflightBudget = 256 << 20 })
		var held heldSum
		rig.router = rerankMemMount(rig, oai.Rerank(rig.deps, oai.RerankJina), &held)
		body := rerankBody("rrpub", "which document is relevant", rerankDocsOf(128, (bodyCap-4096)/128-4))
		req := httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader(body))
		w := httptest.NewRecorder()
		rig.router.ServeHTTP(w, req)
		sum, _ := held.sum()
		t.Logf("W0210 heap rrpub at the cap: body %d B, %d %.120s, held at the refusal %d KiB", len(body), w.Code, w.Body.String(), sum>>10)
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), oai.CodeServerOverloaded) {
			t.Fatalf("%d %s — want the named limit, 503 server_overloaded", w.Code, w.Body.String())
		}
	})
	for _, alias := range []string{"rrwide", "rrpub"} {
		limit := map[string]int{"rrwide": bodyCap, "rrpub": bodyCap / 2}[alias]
		for _, docs := range []int{1, 128, 1000} {
			for _, n := range []int{1, 10} {
				t.Run(fmt.Sprintf("%s/%d docs/n=%d", alias, docs, n), func(t *testing.T) {
					rig := newRerankRig(t, []backends.Backend{row}, 200, 1, withoutCensus)
					rig.setSettings(func(s *oai.Settings) { s.InflightBudget = 256 << 20 })
					var held heldSum
					rig.router = rerankMemMount(rig, oai.Rerank(rig.deps, oai.RerankJina), &held)
					size := (limit - 4096) / docs
					body := rerankBody(alias, "which document is relevant", rerankDocsOf(docs, size-4))
					// A run whose requests no sample saw while they were in
					// flight measures nothing: it is run again, up to three
					// times (D-3).
					var peak, base uint64
					var seen int64
					statuses := make([]int, n)
					for try := 0; try < 3 && seen == 0; try++ {
						held = heldSum{}
						runtime.GC()
						var ms runtime.MemStats
						runtime.ReadMemStats(&ms)
						base = ms.HeapAlloc
						var busy atomic.Int64
						stop := liveSampler(&busy)
						var wg sync.WaitGroup
						for i := range n {
							wg.Add(1)
							busy.Add(1)
							go func() {
								defer wg.Done()
								defer busy.Add(-1)
								req := httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader(body))
								req.Header.Set(foreignTenantHeader, fmt.Sprintf("heap-%d", i))
								w := &discardWriter{h: http.Header{}}
								rig.router.ServeHTTP(w, req)
								statuses[i] = w.status
							}()
						}
						wg.Wait()
						peak, seen = stop()
					}
					if seen == 0 {
						t.Fatalf("three runs, no live-heap sample while a request was in flight — the cell measures nothing")
					}
					sum, count := held.sum()
					over := int64(peak) - int64(base)
					bound := sum + int64(n)*rerankGroundLoad
					served := 0
					for _, s := range statuses {
						if s == http.StatusOK {
							served++
						}
					}
					t.Logf("W0210 heap %s: body %d B, %d served of %d (%v), live peak +%d KiB (%d samples in flight), Σ held %d KiB over %d requests, bound %d KiB",
						t.Name(), len(body), served, n, statuses, over>>10, seen, sum>>10, count, bound>>10)
					if served == 0 {
						t.Fatalf("no request served: %v", statuses)
					}
					if over > bound {
						t.Errorf("the live heap grew by %d KiB, the reservations held %d KiB + %d × %d KiB ground load = %d KiB",
							over>>10, sum>>10, n, rerankGroundLoad>>10, bound>>10)
					}
				})
			}
		}
	}
}

// TestRerankClassGateOverTheRoute: every combination of the class gate
// (RerankClassGateCombos: every parse key × every value form) at 64 KiB,
// over the route to an alias at credentials and one below it — the bytes
// the request allocates on the route's side (TotalAlloc of the request,
// least of three; the lean engine's share is a fixed ground load) stay
// within what its reservation held plus rerankGroundLoad. The parse-side
// gate (TestRerankClassGateBytesPerParse) measures every size; this one
// adds the stages around the parse — the answer, the scan, the wire call,
// the log row's text (body logging full).
func TestRerankClassGateOverTheRoute(t *testing.T) {
	skipUnderRace(t, "the allocations of the rerank route")
	srv := leanRerankEngine(t)
	row := backends.Backend{
		ID: "id-lean", Name: "lean", Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "RERANK-MODEL",
		Roles: []string{"rerank"}, Trust: backends.TrustFull, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
		Priority: 10, Timeouts: map[string]int{"rerank": 60},
	}
	rig := newRerankRig(t, []backends.Backend{row}, 200, 1, withoutCensus)
	rig.setSettings(func(s *oai.Settings) { s.BodyLogging = "full" })
	var held heldSum
	rig.router = rerankMemMount(rig, oai.Rerank(rig.deps, oai.RerankJina), &held)
	red, runs := 0, 0
	answers := map[int]int{}
	for _, alias := range []string{"rrwide", "rrpub"} {
		for _, combo := range oai.RerankClassGateCombos() {
			body, ok := oai.RerankClassGateBody(combo, alias, 64<<10)
			if !ok {
				t.Fatalf("combo %s has no body", combo)
			}
			var lastHeld int64
			var status int
			alloc, _ := allocOf(func() int {
				before, _ := held.sum()
				req := httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader(string(body)))
				w := &discardWriter{h: http.Header{}}
				rig.router.ServeHTTP(w, req)
				after, _ := held.sum()
				lastHeld, status = after-before, w.status
				return w.status
			})
			runs++
			answers[status]++
			if bound := lastHeld + rerankGroundLoad; int64(alloc) > bound+int64(len(body)) {
				red++
				t.Errorf("%s @%s: %d: the request allocated %d B, its reservation held %d B (+ %d ground load + the body's string %d B)",
					combo, alias, status, alloc, lastHeld, rerankGroundLoad, len(body))
			}
		}
	}
	t.Logf("W0210 class gate over the route: combos=%d aliases=2 runs=%d red=%d statuses=%v", len(oai.RerankClassGateCombos()), runs, red, answers)
}
