package ssrfguard

import (
	"context"
	"errors"
	"net"
	"net/netip"
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
// forge api_base host validator and the Camo fetch guard). The second half
// names every GHSA 2026-10-31 §9d gap the former net.IP predicates left open.
func TestIsDeniedIP(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":       true,
		"10.1.2.3":        true,
		"172.16.0.1":      true,
		"192.168.0.1":     true,
		"169.254.169.254": true,
		"::1":             true,
		"fd00::1":         true,
		"fc00::1":         true, // fc00::/7, not only fd00::/8
		"0.0.0.0":         true,
		"8.8.8.8":         false,
		"140.82.112.3":    false,
		"2606:50c0::1":    false,
		"1.1.1.1":         false,
		"2001:4860::8888": false, // outside 2001::/23
		"::ffff:8.8.8.8":  false, // mapped public IPv4 stays allowed

		// §9d gaps.
		"100.64.0.1":         true, // CGNAT
		"100.100.100.200":    true, // Alibaba metadata
		"100.127.255.254":    true,
		"0.1.2.3":            true, // 0/8 beyond 0.0.0.0
		"192.0.0.8":          true,
		"198.18.0.1":         true,
		"198.19.255.254":     true,
		"240.0.0.1":          true,
		"255.255.255.255":    true,
		"224.0.1.1":          true, // multicast outside 224.0.0.0/24
		"239.255.255.250":    true, // SSDP
		"64:ff9b::7f00:1":    true, // NAT64 → 127.0.0.1
		"64:ff9b::a9fe:a9fe": true, // NAT64 → 169.254.169.254
		"64:ff9b:1::1":       true,
		"fec0::1":            true,
		"ff0e::1":            true,
		"2002:7f00:1::":      true, // 6to4 of 127.0.0.1
		"2001:0:4136:e378::": true, // Teredo
		"::127.0.0.1":        true, // IPv4-compatible
		// IPv4-mapped variants are unmapped before the lookup.
		"::ffff:127.0.0.1":       true,
		"::ffff:100.100.100.200": true,
		"::ffff:169.254.169.254": true,
		"::ffff:198.18.0.1":      true,
		"::ffff:0.1.2.3":         true,
		"::ffff:240.0.0.1":       true,
		// Neighbours just outside the new rows stay allowed.
		"100.63.255.255":  false,
		"100.128.0.0":     false,
		"198.17.255.255":  false,
		"198.20.0.0":      false,
		"1.0.0.0":         false,
		"223.255.255.255": false,
	}
	for s, want := range cases {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test literal %q", s)
		}
		if got := IsDeniedIP(ip); got != want {
			t.Errorf("IsDeniedIP(%s) = %v, want %v", s, got, want)
		}
	}
	if !IsDeniedIP(nil) || !IsDeniedIP(net.IP{1, 2, 3}) {
		t.Error("IsDeniedIP must fail closed on a nil/malformed IP")
	}
}

// TestIsDeniedIP_EveryTableRow walks deniedPrefixes: the first and last
// address of every row — and, for IPv4 rows, both in 4-byte and in
// IPv4-mapped 16-byte form — is denied, through IsDeniedIP and through the
// dial-time DialControl hook.
func TestIsDeniedIP_EveryTableRow(t *testing.T) {
	if len(deniedPrefixes) < 30 {
		t.Fatalf("deniedPrefixes has %d rows — table truncated?", len(deniedPrefixes))
	}
	for _, p := range deniedPrefixes {
		first := p.Masked().Addr()
		last := lastAddr(p)
		for _, a := range []netip.Addr{first, last} {
			probes := []net.IP{net.IP(a.AsSlice())}
			if a.Is4() {
				m := netip.AddrFrom16(a.As16())
				probes = append(probes, net.IP(m.AsSlice()))
			}
			for _, ip := range probes {
				if !IsDeniedIP(ip) {
					t.Errorf("row %s: IsDeniedIP(%s) = false, want true", p, ip)
				}
				if err := DialControl("tcp", net.JoinHostPort(ip.String(), "443"), nil); err == nil {
					t.Errorf("row %s: DialControl(%s) = nil, want refusal", p, ip)
				}
			}
		}
	}
}

// lastAddr returns the highest address inside p.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().AsSlice()
	bits := p.Bits()
	for i := range b {
		for j := 0; j < 8; j++ {
			if i*8+j >= bits {
				b[i] |= 0x80 >> j
			}
		}
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}
