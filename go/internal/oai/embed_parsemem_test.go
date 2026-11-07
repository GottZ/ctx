package oai_test

// The parse of an embeddings request against its share of the in-flight
// budget (W02-09 hardening H-1, Fremd-Review 2 R2-1): a body at
// gateway.max_request_bytes made of the smallest elements an array of
// unknown length can hold must cost about the request's own reserve
// (3 × Content-Length, design/02 §4.2.1), not a multiple of it — the parse
// runs before the Resolve, the limiter and any GPU work, and its answer is
// a cheap 400. Measured over the real route with the production defaults
// (8 MiB body cap, 256 MiB budget) and the container's GOMEMLIMIT
// (go/Dockerfile: 200 MiB), for one request and for ten at once (they all
// fit the budget).

import (
	"net/http"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

const (
	// parseMemBodyCap is gateway.max_request_bytes' default (config).
	parseMemBodyCap = 8 << 20
	// parseMemBudget is gateway.inflight_budget's default (config).
	parseMemBudget = 256 << 20
	// parseMemGoMemLimit is GOMEMLIMIT of the container (go/Dockerfile).
	parseMemGoMemLimit = 200 << 20
	// parseMemSlack is the live-heap allowance over the reserves: the
	// harness, the sampler, the recorder bodies and the GC's lag between
	// two samples — a constant, independent of the number of elements.
	parseMemSlack = 24 << 20
)

// parseMemBody is a request body of about limit bytes: the head, then the
// elements elem(0), elem(1), … comma-separated, then tail.
func parseMemBody(head string, elem func(i int) string, tail string, limit int) string {
	var b strings.Builder
	b.Grow(limit)
	b.WriteString(head)
	for i := 0; ; i++ {
		e := elem(i)
		if b.Len()+len(e)+1+len(tail) > limit {
			break
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(e)
	}
	b.WriteString(tail)
	return b.String()
}

func constElem(s string) func(int) string { return func(int) string { return s } }

// heapPeak samples HeapInuse until stop and keeps the maximum.
func heapPeak(stop <-chan struct{}, peak *atomic.Uint64, done chan<- struct{}) {
	defer close(done)
	var m runtime.MemStats
	for {
		runtime.ReadMemStats(&m)
		for {
			p := peak.Load()
			if m.HeapInuse <= p || peak.CompareAndSwap(p, m.HeapInuse) {
				break
			}
		}
		select {
		case <-stop:
			return
		case <-time.After(200 * time.Microsecond):
		}
	}
}

// TestEmbedParseHeapStaysInTheReserve (H-1): the shapes of input whose
// length is unknown until they are read — ≈ 2.8 million one-character
// strings, a token array, nested arrays, a top level of ≈ 1 million keys —
// in an 8-MiB body are refused with 400 at a live-heap peak of at most the
// request reserve plus a constant; ten such requests at once stay under ten
// reserves plus the constant. Red (Fremd-Review 2): 177 MiB for one
// request, 1 316 MiB for ten.
func TestEmbedParseHeapStaysInTheReserve(t *testing.T) {
	skipUnderRace(t, "the live heap of the parse over 8-MiB bodies of the smallest elements (HeapInuse against 3 × the body + parseMemSlack, one request and ten at once)")
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(parseMemGoMemLimit))
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 33, 1)
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget = parseMemBudget; s.MaxRequestBytes = parseMemBodyCap })
	limit := parseMemBodyCap - 64
	for _, tc := range []struct {
		name, body, code, param string
		n                       int
	}{
		{"one-character strings", parseMemBody(`{"model":"emb","input":[`, constElem(`"x"`), `]}`, limit), oai.CodeUnsupportedValue, "input", 1},
		{"one-character strings, ten at once", parseMemBody(`{"model":"emb","input":[`, constElem(`"x"`), `]}`, limit), oai.CodeUnsupportedValue, "input", 10},
		{"token array", parseMemBody(`{"model":"emb","input":[`, constElem(`1`), `]}`, limit), oai.CodeUnsupportedValue, "input", 1},
		{"nested arrays", parseMemBody(`{"model":"emb","input":[`, constElem(`[1]`), `]}`, limit), oai.CodeUnsupportedValue, "input", 1},
		{"top-level keys", parseMemBody(`{"model":"emb","input":"x",`, func(i int) string { return `"k` + strconv.Itoa(i) + `":0` }, `}`, limit), "", "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.hits.Store(0)
			runtime.GC()
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			base := m.HeapInuse
			var peak atomic.Uint64
			stop, sampled := make(chan struct{}), make(chan struct{})
			go heapPeak(stop, &peak, sampled)
			answers := make([]chatAnswer, tc.n)
			var wg sync.WaitGroup
			start := time.Now()
			for i := range tc.n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					answers[i] = rig.postEmbed(t, tc.body)
				}()
			}
			wg.Wait()
			took := time.Since(start)
			close(stop)
			<-sampled
			over := int64(peak.Load()) - int64(base)
			reserve := int64(3 * len(tc.body))
			bound := int64(tc.n)*reserve + parseMemSlack
			t.Logf("%s: %d request(s) of %d B ⇒ %d after %v; HeapInuse peak %d MiB over a base of %d MiB (reserve %d MiB each, bound %d MiB)",
				tc.name, tc.n, len(tc.body), answers[0].status, took.Round(time.Millisecond), over>>20, base>>20, reserve>>20, bound>>20)
			for i, a := range answers {
				code, param, _ := a.envelope(t)
				if a.status != http.StatusBadRequest || (tc.code != "" && (code != tc.code || param != tc.param)) {
					t.Fatalf("request %d: %d %s/%q, want 400 %s/%q", i, a.status, code, param, tc.code, tc.param)
				}
			}
			if e.hits.Load() != 0 {
				t.Errorf("wire hits %d, want 0", e.hits.Load())
			}
			if over > bound {
				t.Errorf("HeapInuse peak %d MiB over the base, want ≤ %d × the request reserve + %d MiB = %d MiB",
					over>>20, tc.n, parseMemSlack>>20, bound>>20)
			}
		})
	}
}
