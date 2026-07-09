// DB-less unit gates for the external-login flow handler (OAuth L5,
// design/04 §4.2/§4.3 + §5): the pure building blocks — open-redirect filter
// (F5), PKCE pair, per-IP limiter (F7), trusted-proxy IP derivation,
// authorize-URL shape (PKCE/nonce OIDC-only, F6) and the ssrfguard default
// client (loopback dial refused). The full flow runs against a mock IdP in
// the integration suite (auth_sso_integration_test.go).

package handler

import (
	"crypto/sha256"
	"encoding/base64"
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
	}
	for _, c := range cases {
		if got := safeReturnTo(c.in); got != c.want {
			t.Errorf("safeReturnTo(%q) = %q, want %q", c.in, got, c.want)
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
