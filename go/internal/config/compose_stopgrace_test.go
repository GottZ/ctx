package config

import (
	"bufio"
	"os"
	"strings"
	"testing"
	"time"
)

// minCtxStopGrace is the shutdown budget ctxd spends after SIGTERM before it
// lets the pool go (cmd/ctxd/main.go shutdownSequence): shutdownHTTPTimeout
// (15 s, srv.Shutdown) + llmlogSettleTimeout (5 s, the final llmlog.Settle).
// A stop grace period below it lets Docker SIGKILL the daemon mid-teardown —
// the compose default of 10 s did exactly that to the 15 s HTTP drain alone.
// When the shutdown grows a step (the /v1 drain window, K-06-29), the step's
// budget is added here and to docker-compose.yml together.
const minCtxStopGrace = 20 * time.Second

// TestComposeCtxStopGracePeriod pins W06-1 (d): the ctx service declares a
// stop_grace_period that covers the daemon's shutdown budget. Same caveat as
// the other compose gates in this package: docker-compose.yml lives outside
// the module, so run it with -count=1 after touching the file.
func TestComposeCtxStopGracePeriod(t *testing.T) {
	raw, ok := ctxServiceKey(t, "stop_grace_period")
	if !ok {
		t.Fatalf("the ctx service declares no stop_grace_period — Docker's 10 s default SIGKILLs ctxd before its %s shutdown budget ends", minCtxStopGrace)
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("ctx stop_grace_period %q is not a duration: %v", raw, err)
	}
	if d < minCtxStopGrace {
		t.Errorf("ctx stop_grace_period = %s, want >= %s (15 s HTTP shutdown + 5 s llmlog settle)", d, minCtxStopGrace)
	}
}

// ctxServiceKey returns the raw value of a service-level key (indent 4) of the
// `ctx` service in the repo-root docker-compose.yml — line-scanned for the same
// reason as ctxServiceEnvValues (no YAML dependency for one scalar). A trailing
// `# comment` and surrounding quotes are stripped.
func ctxServiceKey(t *testing.T, key string) (string, bool) {
	t.Helper()
	const path = "../../../docker-compose.yml" // go/internal/config -> repo root
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v (the compose gate needs the repo root checkout)", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only

	inCtx := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		switch {
		case indent == 2:
			inCtx = trimmed == "ctx:"
		case indent == 4 && inCtx:
			name, value, ok := strings.Cut(trimmed, ":")
			if !ok || strings.TrimSpace(name) != key {
				continue
			}
			value, _, _ = strings.Cut(value, "#")
			return strings.Trim(strings.TrimSpace(value), `"'`), true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return "", false
}
