package backends

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/httpx"
)

// ctxOverrides are the K15 entries of the ctx column (masterplan K15,
// design/04 R-02-16, design/06 K-06-26); every other code of the ctx column
// equals the generic column.
var ctxOverrides = map[string]ErrClass{
	"server_busy":                ClassBusy,
	"concurrency_limit_exceeded": ClassBusy,
	"upstream_timeout":           ClassTimeout,
	"insufficient_quota":         ClassQuotaExhausted,
}

// envelope renders a ctx gateway error body (internal/oai wire form).
func envelope(code string) string {
	return fmt.Sprintf(`{"error":{"message":"m","type":"t","param":null,"code":%q}}`, code)
}

// TestClassifyGatewayCodesCtx: the ctx column, one case per code — read from
// an error event inside a 200 stream.
func TestClassifyGatewayCodesCtx(t *testing.T) {
	for _, c := range gatewayGenericCases {
		want := c.want
		if o, ok := ctxOverrides[c.code]; ok {
			want = o
		}
		if got := Classify(&testStreamErr{Code: c.code}, ProviderCtx); got != want {
			t.Errorf("ctx mid-stream %q: Classify = %s, want %s", c.code, got, want)
		}
	}
	// The masterplan gate verbatim: same event, two provider classes.
	if got := Classify(&testStreamErr{Code: "server_busy"}, ProviderCtx); got != ClassBusy {
		t.Errorf("ctx server_busy = %s, want busy", got)
	}
	if got := Classify(&testStreamErr{Code: "server_busy"}, ProviderGeneric); got != ClassRateLimit {
		t.Errorf("generic server_busy = %s, want rate_limit", got)
	}
}

// TestClassifyCtxStatusBody: on a ctx row the code is also read from the JSON
// body of a non-200 answer and wins over the status (precedence as the
// OpenRouter no-providers body); every other provider class keeps the status
// mapping byte for byte.
func TestClassifyCtxStatusBody(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		provider string
		want     ErrClass
	}{
		{"429 server_busy from ctx", statusErr(429, envelope("server_busy")), ProviderCtx, ClassBusy},
		{"429 server_busy from generic", statusErr(429, envelope("server_busy")), ProviderGeneric, ClassRateLimit},
		{"429 concurrency_limit_exceeded from ctx", statusErr(429, envelope("concurrency_limit_exceeded")), ProviderCtx, ClassBusy},
		{"504 upstream_timeout from ctx", statusErr(504, envelope("upstream_timeout")), ProviderCtx, ClassTimeout},
		{"504 upstream_timeout from generic", statusErr(504, envelope("upstream_timeout")), ProviderGeneric, ClassGateway},
		{"429 insufficient_quota from ctx", statusErr(429, envelope("insufficient_quota")), ProviderCtx, ClassQuotaExhausted},
		{"429 insufficient_quota from openrouter", statusErr(429, envelope("insufficient_quota")), ProviderOpenRouter, ClassRateLimit},
		{"429 rate_limit_exceeded from ctx", statusErr(429, envelope("rate_limit_exceeded")), ProviderCtx, ClassRateLimit},
		{"503 internal_error from ctx", statusErr(503, envelope("internal_error")), ProviderCtx, ClassGateway},
		{"500 upstream_error from ctx", statusErr(500, envelope("upstream_error")), ProviderCtx, ClassServerFault},
		{"401 invalid_api_key from ctx", statusErr(401, envelope("invalid_api_key")), ProviderCtx, ClassAuth},
		{"404 model_not_found from ctx", statusErr(404, envelope("model_not_found")), ProviderCtx, ClassBadRequest},
		// No readable code ⇒ the status mapping, on ctx rows too.
		{"ctx 504 without body", statusErr(504, ""), ProviderCtx, ClassGateway},
		{"ctx 429 plain text", statusErr(429, "server busy"), ProviderCtx, ClassRateLimit},
		{"ctx 429 house format", statusErr(429, `{"error":"server busy — retry shortly"}`), ProviderCtx, ClassRateLimit},
		{"ctx 429 code null", statusErr(429, `{"error":{"message":"m","code":null}}`), ProviderCtx, ClassRateLimit},
		{"ctx 429 numeric code", statusErr(429, `{"error":{"code":429}}`), ProviderCtx, ClassRateLimit},
		{"ctx 429 unknown code", statusErr(429, envelope("teapot")), ProviderCtx, ClassRateLimit},
		{"ctx 429 truncated body", statusErr(429, `{"error":{"code":"server_bu`), ProviderCtx, ClassRateLimit},
		{"ctx 502 html", statusErr(502, "<html>bad gateway</html>"), ProviderCtx, ClassGateway},
		// The context errors keep their precedence on ctx rows.
		{"ctx canceled", fmt.Errorf("x: %w", context.Canceled), ProviderCtx, ClassCanceled},
		{"ctx attempt deadline", context.DeadlineExceeded, ProviderCtx, ClassTimeout},
	}
	for _, c := range cases {
		if got := Classify(c.err, c.provider); got != c.want {
			t.Errorf("%s: Classify = %s, want %s", c.name, got, c.want)
		}
	}
}

// TestQuotaCooldownAfterThreeFailures: A's insufficient_quota with
// Retry-After 6 h cools B's ctx row for the full 6 h — once and after three
// consecutive failures (the doubling's 10-minute cap must not shorten it).
func TestQuotaCooldownAfterThreeFailures(t *testing.T) {
	err := fmt.Errorf("llm: %w", &httpx.StatusError{Code: 429, Body: envelope("insufficient_quota"), RetryAfter: 6 * time.Hour})
	class := Classify(err, ProviderCtx)
	if class != ClassQuotaExhausted {
		t.Fatalf("Classify = %s, want quota_exhausted", class)
	}
	if d := class.Cooldown(6 * time.Hour); d != 6*time.Hour {
		t.Fatalf("Cooldown(6h) = %s, want 6h", d)
	}
	for _, n := range []int{1, 3, 5} {
		until, before, after := cooldownAfter(t, n, class, 6*time.Hour)
		if until.Before(before.Add(6*time.Hour)) || until.After(after.Add(6*time.Hour)) {
			t.Errorf("%d failures: cooldown = %s, want 6h0m0s", n, until.Sub(after).Round(time.Second))
		}
	}
	// A busy answer is a capacity signal: no cooldown, however often.
	until, _, _ := cooldownAfter(t, 3, ClassBusy, 30*time.Second)
	if !until.IsZero() {
		t.Errorf("busy set a cooldown of %s, want none", time.Until(until).Round(time.Second))
	}
}

// TestClassifyStatusBoundaryByProvider pins the tabu of W02-11: the body of
// an error answer is read on ctx rows only. Every other provider class keeps
// the status mapping even when the body carries a gateway code — including
// a code that disagrees with the status. On a ctx row the code wins for
// every table code, not only for the four K15 overrides (review 2, FR2-2).
func TestClassifyStatusBoundaryByProvider(t *testing.T) {
	for _, pc := range []string{ProviderGeneric, ProviderOpenRouter, ProviderLlamaCpp} {
		cases := []struct {
			name string
			err  error
			want ErrClass
		}{
			{"429 server_busy", statusErr(429, envelope("server_busy")), ClassRateLimit},
			{"429 concurrency_limit_exceeded", statusErr(429, envelope("concurrency_limit_exceeded")), ClassRateLimit},
			{"504 upstream_timeout", statusErr(504, envelope("upstream_timeout")), ClassGateway},
			{"429 insufficient_quota", statusErr(429, envelope("insufficient_quota")), ClassRateLimit},
			{"500 with rate_limit_exceeded", statusErr(500, envelope("rate_limit_exceeded")), ClassServerFault},
			{"401 with server_busy", statusErr(401, envelope("server_busy")), ClassAuth},
			{"503 with upstream_error", statusErr(503, envelope("upstream_error")), ClassGateway},
		}
		for _, c := range cases {
			if got := Classify(c.err, pc); got != c.want {
				t.Errorf("%s %s: Classify = %s, want %s (status mapping, body unread)", pc, c.name, got, c.want)
			}
		}
	}
	ctxCases := []struct {
		name string
		err  error
		want ErrClass
	}{
		{"500 with rate_limit_exceeded", statusErr(500, envelope("rate_limit_exceeded")), ClassRateLimit},
		{"401 with server_busy", statusErr(401, envelope("server_busy")), ClassBusy},
		{"503 with upstream_error", statusErr(503, envelope("upstream_error")), ClassServerFault},
	}
	for _, c := range ctxCases {
		if got := Classify(c.err, ProviderCtx); got != c.want {
			t.Errorf("ctx %s: Classify = %s, want %s (the code wins)", c.name, got, c.want)
		}
	}
}

// TestProviderCtxLiteral pins the wire value of the ctx provider class: the
// provider_class CHECK and validate.go open for exactly "ctx" with F-W5
// (design/04 §4.12); a drifted constant would leave every such row outside
// the ctx branch (review 2, FR2-3).
func TestProviderCtxLiteral(t *testing.T) {
	if ProviderCtx != "ctx" {
		t.Fatalf("ProviderCtx = %q, want \"ctx\"", ProviderCtx)
	}
	if got := Classify(statusErr(429, envelope("server_busy")), "ctx"); got != ClassBusy {
		t.Errorf(`provider class "ctx": Classify = %s, want busy`, got)
	}
}
