// PR #9 contract: the /health backend probe is a REACHABILITY check, not a
// model-availability check, and it is credential-free. These tests pin both
// halves of that contract:
//
//   - status class decides: < 500 = reachable (200 local server, 404/405 cloud
//     API root, 401/403 auth-gated API), >= 500 and transport failures = down;
//   - the probe never carries the backend's Authorization/ExtraHeaders, and it
//     issues exactly ONE request — /health is unauthenticated and uncached, so
//     both properties bound what an anonymous poller can make this service emit.
//
// Fixture hygiene follows health_test.go: loopback httptest servers and closed
// loopback ports only (deterministic reachability, no resolver in the path).
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// TestPingHostStatusClass pins the reachability semantics per status code.
// The 404 row is the PR #9 case itself: a cloud API (Voyage and friends)
// routes nothing at its root and answers 404 — the host is demonstrably up.
func TestPingHostStatusClass(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		reachable bool
		wantErr   string
	}{
		{name: "200 local server", status: http.StatusOK, reachable: true},
		{name: "404 cloud api root", status: http.StatusNotFound, reachable: true},
		{name: "405 method not allowed", status: http.StatusMethodNotAllowed, reachable: true},
		{name: "401 auth gated api", status: http.StatusUnauthorized, reachable: true},
		{name: "503 backend down", status: http.StatusServiceUnavailable, reachable: false, wantErr: "status 503"},
		{name: "500 backend broken", status: http.StatusInternalServerError, reachable: false, wantErr: "status 500"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"detail":"probe body"}`))
			}))
			t.Cleanup(srv.Close)

			err := pingHost(context.Background(), srv.URL)

			if tc.reachable && err != nil {
				t.Errorf("status %d: pingHost = %v, want reachable", tc.status, err)
			}
			if !tc.reachable {
				if err == nil {
					t.Fatalf("status %d: pingHost = nil, want unreachable", tc.status)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not name the cause class %q", err, tc.wantErr)
				}
			}
			// One probe = one request. A multi-phase probe would double the
			// egress every unauthenticated /health call produces.
			if got := hits.Load(); got != 1 {
				t.Errorf("%d requests for one probe, want 1", got)
			}
		})
	}
}

// TestPingHostTransportFailure pins that a refused connection is unreachable
// AND that the wrapped transport error survives into the /health warn log —
// the only diagnostic that path has.
func TestPingHostTransportFailure(t *testing.T) {
	err := pingHost(context.Background(), closedPortHost(t))
	if err == nil {
		t.Fatal("pingHost on a closed port = nil, want unreachable")
	}
	msg := err.Error()
	if !strings.Contains(msg, "connecting to host") {
		t.Errorf("error %q does not classify the failure as a transport error", msg)
	}
	if !strings.Contains(msg, "connect") && !strings.Contains(msg, "refused") {
		t.Errorf("error %q drops the underlying transport cause", msg)
	}
}

// TestPingHostTrimsTrailingSlash pins the base-URL normalisation: a pool row
// stored as "http://host/" probes the same target as "http://host".
func TestPingHostTrimsTrailingSlash(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	if err := pingHost(context.Background(), srv.URL+"/"); err != nil {
		t.Fatalf("pingHost with trailing slash: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 {
		t.Fatalf("%d requests, want 1", len(paths))
	}
	if paths[0] != "/" {
		t.Errorf("probe path = %q, want /", paths[0])
	}
}

// TestRoleReachableProbeIsCredentialFree is the security half of the PR #9
// contract: even a backend that carries an API key AND provider headers must
// be probed anonymously. /health is unauthenticated — anything the probe sends
// is egress an anonymous caller can trigger, so no secret may ride along, and
// the probe must not multiply into several requests.
func TestRoleReachableProbeIsCredentialFree(t *testing.T) {
	var hits atomic.Int32
	var mu sync.Mutex
	var seen http.Header
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		mu.Lock()
		seen = r.Header.Clone()
		seenPath = r.URL.Path
		mu.Unlock()
		// A cloud API root: nothing served here, host clearly alive.
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	t.Cleanup(srv.Close)

	snap := []backends.Backend{{
		ID: "cloud", Name: "needle-backend-cloud", Host: srv.URL, Enabled: true,
		Trust: backends.TrustFull, Roles: []string{backends.RoleEmbed},
		APIKey: "sk-needle-must-never-egress-0123456789",
		ExtraHeaders: map[string]string{
			"X-Api-Key":         "needle-extra-key",
			"Anthropic-Version": "2023-06-01",
		},
	}}

	if got := roleReachable(context.Background(), snap, backends.RoleEmbed); got != "ok" {
		t.Errorf("roleReachable on a 404-answering cloud root = %q, want ok (PR #9 false negative)", got)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("%d requests for one backend, want exactly 1", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if seenPath != "/" {
		t.Errorf("probe path = %q, want / (no /v1/models phase)", seenPath)
	}
	if v := seen.Get("Authorization"); v != "" {
		t.Errorf("probe carried Authorization %q — credentials must never leave via /health", v)
	}
	for _, k := range []string{"X-Api-Key", "Anthropic-Version"} {
		if v := seen.Get(k); v != "" {
			t.Errorf("probe carried backend ExtraHeader %s=%q — the probe must stay anonymous", k, v)
		}
	}
}

// TestRoleReachableFiveXXIsDown pins that a 5xx backend does not count as a
// reachable role: the status class is the whole gate, and an overloaded or
// broken host must still surface as "error".
func TestRoleReachableFiveXXIsDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	snap := []backends.Backend{{
		ID: "broken", Name: "needle-backend-broken", Host: srv.URL, Enabled: true,
		Trust: backends.TrustFull, Roles: []string{backends.RoleEmbed},
	}}

	if got := roleReachable(context.Background(), snap, backends.RoleEmbed); got != "error" {
		t.Errorf("roleReachable on a 502 host = %q, want error", got)
	}
}
