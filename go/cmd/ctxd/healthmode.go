// Container health probe mode: `/ctx -health` (analogous to -secret-decrypt).
// The distroless image carries neither curl nor wget, so the binary probes
// itself; docker-compose.yml wires it as the ctx service healthcheck.
//
// SERVING SEMANTICS (E9, design/02 §5.2/§8): the probe answers "can this
// process still serve requests", NOT "is everything green". It fails only on
// three states — the server is unreachable, its answer is not a health
// document, or its own database leg is not ok. Everything else, /health's
// 503 included, counts as serving.
//
// Why the HTTP status code stopped being the verdict: from the β-Schnitt on,
// a fresh install boots with an empty backend pool and stays there until the
// operator seeds it (design/02 §4.2). /health calls that unhealthy/503 by
// design — correctly, no LLM role serves — but a container that reports
// unhealthy for a state only an operator can leave turns every fresh install
// permanently Docker-unhealthy. Anything gated on `depends_on:
// service_healthy` or an LB readiness probe then blocks exactly the window in
// which the seed has to happen; where the LB gates API access, the seed
// becomes unreachable through the very door it needs (hen-and-egg on the
// orchestration layer). The database leg is what the healthcheck genuinely
// exists for, and it stays a hard fail.
//
// The compose file is deliberately NOT touched: foreign compose copies do not
// update with a release, so the semantics have to travel in the binary to
// reach the installed base at all (design/02 §8 E9).

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

// healthBodyCap bounds the parsed health document. /health answers a handful
// of service strings; anything larger is not a health document, and reading
// it unbounded would let a wedged or hijacked listener feed the probe until
// the container OOMs.
const healthBodyCap = 1 << 20 // 1 MiB

// healthCheckURL builds the local probe URL. It reads LISTEN_ADDR RAW and
// falls back to the registry default: the probe must work in a crash-looping
// container where the full config load (settings overlay, DB) cannot run.
func healthCheckURL(getenv func(string) string) string {
	addr := getenv("LISTEN_ADDR")
	if addr == "" {
		addr = defaultListenAddr
	}
	return fmt.Sprintf("http://localhost%s/health", addr)
}

// runHealthCheck implements the -health mode against url and returns the
// process exit code (0 = serving, 1 = not serving). Diagnostics go to stderr;
// every failure names which of the three states it is, because the operator
// reading `docker inspect` output has nothing else to go on.
//
// A missing services.database is a failure, not a pass: an answer that parses
// as JSON but carries no database verdict is a document this probe cannot
// judge, and an unjudgeable answer must never read as healthy.
func runHealthCheck(url string, stderr io.Writer) int {
	resp, err := http.Get(url) //nolint:gosec,noctx // healthcheck is fire-and-forget against the loopback listener
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "health check failed: %v\n", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()

	var doc struct {
		Services map[string]string `json:"services"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, healthBodyCap)).Decode(&doc); err != nil {
		_, _ = fmt.Fprintf(stderr, "health check: unreadable health document (status %d): %v\n", resp.StatusCode, err)
		return 1
	}

	db, ok := doc.Services["database"]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "health check: response carries no services.database (status %d)\n", resp.StatusCode)
		return 1
	}
	if db != "ok" {
		_, _ = fmt.Fprintf(stderr, "health check: database %s (status %d)\n", db, resp.StatusCode)
		return 1
	}
	return 0
}

// dispatchHealthMode runs the -health mode and NEVER RETURNS when argv[1]
// selects it (os.Exit — the -secret-decrypt precedent).
func dispatchHealthMode() {
	if len(os.Args) > 1 && os.Args[1] == "-health" {
		os.Exit(runHealthCheck(healthCheckURL(os.Getenv), os.Stderr))
	}
}
