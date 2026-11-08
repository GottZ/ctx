package oai_test

// The heap of the embeddings route against the sum of its bookings (W02-09
// rebase round 2, part D — the question whether the route must book its
// sensitivity scan as FOLGE-P's chat route does): requests at the body cap
// to an alias below credentials, measured over the route — the strict body
// cap and the in-flight budget in front of the handler, the reservation of
// every request read at the end of its handler — with the production
// defaults (8 MiB body cap, 256 MiB budget) and a live HeapInuse sampler,
// one form and one concurrency per process (the forms of FOLGE-P's heap
// gate that an embeddings body can carry: inputs of plain text, of "<" and
// of short secret-like assignments, the scan's most expensive text per
// byte), run under the container's GOMEMLIMIT:
//
//	CTX_W0209_SCAN_FORM=assign CTX_W0209_N=10 GOMEMLIMIT=200MiB \
//	  go test -run '^TestEmbedScanHeapGate$' -count=1 ./internal/oai/
//
// The bound: peak HeapInuse − the heap before the requests ≤ the sum of the
// reservations the requests held plus n × heapGateBaseLoad. Load-sensitive,
// so a measurement test outside the default suite (skipped without
// CTX_W0209_SCAN_FORM); the deterministic half is
// TestEmbedScanIsBookedBeforeItRuns.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
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

// embedMemMount is mountEmbed plus the strict body cap before the budget and
// the reservation probe right in front of the handler (memMount of the chat
// heap gate, for POST /v1/embeddings).
func embedMemMount(r *chatRig, h http.Handler, held *heldSum) chi.Router {
	rt := chi.NewRouter()
	rt.Route("/v1", func(g chi.Router) {
		g.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				rec := &oai.AccessRecord{}
				ctx := oai.WithAccessRecord(fedhop.WithIdentity(req.Context(), rigIdentity), rec)
				if ca := r.ca.Load(); ca != nil {
					ctx = context.WithValue(ctx, consumerKey{}, ca)
				}
				next.ServeHTTP(w, req.WithContext(ctx))
			})
		})
		g.Use(oai.Recover)
		g.Use(federation.PeerIngress(federation.PeerIngressConfig{Consumer: r.deps.Consumer, Binder: acceptPeerBinder{}, Logger: slog.New(slog.DiscardHandler)}))
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
		g.Method(http.MethodPost, "/embeddings", h)
	})
	return rt
}

// embedScanForm is a body of about lim bytes: inputs of 32 KiB of the
// form's text, as many as fit (at most 256, the alias's max_inputs).
func embedScanForm(t testing.TB, form, model string, lim int) []byte {
	unit := map[string]string{"text": "a", "lt": "<", "assign": "apikey:x "}[form]
	if unit == "" {
		t.Fatalf("unknown form %q", form)
	}
	input := strings.Repeat(unit, (32<<10)/len(unit))
	in, _ := json.Marshal(input)
	pre := `{"model":"` + model + `","input":[`
	var b bytes.Buffer
	b.WriteString(pre)
	for i := 0; i < 256 && b.Len()+len(in)+3 <= lim; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(in)
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

func TestEmbedScanHeapGate(t *testing.T) {
	form := os.Getenv("CTX_W0209_SCAN_FORM")
	if form == "" {
		t.Skip("CTX_W0209_SCAN_FORM not set (measurement test, see the file comment)")
	}
	n, _ := strconv.Atoi(os.Getenv("CTX_W0209_N"))
	n = max(n, 1)
	model := "embpub"
	if os.Getenv("CTX_W0209_CRED") != "" {
		model = "embsec"
	}
	e := newEmbedEngine(t, 8)
	var held heldSum
	opts := []rigOption{withoutCensus}
	if os.Getenv("CTX_W0209_NOSCAN") != "" { // the control: the scan replaced by its result for a scan without a match
		opts = append(opts, withDeps(func(d *oai.Deps) {
			d.Federation.Scan = func(required backends.Sensitivity, _ []byte) backends.Sensitivity { return required }
		}))
	}
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 4*n, 1, opts...)
	rig.reg.reload(t, embedScanAliases())
	rig.setSettings(func(s *oai.Settings) {
		s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes = memBudget, memCap, memRespCap
	})
	rig.router = embedMemMount(rig, oai.Embeddings(rig.deps), &held)
	body := embedScanForm(t, form, model, memCap-16)

	runtime.GC()
	runtime.GC()
	debug.FreeOSMemory()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapInuse
	var peak atomic.Uint64
	var budgetPeak atomic.Int64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		var s runtime.MemStats
		for {
			runtime.ReadMemStats(&s)
			memMax(&peak, s.HeapInuse)
			if u := rig.budget.InUse(); u > budgetPeak.Load() {
				budgetPeak.Store(u)
			}
			select {
			case <-stop:
				return
			default:
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()
	statuses := make([]string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			<-start
			rig.router.ServeHTTP(w, req)
			var env struct{ Error struct{ Code *string } }
			code := ""
			if w.Code != http.StatusOK && json.Unmarshal(w.Body.Bytes(), &env) == nil && env.Error.Code != nil {
				code = *env.Error.Code
			}
			statuses[i] = fmt.Sprintf("%d/%s", w.Code, code)
		}()
	}
	t0 := time.Now()
	close(start)
	wg.Wait()
	dur := time.Since(t0)
	close(stop)
	<-done
	cnt := map[string]int{}
	for _, s := range statuses {
		cnt[s]++
	}
	keys := make([]string, 0, len(cnt))
	for k := range cnt {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sum, handled := held.sum()
	growth := int64(peak.Load()) - int64(base)
	bound := sum + int64(n)*heapGateBaseLoad
	verdict := "ok"
	if growth > bound {
		verdict = "OVER"
	}
	t.Logf("W0209 scanmem form=%s model=%s noscan=%v n=%d bytes=%d answers=%v handled=%d dur=%v base=%dMiB growth=%.1fMiB held_sum=%.1fMiB bound=%.1fMiB budget_peak=%.1fMiB gomemlimit=%d verdict=%s",
		form, model, os.Getenv("CTX_W0209_NOSCAN") != "", n, len(body), cnt, handled, dur.Round(time.Millisecond), base>>20, float64(growth)/(1<<20), float64(sum)/(1<<20),
		float64(bound)/(1<<20), float64(budgetPeak.Load())/(1<<20), debug.SetMemoryLimit(-1), verdict)
	if growth > bound {
		t.Errorf("HeapInuse grew %.1f MiB, more than the %d requests' reservations (%.1f MiB) plus %d × %d KiB harness load = %.1f MiB",
			float64(growth)/(1<<20), handled, float64(sum)/(1<<20), n, heapGateBaseLoad>>10, float64(bound)/(1<<20))
	}
}
