package ssrfguard

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// TestDialControl is the dial-time deny-list. The Control hook fires with the
// RESOLVED ip:port, so a hostname that rebinds to a private/link-local address
// after a public validate-time check is caught here. 169.254.169.254 is the
// cloud-metadata endpoint — the canonical SSRF target.
func TestDialControl(t *testing.T) {
	denied := []string{
		"169.254.169.254:443", // link-local / metadata (rebinding target)
		"127.0.0.1:80",        // loopback
		"10.0.0.1:443",        // RFC1918 private
		"192.168.1.1:443",     // RFC1918 private
		"[::1]:443",           // loopback v6
		"[fd00::1]:443",       // unique-local v6
		"0.0.0.0:80",          // unspecified
	}
	for _, a := range denied {
		if err := DialControl("tcp", a, nil); err == nil {
			t.Errorf("DialControl(%q) = nil, want refusal", a)
		}
	}
	allowed := []string{"140.82.112.3:443", "[2606:50c0::1]:443"} // public GitHub-ish
	for _, a := range allowed {
		if err := DialControl("tcp", a, nil); err != nil {
			t.Errorf("DialControl(%q) = %v, want allow", a, err)
		}
	}
	// A non-IP dial address (should never happen post-resolution) is refused,
	// never silently allowed.
	if err := DialControl("tcp", "not-an-ip:80", nil); err == nil {
		t.Errorf("DialControl(non-IP) = nil, want refusal")
	}
}

// TestGuardedDialer_RebindingRefused is the end-to-end rebinding probe: the
// guarded dialer resolves "localhost" to a loopback IP and the Control hook then
// refuses the dial — exactly the post-DNS-rebinding path (a hostname resolving to
// a forbidden range aborts BEFORE any bytes flow).
func TestGuardedDialer_RebindingRefused(t *testing.T) {
	d := GuardedDialer()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := d.DialContext(ctx, "tcp", "localhost:80")
	if err == nil {
		t.Fatal("guarded dial to localhost succeeded, want SSRF refusal")
	}
	if !strings.Contains(err.Error(), "SSRF guard") && !strings.Contains(err.Error(), "loopback") {
		// The Control error is wrapped by net; assert it is OUR refusal, not a
		// mere connection-refused (which would mean the guard never fired).
		var oe *net.OpError
		if !errors.As(err, &oe) || oe.Err == nil || !strings.Contains(oe.Err.Error(), "SSRF") {
			t.Fatalf("dial failed but not via the SSRF guard: %v", err)
		}
	}
}

// TestIsDeniedIP spot-checks the classifier directly (the branch shared by the
// forge api_base host validator and the Camo fetch guard).
func TestIsDeniedIP(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":       true,
		"10.1.2.3":        true,
		"172.16.0.1":      true,
		"192.168.0.1":     true,
		"169.254.169.254": true,
		"::1":             true,
		"fd00::1":         true,
		"0.0.0.0":         true,
		"8.8.8.8":         false,
		"140.82.112.3":    false,
		"2606:50c0::1":    false,
	}
	for s, want := range cases {
		if got := IsDeniedIP(net.ParseIP(s)); got != want {
			t.Errorf("IsDeniedIP(%s) = %v, want %v", s, got, want)
		}
	}
}
