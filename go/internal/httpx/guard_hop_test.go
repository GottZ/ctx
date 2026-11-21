package httpx

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
)

// #459, tenant view: a tenant row's base_url passes the write-time check and
// resolves to a public address; its server then answers with a redirect to
// an internal address. Two independent layers keep the hop from being sent:
// the origin rule refuses any Location off the row's origin, and the
// ssrfguard dialer of the guarded pool judges the resolved address of every
// dial — first request and each hop alike. Both are driven here through the
// production constructor; only the first hop's dial is routed to a loopback
// listener standing in for the public host.

// publicStandIn is the row's host; the test dialer routes it, and only it,
// to a loopback listener. Every other address goes to the guarded dialer.
const publicStandIn = "tenant-backend.example:80"

func guardedWithStandIn(t *testing.T, standIn string) *http.Client {
	t.Helper()
	c := GuardedPooledClient()
	tr := c.Transport.(*http.Transport).Clone()
	guarded := tr.DialContext
	var plain net.Dialer
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == publicStandIn {
			return plain.DialContext(ctx, network, standIn)
		}
		return guarded(ctx, network, addr)
	}
	c.Transport = tr
	t.Cleanup(tr.CloseIdleConnections)
	return c
}

func TestGuardedPoolRedirectToInternalAddress(t *testing.T) {
	internal := newRedirectSink(t, false, nil)
	_, port, _ := net.SplitHostPort(internal.addr())
	targets := []string{
		"http://127.0.0.1:" + port + "/v1/chat/completions",
		"http://localhost:" + port + "/v1/chat/completions",
		"http://[::1]:" + port + "/v1/chat/completions",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/",
	}
	for _, loc := range targets {
		row := newRedirectSink(t, false, redirectTo(http.StatusTemporaryRedirect, loc))

		// Layer 1, the production client: the hop leaves the row's origin
		// and is not followed; the caller gets the 307 itself.
		c := guardedWithStandIn(t, row.addr())
		code, err := postWithCredentials(t, c, "http://"+publicStandIn+"/v1/chat/completions")
		if err != nil || code != http.StatusTemporaryRedirect {
			t.Errorf("%s: status %d, err %v; want the 307 itself", loc, code, err)
		}

		// Layer 2, the same transport without the origin rule (a client
		// whose CheckRedirect lets the hop through): the guard refuses the
		// hop's dial by its resolved address.
		open := guardedWithStandIn(t, row.addr())
		open.CheckRedirect = nil
		_, err = postWithCredentials(t, open, "http://"+publicStandIn+"/v1/chat/completions")
		if err == nil || !strings.Contains(err.Error(), "SSRF guard") {
			t.Errorf("%s: the hop's dial was not refused by the guard: %v", loc, err)
		}
		if row.hits() != 2 {
			t.Errorf("%s: the row's server saw %d requests, want 2 (one per layer)", loc, row.hits())
		}
	}
	if n := internal.hits(); n != 0 {
		t.Fatalf("the internal address was reached: %s", internal.log())
	}
}
