package oai_test

// The booking of the sensitivity scan (FOLGE-P hardening H-8, Fremd-Review 1
// S-1): before federation.ScanRaise runs over the body of a request below
// credentials, the route books scanReserveFactor × the body + scanReserveBase
// — what the scan allocates, measured here against the constants (a
// relational pin: a form that needs more makes the booking untrue, a scan
// that needs a whole byte less per body byte makes it cost requests for
// nothing) — and past the budget it answers 503 server_overloaded without
// the scan, without the meter.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/oai"
)

// scanForms are bodies of the public alias "pub" the route accepts, at about
// lim bytes: the forms Fremd-Review 1 named (text, text with spaces, many
// messages, parts, escaped text) and the ones that cost the scan the most
// per byte — short secret-like assignments the scanner's patterns collect,
// in a text and in tool_calls, as keys, values and both, and many small
// tokens. None of them carries a credential (the scan would stop at it).
func scanForms(lim int) ([]string, map[string][]byte) {
	pre, suf := `{"model":"pub","messages":[{"role":"user","content":"`, `"}]}`
	text := func(fill string) []byte {
		return []byte(pre + strings.Repeat(fill, max(lim-len(pre)-len(suf), len(fill))/len(fill)) + suf)
	}
	tc := `{"model":"pub","messages":[{"role":"user","content":"x"},{"role":"assistant","content":null,"tool_calls":[`
	escE := fmt.Sprintf("%cu00e9", '\\')
	escA := fmt.Sprintf("%cu0061", '\\')
	f := map[string][]byte{
		"text_a_space":      text("a "),
		"text_lorem":        text("lorem ipsum "),
		"text_escapes":      text(escE),
		"messages":          memGen(`{"model":"pub","messages":[`, `]}`, lim, memConst(`{"role":"user","content":"x"}`)),
		"parts":             memGen(`{"model":"pub","messages":[{"role":"user","content":[`, `]}]}`, lim, memConst(`{"type":"text","text":"x"}`)),
		"assign_text":       text("apikey:x "),
		"assign_semi":       text("secret=x;"),
		"assign_upper":      text("APIKEY:x "),
		"assign_esc_kw":     text(escA + "pikey:x "),
		"b64_text":          text(strings.Repeat("a", 32) + " "),
		"hex_labelled":      text("sha256 " + strings.Repeat("0", 64) + " "),
		"tc_numbers":        memGen(tc, `]}]}`, lim, memConst(`1`)),
		"tc_strings":        memGen(tc, `]}]}`, lim, memConst(`"a"`)),
		"tc_assign_strs":    memGen(tc, `]}]}`, lim, memConst(`"apikey:x"`)),
		"tc_kv_assign":      memGen(tc+`{`, `}]}]}`, lim, memConst(`"apikey":1`)),
		"tc_kv_short":       memGen(tc+`{`, `}]}]}`, lim, memConst(`"a":1`)),
		"tc_kv_valassign":   memGen(tc+`{`, `}]}]}`, lim, memConst(`"a":"apikey:x"`)),
		"tc_kv_emptykey":    memGen(tc+`{`, `}]}]}`, lim, memConst(`"":"apikey:x"`)),
		"tc_kv_keyassign":   memGen(tc+`{`, `}]}]}`, lim, memConst(`"apikey:x":1`)),
		"tc_objs_valassign": memGen(tc, `]}]}`, lim, memConst(`{"a":"apikey:x"}`)),
		"msgs_assign":       memGen(`{"model":"pub","messages":[`, `]}`, lim, memConst(`{"role":"user","content":"apikey:x"}`)),
		// Delta-Review 2 F-1: one key of the whole body. With null it is
		// scanned once, as a key; with a number or a string the scan reads
		// it a second time (scanTwiceForms).
		"tc_hugekey_null": scanHugeKey(lim, "null"),
	}
	names := make([]string, 0, len(f))
	for k := range f {
		names = append(names, k)
	}
	return names, f
}

// scanTwiceForms are the forms the scan reads twice in a row (Delta-Review 2
// F-1): one key of the whole body with a scalar value — federation's
// bodyHasCredentials scans the key as a token, then the probe
// key + ": " + value (the assignment rule's view of a member, policy.go).
// The one form whose scan allocates more than one booking in total; what it
// holds at once is one scan's (TestScanTwiceHoldsOneScanAtOnce).
func scanTwiceForms(lim int) ([]string, map[string][]byte) {
	return []string{"tc_hugekey_num", "tc_hugekey_str"}, map[string][]byte{
		"tc_hugekey_num": scanHugeKey(lim, "1"),
		"tc_hugekey_str": scanHugeKey(lim, `"v"`),
	}
}

// scanHugeKey is an assistant message whose one tool call has one key of
// short secret-like assignments that fills the body to about lim, and value
// val.
func scanHugeKey(lim int, val string) []byte {
	pre := `{"model":"pub","messages":[{"role":"user","content":"x"},{"role":"assistant","content":null,"tool_calls":[{"`
	suf := `":` + val + `}]}]}`
	const fill = "apikey:x "
	return []byte(pre + strings.Repeat(fill, max(lim-len(pre)-len(suf), len(fill))/len(fill)) + suf)
}

// scanAlloc is what ScanRaise allocates over body (TotalAlloc, the minimum
// of two runs, each after two collections: the patterns' matcher pools cold
// — one collection leaves them in sync.Pool's victim cache, and a measurement
// then depends on which matchers survived there).
func scanAlloc(required backends.Sensitivity, body []byte) (alloc int64, raised backends.Sensitivity) {
	alloc = 1 << 62
	for range 2 {
		runtime.GC()
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		raised = federation.ScanRaise(required, body)
		runtime.ReadMemStats(&b)
		alloc = min(alloc, int64(b.TotalAlloc-a.TotalAlloc))
	}
	return alloc, raised
}

// TestScanAtCredentialsAllocatesNothing pins the short circuit the route's
// booking relies on: at credentials (and at an unknown requirement, which
// ranks as credentials) ScanRaise returns without scanning.
func TestScanAtCredentialsAllocatesNothing(t *testing.T) {
	_, forms := scanForms(256 << 10)
	for _, req := range []backends.Sensitivity{backends.SensCredentials, ""} {
		if alloc, raised := scanAlloc(req, forms["assign_text"]); alloc != 0 || raised != backends.SensCredentials {
			t.Errorf("ScanRaise(%q) allocates %d bytes and answers %q, want 0 and credentials", req, alloc, raised)
		}
	}
}

// TestScanIsBookedBeforeItRuns: a budget that holds the door's share, the
// answer share and the upstream body but not the scan's booking is 503
// server_overloaded for the public alias — before the scan and before the
// meter (no Admit), no wire call — and 200 for the credentials alias, which
// books no scan; with room, the request share held the booking.
func TestScanIsBookedBeforeItRuns(t *testing.T) {
	_, forms := scanForms(256 << 10)
	pub := bytes.Replace(forms["text_a_space"], []byte(`"content":"`), []byte(`"content":"`+ledgerMark), 1)
	cred := bytes.Replace(pub, []byte(`"model":"pub"`), []byte(`"model":"chat"`), 1)
	answerShare := 2 * chatSettings().MaxResponseBytes
	tight := 3*int64(len(pub)) + answerShare + int64(len(pub)) + 64<<10 // door + answer + upstream + frame
	for _, tc := range []struct {
		name   string
		body   []byte
		budget int64
		status int
		scans  int64
	}{
		{"public alias, no room for the scan", pub, tight, http.StatusServiceUnavailable, 0},
		{"credentials alias, the same budget", cred, tight, http.StatusOK, 1},
		{"public alias with room", pub, 256 << 20, http.StatusOK, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
			rig.setSettings(func(s *oai.Settings) { s.InflightBudget = tc.budget })
			var scans atomic.Int64
			scan := rig.deps.Federation.Scan
			rig.deps.Federation.Scan = func(r backends.Sensitivity, b []byte) backends.Sensitivity {
				scans.Add(1)
				return scan(r, b)
			}
			w, held := ledgerPost(t, rig, tc.body, false)
			if w.Code != tc.status {
				t.Fatalf("answer %d %.300s, want %d", w.Code, w.Body.Bytes(), tc.status)
			}
			if got := scans.Load(); got != tc.scans {
				t.Errorf("%d scans, want %d", got, tc.scans)
			}
			if tc.status != http.StatusOK {
				var e struct {
					Error struct{ Code string } `json:"error"`
				}
				_ = json.Unmarshal(w.Body.Bytes(), &e)
				if e.Error.Code != oai.CodeServerOverloaded || w.Header().Get("Retry-After") != "1" {
					t.Errorf("answer %s (Retry-After %q), want server_overloaded, Retry-After 1", w.Body.Bytes(), w.Header().Get("Retry-After"))
				}
				if n := rig.limiter.admits.Load(); n != 0 || len(ownRaws(u)) != 0 {
					t.Errorf("%d meter admits, %d wire hits, want none", n, len(ownRaws(u)))
				}
			}
			if tc.name == "public alias with room" {
				want := int64(len(pub)) + oai.ScanReserveFactor*int64(len(pub)) + oai.ScanReserveBase // the read + the scan's booking
				for _, raw := range ownRaws(u) {
					want += int64(len(raw))
				}
				if got := held - answerShare; got < want {
					t.Errorf("the request share held %d bytes at the end, want ≥ %d (read + scan + upstream)", got, want)
				}
			}
			if rig.budget.InUse() != 0 {
				t.Errorf("the budget holds %d bytes after the request", rig.budget.InUse())
			}
		})
	}
}

// TestScanDepthIsBookedBeforeTheScan (hardening 7, S-H6-1): the scan's
// booking includes its nesting state, scanDepthStateBytes per level of the
// body, and the route takes it before the scan runs. The deepest request
// the route accepts at a 32 KiB cap to the public alias is served at the
// floor; at the floor without the scan's nesting term it is 503
// server_overloaded — before the scan, before the meter, no wire call
// (without the term in the booking it was served there: the floor of
// hardening 5 had 32 704 B to spare for it, the floor with the read term of
// Nachtrag C-1 98 856 B; the term takes 1 040 000).
func TestScanDepthIsBookedBeforeTheScan(t *testing.T) {
	const capBytes, resp = 32 << 10, 4 << 20
	body := strings.Replace(deepestBody(capBytes, "pub", "user"), `"content":"`+strings.Repeat("a", len(ledgerMark)), `"content":"`+ledgerMark, 1)
	floor := chatFloor(capBytes, resp)
	for _, tc := range []struct {
		name   string
		budget int64
		status int
		scans  int64
	}{
		{"floor without the scan's nesting term", floor - oai.ScanDepthReserve, http.StatusServiceUnavailable, 0},
		{"floor", floor, http.StatusOK, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
			rig.setSettings(func(s *oai.Settings) {
				s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes = tc.budget, capBytes, resp
			})
			var scans atomic.Int64
			scan := rig.deps.Federation.Scan
			rig.deps.Federation.Scan = func(r backends.Sensitivity, b []byte) backends.Sensitivity {
				scans.Add(1)
				return scan(r, b)
			}
			w, _ := ledgerPost(t, rig, []byte(body), false)
			if w.Code != tc.status {
				t.Fatalf("budget %d (floor %d): %d %.300s, want %d", tc.budget, floor, w.Code, w.Body.Bytes(), tc.status)
			}
			if got := scans.Load(); got != tc.scans {
				t.Errorf("%d scans, want %d", got, tc.scans)
			}
			if tc.status != http.StatusOK {
				var e struct {
					Error struct{ Code string } `json:"error"`
				}
				_ = json.Unmarshal(w.Body.Bytes(), &e)
				if e.Error.Code != oai.CodeServerOverloaded || w.Header().Get("Retry-After") != "1" {
					t.Errorf("answer %s (Retry-After %q), want server_overloaded, Retry-After 1", w.Body.Bytes(), w.Header().Get("Retry-After"))
				}
				if n := rig.limiter.admits.Load(); n != 0 || len(ownRaws(u)) != 0 {
					t.Errorf("%d meter admits, %d wire hits, want none", n, len(ownRaws(u)))
				}
			}
		})
	}
}
