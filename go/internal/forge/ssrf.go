// Dial-time SSRF guard (design/03 §5.7 point 2 / §9.2(h) — the "Achse-02 client
// contract"). handler.validateForge already rejects a private/loopback/link-local
// forge.api_base at PATCH time, but that check is blind to DNS rebinding: a
// hostname can resolve publicly at PATCH time and privately at sync time. The
// net.Dialer.Control hook fires AFTER resolution with the concrete ip:port about
// to be dialed, so it re-checks the RESOLVED address against the same deny-list —
// whatever the hostname was, once it resolves to a forbidden range we refuse.
package forge

import (
	"fmt"
	"net"
	"syscall"
	"time"
)

// isDeniedIP reports whether ip is an SSRF-forbidden target: loopback (127/8,
// ::1), private (RFC1918 + fd00::/8), link-local (169.254/16 incl. the cloud
// metadata endpoint 169.254.169.254, + fe80::/10), or unspecified (0.0.0.0/::).
// Mirrors handler.isDeniedHost's IP branch (§5.7) — kept in-package so forge does
// not import handler (layering), the one intentional duplication of the deny-list.
func isDeniedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified()
}

// ssrfDialControl is the net.Dialer.Control hook: address is the resolved
// "ip:port" the dialer is about to connect to. A non-IP or denied address aborts
// the dial (the sync run then treats it as a wire error → backoff, never a
// conflict). This is the dial-time half of the two-layer guard (§5.7).
func ssrfDialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("forge: unparseable dial address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("forge: refusing to dial non-IP address %q", host)
	}
	if isDeniedIP(ip) {
		return fmt.Errorf("forge: refusing to dial %s — private/loopback/link-local (SSRF guard §5.7)", ip)
	}
	return nil
}

// guardedDialer is the SSRF-guarded dialer shared by the production forge client.
func guardedDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   ssrfDialControl,
	}
}
