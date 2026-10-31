// DB-less unit gates for the external-login flow handler (OAuth L5,
// design/04 §4.2/§4.3 + §5): the pure building blocks — open-redirect filter
// (F5), PKCE pair, per-IP limiter (F7), trusted-proxy IP derivation,
// authorize-URL shape (PKCE/nonce OIDC-only, F6) and the ssrfguard default
// client (loopback dial refused). The full flow runs against a mock IdP in
// the integration suite (auth_sso_integration_test.go).

package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

func TestSafeReturnTo(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/blocks?q=test", "/blocks?q=test"},
		{"/a", "/a"},
		{"", "/"},
		{"/", "/"},
		// Open-redirect shapes (design/04 §5 F5) — all collapse to "/".
		{"//evil.example", "/"},
		{`/\evil`, "/"},
		{"https://evil.example/", "/"},
		{"evil", "/"},
		{`\\evil`, "/"},
		{"///evil.example", "/"},
		// GHSA 2026-10-31 §9b: browsers strip TAB/CR/LF from URLs, so
		// "/\t/evil.example" is read as "//evil.example". Any control byte
		// (raw, or percent-decoded in the path) collapses to "/".
		{"/\t/evil.example", "/"},
		{"\t//evil.example", "/"},
		{"/\r\n/evil.example", "/"},
		{"/\n/evil.example", "/"},
		{"/a\x00b", "/"},
		{"/a\x7fb", "/"},
		{"/blocks?q=\r\nSet-Cookie:x", "/"},
		{"/%09/evil.example", "/"},
		{"/%0d%0a/evil.example", "/"},
		// Encoded slash/backslash at the second position: decoded "//" / "/\".
		{"/%2F/evil.example", "/"},
		{"/%2fevil.example", "/"},
		{"/%5Cevil.example", "/"},
		// path.Clean shapes http.Redirect would otherwise turn into "/\evil".
		{`/./\evil`, "/"},
		{`/../\evil`, "/"},
		{"/.//evil.example", "/evil.example"}, // cleaned to a same-origin path
		// Absolute / scheme / userinfo / opaque shapes.
		{"/\\evil.example", "/"},
		{"https:/evil.example", "/"},
		{"javascript:alert(1)", "/"},
		{"//user@evil.example/", "/"},
		{"%2F%2Fevil.example", "/"},
		// Normalised output (u.RequestURI form, fragment dropped, cleaned).
		{"/blocks#frag", "/blocks"},
		{"/a/../b", "/b"},
		{"/a//b", "/a/b"},
		{"/sub/", "/sub/"},
		{"/a b", "/a%20b"},
		{"/?x=1", "/?x=1"},
		{"/a%2Fb", "/a/b"},
		{"/@evil.example", "/@evil.example"},
	}
	for _, c := range cases {
		got := safeReturnTo(c.in)
		if got != c.want {
			t.Errorf("safeReturnTo(%q) = %q, want %q", c.in, got, c.want)
		}
		// Idempotent: the callback re-filters the sealed value.
		if again := safeReturnTo(got); again != got {
			t.Errorf("safeReturnTo not idempotent: %q → %q → %q", c.in, got, again)
		}
		// http.Redirect's own path.Clean must not change the value: what
		// was checked is exactly what reaches the Location header.
		rec := httptest.NewRecorder()
		http.Redirect(rec, httptest.NewRequest("GET", "/auth/callback/x", nil), got, http.StatusFound)
		if loc := rec.Header().Get("Location"); loc != got {
			t.Errorf("safeReturnTo(%q) = %q, but http.Redirect emits Location %q", c.in, got, loc)
		}
	}
}

func TestGeneratePKCE(t *testing.T) {
	verifier, challenge, err := generatePKCE()
	if err != nil {
		t.Fatalf("generatePKCE: %v", err)
	}
	if len(verifier) != 43 { // 32 bytes base64url without padding
		t.Errorf("verifier length = %d, want 43", len(verifier))
	}
	sum := sha256.Sum256([]byte(verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); challenge != want {
		t.Errorf("challenge is not S256(verifier)")
	}
	v2, _, err := generatePKCE()
	if err != nil {
		t.Fatalf("generatePKCE second call: %v", err)
	}
	if v2 == verifier {
		t.Errorf("two PKCE verifiers are identical — randomness broken")
	}
}

func TestIPLimiter(t *testing.T) {
	l := newIPLimiter(3, time.Minute)
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if !l.allow("10.0.0.1") {
			t.Fatalf("request %d denied within limit", i+1)
		}
	}
	// N+1 → deny.
	if l.allow("10.0.0.1") {
		t.Fatalf("request 4 allowed, want deny (limit 3)")
	}
	// Independent IP is unaffected.
	if !l.allow("10.0.0.2") {
		t.Fatalf("independent ip denied")
	}
	// Window expiry resets the bucket.
	now = now.Add(time.Minute + time.Second)
	if !l.allow("10.0.0.1") {
		t.Fatalf("request after window expiry denied")
	}
}

func TestClientIP(t *testing.T) {
	req := httptest.NewRequest("GET", "/auth/login/x", nil)
	req.RemoteAddr = "203.0.113.7:41234"
	req.Header.Set("X-Forwarded-For", "198.51.100.9, 192.0.2.3")

	// No trusted proxy configured → XFF ignored, peer wins.
	if got := clientIP(req, ""); got != "203.0.113.7" {
		t.Errorf("no proxy: got %q, want peer ip", got)
	}
	// Peer is NOT the trusted proxy → XFF still ignored (forgeable).
	if got := clientIP(req, "10.1.2.3"); got != "203.0.113.7" {
		t.Errorf("untrusted peer: got %q, want peer ip", got)
	}
	// Peer IS the trusted proxy → rightmost XFF entry (the hop the proxy
	// appended); left entries stay untrusted.
	if got := clientIP(req, "203.0.113.7"); got != "192.0.2.3" {
		t.Errorf("trusted peer: got %q, want rightmost XFF entry", got)
	}
}

// §9f: without CTX_TRUSTED_PROXY a request carrying X-Forwarded-For logs ONE
// value-free warning per process; requests without XFF log nothing, and XFF is
// still not trusted for the rate-limit key.
func TestProxyWarnerOncePerProcess(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	pw := &proxyWarner{}

	plain := httptest.NewRequest("GET", "/auth/login/x", nil)
	plain.RemoteAddr = "172.18.0.5:5000"
	pw.check(log, plain)
	if buf.Len() != 0 {
		t.Fatalf("request without X-Forwarded-For logged: %s", buf.String())
	}

	fwd := httptest.NewRequest("POST", "/auth/login", nil)
	fwd.RemoteAddr = "172.18.0.5:5000"
	fwd.Header.Set("X-Forwarded-For", "198.51.100.9")
	for range 3 {
		pw.check(log, fwd)
	}
	out := buf.String()
	if n := strings.Count(out, "CTX_TRUSTED_PROXY is unset"); n != 1 {
		t.Fatalf("warning count = %d, want exactly 1: %s", n, out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("warning not at WARN level: %s", out)
	}
	for _, leak := range []string{"198.51.100.9", "172.18.0.5"} {
		if strings.Contains(out, leak) {
			t.Errorf("warning carries a request value %q: %s", leak, out)
		}
	}
}

// TestClientIPWiresProxyWarner pins that clientIP — the login/SSO rate-limit
// key — reports to the process warner only when no trusted proxy is set.
func TestClientIPWiresProxyWarner(t *testing.T) {
	var buf bytes.Buffer
	prevLog, prevWarner := slog.Default(), unsetProxyWarner
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	unsetProxyWarner = &proxyWarner{}
	t.Cleanup(func() { slog.SetDefault(prevLog); unsetProxyWarner = prevWarner })

	req := httptest.NewRequest("POST", "/auth/login", nil)
	req.RemoteAddr = "203.0.113.7:41234"
	req.Header.Set("X-Forwarded-For", "198.51.100.9")

	if got := clientIP(req, "203.0.113.7"); got != "198.51.100.9" || buf.Len() != 0 {
		t.Fatalf("trusted proxy set: ip=%q, log=%q — want XFF ip and no warning", got, buf.String())
	}
	if got := clientIP(req, ""); got != "203.0.113.7" {
		t.Fatalf("no proxy: ip=%q, want the peer (XFF never trusted)", got)
	}
	if !strings.Contains(buf.String(), "CTX_TRUSTED_PROXY is unset") {
		t.Fatalf("no proxy + XFF: warning missing: %q", buf.String())
	}
}

func TestBuildAuthorizeURL_OIDCOnlyMechanics(t *testing.T) {
	oidcProv := &store.OAuthProvider{
		Type: "oidc", ClientID: "cid", Scopes: []string{"openid", "email"},
	}
	u := buildAuthorizeURL("https://idp.example/authorize", oidcProv, "https://ctx.example/auth/callback/x", "chal", "state-1", "nonce-1")
	for _, want := range []string{"code_challenge=chal", "code_challenge_method=S256", "nonce=nonce-1", "state=state-1", "response_type=code", "scope=openid+email"} {
		if !strings.Contains(u, want) {
			t.Errorf("oidc authorize URL missing %q: %s", want, u)
		}
	}

	// GitHub (classic OAuth app, F6): NO PKCE, NO nonce.
	ghProv := &store.OAuthProvider{
		Type: "github", ClientID: "cid", Scopes: []string{"read:user"},
	}
	u = buildAuthorizeURL("https://github.com/login/oauth/authorize", ghProv, "https://ctx.example/auth/callback/gh", "", "state-2", "nonce-2")
	for _, forbidden := range []string{"code_challenge", "nonce"} {
		if strings.Contains(u, forbidden) {
			t.Errorf("github authorize URL must not carry %q (F6): %s", forbidden, u)
		}
	}
	if !strings.Contains(u, "state=state-2") {
		t.Errorf("github authorize URL missing state: %s", u)
	}
}

// TestNewSSOHandlerDefaultClientBlocksLoopback probes that the PRODUCTION
// client wiring (NewSSOHandler) refuses loopback targets at dial time — the
// ssrfguard injection (design/04 §5 SSRF row) is active, not decorative.
// Integration tests inject a plain client for exactly this reason.
func TestNewSSOHandlerDefaultClientBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(nil)
	defer srv.Close()

	h := NewSSOHandler(nil)
	resp, err := h.client.Get(srv.URL) //nolint:noctx // dial-refusal probe, no body expected
	if err == nil {
		resp.Body.Close() //nolint:errcheck
		t.Fatalf("default SSO client fetched a loopback URL — ssrfguard not wired")
	}
	if !strings.Contains(err.Error(), "SSRF guard") {
		t.Fatalf("loopback fetch failed for the wrong reason: %v", err)
	}
}

// TestNewSSOHandlerDialsDirect pins the §9d proxy half (GHSA 2026-10-31): the
// production SSO transport must not consult HTTP(S)_PROXY — through a proxy
// the dial-time guard checks only the proxy's IP — and a special-purpose
// target the old predicates missed (CGNAT, Alibaba metadata) is refused by
// the guard itself.
func TestNewSSOHandlerDialsDirect(t *testing.T) {
	h := NewSSOHandler(nil)
	tr, ok := h.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("SSO transport is %T, want *http.Transport", h.client.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("SSO transport honours a proxy — the ssrfguard would only see the proxy's IP")
	}
	resp, err := h.client.Get("http://100.100.100.200/latest/meta-data/") //nolint:noctx // dial-refusal probe
	if err == nil {
		resp.Body.Close() //nolint:errcheck
		t.Fatal("default SSO client fetched 100.100.100.200 — CGNAT/metadata not denied")
	}
	if !strings.Contains(err.Error(), "SSRF guard") {
		t.Fatalf("100.100.100.200 failed for the wrong reason: %v", err)
	}
}
