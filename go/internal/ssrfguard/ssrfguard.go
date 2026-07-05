// Package ssrfguard is the shared dial-time SSRF guard: it refuses to connect to
// loopback / private / link-local / unspecified addresses AFTER DNS resolution,
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
	"syscall"
	"time"
)

// IsDeniedIP reports whether ip is an SSRF-forbidden target: loopback (127/8,
// ::1), private (RFC1918 + fd00::/8), link-local (169.254/16 incl. the cloud
// metadata endpoint 169.254.169.254, + fe80::/10), or unspecified (0.0.0.0/::).
func IsDeniedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified()
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
		return fmt.Errorf("ssrfguard: refusing to dial %s — private/loopback/link-local (SSRF guard)", ip)
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
