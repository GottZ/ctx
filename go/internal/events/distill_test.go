// Unit half of the A02-5 gate: the decisions the distiller arm makes without a
// database. The journal half lives in distill_integration_test.go, the BA10 half
// in distill_blocking_integration_test.go.
package events

import (
	"testing"
	"time"
)

func TestDistillInterval(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"configured", 900 * time.Second, 900 * time.Second},
		{"zero falls back", 0, distillDefaultInterval},
		{"negative falls back", -time.Second, distillDefaultInterval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := distillInterval(tc.in); got != tc.want {
				t.Fatalf("distillInterval(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestDistillSourceKey pins the THREE-part shape (§4.5.1). A two-part key would
// make the cross-run dedup ledger scope-blind, and the empty-session form is
// what the gates ahead of the candidate list write under.
func TestDistillSourceKey(t *testing.T) {
	if got := distillSourceKey("ctx-checkpoint", "private", "20260712_205012_837f2c"); got != "ctx-checkpoint:private:20260712_205012_837f2c" {
		t.Fatalf("session key = %q", got)
	}
	tick := distillSourceKey("ctx-checkpoint", "shared", "")
	if tick != "ctx-checkpoint:shared:" {
		t.Fatalf("tick key = %q", tick)
	}
	// The tick key must not be reachable as a session key: a root session id is
	// never empty, so the two series can never merge.
	if tick == distillSourceKey("ctx-checkpoint", "shared", "root") {
		t.Fatal("tick key collides with a session key")
	}
	// Two scopes, one label, one root ⇒ two series.
	if distillSourceKey("ctx-checkpoint", "private", "r") == distillSourceKey("ctx-checkpoint", "work", "r") {
		t.Fatal("scope does not separate the watermark series")
	}
}

// TestDistillScopeAllowed is gate 5's decision table (§4.5.3 + §4.2.1).
func TestDistillScopeAllowed(t *testing.T) {
	owned := []string{"private", "shared", "work"}
	for _, tc := range []struct {
		name  string
		scope string
		owned []string
		want  bool
	}{
		{"owned scope passes", "private", owned, true},
		{"shared refused even though owned", "shared", owned, false},
		{"foreign scope refused", "fremd", owned, false},
		{"empty scope refused", "", owned, false},
		// owned == nil means the entitlement set could not be established
		// (ListTenants failed, or no active default tenant), NOT "no
		// restriction". A write-path guard fails closed there — deliberately
		// unlike effectiveHomeScope, whose owned==nil passthrough preserves a
		// byte-identical pre-multi-tenant READ path.
		{"unresolved entitlements refuse a normal scope", "private", nil, false},
		{"unresolved entitlements refuse shared", "shared", nil, false},
		{"unresolved entitlements refuse empty", "", nil, false},
		{"empty entitlement list refuses everything", "private", []string{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := distillScopeAllowed(tc.scope, tc.owned); got != tc.want {
				t.Fatalf("distillScopeAllowed(%q, %v) = %v, want %v", tc.scope, tc.owned, got, tc.want)
			}
		})
	}
}

func TestDistillNull(t *testing.T) {
	if distillNull("") != nil {
		t.Fatal("empty string must map to SQL NULL")
	}
	if got := distillNull("demand"); got != any("demand") {
		t.Fatalf("distillNull(%q) = %v", "demand", got)
	}
}
