// #265: /health probes its roles concurrently, so a healthy server answers in
// the time of its slowest role instead of the sum of all of them, and a base
// URL that several roles share is requested once per call. The egress bound of
// the PR #9 doctrine (pingHost) stays: a role still stops at its first
// reachable candidate and never requests more than its own ordered walk, the
// bound of a call stays the operator rows of the probed roles — sharing only
// shrinks what is requested.
//
// Fixture hygiene follows health_test.go: loopback httptest servers and closed
// loopback ports only.
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
)

// healthProbeDelay is how long each slow fake backend takes to answer. It is
// large against scheduler noise under -race, coverage and host load, and the
// gate built on it is relative: three probes of d each must finish in less
// than 2·d together. The sequential form needs at least 3·d, the concurrent
// one about d — the margin is d on either side.
const healthProbeDelay = 500 * time.Millisecond

// slowBackend serves GET / after d (or as soon as the prober gives up) and
// counts its requests in hits when hits is non-nil.
func slowBackend(t *testing.T, d time.Duration, hits *atomic.Int32) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// operatorRow is a _global pool row with the given roles on host.
func operatorRow(id, host string, roles ...string) backends.Backend {
	return backends.Backend{
		ID: id, Name: "row-" + id, Host: host, Enabled: true,
		Trust: backends.TrustFull, Roles: roles,
	}
}

// TestHealthProbesRolesConcurrently is the latency half of #265: three slow
// backends, one per role, and /health answers in about one probe's time.
func TestHealthProbesRolesConcurrently(t *testing.T) {
	d := healthProbeDelay
	st := &swapStore{}
	st.p.Store(healthTestConfig()) // dream enabled: three roles are probed
	bp := healthTestPool(slowBackend(t, d, nil), slowBackend(t, d, nil), slowBackend(t, d, nil))
	h := NewHealthHandler(brokenPool(t), st, bp, nil)

	rec := httptest.NewRecorder()
	start := time.Now()
	h.Health(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	elapsed := time.Since(start)

	var resp struct {
		Services map[string]string `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.Bytes())
	}
	for _, role := range []string{"embed", "chat", "dream"} {
		if resp.Services[role] != "ok" {
			t.Errorf("services[%s] = %q, want ok (the slow backend answers well inside the 5 s budget)", role, resp.Services[role])
		}
	}
	t.Logf("three role probes of %v each: /health answered in %v", d, elapsed)
	if elapsed >= 2*d {
		t.Errorf("/health took %v for three probes of %v each, want < %v — the roles are probed one after the other", elapsed, d, 2*d)
	}
}

// TestHealthProbesSharedHostOnce is the request half of #265: a base URL that
// several roles share is requested once per /health call, and the egress bound
// of the sequential form holds.
func TestHealthProbesSharedHostOnce(t *testing.T) {
	t.Run("one base URL serves three roles", func(t *testing.T) {
		var hits atomic.Int32
		url := slowBackend(t, 0, &hits)
		// The trailing slash is the same probe target (pingHost trims it).
		snap := []backends.Backend{
			operatorRow("e", url, backends.RoleEmbed),
			operatorRow("cd", url+"/", backends.RoleSynthesis, backends.RoleDream),
		}
		resp := HealthStatus(context.Background(), brokenPool(t), seededBackends(snap...), true, "ok")
		for _, role := range []string{"embed", "chat", "dream"} {
			if resp.Services[role] != "ok" {
				t.Errorf("services[%s] = %q, want ok", role, resp.Services[role])
			}
		}
		if n := hits.Load(); n != 1 {
			t.Errorf("the shared backend saw %d requests for one /health call, want exactly 1", n)
		}
	})

	t.Run("a fallback reuses the probe another role made", func(t *testing.T) {
		var hits atomic.Int32
		shared := slowBackend(t, 0, &hits)
		snap := []backends.Backend{
			operatorRow("down", closedPortHost(t), backends.RoleEmbed),
			operatorRow("shared", shared, backends.RoleEmbed, backends.RoleSynthesis),
		}
		resp := HealthStatus(context.Background(), brokenPool(t), seededBackends(snap...), false, "ok")
		if resp.Services["embed"] != "ok" || resp.Services["chat"] != "ok" {
			t.Errorf("services = %v, want embed ok (via its fallback) and chat ok", resp.Services)
		}
		if n := hits.Load(); n != 1 {
			t.Errorf("the shared backend saw %d requests, want exactly 1 (embed's fallback and chat's first candidate are one probe)", n)
		}
	})

	t.Run("a role stops at its first reachable candidate", func(t *testing.T) {
		var first, second atomic.Int32
		snap := []backends.Backend{
			operatorRow("a", slowBackend(t, 0, &first), backends.RoleEmbed),
			operatorRow("b", slowBackend(t, 0, &second), backends.RoleEmbed),
		}
		resp := HealthStatus(context.Background(), brokenPool(t), seededBackends(snap...), false, "ok")
		if resp.Services["embed"] != "ok" {
			t.Errorf("services[embed] = %q, want ok", resp.Services["embed"])
		}
		if first.Load() != 1 || second.Load() != 0 {
			t.Errorf("requests first=%d second=%d, want 1 and 0 — /health must not request more than the first reachable candidate of a role", first.Load(), second.Load())
		}
	})
}

// TestHostProbesFinishedResultStands: a role that reaches a probe another role
// already finished gets that result, also when the call's context has ended
// in the meantime (a slow earlier candidate used the whole budget). The answer
// must not depend on which of the two became ready first.
func TestHostProbesFinishedResultStands(t *testing.T) {
	var hits atomic.Int32
	url := slowBackend(t, 0, &hits)
	probes := newHostProbes()
	if err := probes.ping(context.Background(), url); err != nil {
		t.Fatalf("first probe: %v", err)
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	for i := range 100 {
		if err := probes.ping(ended, url); err != nil {
			t.Fatalf("lookup %d of a finished reachable probe on an ended context = %v, want its result (nil)", i, err)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("backend saw %d requests, want 1", n)
	}
}

// TestHealthStatusEndsWithItsContext: HealthStatus returns when the caller's
// context ends, against backends that never answer — the caller does not wait
// for its probe round, which goes on to its budget and records what it finds
// (issue #519).
func TestHealthStatusEndsWithItsContext(t *testing.T) {
	release := make(chan struct{})
	hang := func() string {
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	snap := []backends.Backend{
		operatorRow("e", hang(), backends.RoleEmbed),
		operatorRow("c", hang(), backends.RoleSynthesis),
		operatorRow("d", hang(), backends.RoleDream),
	}
	t.Cleanup(func() { waitHealthRounds(t) }) // runs after the release below
	t.Cleanup(func() { close(release) })      // runs before the srv.Close cleanups above
	pool := brokenPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan healthResponse, 1)
	go func() { done <- HealthStatus(ctx, pool, seededBackends(snap...), true, "ok") }()

	select {
	case resp := <-done:
		for _, role := range []string{"embed", "chat", "dream"} {
			if resp.Services[role] != "error" {
				t.Errorf("services[%s] = %q, want error (the backend never answered)", role, resp.Services[role])
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("HealthStatus did not return 10 s after its context ended — a probe outlives the call")
	}
}
