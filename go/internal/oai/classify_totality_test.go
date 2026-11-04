package oai_test

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/oai"
)

// streamErr stands in for llm.StreamError (this test binary stays free of
// internal/llm): an error event inside an HTTP-200 stream.
type streamErr struct{ code string }

func (e *streamErr) Error() string             { return "stream error event (code " + e.code + ")" }
func (e *streamErr) ProviderErrorType() string { return e.code }

// ctxOverrides are the codes a ctx row classifies differently from every
// other provider class (masterplan K15; design/04 R-02-16; design/06
// K-06-26). Every other code takes, on every provider class, the class its
// own status gets on the status path.
var ctxOverrides = map[string]backends.ErrClass{
	oai.CodeServerBusy:               backends.ClassBusy,
	oai.CodeConcurrencyLimitExceeded: backends.ClassBusy,
	oai.CodeUpstreamTimeout:          backends.ClassTimeout,
	oai.CodeInsufficientQuota:        backends.ClassQuotaExhausted,
}

// TestClassifierTotality (W02-11): every code of the gateway vocabulary has
// an explicit entry in the failover classifier of a peer ctx — on every
// provider class, no default case — and fails over there exactly as its
// wire form says: inside a 200 stream and, on a ctx row, as the body of the
// error answer this package writes. The vocabulary is the source of truth;
// a code added here without a classifier entry turns this test red.
func TestClassifierTotality(t *testing.T) {
	providers := []string{backends.ProviderGeneric, backends.ProviderLlamaCpp, backends.ProviderOpenRouter, backends.ProviderCtx}
	vocabulary := map[string]bool{}
	for _, spec := range oai.Codes() {
		vocabulary[spec.Code] = true
		byStatus := backends.Classify(&httpx.StatusError{Code: spec.Status}, backends.ProviderGeneric)

		// The error answer exactly as A writes it (envelope + headers).
		rec := httptest.NewRecorder()
		if spec.Code == oai.CodeInsufficientQuota {
			rec.Header().Set("Retry-After", "21600")
		}
		oai.Write(rec, oai.Error{Status: spec.Status, Type: spec.Type, Code: spec.Code, Message: "m"})
		answer := httpx.NewStatusError(rec.Result(), rec.Body.Bytes())

		for _, pc := range providers {
			want := byStatus
			if o, ok := ctxOverrides[spec.Code]; ok && pc == backends.ProviderCtx {
				want = o
			}
			got, ok := backends.GatewayCodeClass(pc, spec.Code)
			if !ok {
				t.Errorf("%s: code %q has no explicit classifier entry", pc, spec.Code)
				continue
			}
			if got != want {
				t.Errorf("%s: code %q (status %d) classifies as %s, want %s", pc, spec.Code, spec.Status, got, want)
			}
			if got := backends.Classify(&streamErr{code: spec.Code}, pc); got != want {
				t.Errorf("%s: mid-stream %q classifies as %s, want %s", pc, spec.Code, got, want)
			}
			// The body is read on ctx rows only; everyone else keeps the
			// status mapping byte for byte.
			wantAnswer := byStatus
			if pc == backends.ProviderCtx {
				wantAnswer = want
			}
			if got := backends.Classify(answer, pc); got != wantAnswer {
				t.Errorf("%s: %d %q answer classifies as %s, want %s", pc, spec.Status, spec.Code, got, wantAnswer)
			}
		}
	}
	for code := range ctxOverrides {
		if !vocabulary[code] {
			t.Errorf("ctx override %q is not a gateway code", code)
		}
	}

	// K-06-26 through the wire form: A's quota answer with Retry-After 6 h
	// cools B's ctx row for the full 6 h.
	rec := httptest.NewRecorder()
	rec.Header().Set("Retry-After", "21600")
	oai.Write(rec, oai.Error{Status: 429, Type: oai.TypeInsufficientQuota, Code: oai.CodeInsufficientQuota, Message: "m"})
	answer := httpx.NewStatusError(rec.Result(), rec.Body.Bytes())
	class := backends.Classify(answer, backends.ProviderCtx)
	if d := class.Cooldown(answer.RetryAfter); d != 6*time.Hour {
		t.Errorf("quota answer: class %s, cooldown %s, want quota_exhausted for 6h", class, d)
	}
}
