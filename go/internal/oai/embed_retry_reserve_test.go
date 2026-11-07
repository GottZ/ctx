//go:build !race

package oai_test

// The measurement pin of the retry's term (FOLGE-E hardening 2, L-4),
// outside -race like the route's other measurement pins
// (embed_wire_call_reserve_test.go): httpx.DoRetryOnce sends a block's body
// a second time when the first call dies before its answer — on a fresh
// connection, through the transport's copy once more — and the route books
// that retry with the first call of every block.

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
)

// retryAlloc is the least TotalAlloc of five requests of inputs to the
// credentials alias at E = 1 (a block per input) on leanEmbedEngine, or on
// retryEmbedEngine when retry is set — with the calls the engine dropped
// per request.
func retryAlloc(t *testing.T, inputs []string, retry bool) (best uint64, dropped int64) {
	t.Helper()
	var srv *httptest.Server
	var aborts *atomic.Int64
	if retry {
		srv, aborts = retryEmbedEngine(t, 8)
	} else {
		srv = leanEmbedEngine(t, 8)
	}
	row := backends.Backend{
		ID: "id-lean", Name: "lean", Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "EMBED-MODEL",
		Roles: []string{"embed"}, Trust: backends.TrustFull, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
		Priority: 10, Timeouts: map[string]int{"embed": 30},
	}
	rig := newEmbedRig(t, []backends.Backend{row}, 2, 1, withoutCensus)
	rig.reg.reload(t, embedScanAliases())
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget, s.MaxRequestBytes = memBudget, memCap })
	rig.deps.Record = func(row llmlog.GatewayRow) { _ = llmlog.GatewayEntry(row) }
	var held heldSum
	rig.router = embedMemMount(rig, oai.Embeddings(rig.deps), &held)
	body := embedBody("embsec", inputs)
	const runs = 5
	best = ^uint64(0)
	for range runs {
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
		w := httptest.NewRecorder()
		rig.router.ServeHTTP(w, req)
		runtime.ReadMemStats(&m1)
		if w.Code != http.StatusOK {
			t.Fatalf("retry %v: %d %s", retry, w.Code, w.Body.String())
		}
		best = min(best, m1.TotalAlloc-m0.TotalAlloc)
	}
	if aborts != nil {
		dropped = aborts.Load() / runs
	}
	return best, dropped
}

// retryPerBlock is what a request of inputs at a block per input allocates
// per block more when every block's first call is dropped and retried: the
// median of three passes (a single pass spread 26.2 … 29.6 KB for
// three-byte blocks, the median of three stays within one 4-KiB step),
// with the passes' values.
func retryPerBlock(t *testing.T, inputs []string) (median float64, passes []float64) {
	t.Helper()
	for range 3 {
		plain, _ := retryAlloc(t, inputs, false)
		retried, dropped := retryAlloc(t, inputs, true)
		if dropped != int64(len(inputs)) {
			t.Fatalf("the engine dropped %d calls per request, want one per block (%d)", dropped, len(inputs))
		}
		passes = append(passes, float64(int64(retried)-int64(plain))/float64(len(inputs)))
	}
	sorted := slices.Clone(passes)
	slices.Sort(sorted)
	return sorted[1], passes
}

// TestEmbedWireRetryReserveIsTheMeasuredOne is the relational pin of the
// retry's terms: a request of 256 three-byte texts at E = 1, whose every
// block is dropped once and retried, allocates per block (median of three
// passes) at most
// oai.EmbedWireRetryBytes more than the same request answered at once, and
// more than one 4-KiB step below it (the copy of a three-byte block is a
// few bytes); a request of 128 blocks past the transport's copy buffer
// allocates per block at most embed.WireCopyBuffer more than that — the
// second copy the route books beside the retry's term.
func TestEmbedWireRetryReserveIsTheMeasuredOne(t *testing.T) {
	small, sp := retryPerBlock(t, repeatInputs("abc", 256))
	t.Logf("FOLGEE wire retry: %.0f B per retried block of three bytes (passes %.0f), booked %d", small, sp, oai.EmbedWireRetryBytes)
	if small > float64(oai.EmbedWireRetryBytes) || small <= float64(oai.EmbedWireRetryBytes-4<<10) {
		t.Errorf("a retried call allocates %.0f B, the route books %d B for it (want at most that and more than one 4-KiB step below)", small, oai.EmbedWireRetryBytes)
	}
	large, lp := retryPerBlock(t, repeatInputs(strings.Repeat("a", 32<<10), 128))
	t.Logf("FOLGEE wire retry: %.0f B per retried block of 32 KiB (passes %.0f), booked %d", large, lp, oai.EmbedWireRetryBytes+oai.EmbedWireCopyReserve)
	if large > float64(oai.EmbedWireRetryBytes+oai.EmbedWireCopyReserve) {
		t.Errorf("a retried call of a block past the copy buffer allocates %.0f B, the route books %d B for it (the retry's term and the second copy)", large, oai.EmbedWireRetryBytes+oai.EmbedWireCopyReserve)
	}
}
