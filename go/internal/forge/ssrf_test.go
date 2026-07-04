package forge

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// TestSSRFDialControl is the dial-time deny-list (design/03 §5.7 point 2). The
// Control hook fires with the RESOLVED ip:port, so a hostname that rebinds to a
// private/link-local address after a public PATCH-time check is caught here.
// 169.254.169.254 is the cloud-metadata endpoint — the canonical SSRF target.
func TestSSRFDialControl(t *testing.T) {
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
		if err := ssrfDialControl("tcp", a, nil); err == nil {
			t.Errorf("ssrfDialControl(%q) = nil, want refusal", a)
		}
	}
	allowed := []string{"140.82.112.3:443", "[2606:50c0::1]:443"} // public GitHub-ish
	for _, a := range allowed {
		if err := ssrfDialControl("tcp", a, nil); err != nil {
			t.Errorf("ssrfDialControl(%q) = %v, want allow", a, err)
		}
	}
}

// TestGuardedDialer_RebindingRefused is the end-to-end rebinding probe: the
// guarded dialer resolves the hostname "localhost" to a loopback IP and the
// Control hook then refuses the dial — exactly the post-DNS-rebinding path
// (a hostname resolving to a forbidden range aborts BEFORE any bytes flow).
func TestGuardedDialer_RebindingRefused(t *testing.T) {
	d := guardedDialer()
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
