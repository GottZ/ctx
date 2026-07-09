package handler

import (
	"net/http/httptest"
	"testing"
	"time"
)

// DB-less probes for the 02-W4b guardrails: the fixed-window limiter, the
// trusted-proxy IP resolution, and the handler-level 429 (the rate guard
// sits BEFORE body parsing, so invalid bodies exercise it without a DB).
// The 201-path gates (cap → 403, other IP → still 201) live in
// oauth_register_integration_test.go.

func TestDCRRateLimiter_WindowAndIPIsolation(t *testing.T) {
	l := &dcrRateLimiter{hits: map[string]*dcrWindow{}}
	now := time.Now()

	for i := 1; i <= 3; i++ {
		if !l.allow("10.1.1.1", 3, now) {
			t.Fatalf("request %d within limit 3 denied", i)
		}
	}
	if l.allow("10.1.1.1", 3, now) {
		t.Error("request 4 over limit 3 allowed")
	}
	if !l.allow("10.1.1.2", 3, now) {
		t.Error("other IP throttled by first IP's budget (must count per IP, not globally)")
	}
	if !l.allow("10.1.1.1", 3, now.Add(dcrRateWindow)) {
		t.Error("expired window not reset — budget must recover after the window")
	}
}

func TestDCRClientIP_TrustedProxyRules(t *testing.T) {
	cases := []struct {
		name       string
		trusted    string
		remoteAddr string
		xff        string
		want       string
	}{
		{"no trusted proxy: RemoteAddr host, XFF ignored",
			"", "198.51.100.9:1234", "6.6.6.6", "198.51.100.9"},
		{"trusted proxy mismatch: XFF ignored (forgeable)",
			"192.0.2.10", "198.51.100.9:1234", "6.6.6.6", "198.51.100.9"},
		{"trusted proxy match: LAST XFF entry (the hop's own appendix)",
			"192.0.2.10", "192.0.2.10:8443", "6.6.6.6, 203.0.113.5", "203.0.113.5"},
		{"trusted proxy match, empty XFF: fall back to hop",
			"192.0.2.10", "192.0.2.10:8443", "", "192.0.2.10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvTrustedProxy, tc.trusted)
			req := httptest.NewRequest("POST", "/register", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := dcrClientIP(req); got != tc.want {
				t.Errorf("dcrClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDCRRegister_RateLimit429_BeforeParsing(t *testing.T) {
	t.Setenv(EnvDCRMode, "open")
	t.Setenv(EnvDCRRegisterRate, "2")
	h := &OAuthHandler{}

	sameIP := "203.0.113.99:7777"
	fire := func(t *testing.T, ip string) int {
		t.Helper()
		req := httptest.NewRequest("POST", "/register", nil) // no body: parse would 400
		req.RemoteAddr = ip
		rec := httptest.NewRecorder()
		h.Register(rec, req)
		return rec.Code
	}

	if code := fire(t, sameIP); code != 400 {
		t.Fatalf("request 1: status = %d, want 400 (within budget, fails validation)", code)
	}
	if code := fire(t, sameIP); code != 400 {
		t.Fatalf("request 2: status = %d, want 400", code)
	}
	if code := fire(t, sameIP); code != 429 {
		t.Errorf("request 3 over budget 2: status = %d, want 429", code)
	}
	if code := fire(t, "203.0.113.100:7777"); code != 400 {
		t.Errorf("other IP: status = %d, want 400 (own budget, not throttled)", code)
	}
}
