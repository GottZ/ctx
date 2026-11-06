//go:build !race

package oai_test

// The CPU pin of the embeddings route (W02-09 rebase round 2, part A; the
// chat route's is parse_cpu_route_test.go): one request of 1 MiB whose two
// top-level keys share a prefix of invalid UTF-8 — the shape Delta-Review 3
// R3-0 made quadratic in FOLGE-P's cmpKeys, which the embeddings parse
// compares with since the port — is refused within routeCPURatioMax × the
// parse before the port of the same body. Outside -race and coverage builds
// like the chat pin.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

func TestEmbedRefusalOfASharedInvalidPrefixIsLinearOverTheRoute(t *testing.T) {
	if mode := testing.CoverMode(); mode != "" {
		t.Skipf("coverage instrumentation (-covermode=%s) weighs the parse, not the base parse in the test files — the pin runs in the CI integration job without coverage (push, pull request)", mode)
	}
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 33, 1)
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget = parseMemBudget; s.MaxRequestBytes = parseMemBodyCap })
	const size = 1 << 20
	pre := `{"model":"emb","input":"x","`
	for _, unit := range []string{"\xff", "é\xff"} {
		p := strings.Repeat(unit, (size-len(pre)-16)/2/len(unit))
		body := []byte(pre + p + `1":0,"` + p + `2":0}`)
		wall, base := time.Duration(1<<62), time.Duration(1<<62)
		for range 3 {
			req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			t0 := time.Now()
			rig.router.ServeHTTP(w, req)
			wall = min(wall, time.Since(t0))
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"unsupported_parameter"`) {
				t.Fatalf("unit %q: status %d %.300s, want 400 unsupported_parameter", unit, w.Code, w.Body.String())
			}
			t0 = time.Now()
			oai.BaseEmbedTop(body)
			base = min(base, time.Since(t0))
		}
		ratio := float64(wall) / float64(max(base, 1))
		t.Logf("W0209 cpu route unit=%q bytes=%d wall=%v base=%v ratio=%.2f", unit, len(body), wall, base, ratio)
		if ratio > routeCPURatioMax {
			t.Errorf("unit %q: the route refuses %d B in %v, %.1f × the base parse's %v (at most %d ×)", unit, len(body), wall, ratio, base, routeCPURatioMax)
		}
	}
}
