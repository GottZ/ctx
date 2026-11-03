package backends

import (
	"testing"
	"time"
)

// testStreamErr stands in for llm.StreamError (backends must not import
// llm): an error event inside an HTTP-200 stream, carrying the provider's
// error code the way the openai wire reads it from error.code.
type testStreamErr struct{ Code string }

func (e *testStreamErr) Error() string             { return "stream error event (code " + e.Code + ")" }
func (e *testStreamErr) ProviderErrorType() string { return e.Code }

// gatewayGenericCases is the generic column of the gateway-code table: every
// code of the ctx gateway vocabulary (internal/oai Codes, W02-02), one case
// each, mapped onto the class its own status gets on the status path —
// design/02 §4.10.3 corrected by K6 (server_busy, concurrency_limit_exceeded
// are 429 codes). internal/oai TestClassifierTotality checks the same table
// against the vocabulary itself.
var gatewayGenericCases = []struct {
	code string
	want ErrClass
}{
	// 429
	{"rate_limit_exceeded", ClassRateLimit},
	{"server_busy", ClassRateLimit},
	{"concurrency_limit_exceeded", ClassRateLimit},
	{"insufficient_quota", ClassRateLimit},
	// 503, 504, 508 (unknown 5xx on the status path)
	{"upstream_unavailable", ClassGateway},
	{"server_overloaded", ClassGateway},
	{"internal_error", ClassGateway},
	{"model_unavailable", ClassGateway},
	{"no_eligible_backend", ClassGateway},
	{"service_unavailable", ClassGateway},
	{"upstream_timeout", ClassGateway},
	{"federation_loop", ClassGateway},
	{"federation_depth", ClassGateway},
	// 500
	{"upstream_error", ClassServerFault},
	// 401, 403
	{"invalid_api_key", ClassAuth},
	{"permission_denied", ClassAuth},
	{"peer_instance_mismatch", ClassAuth},
	// 400, 404, 405, 413
	{"upstream_rejected", ClassBadRequest},
	{"context_length_exceeded", ClassBadRequest},
	{"unsupported_parameter", ClassBadRequest},
	{"unsupported_value", ClassBadRequest},
	{"model_not_found", ClassBadRequest},
	{"not_found", ClassBadRequest},
	{"method_not_allowed", ClassBadRequest},
	{"request_too_large", ClassBadRequest},
	{"invalid_json", ClassBadRequest},
	{"missing_required_parameter", ClassBadRequest},
	{"invalid_type", ClassBadRequest},
	{"model_not_supported", ClassBadRequest},
	{"federation_header_invalid", ClassBadRequest},
	{"federation_hop_required", ClassBadRequest},
}

// TestClassifyGatewayCodesGeneric: a gateway code inside a 200 stream fails
// over like the HTTP error it stands for, on every non-ctx provider class.
// Before W02-11 every one of them fell through to the stop-the-chain default
// (server_fault).
func TestClassifyGatewayCodesGeneric(t *testing.T) {
	for _, pc := range []string{ProviderGeneric, ProviderLlamaCpp, ProviderOpenRouter} {
		for _, c := range gatewayGenericCases {
			if got := Classify(&testStreamErr{Code: c.code}, pc); got != c.want {
				t.Errorf("%s mid-stream %q: Classify = %s, want %s", pc, c.code, got, c.want)
			}
		}
	}
	// An unknown code keeps the stop-the-chain default.
	if got := Classify(&testStreamErr{Code: "502"}, ProviderOpenRouter); got != ClassServerFault {
		t.Errorf("unknown mid-stream code: Classify = %s, want server_fault", got)
	}
}

// cooldownAfter reports n consecutive failures of one class and returns the
// window the resulting cooldownUntil must lie in: [before+cd, after+cd].
func cooldownAfter(t *testing.T, n int, class ErrClass, retryAfter time.Duration) (until, before, after time.Time) {
	t.Helper()
	p := seedPool(poolFixture())
	before = time.Now()
	for i := 0; i < n; i++ {
		p.ReportFailure("2", class, retryAfter)
	}
	after = time.Now()
	p.healthM.Lock()
	until = p.health["2"].cooldownUntil
	p.healthM.Unlock()
	return until, before, after
}

// TestReportFailureKeepsClassCooldown: the doubling after three consecutive
// failures backs off short cooldowns up to 10 minutes but never shortens a
// class cooldown that is longer than that.
func TestReportFailureKeepsClassCooldown(t *testing.T) {
	cases := []struct {
		name       string
		class      ErrClass
		retryAfter time.Duration
		want       time.Duration
	}{
		{"billing keeps its hour", ClassBilling, 0, time.Hour},
		{"no_providers keeps its hour", ClassNoProviders, 0, time.Hour},
		{"auth keeps its 15 minutes", ClassAuth, 0, 15 * time.Minute},
		// The back-off itself is unchanged below the cap.
		{"transport doubles", ClassTransport, 0, time.Minute},
		{"gateway doubles", ClassGateway, 0, time.Minute},
		{"rate limit doubles", ClassRateLimit, 4 * time.Minute, 8 * time.Minute},
		{"rate limit doubling capped", ClassRateLimit, 5 * time.Minute, 10 * time.Minute},
		{"bad request earns none", ClassBadRequest, 0, 0},
	}
	for _, c := range cases {
		until, before, after := cooldownAfter(t, 3, c.class, c.retryAfter)
		if c.want == 0 {
			if !until.IsZero() {
				t.Errorf("%s: cooldown set to %s, want none", c.name, time.Until(until))
			}
			continue
		}
		if until.Before(before.Add(c.want)) || until.After(after.Add(c.want)) {
			t.Errorf("%s: cooldown after 3 failures = %s, want %s", c.name, until.Sub(after).Round(time.Second), c.want)
		}
	}
}
