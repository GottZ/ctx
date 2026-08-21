package main

// A02-W6/β1 (E9) — the serving semantics of `/ctx -health`.
//
// Load-bearing because the probe is the only thing standing between a fresh
// install and a permanently Docker-unhealthy container: from the β-Schnitt on
// an unseeded pool is the ORDINARY state of a new deployment, and /health
// answers 503 for it by design. A probe that keeps reading the status code as
// the verdict would gate `depends_on: service_healthy` and LB readiness on a
// state only an operator can leave. The other direction is just as easy to
// break silently: a probe that passes on anything it receives stops noticing
// the one failure the healthcheck genuinely exists for — a dead database.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// healthServer answers one canned /health document with one status code.
func healthServer(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/health"
}

// TestHealthServingSemantics is the wave gate: the verdict follows
// services.database, not the HTTP status code.
//
// Mutation probe: reinstate `if resp.StatusCode != http.StatusOK { return 1 }`
// in runHealthCheck and the two 503-with-database-ok rows go red.
func TestHealthServingSemantics(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{
			name:   "everything green",
			status: http.StatusOK,
			body:   `{"status":"ok","services":{"database":"ok","embed":"ok","chat":"ok"}}`,
			want:   0,
		},
		{
			name:   "empty pool: unhealthy/503 but the database serves",
			status: http.StatusServiceUnavailable,
			body:   `{"status":"unhealthy","services":{"database":"ok","embed":"unreachable","chat":"unreachable"}}`,
			want:   0,
		},
		{
			name:   "degraded chat still serves",
			status: http.StatusOK,
			body:   `{"status":"degraded","services":{"database":"ok","embed":"ok","chat":"unreachable"}}`,
			want:   0,
		},
		{
			name:   "database error is the hard fail",
			status: http.StatusServiceUnavailable,
			body:   `{"status":"unhealthy","services":{"database":"error","embed":"ok","chat":"ok"}}`,
			want:   1,
		},
		{
			name:   "database error on a 200 body fails too",
			status: http.StatusOK,
			body:   `{"status":"ok","services":{"database":"error"}}`,
			want:   1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stderr bytes.Buffer
			got := runHealthCheck(healthServer(t, c.status, c.body), &stderr)
			if got != c.want {
				t.Errorf("exit code %d, want %d (stderr: %s)", got, c.want, stderr.String())
			}
		})
	}
}

// TestHealthUnjudgeableAnswersFail pins the fail-closed half: an answer this
// probe cannot judge must never read as healthy. Covers both shapes the
// design names — unparsable body and a document without the database verdict
// — plus the empty body a wedged listener produces.
//
// Mutation probe: default the missing-key branch to "ok" and the last two
// rows go green while a database-less answer passes the healthcheck.
func TestHealthUnjudgeableAnswersFail(t *testing.T) {
	cases := map[string]string{
		"html error page":        `<html><body>502 Bad Gateway</body></html>`,
		"empty body":             ``,
		"truncated json":         `{"services":{"database":"o`,
		"no services key":        `{"status":"ok"}`,
		"services without db":    `{"status":"ok","services":{"embed":"ok"}}`,
		"services is not an obj": `{"status":"ok","services":"ok"}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := runHealthCheck(healthServer(t, http.StatusOK, body), &stderr); got != 1 {
				t.Errorf("exit code %d, want 1 — an unjudgeable answer passed the healthcheck", got)
			}
			if stderr.Len() == 0 {
				t.Error("failed probe wrote nothing to stderr — docker inspect shows the operator nothing")
			}
		})
	}
}

// TestHealthUnreachableFails: no listener at all is the third failure state.
func TestHealthUnreachableFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL + "/health"
	srv.Close() // the port is now dead

	var stderr bytes.Buffer
	if got := runHealthCheck(url, &stderr); got != 1 {
		t.Errorf("exit code %d, want 1 — an unreachable server passed the healthcheck", got)
	}
	if !strings.Contains(stderr.String(), "health check failed") {
		t.Errorf("unreachable server did not report the transport error: %q", stderr.String())
	}
}

// TestHealthCheckURL pins the crash-loop-safe address resolution: LISTEN_ADDR
// raw, registry default as fallback (TestBootDefaults pins that the constant
// and the registry agree).
func TestHealthCheckURL(t *testing.T) {
	cases := map[string]string{
		"":      "http://localhost" + defaultListenAddr + "/health",
		":9090": "http://localhost:9090/health",
	}
	for addr, want := range cases {
		got := healthCheckURL(func(k string) string {
			if k != "LISTEN_ADDR" {
				t.Errorf("probe read env %q — it must read LISTEN_ADDR only", k)
			}
			return addr
		})
		if got != want {
			t.Errorf("LISTEN_ADDR=%q → %q, want %q", addr, got, want)
		}
	}
}
