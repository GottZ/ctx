package oai_test

// W06-18 warning header at the chat route (RC-D cluster C2): with
// gateway.soft_ratio 0.5 and a consumer day budget of 2000 tokens, an
// admitted chat request reserving 1042 tokens (52 %) answers 200 with
// x-ctx-quota-warning "daily_tokens;pct=52"; at ratio 0.9 the same request
// answers without it; an upstream error after the admission carries it
// never (the header rides with x-ratelimit-* on success answers only); the
// body is never touched.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/oai"
)

func TestQuotaWarningHeaderOnSuccessOnly(t *testing.T) {
	run := func(ratio float64, fail bool) *httptest.ResponseRecorder {
		t.Helper()
		u := newUpstream(t, answerSGLang)
		lim := keyquota.Limits{Concurrency: noL1, RPM: 1 << 20, TPM: 60_000_000, CompletionReserve: 1024,
			Consumer: keyquota.Budget{DailyTokens: 2000}, SoftRatio: ratio}
		meter := newRigMeter(lim, true)
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 64, 1, withDeps(func(d *oai.Deps) {
			d.Limiter = meter
		}))
		if fail {
			u.setRespond(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"engine down"}}`)
			})
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody("chat")))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		rig.router.ServeHTTP(w, req)
		return w
	}

	w := run(0.5, false)
	if w.Code != http.StatusOK {
		t.Fatalf("success at ratio 0.5: %d %s", w.Code, w.Body.Bytes())
	}
	if got := w.Result().Header.Get(keyquota.QuotaWarningHeader); got != "daily_tokens;pct=52" {
		t.Errorf("W06-18 gate (warning header over the share): %s = %q, want \"daily_tokens;pct=52\" (1042 of 2000 at ratio 0.5)",
			keyquota.QuotaWarningHeader, got)
	}
	if strings.Contains(w.Body.String(), "quota") {
		t.Errorf("W06-18 gate (no body change): the answer's body mentions the quota: %s", w.Body.Bytes())
	}

	if w := run(0.9, false); w.Code != http.StatusOK || w.Result().Header.Get(keyquota.QuotaWarningHeader) != "" {
		t.Errorf("W06-18 gate (below the share): %d, %s = %q, want 200 without the header", w.Code,
			keyquota.QuotaWarningHeader, w.Result().Header.Get(keyquota.QuotaWarningHeader))
	}
	if w := run(0.5, true); w.Code == http.StatusOK || w.Result().Header.Get(keyquota.QuotaWarningHeader) != "" {
		t.Errorf("W06-18 gate (never on an error answer): %d, %s = %q, want an error without the header", w.Code,
			keyquota.QuotaWarningHeader, w.Result().Header.Get(keyquota.QuotaWarningHeader))
	}
}
