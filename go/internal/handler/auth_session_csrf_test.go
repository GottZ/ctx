// DB-less gate for the login-CSRF check on POST /auth/login (GHSA
// 2026-10-31 §9a): the Content-Type × Origin/Referer/Sec-Fetch-Site matrix
// of loginRequestAllowed, and HandleLogin answering 403 before any key
// lookup (the handler runs with a nil pool — a rejected request must never
// reach the database).

package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoginRequestAllowed(t *testing.T) {
	const host = "ctx.example"
	type hdr map[string]string
	cases := []struct {
		name   string
		issuer string
		h      hdr
		want   bool
	}{
		{"json, no browser headers (CLI)", "", hdr{"Content-Type": "application/json"}, true},
		{"json with charset", "", hdr{"Content-Type": "application/json; charset=utf-8"}, true},
		{"json, same-origin Origin", "", hdr{"Content-Type": "application/json", "Origin": "https://ctx.example"}, true},
		{"json, Origin http on r.Host (dev)", "", hdr{"Content-Type": "application/json", "Origin": "http://ctx.example"}, true},
		{"json, Origin = canonical issuer", "https://public.example", hdr{"Content-Type": "application/json", "Origin": "https://public.example"}, true},
		{"json, Referer same origin", "", hdr{"Content-Type": "application/json", "Referer": "https://ctx.example/login?x=1"}, true},
		{"json, Sec-Fetch-Site same-origin", "", hdr{"Content-Type": "application/json", "Sec-Fetch-Site": "same-origin"}, true},
		{"json, Sec-Fetch-Site none", "", hdr{"Content-Type": "application/json", "Sec-Fetch-Site": "none"}, true},

		// The advisory shape: <form enctype="text/plain">.
		{"text/plain form", "", hdr{"Content-Type": "text/plain", "Origin": "https://evil.example"}, false},
		{"text/plain same origin", "", hdr{"Content-Type": "text/plain", "Origin": "https://ctx.example"}, false},
		{"urlencoded form", "", hdr{"Content-Type": "application/x-www-form-urlencoded"}, false},
		{"multipart form", "", hdr{"Content-Type": "multipart/form-data; boundary=x"}, false},
		{"no content type", "", hdr{}, false},
		{"json-ish suffix", "", hdr{"Content-Type": "application/json-patch+json"}, false},
		{"malformed content type", "", hdr{"Content-Type": "application/json;;=="}, false},

		{"json, foreign Origin", "", hdr{"Content-Type": "application/json", "Origin": "https://evil.example"}, false},
		{"json, Origin null", "", hdr{"Content-Type": "application/json", "Origin": "null"}, false},
		{"json, Origin sibling subdomain", "", hdr{"Content-Type": "application/json", "Origin": "https://a.ctx.example"}, false},
		{"json, Origin port differs", "", hdr{"Content-Type": "application/json", "Origin": "https://ctx.example:8443"}, false},
		{"json, Origin wins over same-origin Referer", "", hdr{"Content-Type": "application/json", "Origin": "https://evil.example", "Referer": "https://ctx.example/"}, false},
		{"json, issuer set, Origin scheme differs from issuer, host foreign", "https://public.example", hdr{"Content-Type": "application/json", "Origin": "http://public.example"}, false},
		{"json, foreign Referer", "", hdr{"Content-Type": "application/json", "Referer": "https://evil.example/ctx.example"}, false},
		{"json, Sec-Fetch-Site cross-site", "", hdr{"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site"}, false},
		{"json, Sec-Fetch-Site same-site", "", hdr{"Content-Type": "application/json", "Sec-Fetch-Site": "same-site"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &SessionHandler{issuer: c.issuer}
			r := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"api_key":"k"}`))
			r.Host = host
			for k, v := range c.h {
				r.Header.Set(k, v)
			}
			if got := h.loginRequestAllowed(r); got != c.want {
				t.Fatalf("loginRequestAllowed = %v, want %v", got, c.want)
			}
		})
	}
}

// TestHandleLogin_CrossSiteFormRejected replays the advisory PoC body against
// the handler: 403, the uniform value-free error, no cookie — and no DB access
// (nil pool: reaching auth.Authenticate would panic).
func TestHandleLogin_CrossSiteFormRejected(t *testing.T) {
	h := &SessionHandler{limiter: newIPLimiter(100, sessionRateWindow)}
	r := httptest.NewRequest(http.MethodPost, "/auth/login",
		strings.NewReader(`{"api_key":"attackerkey","x":"="}`))
	r.Host = "ctx.example"
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	h.HandleLogin(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "attackerkey") {
		t.Fatalf("reject echoes request input: %s", rec.Body.String())
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatalf("reject set cookies: %v", rec.Result().Cookies())
	}
}
