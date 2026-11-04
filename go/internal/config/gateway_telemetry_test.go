package config

// W06-9 config gates (design/06 §4.11, masterplan K10): gateway.body_logging
// is the one body policy of /v1 rows, an enum validated against the
// llmlog vocabulary (V43, the V42 shape: trim + lower in place, unknown ⇒
// fatal — boot abort, settings write rejected); gateway.access_log turns
// the per-request success line on (default) or off. Both reach
// oai.Settings (TestGatewayKeysReachSettings) and the boot record.

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/llmlog"
)

func TestGatewayBodyLoggingV43(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
		fatal           bool
	}{
		{name: "default", raw: "", want: llmlog.BodyLoggingNone},
		{name: "errors", raw: "errors", want: llmlog.BodyLoggingErrors},
		{name: "full", raw: "full", want: llmlog.BodyLoggingFull},
		{name: "case and blanks normalize", raw: "  FULL ", want: llmlog.BodyLoggingFull},
		{name: "unknown policy", raw: "foo", fatal: true},
		{name: "a list is not a policy", raw: "errors,full", fatal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := map[string]string{}
			if tc.raw != "" {
				src["gateway.body_logging"] = tc.raw
			}
			cfg := validCfg(t, src)
			issues := Validate(cfg)
			got := severityFor(issues, "gateway.body_logging")
			if tc.fatal {
				if got != SeverityError {
					t.Fatalf("gateway.body_logging=%q: severity %d (-1 = no issue), want ERROR (boot abort / 422)", tc.raw, got)
				}
				if err := NewStore(validCfg(t, map[string]string{})).Replace(cfg); err == nil || !strings.Contains(err.Error(), "gateway.body_logging") {
					t.Fatalf("settings write with gateway.body_logging=%q accepted: %v", tc.raw, err)
				}
				return
			}
			if got != -1 || cfg.Gateway.BodyLogging != tc.want || cfg.GatewaySettings().BodyLogging != tc.want {
				t.Fatalf("gateway.body_logging=%q: severity %v, value %q / settings %q, want clean %q",
					tc.raw, got, cfg.Gateway.BodyLogging, cfg.GatewaySettings().BodyLogging, tc.want)
			}
		})
	}
	if got := defaultFor("gateway.body_logging"); got != llmlog.BodyLoggingNone {
		t.Fatalf("gateway.body_logging default = %v, want none (masterplan K10)", got)
	}
	// An empty value written over the settings surface reads as the
	// default, like an unset key — never as a policy of its own.
	c := Defaults()
	c.Gateway.BodyLogging = " "
	if issues := Validate(c); severityFor(issues, "gateway.body_logging") != -1 || c.Gateway.BodyLogging != llmlog.BodyLoggingNone {
		t.Fatalf("blank body_logging: issues %+v, value %q, want none without an issue", issues, c.Gateway.BodyLogging)
	}
}

func TestGatewayAccessLogKey(t *testing.T) {
	if got := defaultFor("gateway.access_log"); got != true {
		t.Fatalf("gateway.access_log default = %v, want true (design/06 §4.11)", got)
	}
	c := validCfg(t, map[string]string{"gateway.access_log": "false"})
	if c.Gateway.AccessLog || c.GatewaySettings().AccessLog {
		t.Fatalf("gateway.access_log=false does not reach the settings")
	}
	if !Defaults().GatewaySettings().AccessLog {
		t.Fatalf("default settings carry access_log=false")
	}
}

// TestBootDumpShowsGatewayTelemetry: the boot record carries both keys with
// their defaults inside the gateway group.
func TestBootDumpShowsGatewayTelemetry(t *testing.T) {
	c, issues := cfgFrom(t, map[string]string{})
	args := BootDumpArgs(c, issues)
	var group map[string]any
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] == "gateway" {
			group, _ = args[i+1].(map[string]any)
		}
	}
	if group == nil {
		t.Fatalf("config: effective lacks the gateway group: %v", args)
	}
	if group["body_logging"] != "none" || group["access_log"] != true {
		t.Fatalf("gateway group body_logging=%v (%T) access_log=%v (%T), want none / true",
			group["body_logging"], group["body_logging"], group["access_log"], group["access_log"])
	}
}
