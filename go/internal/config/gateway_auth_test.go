package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestValidateGatewayAuth is V40 (wave W01-6): the model-key door's four
// keys refuse the values that parse and still cannot mean what they say —
// one error each — and accept their legal edges.
func TestValidateGatewayAuth(t *testing.T) {
	g := func(c *Config) *GatewayConfig { return &c.Gateway }
	cases := []struct {
		key string
		mut func(c *Config)
	}{
		{"gateway.auth_failure_limit", func(c *Config) { g(c).AuthFailureLimit = 0 }},
		{"gateway.auth_failure_window", func(c *Config) { g(c).AuthFailureWindow = 0 }},
		{"gateway.auth_probe_rate", func(c *Config) { g(c).AuthProbeRate = 0 }},
		{"gateway.auth_db_concurrency", func(c *Config) { g(c).AuthDBConcurrency = 0 }},
		{"gateway.auth_db_concurrency", func(c *Config) { g(c).AuthDBConcurrency = gatewayDBPoolConns }},
		{"gateway.auth_db_concurrency", func(c *Config) { g(c).AuthDBConcurrency = 1000 }},
	}
	for _, tc := range cases {
		c := Defaults()
		tc.mut(c)
		hits := issuesOn(Validate(c), tc.key)
		if len(hits) != 1 || hits[0].Severity != SeverityError {
			t.Errorf("%s: want exactly one error, got %+v", tc.key, hits)
		}
	}
	legal := []struct {
		name string
		mut  func(c *Config)
	}{
		{"one failure locks", func(c *Config) { g(c).AuthFailureLimit = 1 }},
		{"one-second window", func(c *Config) { g(c).AuthFailureWindow = time.Second }},
		{"one probe per second", func(c *Config) { g(c).AuthProbeRate = 1 }},
		{"concurrency one below the pool", func(c *Config) { g(c).AuthDBConcurrency = gatewayDBPoolConns - 1 }},
	}
	for _, tc := range legal {
		c := Defaults()
		tc.mut(c)
		if issues := validateGateway(c); len(issues) != 0 {
			t.Errorf("%s is legal, got %+v", tc.name, issues)
		}
	}
	d := Defaults().Gateway
	if d.AuthFailureLimit != 30 || d.AuthFailureWindow != 60*time.Second || d.AuthProbeRate != 50 || d.AuthDBConcurrency != 4 {
		t.Errorf("defaults %d/%v/%d/%d, want 30/60s/50/4 (design/01 §4.9)", d.AuthFailureLimit, d.AuthFailureWindow, d.AuthProbeRate, d.AuthDBConcurrency)
	}
}

// TestGatewayDBPoolMirror pins gatewayDBPoolConns to the pool size
// store/pool.go actually configures: config may not import the store, so V40
// checks against a mirror, and the mirror must not drift.
func TestGatewayDBPoolMirror(t *testing.T) {
	src, err := os.ReadFile("../store/pool.go")
	if err != nil {
		t.Fatalf("read store/pool.go: %v", err)
	}
	m := regexp.MustCompile(`(?m)^\s*config\.MaxConns = (\d+)\s*$`).FindAllSubmatch(src, -1)
	if len(m) != 1 {
		t.Fatalf("store/pool.go: %d MaxConns assignment(s), want exactly 1", len(m))
	}
	n, _ := strconv.Atoi(string(m[0][1]))
	if n != gatewayDBPoolConns {
		t.Fatalf("store/pool.go MaxConns = %d, config mirror gatewayDBPoolConns = %d", n, gatewayDBPoolConns)
	}
}

// TestBootDumpShowsGatewayAuth: the boot record "config: effective" carries
// the door's four limits inside the gateway group.
func TestBootDumpShowsGatewayAuth(t *testing.T) {
	args := BootDumpArgs(Defaults(), nil)
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] != "gateway" {
			continue
		}
		dump := fmt.Sprint(args[i+1])
		for _, k := range []string{"auth_failure_limit", "auth_failure_window", "auth_probe_rate", "auth_db_concurrency"} {
			if !strings.Contains(dump, k) {
				t.Errorf("boot dump gateway group lacks %s: %s", k, dump)
			}
		}
		return
	}
	t.Fatal("BootDumpArgs carries no gateway group")
}
