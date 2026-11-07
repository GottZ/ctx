package config

import (
	"bufio"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// minCtxStopGrace is the bounded part of the shutdown ctxd spends after
// SIGTERM before it lets the pool go (cmd/ctxd/main.go shutdownSequence):
// shutdownHTTPTimeout (15 s, srv.Shutdown) + rejectionFlushTimeout (5 s, the
// refusal ledger's final flush, its own step since W06-12) + 2 ×
// llmlogSettleTimeout (5 s each: one llmlog.Settle before scheduler.Wait,
// one after it); the model gateway's final flush (gatewayStopTimeout, 5 s,
// W02-05) runs alongside the first settle and adds nothing. scheduler.Wait
// itself is unbounded and only covered by the margin above this floor. A stop
// grace period below it lets Docker SIGKILL the daemon mid-teardown — the
// compose default of 10 s did exactly that to the 15 s HTTP drain alone. When
// the shutdown grows a step (the /v1 drain window, K-06-29), the step's budget
// is added here and to docker-compose.yml together.
const minCtxStopGrace = 30 * time.Second

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
		t.Errorf("ctx stop_grace_period = %s, want >= %s (15 s HTTP shutdown + 5 s refusal ledger flush + 2 × 5 s llmlog settle, the gateway flush alongside the first)", d, minCtxStopGrace)
	}
}

// ctxServiceKey returns the raw value of a service-level key (indent 4) of the
// `ctx` service in the repo-root docker-compose.yml — line-scanned for the same
// reason as ctxServiceEnvValues (no YAML dependency for one scalar).
func ctxServiceKey(t *testing.T, key string) (string, bool) {
	t.Helper()
	const path = "../../../docker-compose.yml" // go/internal/config -> repo root
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v (the compose gate needs the repo root checkout)", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only
	v, ok, err := scanCtxServiceKey(f, key)
	if err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return v, ok
}

// scanCtxServiceKey is the scanner behind ctxServiceKey. It follows the
// top-level section, so only the `ctx` entry under `services:` counts — a
// `ctx` volume or network is not the service. Keys may be quoted and may carry
// a trailing comment (`ctx: # the daemon`); the returned value has its comment
// and quotes stripped.
func scanCtxServiceKey(r io.Reader, key string) (string, bool, error) {
	section, inCtx := "", false
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		name, value, isKey := yamlKeyValue(trimmed)
		switch {
		case indent == 0:
			section, inCtx = "", false
			if isKey {
				section = name
			}
		case indent == 2:
			inCtx = section == "services" && isKey && name == "ctx"
		case indent == 4 && inCtx && isKey && name == key:
			return value, true, nil
		}
	}
	return "", false, sc.Err()
}

// yamlKeyValue splits one trimmed `key: value  # comment` mapping line into
// its unquoted key and its value without comment and quotes. A `#` starts a
// comment only at the start of the value or after a space (YAML rule).
func yamlKeyValue(trimmed string) (key, value string, ok bool) {
	k, v, ok := strings.Cut(trimmed, ":")
	if !ok {
		return "", "", false
	}
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "#") {
		v = ""
	} else if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return strings.Trim(strings.TrimSpace(k), `"'`), strings.Trim(v, `"'`), true
}

// TestCtxServiceKeyScanner pins the scanner behind the stop_grace_period gate
// against the compose shapes that fooled the first version: a `ctx:` entry
// under another top-level section (volumes, networks) must not count, while
// a comment after `ctx:` and a quoted service key must still be found.
func TestCtxServiceKeyScanner(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       string
		found      bool
	}{
		{"plain", "services:\n  ctx:\n    image: x\n    stop_grace_period: 30s\n", "30s", true},
		{"value_comment_and_quotes", "services:\n  ctx:\n    stop_grace_period: \"45s\" # why\n", "45s", true},
		{"other_service_only", "services:\n  ctx-rerank:\n    stop_grace_period: 30s\n  ctx:\n    image: x\n", "", false},
		{"ctx_under_volumes_is_not_the_service",
			"services:\n  ctx:\n    image: x\nvolumes:\n  ctx:\n    stop_grace_period: 30s\n", "", false},
		{"comment_after_service_key", "services:\n  ctx: # the daemon\n    stop_grace_period: 30s\n", "30s", true},
		{"quoted_service_key", "services:\n  \"ctx\":\n    stop_grace_period: 30s\n", "30s", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, found, err := scanCtxServiceKey(strings.NewReader(tc.yaml), "stop_grace_period")
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if found != tc.found || got != tc.want {
				t.Errorf("scan = (%q, %v), want (%q, %v)", got, found, tc.want, tc.found)
			}
		})
	}
}
