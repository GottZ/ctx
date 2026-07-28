// H13c — negative probe for "outgoing fetches NEVER reach private addresses",
// consumer 3 of 3 plus the string-level sibling (design/04 §4.8 table row 3
// and its Zusatz).
//
//  1. TestSSODiscoveryCacheRefusesLoopback — the OIDC DISCOVERY path
//     (auth_sso.go:126). TestNewSSOHandlerDefaultClientBlocksLoopback already
//     probes h.client, the token-exchange/userinfo client. The discovery and
//     JWKS fetches do NOT go through that field directly: they run through the
//     oidc.Cache, which is constructed with its own Options{Client: …}. A
//     regression to oidc.NewCache(oidc.Options{}) — a default client with the
//     default transport — would leave the existing probe green while every
//     discovery/JWKS fetch loses the guard. This test pins that second seam.
//
//  2. TestIsDeniedHost — the validate-time HOST STRING check
//     (project.go:417-426). It has no dialer, so the dial-time mutation cannot
//     reach it; it needs its own probe of a different kind. Its own branch is
//     the literal 'localhost' — the IP branch merely delegates to
//     ssrfguard.IsDeniedIP, which is tested in that package.
//
// Mutations that turn these RED (documented, test-local for 1, source-local
// for 2): (1) build the handler's cache with oidc.NewCache(oidc.Options{})
// ⇒ the discovery document comes back and hits == 1; (2) drop the
// strings.EqualFold(host, "localhost") branch from isDeniedHost ⇒ 'localhost'
// and its case variants report false.
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSSODiscoveryCacheRefusesLoopback(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issuer":"http://internal","authorization_endpoint":"http://internal/a","token_endpoint":"http://internal/t","jwks_uri":"http://internal/jwks"}`))
	}))
	defer srv.Close()
	discoveryURL := srv.URL + "/.well-known/openid-configuration"

	h := NewSSOHandler(nil)

	// --- discovery seam: the cache must carry the guarded client ---
	body, err := h.cache.Discovery(context.Background(), discoveryURL)
	if err == nil {
		t.Fatalf("SSO discovery fetched a loopback issuer (%d bytes) — ssrfguard is not wired into the handler's oidc.Cache", len(body))
	}
	if !strings.Contains(err.Error(), "SSRF guard") {
		t.Fatalf("discovery fetch failed for the wrong reason (want the ssrfguard marker): %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the loopback issuer saw %d requests — the dial was not aborted before the bytes flowed", n)
	}

	// --- JWKS uses the same cache and must not have its own client ---
	if _, jerr := h.cache.JWKS(context.Background(), srv.URL+"/jwks"); jerr == nil {
		t.Fatal("SSO JWKS fetched a loopback URL — the guard is missing on the JWKS bucket")
	} else if !strings.Contains(jerr.Error(), "SSRF guard") {
		t.Fatalf("JWKS fetch failed for the wrong reason: %v", jerr)
	}

	// --- control half: the very same target IS reachable without the guard ---
	resp, cerr := srv.Client().Get(discoveryURL) //nolint:noctx // reachability control, no deadline needed
	if cerr != nil {
		t.Fatalf("control fetch failed — the fixture is unreachable, so the probes above prove nothing: %v", cerr)
	}
	defer resp.Body.Close() //nolint:errcheck
	if n := hits.Load(); n != 1 {
		t.Fatalf("control hits = %d, want exactly 1 (the guarded halves must have contributed none)", n)
	}
}

func TestIsDeniedHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
		why  string
	}{
		// The own branch: the literal name, case-insensitively, and the empty
		// host (a URL without an authority is never a valid api_base).
		{"localhost", true, "literal localhost"},
		{"LOCALHOST", true, "localhost is case-insensitive"},
		{"LocalHost", true, "localhost is case-insensitive"},
		{"", true, "empty host"},
		// The delegating branch: IP literals across every denied range.
		{"127.0.0.1", true, "IPv4 loopback"},
		{"127.13.37.1", true, "IPv4 loopback, whole /8"},
		{"::1", true, "IPv6 loopback"},
		{"10.0.0.1", true, "RFC1918 10/8"},
		{"172.16.0.1", true, "RFC1918 172.16/12"},
		{"192.168.1.1", true, "RFC1918 192.168/16"},
		{"fd00::1", true, "IPv6 unique local"},
		{"169.254.169.254", true, "link-local, the cloud metadata endpoint"},
		{"fe80::1", true, "IPv6 link-local"},
		{"0.0.0.0", true, "unspecified"},
		{"::", true, "IPv6 unspecified"},
		// Public targets pass — a validator that denies everything is useless.
		{"api.github.com", false, "public hostname"},
		{"8.8.8.8", false, "public IPv4"},
		{"2606:4700:4700::1111", false, "public IPv6"},
		// A hostname that RESOLVES privately still passes here by design: the
		// string check cannot see DNS. The dial-time guard re-checks it.
		{"internal.corp.example", false, "non-IP hostname, deferred to the dial-time guard"},
	}
	for _, tc := range cases {
		if got := isDeniedHost(tc.host); got != tc.want {
			t.Errorf("isDeniedHost(%q) = %v, want %v (%s)", tc.host, got, tc.want, tc.why)
		}
	}
}
