package handler

import "testing"

// TestValidateRegisteredRedirectURI pins the REGISTRATION rule (design 02
// §4b, OAuth 2.1 §5.2): https for real hosts, plain http only on loopback.
// The negative cases are the point — a registered http foreign host would be
// a permanent code/key exfiltration channel once 03 matches this list
// exactly at /authorize.
func TestValidateRegisteredRedirectURI(t *testing.T) {
	valid := []string{
		"https://claude.ai/api/mcp/auth_callback",
		"https://example.com/cb?x=1",
		"http://localhost/cb",
		"http://localhost:7777/cb",
		"http://127.0.0.1:7777/cb",
		"http://[::1]:7777/cb",
	}
	for _, uri := range valid {
		if err := validateRegisteredRedirectURI(uri); err != nil {
			t.Errorf("want valid, got reject for %q: %v", uri, err)
		}
	}

	invalid := []string{
		"http://attacker.example/cb",       // §4b core case: non-loopback http
		"http://127.0.0.1.evil.example/cb", // loopback-prefixed foreign host
		"http://localhost.evil.example/cb", // dito with localhost prefix
		"myapp://cb",                       // custom scheme (no host)
		"javascript:alert(1)",              // script scheme
		"https://",                         // empty host
		"/relative/path",                   // not absolute
		"",                                 // empty
	}
	for _, uri := range invalid {
		if err := validateRegisteredRedirectURI(uri); err == nil {
			t.Errorf("want reject, got valid for %q", uri)
		}
	}
}
