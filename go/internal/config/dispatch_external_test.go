package config

// A03-W3 config gates (DECISIONS A-E2 no. 1–3): dispatch.external_order is
// an enum validated against the dispatcher's own named orders (V42, the V21
// shape: trim + lower in place, unknown ⇒ fatal — boot abort, settings write
// rejected), and the boot record "config: effective" carries the dispatch
// group with the order, the three external caps and external_lease_max.

import (
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/dispatch"
)

func TestDispatchExternalOrderV42(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
		fatal           bool
	}{
		{name: "default", raw: "", want: dispatch.ExternalOrderO1},
		{name: "o2", raw: "o2", want: dispatch.ExternalOrderO2},
		{name: "case and blanks normalize", raw: "  O2 ", want: dispatch.ExternalOrderO2},
		{name: "unknown order", raw: "o3", want: "o3", fatal: true},
		{name: "a rank list is not an order", raw: "interactive,external,background", fatal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := map[string]string{}
			if tc.raw != "" {
				src["dispatch.external_order"] = tc.raw
			}
			cfg := validCfg(t, src)
			issues := Validate(cfg)
			got := severityFor(issues, "dispatch.external_order")
			if tc.fatal {
				if got != SeverityError {
					t.Fatalf("dispatch.external_order=%q: severity %d (-1 = no issue), want ERROR (boot abort / 422)", tc.raw, got)
				}
				if err := NewStore(validCfg(t, map[string]string{})).Replace(cfg); err == nil || !strings.Contains(err.Error(), "dispatch.external_order") {
					t.Fatalf("settings write with dispatch.external_order=%q accepted: %v", tc.raw, err)
				}
				return
			}
			if got != -1 || cfg.Dispatch.ExternalOrder != tc.want {
				t.Fatalf("dispatch.external_order=%q: severity %v, value %q, want clean %q", tc.raw, got, cfg.Dispatch.ExternalOrder, tc.want)
			}
		})
	}
	if got := defaultFor("dispatch.external_order"); got != dispatch.ExternalOrderO1 {
		t.Fatalf("dispatch.external_order default = %q, want o1 (E2 decision)", got)
	}
}

// TestBootDumpCarriesDispatchGroup is the A-E2 no. 3 gate: the dispatch
// group is part of the boot record, with the external order, the external
// cap ladder and the external lease ceiling at their registry defaults.
func TestBootDumpCarriesDispatchGroup(t *testing.T) {
	c, issues := cfgFrom(t, map[string]string{})
	args := BootDumpArgs(c, issues)
	var group map[string]any
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] == "dispatch" {
			group, _ = args[i+1].(map[string]any)
		}
	}
	if group == nil {
		t.Fatalf("config: effective lacks the dispatch group: %v", args)
	}
	for key, want := range map[string]any{
		"external_order":              "o1",
		"external_queue_per_consumer": 8,
		"external_queue_per_tenant":   32,
		"external_queue_max":          64,
		"external_lease_max":          (900 * time.Second).String(),
		"external_max_wait":           (20 * time.Second).String(),
		"external_retry_after_steps":  "1,2,5,10,20,30",
		"enabled":                     true,
	} {
		if got := group[key]; got != want {
			t.Errorf("dispatch.%s in the boot record = %v (%T), want %v", key, got, got, want)
		}
	}
}

// TestDispatchExternalMaxWait is the A03-W5 config gate (DECISIONS A-E2
// no. 2, design/03 §3.3): dispatch.external_max_wait is bare seconds with the
// registry default 20 s, 0 is a legitimate value ("external never waits"),
// and a negative value is refused by the generic V17 duration walk (ERROR —
// boot abort, settings write rejected). The <= 45 s ceiling is V43
// (A03-W17, masterplan K21): TestDispatchExternalMaxWaitCeilingV43.
func TestDispatchExternalMaxWait(t *testing.T) {
	if got := defaultFor("dispatch.external_max_wait"); got != 20*time.Second {
		t.Fatalf("dispatch.external_max_wait default = %v, want 20s", got)
	}
	for _, tc := range []struct {
		raw   string
		want  time.Duration
		fatal bool
	}{
		{raw: "", want: 20 * time.Second},
		{raw: "7", want: 7 * time.Second},
		{raw: "0", want: 0},
		{raw: "-5", fatal: true},
	} {
		src := map[string]string{}
		if tc.raw != "" {
			src["dispatch.external_max_wait"] = tc.raw
		}
		cfg := validCfg(t, src)
		got := severityFor(Validate(cfg), "dispatch.external_max_wait")
		if tc.fatal {
			if got != SeverityError {
				t.Fatalf("dispatch.external_max_wait=%q: severity %d (-1 = no issue), want the V17 ERROR", tc.raw, got)
			}
			continue
		}
		if got != -1 || cfg.Dispatch.ExternalMaxWait != tc.want {
			t.Fatalf("dispatch.external_max_wait=%q: severity %d, value %v, want clean %v", tc.raw, got, cfg.Dispatch.ExternalMaxWait, tc.want)
		}
	}
}

// TestDispatchExternalMaxWaitCeilingV43 is the A03-W17 config gate (masterplan
// K21, design/02 §4.11.2): dispatch.external_max_wait above 45 s — the stream
// commit grace ceiling (50 s, V39) minus the 5 s the header commit needs above
// the wait budget — is ONE error on the key (boot abort, settings write
// rejected); 45 s itself and everything below are clean, and a negative value
// stays the V17 walk's single issue (no second, contradicting report).
func TestDispatchExternalMaxWaitCeilingV43(t *testing.T) {
	if dispatchExternalMaxWaitCeiling != 45*time.Second {
		t.Fatalf("ceiling %v, want 45s = grace ceiling %v − commit distance %v", dispatchExternalMaxWaitCeiling, gatewayCommitGraceCeiling, gatewayCommitDistance)
	}
	count := func(issues []Issue) (n int, sev Severity) {
		sev = -1
		for _, is := range issues {
			if is.Field == "dispatch.external_max_wait" {
				n++
				if is.Severity > sev {
					sev = is.Severity
				}
			}
		}
		return n, sev
	}
	for _, tc := range []struct {
		raw   string
		want  int
		fatal bool
	}{
		{raw: "46", want: 1, fatal: true},
		{raw: "3600", want: 1, fatal: true},
		{raw: "45", want: 0},
		{raw: "20", want: 0},
		{raw: "-46", want: 1, fatal: true}, // V17 alone
	} {
		cfg := validCfg(t, map[string]string{"dispatch.external_max_wait": tc.raw})
		n, sev := count(Validate(cfg))
		if n != tc.want || (tc.fatal && sev != SeverityError) {
			t.Fatalf("dispatch.external_max_wait=%s: %d issue(s), worst severity %d (-1 = none), want %d (fatal %v)", tc.raw, n, sev, tc.want, tc.fatal)
		}
		if !tc.fatal {
			continue
		}
		if err := NewStore(validCfg(t, map[string]string{})).Replace(cfg); err == nil || !strings.Contains(err.Error(), "dispatch.external_max_wait") {
			t.Fatalf("settings write with dispatch.external_max_wait=%s accepted: %v", tc.raw, err)
		}
	}
}

// TestDispatchExternalRetryAfterStepsV46 is the A03-W6 config gate (DECISIONS
// A-E2 no. 2 verbatim: the Retry-After steps are a setting): the default
// "1,2,5,10,20,30" is clean, blanks around the entries are fine, and every
// malformed list — an entry below 1, not ascending, not a whole number, an
// explicitly empty value, or a largest step above dispatch.external_lease_max
// (no Retry-After may outlast the longest external lease) — is a fatal V46
// issue: boot abort, settings write rejected (422).
func TestDispatchExternalRetryAfterStepsV46(t *testing.T) {
	if got := defaultFor("dispatch.external_retry_after_steps"); got != "1,2,5,10,20,30" {
		t.Fatalf("dispatch.external_retry_after_steps default = %q, want 1,2,5,10,20,30", got)
	}
	for _, tc := range []struct {
		name  string
		src   map[string]string
		fatal bool
	}{
		{name: "default", src: map[string]string{}},
		{name: "two steps", src: map[string]string{"dispatch.external_retry_after_steps": "3,7"}},
		{name: "blanks", src: map[string]string{"dispatch.external_retry_after_steps": " 3 , 7 "}},
		{name: "largest == lease max", src: map[string]string{"dispatch.external_retry_after_steps": "1,20", "dispatch.external_lease_max": "20"}},
		{name: "lease max 0 sets no bound", src: map[string]string{"dispatch.external_retry_after_steps": "1,3600", "dispatch.external_lease_max": "0"}},
		{name: "entry 0", src: map[string]string{"dispatch.external_retry_after_steps": "0,5"}, fatal: true},
		{name: "descending", src: map[string]string{"dispatch.external_retry_after_steps": "5,2"}, fatal: true},
		{name: "not a number", src: map[string]string{"dispatch.external_retry_after_steps": "a"}, fatal: true},
		{name: "explicitly empty", src: map[string]string{"dispatch.external_retry_after_steps": ""}, fatal: true},
		{name: "largest above lease max", src: map[string]string{"dispatch.external_retry_after_steps": "1,30", "dispatch.external_lease_max": "20"}, fatal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validCfg(t, tc.src)
			got := severityFor(Validate(cfg), "dispatch.external_retry_after_steps")
			if !tc.fatal {
				if got != -1 {
					t.Fatalf("%v: severity %d, want no issue", tc.src, got)
				}
				return
			}
			if got != SeverityError {
				t.Fatalf("%v: severity %d (-1 = no issue), want ERROR (boot abort / 422)", tc.src, got)
			}
			if err := NewStore(validCfg(t, map[string]string{})).Replace(cfg); err == nil || !strings.Contains(err.Error(), "dispatch.external_retry_after_steps") {
				t.Fatalf("settings write with %v accepted: %v", tc.src, err)
			}
		})
	}
}
