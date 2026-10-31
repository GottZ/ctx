// Package ssrfguard is the shared dial-time SSRF guard: it refuses to connect to
// loopback / private / link-local / CGNAT / unspecified / multicast / reserved
// and other special-purpose addresses (deniedPrefixes) AFTER DNS resolution,
// so a hostname that resolves publicly at validate time but privately at fetch
// time (DNS rebinding) is still caught. The net.Dialer.Control hook fires with
// the concrete resolved ip:port about to be dialed and re-checks it against the
// deny-list — whatever the hostname was, once it resolves to a forbidden range
// the dial aborts before any bytes flow.
//
// It exists to end the "one intentional duplication" of the deny-list: the same
// check was copied into internal/forge (the forge sync client) and paraphrased in
// internal/handler.isDeniedHost (forge api_base validation). Both now delegate to
// this package, and the Camo image proxy (internal/camo) uses the identical
// dialer — one deny-list, no drift (design 07-camo §5, D8).
package ssrfguard

import (
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// deniedPrefixes is the SSRF deny-list: every IANA special-purpose block
// (RFC 6890 and its successors) that is not globally reachable unicast, plus
// multicast and the reserved/broadcast space. An explicit table instead of
// the net.IP predicates (GHSA 2026-10-31 §9d): IsPrivate/IsLoopback/
// IsLinkLocal* left CGNAT (100.64.0.0/10 — Alibaba's metadata endpoint
// 100.100.100.200 sits there), 0.0.0.0/8 beyond 0.0.0.0, 192.0.0.0/24,
// 198.18.0.0/15, 240.0.0.0/4, most multicast, NAT64 and site-local open.
// IPv4-mapped IPv6 (::ffff:a.b.c.d) is unmapped BEFORE the lookup, so the
// IPv4 rows cover it.
var deniedPrefixes = []netip.Prefix{
	// IPv4
	netip.MustParsePrefix("0.0.0.0/8"),          // "this network" (RFC 791/1122)
	netip.MustParsePrefix("10.0.0.0/8"),         // private (RFC 1918)
	netip.MustParsePrefix("100.64.0.0/10"),      // shared address space / CGNAT (RFC 6598)
	netip.MustParsePrefix("127.0.0.0/8"),        // loopback
	netip.MustParsePrefix("169.254.0.0/16"),     // link-local, cloud metadata (RFC 3927)
	netip.MustParsePrefix("172.16.0.0/12"),      // private (RFC 1918)
	netip.MustParsePrefix("192.0.0.0/24"),       // IETF protocol assignments (RFC 6890)
	netip.MustParsePrefix("192.0.2.0/24"),       // TEST-NET-1 (RFC 5737)
	netip.MustParsePrefix("192.88.99.0/24"),     // 6to4 relay anycast (RFC 7526)
	netip.MustParsePrefix("192.168.0.0/16"),     // private (RFC 1918)
	netip.MustParsePrefix("198.18.0.0/15"),      // benchmarking (RFC 2544)
	netip.MustParsePrefix("198.51.100.0/24"),    // TEST-NET-2 (RFC 5737)
	netip.MustParsePrefix("203.0.113.0/24"),     // TEST-NET-3 (RFC 5737)
	netip.MustParsePrefix("224.0.0.0/4"),        // multicast (RFC 5771)
	netip.MustParsePrefix("240.0.0.0/4"),        // reserved (RFC 1112)
	netip.MustParsePrefix("255.255.255.255/32"), // limited broadcast (inside 240/4; listed for the record)
	// IPv6
	netip.MustParsePrefix("::/128"),         // unspecified
	netip.MustParsePrefix("::1/128"),        // loopback
	netip.MustParsePrefix("::/96"),          // IPv4-compatible, deprecated (RFC 4291)
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64 well-known prefix (RFC 6052)
	netip.MustParsePrefix("64:ff9b:1::/48"), // NAT64 local-use (RFC 8215)
	netip.MustParsePrefix("100::/64"),       // discard-only (RFC 6666)
	netip.MustParsePrefix("2001::/23"),      // IETF protocol assignments incl. Teredo (RFC 2928/4380)
	netip.MustParsePrefix("2001:db8::/32"),  // documentation (RFC 3849)
	netip.MustParsePrefix("2002::/16"),      // 6to4, embeds IPv4 (RFC 3056)
	netip.MustParsePrefix("3fff::/20"),      // documentation (RFC 9637)
	netip.MustParsePrefix("fc00::/7"),       // unique-local (RFC 4193)
	netip.MustParsePrefix("fe80::/10"),      // link-local
	netip.MustParsePrefix("fec0::/10"),      // site-local, deprecated (RFC 3879)
	netip.MustParsePrefix("ff00::/8"),       // multicast
}

// IsDeniedIP reports whether ip is an SSRF-forbidden target: any address in
// deniedPrefixes — loopback (127/8, ::1), private (RFC 1918, fc00::/7),
// link-local (169.254/16 incl. the cloud metadata endpoint 169.254.169.254,
// fe80::/10), CGNAT (100.64/10), unspecified/"this network" (0/8, ::),
// multicast, reserved/broadcast, documentation/benchmark ranges, NAT64,
// 6to4/Teredo and site-local. IPv4-mapped IPv6 is unmapped first. A nil or
// malformed ip is denied (fail-closed).
func IsDeniedIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	addr = addr.Unmap()
	for _, p := range deniedPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// DialControl is the net.Dialer.Control hook: address is the resolved "ip:port"
// the dialer is about to connect to. A non-IP or denied address aborts the dial.
// The error carries the "SSRF guard" marker so callers can distinguish a guard
// refusal from an ordinary connection-refused.
func DialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("ssrfguard: unparseable dial address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("ssrfguard: refusing to dial non-IP address %q", host)
	}
	if IsDeniedIP(ip) {
		return fmt.Errorf("ssrfguard: refusing to dial %s — private/loopback/link-local/special-purpose (SSRF guard)", ip)
	}
	return nil
}

// GuardedDialer returns a net.Dialer whose Control hook enforces the dial-time
// deny-list. Timeout/KeepAlive match the forge production client (the original
// home of this guard); callers that need tighter connect timeouts wrap the
// returned dialer's DialContext behind their own http.Client.Timeout.
func GuardedDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   DialControl,
	}
}
