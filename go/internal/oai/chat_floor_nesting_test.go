package oai_test

// Hardening 5, B-2 (Copilot on PR #155): the configuration's budget floor
// (config/validate.go V39) promises one request at every cap. The route
// books the JSON check's nesting state before it runs (chat.go
// c.cover(jsonStateBytes(body))), up to the decoder's depth limit, on top of
// the request share — a request at the cap with the deepest nesting the
// route accepts, to an alias below credentials, must be served with the
// budget exactly at the floor. Below a body of 120 012 B the request share's
// rest (2 × the body) cannot hold the state; at any cap a rewritten message
// array spends that rest (TestFloorNeedOfTheDeepestForms measures both), so
// the floor counts the state at every cap.

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// chatFloor is the floor of config/validate.go gatewayBudgetFloor for a
// chat request (its largest response share is 2 × max_response_bytes here);
// the red run against 22bfacae used it without oai.JSONStateReserve, the
// floor as it was.
func chatFloor(maxRequest, maxResponse int64) int64 {
	return (oai.RequestReserveFactor+oai.ScanReserveFactor)*maxRequest + oai.ScanReserveBase + oai.JSONStateReserve + 2*maxResponse
}

// deepestBody is a body of exactly size bytes to model whose nesting is the
// deepest the route accepts (the decoder's limit, 10 000: object, messages,
// message, tool_calls and 9 996 arrays), its first message of role role.
func deepestBody(size int, model, role string) string {
	nest := strings.Repeat("[", 9996) + strings.Repeat("]", 9996)
	pre := `{"model":"` + model + `","messages":[{"role":"` + role + `","content":"`
	suf := `"},{"role":"assistant","content":null,"tool_calls":[` + nest + `]}]}`
	return pre + strings.Repeat("a", size-len(pre)-len(suf)) + suf
}

func TestDeepestRequestAtTheCapFitsTheFloor(t *testing.T) {
	const resp = 4 << 20
	for _, maxRequest := range []int64{32 << 10, 64 << 10, 128 << 10, 1 << 20, 8 << 20} {
		t.Run(fmt.Sprintf("cap=%d", maxRequest), func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
			body := deepestBody(int(maxRequest), "pub", "user")
			floor := chatFloor(maxRequest, resp)
			for _, budget := range []int64{10 * floor, floor} {
				rig.setSettings(func(s *oai.Settings) {
					s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes = budget, maxRequest, resp
				})
				a := rig.post(t, body)
				t.Logf("FOLGEP floor cap=%d body=%d budget=%d floor=%d status=%d", maxRequest, len(body), budget, floor, a.status)
				if a.status != http.StatusOK {
					t.Errorf("cap %d, budget %d (floor %d): %d %s, want 200", maxRequest, budget, floor, a.status, a.body)
				}
			}
		})
	}
}

// TestFloorNeedOfTheDeepestForms is the measurement behind the floor's
// nesting term (CTX_FOLGEP_FLOOR_PROBE=1): the least budget that serves one
// request at the cap, against the floor — deepest and flat, with and
// without a developer message (whose rewritten array spends the request
// share's rest). Report §Härtung 5 B-2.
func TestFloorNeedOfTheDeepestForms(t *testing.T) {
	if os.Getenv("CTX_FOLGEP_FLOOR_PROBE") == "" {
		t.Skip("CTX_FOLGEP_FLOOR_PROBE not set (measurement test)")
	}
	const resp = 4 << 20
	flat := func(size int, role string) string {
		pre, suf := `{"model":"pub","messages":[{"role":"`+role+`","content":"`, `"}]}`
		return pre + strings.Repeat("a", size-len(pre)-len(suf)) + suf
	}
	for _, maxRequest := range []int64{32 << 10, 128 << 10, 1 << 20, 8 << 20} {
		for _, f := range []struct{ name, body string }{
			{"deepest user", deepestBody(int(maxRequest), "pub", "user")},
			{"deepest developer", deepestBody(int(maxRequest), "pub", "developer")},
			{"flat developer", flat(int(maxRequest), "developer")},
			{"flat user", flat(int(maxRequest), "user")},
		} {
			u := newUpstream(t, answerSGLang)
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
			floor := chatFloor(maxRequest, resp)
			served := func(budget int64) bool {
				rig.setSettings(func(s *oai.Settings) {
					s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes = budget, maxRequest, resp
				})
				return rig.post(t, f.body).status == http.StatusOK
			}
			lo, hi := floor-3*maxRequest, floor+maxRequest+1<<20
			if !served(hi) {
				t.Fatalf("%s at cap %d: not served at floor + cap + 1 MiB", f.name, maxRequest)
			}
			for lo+1 < hi {
				if m := (lo + hi) / 2; served(m) {
					hi = m
				} else {
					lo = m
				}
			}
			t.Logf("FOLGEP floor need cap=%d form=%q need=%d floor=%d need-floor=%d", maxRequest, f.name, hi, floor, hi-floor)
		}
	}
}
