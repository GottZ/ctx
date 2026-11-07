package oai_test

// Hardening 5, B-2 (Copilot on PR #155): the configuration's budget floor
// (config/validate.go V39) promises one request at every cap. The route
// books the JSON check's nesting state before it runs (chat.go serve,
// jsonDepth × 24 B), up to the decoder's depth limit, on top of
// the request share — a request at the cap with the deepest nesting the
// route accepts, to an alias below credentials, must be served with the
// budget exactly at the floor. Below a body of 120 012 B the request share's
// rest (2 × the body) cannot hold the state; at any cap a rewritten message
// array spends that rest (TestFloorNeedOfTheDeepestForms measures both), so
// the floor counts the state at every cap.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// chatFloor is the floor of config/validate.go gatewayBudgetFloor for a
// chat request (its largest response share is 2 × max_response_bytes here);
// the red run against 22bfacae used it without oai.JSONStateReserve, the
// floor as it was, the red run of hardening 7 without oai.ScanDepthReserve
// (the scan's nesting state, booked before the scan), and the red run of
// Nachtrag C-1 without chunkedReadTerm.
func chatFloor(maxRequest, maxResponse int64) int64 {
	return (oai.RequestReserveFactor+oai.ScanReserveFactor)*maxRequest + chunkedReadTerm(maxRequest) + oai.ScanReserveBase + oai.ScanDepthReserve + oai.JSONStateReserve + 2*maxResponse
}

// chunkedReadTerm is what the read of a body of n bytes without a declared
// length spends beyond a declared read — hand-computed here, n + min(n +
// 512, 1 MiB) (oai.ChunkedReadReserve, pinned against the reader by
// TestChunkedReadReserveIsTheRead).
func chunkedReadTerm(n int64) int64 { return n + min(n+512, 1<<20) }

// The sums of the read's chunks (512 B doubling to 1 MiB) where the read of
// a body without a declared length spends the most: 512 × (2^6 − 1) and
// 512 × (2^11 − 1) — the read allocates one more, empty chunk there.
const (
	chunkSum6  = 32256
	chunkSum11 = 1048064
)

// postUndeclared sends body to the rig's router without a declared length
// (the body readRequestBody reads in chunks).
func postUndeclared(rig *chatRig, body string) chatAnswer {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.ContentLength = -1
	req.Body = io.NopCloser(struct{ io.Reader }{strings.NewReader(body)})
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.router.ServeHTTP(w, req)
	return chatAnswer{status: w.Code, header: w.Header(), body: w.Body.Bytes()}
}

// postAs sends body with or without a declared length.
func postAs(t *testing.T, rig *chatRig, body string, undeclared bool) chatAnswer {
	t.Helper()
	if undeclared {
		return postUndeclared(rig, body)
	}
	return rig.post(t, body)
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

// deepestRaceCapMax is the largest cap the two deepest-request route pins
// run under the race detector (FOLGE-P hardening 8, Delta-Review 6 L-4):
// their 8 MiB rows took 637 s and 245 s of `go test -short -race` (load
// 21–35; here, at load 11–13, the two tests 370 s and 161 s with them, 45 s
// and 20 s without them), and the CI's unit job gives each package 5
// minutes (-race -timeout=5m). Each pin's statement is the same at every
// cap; the 8 MiB rows run in every build without -race — the plain
// `go test` and the CI's integration job
// (TestDeepestCapsWithoutRaceIncludeTheLargest pins it).
const deepestRaceCapMax = 1 << 20

// deepestCaps are the caps of the deepest-request route pins: all of them,
// under the race detector those up to deepestRaceCapMax.
func deepestCaps(caps ...int64) []int64 {
	if !raceEnabled {
		return caps
	}
	return slices.DeleteFunc(slices.Clone(caps), func(c int64) bool { return c > deepestRaceCapMax })
}

// deepestUndeclaredCaps are the caps at which the deepest-request route pins
// send the body without a declared length (Nachtrag C-1): the sums of the
// read's chunks, where the read spends the floor's read term exactly — in
// the doubling range and at its end. Under the race detector only the first:
// the 1 MiB rows cost about 8 s a post there, and the two pins' rows without
// a declared length at every cap added 89 s to the package (154.9 s for the
// two pins against 65.6 s, load 9–14), against the CI unit job's 5 minutes
// per package. TestChunkedReadReserveIsTheRead pins the term against the
// reader at every chunk sum up to 9 MiB in every build.
func deepestUndeclaredCaps() []int64 {
	if raceEnabled {
		return []int64{chunkSum6}
	}
	return []int64{chunkSum6, chunkSum11}
}

// TestDeepestCapsWithoutRaceIncludeTheLargest pins the race guard of the
// deepest-request route pins: raceEnabled tells the truth (the build's
// -race setting), and only a race build leaves out the caps above
// deepestRaceCapMax — a guard active in every build would leave the 8 MiB
// rows pinned nowhere.
func TestDeepestCapsWithoutRaceIncludeTheLargest(t *testing.T) {
	race := false
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "-race" {
				race = s.Value == "true"
			}
		}
	}
	if race != raceEnabled {
		t.Fatalf("raceEnabled is %v, the build's -race setting says %v", raceEnabled, race)
	}
	all := []int64{32 << 10, 64 << 10, 128 << 10, 1 << 20, 8 << 20}
	got := deepestCaps(all...)
	want := all
	if raceEnabled {
		want = all[:4]
	}
	if !slices.Equal(got, want) {
		t.Errorf("race build %v: the deepest-request pins run the caps %v, want %v", raceEnabled, got, want)
	}
	undeclared, wantUndeclared := deepestUndeclaredCaps(), []int64{chunkSum6, chunkSum11}
	if raceEnabled {
		wantUndeclared = wantUndeclared[:1]
	}
	if !slices.Equal(undeclared, wantUndeclared) {
		t.Errorf("race build %v: the deepest-request pins send without a declared length at %v, want %v", raceEnabled, undeclared, wantUndeclared)
	}
}

// The deepest request at the cap is served at a budget equal to the floor,
// with a declared length and without one (Nachtrag C-1: read in chunks, it
// was 503 at every cap from 32 KiB to 8 MiB — the read spent the request
// share, the upstream body booked past it); without one at the sums of the
// read's chunks, where the read spends the most (deepestUndeclaredCaps).
func TestDeepestRequestAtTheCapFitsTheFloor(t *testing.T) {
	const resp = 4 << 20
	type row struct {
		maxRequest int64
		undeclared bool
	}
	var rows []row
	for _, c := range deepestCaps(32<<10, 64<<10, 128<<10, 1<<20, 8<<20) {
		rows = append(rows, row{c, false})
	}
	for _, c := range deepestUndeclaredCaps() {
		rows = append(rows, row{c, true})
	}
	for _, r := range rows {
		maxRequest, undeclared := r.maxRequest, r.undeclared
		t.Run(fmt.Sprintf("cap=%d/undeclared=%v", maxRequest, undeclared), func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
			body := deepestBody(int(maxRequest), "pub", "user")
			floor := chatFloor(maxRequest, resp)
			for _, budget := range []int64{10 * floor, floor} {
				rig.setSettings(func(s *oai.Settings) {
					s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes = budget, maxRequest, resp
				})
				a := postAs(t, rig, body, undeclared)
				t.Logf("FOLGEP floor cap=%d undeclared=%v body=%d budget=%d floor=%d status=%d", maxRequest, undeclared, len(body), budget, floor, a.status)
				if a.status != http.StatusOK {
					t.Errorf("cap %d, undeclared %v, budget %d (floor %d): %d %s, want 200", maxRequest, undeclared, budget, floor, a.status, a.body)
				}
			}
		})
	}
}

// TestDeepestDeveloperRequestLacksOnlyTheUpstreamFrame (hardening 6, P-5;
// Delta-Review 5 L-2; hardening 7; Nachtrag C-1) holds what the floor's
// comment and docs say: the floor does not hold the upstream frame of the
// backend row (the upstream body's bytes besides the rewritten array) of the
// deepest request at the cap whose message array is rewritten and whose body
// comes without a declared length at a sum of the read's chunks — where the
// read spends exactly the floor's read term — beyond the little the floor
// has to spare there: it counts both nesting states one level past the
// decoder's limit (24 + 104 B), and the request is nested at most to it.
// With the test row's 12-byte model name (frame 114 B) the request is served
// at a budget equal to the floor (73 B to spare, TestFloorNeedOfTheDeepestForms);
// with a 212-byte name (frame 314 B) it is 503 at the floor and 200 at the
// floor plus that frame. With a declared length the read term, which such a
// read does not spend, holds the frame: 200 at the floor with either name
// (until Nachtrag C-1 this was the 503 case, at every cap).
//
// RC-D C8: the floor now carries oai.UpstreamFrameReserve (config V39: the
// largest route need + the reserve), so at the floor the 212-byte name is
// served as well — the pin keeps the old floor (chatFloor, the chat need
// alone) as the case that shows what the reserve holds: 503 there with the
// 212-byte name, 200 at chatFloor + the reserve with either name and either
// read.
func TestDeepestDeveloperRequestLacksOnlyTheUpstreamFrame(t *testing.T) {
	const resp = 4 << 20
	type row struct {
		maxRequest int64
		undeclared bool
	}
	var rows []row
	for _, c := range deepestCaps(32<<10, 128<<10, 1<<20, 8<<20) {
		rows = append(rows, row{c, false})
	}
	for _, c := range deepestUndeclaredCaps() {
		rows = append(rows, row{c, true})
	}
	for _, r := range rows {
		maxRequest, undeclared := r.maxRequest, r.undeclared
		for _, pad := range []int{0, 200} {
			t.Run(fmt.Sprintf("cap=%d/undeclared=%v/model_pad=%d", maxRequest, undeclared, pad), func(t *testing.T) {
				u := newUpstream(t, answerSGLang)
				row := u.row("spark", backends.TrustFull, 10)
				row.Model += strings.Repeat("M", pad)
				rig := newChatRig(t, []backends.Backend{row}, 4, 1)
				body := deepestBody(int(maxRequest), "pub", "developer")
				floor := chatFloor(maxRequest, resp)
				post := func(budget int64) chatAnswer {
					rig.setSettings(func(s *oai.Settings) {
						s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes = budget, maxRequest, resp
					})
					return postAs(t, rig, body, undeclared)
				}
				if a := post(10 * floor); a.status != http.StatusOK {
					t.Fatalf("budget 10 × floor: %d %s", a.status, a.body)
				}
				u.mu.Lock()
				upstream := len(u.raws[len(u.raws)-1])
				u.mu.Unlock()
				// The rewritten array: the client's, "developer" → "system".
				rewritten := len(body) - len(`{"model":"pub","messages":`) - len(`}`) - (len("developer") - len("system"))
				frame := int64(upstream - rewritten)
				a := post(floor)
				t.Logf("FOLGEP floor developer cap=%d undeclared=%v model_pad=%d body=%d upstream=%d frame=%d floor=%d status=%d", maxRequest, undeclared, pad, len(body), upstream, frame, floor, a.status)
				if a := post(floor + oai.UpstreamFrameReserve); a.status != http.StatusOK {
					t.Errorf("budget = floor %d + oai.UpstreamFrameReserve, frame %d, undeclared %v: %d %s, want 200 (the reserve holds the row's frame)", floor, frame, undeclared, a.status, a.body)
				}
				if pad == 0 || !undeclared {
					if a.status != http.StatusOK {
						t.Errorf("budget = floor %d, frame %d, undeclared %v: %d %s, want 200 (the floor's slack at the depth limit, or a declared body's unspent read term, holds this frame)", floor, frame, undeclared, a.status, a.body)
					}
					return
				}
				code, _, _ := a.envelope(t)
				if a.status != http.StatusServiceUnavailable || code != oai.CodeServerOverloaded {
					t.Errorf("budget = floor %d, frame %d: %d %s, want 503 server_overloaded (the chat need alone lacks the upstream frame)", floor, frame, a.status, a.body)
				}
				if a := post(floor + frame); a.status != http.StatusOK {
					t.Errorf("budget = floor %d + frame %d: %d %s, want 200", floor, frame, a.status, a.body)
				}
			})
		}
	}
}

// TestFloorNeedOfTheDeepestForms is the measurement behind the floor's
// nesting term (CTX_FOLGEP_FLOOR_PROBE=1): the least budget that serves one
// request at the cap, against the floor — deepest and flat, with and
// without a developer message (whose rewritten array spends the request
// share's rest), with and without a declared length (Nachtrag C-1). Report
// §Härtung 5 B-2, §Nachtrag.
func TestFloorNeedOfTheDeepestForms(t *testing.T) {
	if os.Getenv("CTX_FOLGEP_FLOOR_PROBE") == "" {
		t.Skip("CTX_FOLGEP_FLOOR_PROBE not set (measurement test)")
	}
	const resp = 4 << 20
	flat := func(size int, role string) string {
		pre, suf := `{"model":"pub","messages":[{"role":"`+role+`","content":"`, `"}]}`
		return pre + strings.Repeat("a", size-len(pre)-len(suf)) + suf
	}
	for _, maxRequest := range []int64{chunkSum6, 32 << 10, 128 << 10, chunkSum11, 1 << 20, 8 << 20} {
		for _, f := range []struct {
			name, body string
			undeclared bool
		}{
			{"deepest user", deepestBody(int(maxRequest), "pub", "user"), false},
			{"deepest developer", deepestBody(int(maxRequest), "pub", "developer"), false},
			{"flat developer", flat(int(maxRequest), "developer"), false},
			{"flat user", flat(int(maxRequest), "user"), false},
			{"deepest user, undeclared", deepestBody(int(maxRequest), "pub", "user"), true},
			{"deepest developer, undeclared", deepestBody(int(maxRequest), "pub", "developer"), true},
			{"flat developer, undeclared", flat(int(maxRequest), "developer"), true},
			{"flat user, undeclared", flat(int(maxRequest), "user"), true},
		} {
			u := newUpstream(t, answerSGLang)
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
			floor := chatFloor(maxRequest, resp)
			served := func(budget int64) bool {
				rig.setSettings(func(s *oai.Settings) {
					s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes = budget, maxRequest, resp
				})
				return postAs(t, rig, f.body, f.undeclared).status == http.StatusOK
			}
			lo, hi := floor-6*maxRequest-2<<20, floor+maxRequest+1<<20
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
