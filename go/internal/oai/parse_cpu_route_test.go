//go:build !race

package oai_test

// The CPU pin of the chat route (FOLGE-P hardening 3, Delta-Review 3 R3-0):
// one request of 1 MiB whose two top-level keys share a prefix of invalid
// UTF-8 is refused within routeCPURatioMax × the base parse of the same
// body. Hardening 2's run copy made this request quadratic in its prefix —
// 8.2 s over the route where the parse before it took 43.6 ms, 11 min at
// 8 MiB — a CPU lever for every consumer with a valid key, below every
// budget bound (the reservation books memory, not time). Outside -race and
// coverage builds like the parse's CPU gate (parse_cpu_gate_test.go): both
// weigh the byte decoder, not the base parse.

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

// routeCPURatioMax bounds the route's wall time for the request against the
// base parse of its body (the minimum of the rounds each). Measured (bau
// report FOLGE-P §Härtung 3): 0.88–1.26 on a quiet host (30 runs), 146 and
// 180 with hardening 2's run copy.
const routeCPURatioMax = 3

func TestRefusalOfASharedInvalidPrefixIsLinearOverTheRoute(t *testing.T) {
	if mode := testing.CoverMode(); mode != "" {
		t.Skipf("coverage instrumentation (-covermode=%s) weighs the parse, not the base parse in the test files — the pin runs in the CI integration job without coverage (push, pull request)", mode)
	}
	up := newMemEngine(t, false)
	rig := newChatRig(t, []backends.Backend{up.row("good", 10)}, 4, 1)
	rig.reg.reload(t, chatAliases())
	rig.setSettings(func(s *oai.Settings) {
		s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes = memBudget, memCap, memRespCap
	})
	var held heldSum
	rig.router = memMount(rig, oai.ChatCompletions(rig.deps), &held)
	const size = 1 << 20
	pre := `{"model":"chat","messages":[{"role":"user","content":"x"}],"`
	// a lone 0xff (the reviewer's form) and a valid rune before it (the one
	// an early exit at an invalid first rune would leave quadratic)
	for _, unit := range []string{"\xff", "é\xff"} {
		p := strings.Repeat(unit, (size-len(pre)-16)/2/len(unit))
		body := []byte(pre + p + `1":0,"` + p + `2":0}`)
		wall, base := time.Duration(1<<62), time.Duration(1<<62)
		for range 3 {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			t0 := time.Now()
			rig.router.ServeHTTP(w, req)
			wall = min(wall, time.Since(t0))
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"unsupported_parameter"`) {
				t.Fatalf("unit %q: status %d %.300s, want 400 unsupported_parameter", unit, w.Code, w.Body.String())
			}
			t0 = time.Now()
			oai.BaseParse(body)
			base = min(base, time.Since(t0))
		}
		ratio := float64(wall) / float64(max(base, 1))
		t.Logf("FOLGEP cpu route unit=%q bytes=%d wall=%v base=%v ratio=%.2f", unit, len(body), wall, base, ratio)
		if ratio > routeCPURatioMax {
			t.Errorf("unit %q: the route refuses %d B in %v, %.1f × the base parse's %v (at most %d ×)", unit, len(body), wall, ratio, base, routeCPURatioMax)
		}
	}
}
