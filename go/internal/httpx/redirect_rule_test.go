package httpx

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// hop builds the request net/http hands CheckRedirect for one redirect.
func hop(t *testing.T, raw string, code int) *http.Request {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Request{URL: u, Response: &http.Response{StatusCode: code}}
}

// TestSameOriginRedirectRule pins the origin rule (design/04 §4.3): a hop is
// followed only on the origin of the FIRST request (scheme, host, port, with
// the scheme's default port filled in) or as an http→https upgrade on the same
// host (80→443, or the same explicit port). Everything else is refused with
// http.ErrUseLastResponse.
func TestSameOriginRedirectRule(t *testing.T) {
	cases := []struct {
		name     string
		via      []string
		to       string
		followed bool
	}{
		{"same origin, other path", []string{"http://api.example/v1/a"}, "http://api.example/v1/b?x=1", true},
		{"default port spelled out", []string{"http://api.example/v1"}, "http://api.example:80/v1", true},
		{"default https port spelled out", []string{"https://api.example:443/v1"}, "https://api.example/v1", true},
		{"host case differs", []string{"http://API.Example/v1"}, "http://api.example/v1", true},
		{"userinfo is not part of the origin", []string{"http://api.example/v1"}, "http://u:p@api.example/v1", true},
		{"http to https, default ports", []string{"http://api.example/v1"}, "https://api.example/v1", true},
		{"http to https, explicit 80 to 443", []string{"http://api.example:80/v1"}, "https://api.example:443/v1", true},
		{"http to https, same explicit port", []string{"http://api.example:8080/v1"}, "https://api.example:8080/v1", true},
		{"same explicit port", []string{"http://10.0.0.5:11434/api"}, "http://10.0.0.5:11434/api/chat", true},

		{"other host", []string{"http://api.example/v1"}, "http://elsewhere.example/v1", false},
		{"subdomain", []string{"http://api.example/v1"}, "http://eu.api.example/v1", false},
		{"parent domain", []string{"http://eu.api.example/v1"}, "http://api.example/v1", false},
		{"trailing dot is another name", []string{"http://api.example/v1"}, "http://api.example./v1", false},
		{"ip literal vs name", []string{"http://127.0.0.1/v1"}, "http://localhost/v1", false},
		{"ipv6 literal vs ipv4", []string{"http://[::1]:8080/v1"}, "http://127.0.0.1:8080/v1", false},
		{"other port", []string{"http://api.example/v1"}, "http://api.example:8080/v1", false},
		{"other port on https", []string{"https://api.example/v1"}, "https://api.example:8443/v1", false},
		{"https to http downgrade", []string{"https://api.example/v1"}, "http://api.example/v1", false},
		{"https to http, same explicit port", []string{"https://api.example:8443/v1"}, "http://api.example:8443/v1", false},
		{"http to https onto another port", []string{"http://api.example/v1"}, "https://api.example:8443/v1", false},
		{"http on a custom port to https 443", []string{"http://api.example:8080/v1"}, "https://api.example/v1", false},
		{"other scheme", []string{"http://api.example/v1"}, "ftp://api.example/v1", false},

		// Host names compare byte for byte, ASCII case aside: strings.ToLower
		// folds U+0130 'İ' to ASCII 'i', while net/http dials the IDNA form
		// (xn--ibackend-o0e) — a Unicode fold would call two hosts equal.
		{"unicode İ folds to i but dials xn--", []string{"http://ibackend.example/v1"}, "http://%C4%B0backend.example/v1", false},
		{"unicode İ raw utf-8", []string{"http://ibackend.example/v1"}, "http://\u0130backend.example/v1", false},
		{"kelvin sign is not k", []string{"http://kbackend.example/v1"}, "http://\u212Abackend.example/v1", false},
		{"non-ascii case is not folded", []string{"http://b\u00fccher.example/v1"}, "http://b\u00dccher.example/v1", false},
		{"identical non-ascii host", []string{"http://b\u00fccher.example/a"}, "http://b\u00fccher.example/b", true},
		{"ipv6 zone keeps its case", []string{"http://[fe80::1%25eth0]:8080/v1"}, "http://[fe80::1%25ETH0]:8080/v1", false},
		{"ipv6 zone, same zone", []string{"http://[fe80::1%25eth0]:8080/v1"}, "http://[FE80::1%25eth0]:8080/v2", true},

		// Chains compare against via[0] — the configured base_url that went
		// through Trust × Sensitivity — not against the previous hop.
		{"chain A→A→B", []string{"http://api.example/a", "http://api.example/b"}, "http://elsewhere.example/c", false},
		{"chain upgrade then same origin", []string{"http://api.example/a", "https://api.example/b"}, "https://api.example/c", true},
		{"chain back to the configured origin", []string{"http://api.example/a", "https://api.example/b"}, "http://api.example/c", true},
		{"chain https origin, later downgrade", []string{"https://api.example/a", "https://api.example/b"}, "http://api.example/c", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			via := make([]*http.Request, 0, len(tc.via))
			for _, v := range tc.via {
				via = append(via, hop(t, v, 0))
			}
			err := sameOriginRedirect(hop(t, tc.to, http.StatusTemporaryRedirect), via)
			switch {
			case tc.followed && err != nil:
				t.Errorf("%v → %s refused (%v), want followed", tc.via, tc.to, err)
			case !tc.followed && !errors.Is(err, http.ErrUseLastResponse):
				t.Errorf("%v → %s: err = %v, want http.ErrUseLastResponse", tc.via, tc.to, err)
			}
		})
	}
}

// TestSameOriginRedirectHopLimit: setting CheckRedirect replaces net/http's
// defaultCheckRedirect, so its ten-hop limit is restated — same error text,
// checked before the origin rule.
func TestSameOriginRedirectHopLimit(t *testing.T) {
	via := make([]*http.Request, 0, 10)
	for range 9 {
		via = append(via, hop(t, "http://api.example/v1", 0))
	}
	if err := sameOriginRedirect(hop(t, "http://api.example/v1", 307), via); err != nil {
		t.Fatalf("hop 10 refused: %v", err)
	}
	via = append(via, hop(t, "http://api.example/v1", 0))
	err := sameOriginRedirect(hop(t, "http://api.example/v1", 307), via)
	if err == nil || err.Error() != "stopped after 10 redirects" || errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("hop 11: err = %v, want net/http's \"stopped after 10 redirects\"", err)
	}
}

// TestSameOriginRedirectLog: a refusal writes exactly one WARN "redirect
// refused" naming both origins and the status — never a path, a query or
// userinfo (they can carry tokens); a followed hop writes nothing.
func TestSameOriginRedirectLog(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	via := []*http.Request{hop(t, "https://svc.example/v1/chat?sig=FROMSECRET", 0)}
	if err := sameOriginRedirect(hop(t, "https://svc.example/v1/other", 307), via); err != nil {
		t.Fatalf("same-origin hop refused: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("a followed hop logged: %s", buf.String())
	}

	err := sameOriginRedirect(hop(t, "http://user:TOSECRET@Other.Example:8080/steal/path?token=TOSECRET", 308), via)
	if !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("err = %v, want http.ErrUseLastResponse", err)
	}
	out := buf.String()
	if n := strings.Count(out, "\n"); n != 1 {
		t.Fatalf("want exactly one log line, got %d: %s", n, out)
	}
	for _, want := range []string{"level=WARN", `msg="redirect refused"`, "from_origin=https://svc.example:443", "to_origin=http://other.example:8080", "status=308"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q: %s", want, out)
		}
	}
	for _, leak := range []string{"SECRET", "/v1", "/steal", "token", "user"} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaks %q: %s", leak, out)
		}
	}
}
