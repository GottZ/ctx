package oai_test

// Pins of the lead's own Fremd-Review 1 (W02-07): F-1 (no empty 200 after a
// shutdown before the plan), F-3 (an abort of the route is a cancellation,
// never a fault of a healthy row), F-5 (the relay sanitizer), F-7 (the
// unscoped operator row, the reservation of a walk without a wire attempt,
// the answer reserve, base64 inside the payload).

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/handler/v1census"
	"github.com/GottZ/ctx/internal/oai"
)

// TestShutdownBeforePlanRefusalIsNoEmpty200 (F-1): the server stops before
// the planner refuses (service_tier flex ⇒ the bulk lane, not offered yet).
// The writer stays silent on the canceled request context — the route must
// answer the shutdown itself: 503 internal_error with x-should-retry true,
// never an empty 200. Control: the same shutdown without the refusal.
func TestShutdownBeforePlanRefusalIsNoEmpty200(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	life, cancel := context.WithCancel(context.Background())
	cancel()
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 2, 1, func(r *chatRig) { r.deps.Lifecycle = life })
	for _, body := range []string{chatBody("chat", `"service_tier":"flex"`), chatBody("chat")} {
		a := rig.post(t, body)
		code, _, _ := a.envelope(t)
		if a.status != http.StatusServiceUnavailable || code != oai.CodeInternalError || a.header.Get("x-should-retry") != "true" {
			t.Errorf("%s: %d %q (x-should-retry %q), want 503 internal_error, x-should-retry true", body, a.status, a.body, a.header.Get("x-should-retry"))
		}
	}
	if n := u.hits.Load(); n != 0 {
		t.Errorf("wire hits = %d, want 0", n)
	}
}

// consecutiveFails reads a row's failure streak from the pool status.
func consecutiveFails(p *backends.Pool, name string) int {
	for _, s := range p.Status() {
		if s.Name == name {
			return s.ConsecutiveFails
		}
	}
	return -1
}

// TestWatcherAbortIsNoHealthFault (F-3): a kill switch that aborts a running
// upstream call leaves the row healthy — the abort is ClassCanceled (no
// health report), and the row's metadata.chain says canceled, not
// server_fault.
func TestWatcherAbortIsNoHealthFault(t *testing.T) {
	defer oai.SetWatchEvery(50 * time.Millisecond)()
	u := newUpstream(t, answerSGLang)
	b := newBlocker()
	defer b.open()
	u.setRespond(b.respond(answerSGLang))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	done := make(chan chatAnswer, 1)
	go func() { done <- rig.post(t, chatBody("chat")) }()
	waitEntered(t, b, 1)
	rig.setSettings(func(s *oai.Settings) { s.Enabled = false })
	if a := <-done; a.status != http.StatusNotFound {
		t.Fatalf("answer %d %s, want 404", a.status, a.body)
	}
	if n := consecutiveFails(rig.pool, "spark"); n != 0 {
		t.Errorf("consecutive_fails of the aborted row = %d, want 0 (the abort is no fault of the row)", n)
	}
	rows := rig.rows.all()
	if len(rows) != 1 || len(rows[0].Attempts) != 1 || rows[0].Attempts[0].Class != backends.ClassCanceled.String() {
		t.Fatalf("rows %+v, want one row whose attempt class is canceled", rows)
	}
}

// TestSanitizeUpstreamTextIdentities (F-5): every identity form of the pool
// snapshot is replaced in one pass, and every scrubber has a pin.
func TestSanitizeUpstreamTextIdentities(t *testing.T) {
	snap := []backends.Backend{
		{Name: "together", Host: "https://api.together.xyz/v1", Model: "meta-llama/Llama-3.3-70B-Instruct-Turbo", ProviderClass: "openrouter"},
		{Name: "a", Host: "http://10.0.0.7:30000", ModelMap: map[string]backends.ModelSpec{"chat": {Model: "qwen38-27b"}}},
	}
	for _, tc := range []struct{ name, in, mustNot, must string }{
		{"host without port and scheme", "Error from api.together.xyz: rate limited", "together.xyz", "chat"},
		{"model basename", "model Llama-3.3-70B-Instruct-Turbo is overloaded", "Llama-3.3", "model chat"},
		{"case-insensitive", "QWEN38-27B rejected the prompt", "qwen38", "chat rejected"},
		{"short row name only as a word", "Bad request: a parameter was invalid", "cchat", "Bad request: chat parameter"},
		{"provider class", "OpenRouter refused", "penrouter", "chat refused"},
		{"url", "see https://docs.example.com/limits for details", "docs.example.com", "see for details"},
		{"ipv4 with port", "upstream 192.168.1.9:8080 refused", "192.168", "upstream refused"},
		{"ipv6", "listener fe80::1ff:fe23:4567:890a refused", "fe80", "listener refused"},
		{"absolute path", "cannot open /srv/models/weights.bin", "/srv", "cannot open"},
		{"host with port without scheme", "connect 10.0.0.7:30000 failed", "10.0.0.7", "connect chat failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := oai.SanitizeUpstreamText(tc.in, "chat", snap)
			if strings.Contains(strings.ToLower(got), strings.ToLower(tc.mustNot)) || !strings.Contains(got, tc.must) {
				t.Errorf("sanitize(%q) = %q, want no %q and %q in it", tc.in, got, tc.mustNot, tc.must)
			}
		})
	}
	long := strings.Repeat("ä", 400) // 800 bytes, 2 bytes a rune
	if got := oai.SanitizeUpstreamText(long, "chat", nil); len(got) > 512 || !strings.HasPrefix(long, got) || len(got) != 512 {
		t.Errorf("cut: %d bytes, want 512 on a rune boundary", len(got))
	}
}

// TestExportFilterUnscopedOperatorRow (F-7 E2): an unscoped row (Scope "",
// shared like _global for VisibleTo) is an operator row too: a tenant alias
// whose role is not exported does not reach it.
func TestExportFilterUnscopedOperatorRow(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	row := u.row("legacy", backends.TrustFull, 10)
	row.Scope = ""
	rig := newChatRig(t, []backends.Backend{row}, 4, 1)
	a := rig.post(t, chatBody("tchat"))
	code, _, _ := a.envelope(t)
	if a.status != http.StatusServiceUnavailable || code != oai.CodeNoEligibleBackend || u.hits.Load() != 0 {
		t.Errorf("tenant alias over an unscoped row: %d %s, %d hits; want 503 no_eligible_backend, 0 hits", a.status, a.body, u.hits.Load())
	}
}

// TestWalkWithoutWireAttemptAbortsReservation (F-7 LM1): a chain whose only
// row has no model for the role ends the walk without one wire attempt —
// 503 no_eligible_backend (no upstream fault), no llmlog row, and the
// limiter's reservation is aborted exactly once (close()).
func TestWalkWithoutWireAttemptAbortsReservation(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	row := u.row("nomodel", backends.TrustFull, 10)
	row.Model = ""
	rig := newChatRig(t, []backends.Backend{row}, 4, 1)
	a := rig.post(t, chatBody("chat"))
	code, _, _ := a.envelope(t)
	if a.status != http.StatusServiceUnavailable || code != oai.CodeNoEligibleBackend {
		t.Fatalf("answer %d %s, want 503 no_eligible_backend", a.status, a.body)
	}
	if ab, se, rj := rig.limiter.aborts.Load(), rig.limiter.settles.Load(), rig.limiter.rejects.Load(); ab != 1 || se != 0 || rj != 0 {
		t.Errorf("reservation ends: abort %d settle %d reject %d, want 1/0/0", ab, se, rj)
	}
	if n := len(rig.rows.all()); n != 0 || u.hits.Load() != 0 {
		t.Errorf("rows %d, hits %d, want 0/0", n, u.hits.Load())
	}
}

// TestAnswerReserveRefusedBeforeAcquire (F-7 RS1): a budget that holds the
// request but not its answer share refuses with 503 server_overloaded
// before any acquire or wire contact (design/02 §4.2.1).
func TestAnswerReserveRefusedBeforeAcquire(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget = 2*s.MaxResponseBytes - 1 })
	rig.recorder.Take()
	a := rig.post(t, chatBody("chat"))
	code, _, _ := a.envelope(t)
	if a.status != http.StatusServiceUnavailable || code != oai.CodeServerOverloaded {
		t.Fatalf("answer %d %s, want 503 server_overloaded", a.status, a.body)
	}
	for _, e := range rig.recorder.Take() {
		if e.Kind == v1census.KindAcquire || e.Kind == v1census.KindTry {
			t.Errorf("an acquire ran before the reserve refusal: %+v", e)
		}
	}
	if u.hits.Load() != 0 {
		t.Errorf("wire hits %d, want 0", u.hits.Load())
	}
}

// TestImageBase64InvalidInsideThePayload (F-7 A5e): a character outside the
// base64 alphabet in the middle of the payload is a 400, not only one in
// the last quantum.
func TestImageBase64InvalidInsideThePayload(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	// 16 characters, no padding: the last quantum "AAAA" is valid, the
	// invalid character sits in the second one.
	payload := "AAAA" + "AA*A" + "AAAA" + "AAAA"
	body := `{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + payload + `"}}]}]}`
	a := rig.post(t, body)
	code, param, _ := a.envelope(t)
	if a.status != http.StatusBadRequest || code != oai.CodeUnsupportedValue || param != "messages[0].content[0].image_url.url" || u.hits.Load() != 0 {
		t.Errorf("answer %d %s (%d hits), want 400 unsupported_value on the url, 0 hits", a.status, a.body, u.hits.Load())
	}
}
