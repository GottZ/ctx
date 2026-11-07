//go:build !race

package oai_test

// The measurement pin of the per-call term (FOLGE-E), outside -race: the
// race detector changes what a wire call allocates (33 037 B per call under
// -race against 12 901 B in a production build) — the term books the
// production build's allocations.

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
)

// TestEmbedWireCallReserveIsTheMeasuredOne is the relational pin of the
// per-call term: one request of 256 three-byte texts over the route at
// E = 1 (256 wire calls) and E = 256 (one) — the allocation per further
// call (the in-process engine's server side included) is at most
// oai.EmbedWireCallBytes and more than one 4-KiB step below it.
func TestEmbedWireCallReserveIsTheMeasuredOne(t *testing.T) {
	inputs := make([]string, 256)
	for i := range inputs {
		inputs[i] = "abc"
	}
	body := embedBody("embsec", inputs)
	alloc := func(slots int) uint64 {
		e := newEmbedEngine(t, 8)
		rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, slots, 1, withoutCensus)
		rig.reg.reload(t, embedScanAliases())
		rig.deps.Record = func(row llmlog.GatewayRow) { _ = llmlog.GatewayEntry(row) }
		var held heldSum
		rig.router = embedMemMount(rig, oai.Embeddings(rig.deps), &held)
		best := ^uint64(0)
		for range 5 {
			var m0, m1 runtime.MemStats
			runtime.ReadMemStats(&m0)
			req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
			w := httptest.NewRecorder()
			rig.router.ServeHTTP(w, req)
			runtime.ReadMemStats(&m1)
			if w.Code != http.StatusOK {
				t.Fatalf("E = %d: %d %s", slots-1, w.Code, w.Body.String())
			}
			best = min(best, m1.TotalAlloc-m0.TotalAlloc)
		}
		return best
	}
	many, one := alloc(2), alloc(257)
	per := float64(many-one) / 255
	t.Logf("FOLGEE wire call: %d B at 256 calls, %d B at one, %.0f B per further call, booked %d", many, one, per, oai.EmbedWireCallBytes)
	if per > float64(oai.EmbedWireCallBytes) || per <= float64(oai.EmbedWireCallBytes-4<<10) {
		t.Errorf("a wire call allocates %.0f B, the route books %d B (want at most that and more than one 4-KiB step below)", per, oai.EmbedWireCallBytes)
	}
}
