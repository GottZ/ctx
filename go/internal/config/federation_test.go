package config

// F-W8 G6 (design/04 §3.1, DECISIONS A-E2): federation.max_depth is a
// registry key — int, default 3, hot, global-only, strict, settings-only —
// checked by V45 to 1 ≤ max_depth ≤ fedhop.MaxHopEntries, and visible in the
// boot record under its own group.

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/fedhop"
)

func TestFederationMaxDepthRegistry(t *testing.T) {
	info, ok := KeyByName("federation.max_depth")
	if !ok {
		t.Fatal("federation.max_depth is not in the registry")
	}
	if info.Type != "int" || info.Mutability != "hot" || info.Tenancy != TenancyGlobalOnly || info.EnvVar != "" {
		t.Errorf("federation.max_depth = %+v, want int, hot, global-only, settings-only (no env)", info)
	}
	if info.Default != 3 {
		t.Errorf("default = %v, want 3 (E-04-7)", info.Default)
	}
	var strict bool
	for _, e := range registry() {
		if e.Key == "federation.max_depth" {
			strict = e.Strict
		}
	}
	if !strict {
		t.Error("federation.max_depth is not parse:\"strict\"")
	}
	if got := Defaults().Federation.MaxDepth; got != 3 {
		t.Errorf("Defaults().Federation.MaxDepth = %d, want 3", got)
	}
}

// TestValidateFederationMaxDepth is V45: 0 would refuse every request (a
// chain of length 0 is already "≥ 0"), a value above fedhop.MaxHopEntries can
// never be reached (ParseInbound refuses a longer chain with 400 first), so
// both are errors; a negative value is V35's single error, not a second one.
func TestValidateFederationMaxDepth(t *testing.T) {
	const key = "federation.max_depth"
	for _, v := range []int{0, fedhop.MaxHopEntries + 1, -1} {
		t.Run(strconv.Itoa(v), func(t *testing.T) {
			got := issuesOn(Validate(validCfg(t, map[string]string{key: strconv.Itoa(v)})), key)
			if len(got) != 1 || got[0].Severity != SeverityError {
				t.Fatalf("%s = %d: %v, want exactly one SeverityError", key, v, got)
			}
		})
	}
	for _, v := range []int{1, 3, fedhop.MaxHopEntries} {
		t.Run(strconv.Itoa(v), func(t *testing.T) {
			c := validCfg(t, map[string]string{key: strconv.Itoa(v)})
			if got := issuesOn(Validate(c), key); len(got) != 0 {
				t.Fatalf("%s = %d: %v, want no issue", key, v, got)
			}
			if c.Federation.MaxDepth != v {
				t.Errorf("parsed %d, want %d", c.Federation.MaxDepth, v)
			}
		})
	}
}

// TestBootDumpCarriesFederation: the boot record shows federation:
// max_depth=3 — the limit is an operator's first question when a peer is
// refused with 508.
func TestBootDumpCarriesFederation(t *testing.T) {
	c, _ := cfgFrom(t, map[string]string{})
	args := BootDumpArgs(c, nil)
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] != "federation" {
			continue
		}
		g, ok := args[i+1].(map[string]any)
		if !ok || g["max_depth"] != 3 {
			t.Fatalf("federation group = %v, want max_depth=3", args[i+1])
		}
		return
	}
	t.Fatalf("boot dump has no federation group: %v", args)
}

// guard:"federation-ingress" means "turning this key on needs the registered
// federation ingress" — a property of a bool switch. On any other type the
// handler's raw == "true" check would never fire and the key would carry a
// guard nobody enforces, so buildEntry refuses it (Fremd-Review 2, FR2-3).
type guardIngressIntGroup struct {
	X int `key:"legacy.guard_ingress_int" env:"-" default:"0" mut:"hot" guard:"federation-ingress" tenancy:"global-only"`
}

type guardIngressBoolGroup struct {
	X bool `key:"legacy.guard_ingress_bool" env:"-" default:"false" mut:"hot" guard:"federation-ingress" tenancy:"global-only"`
}

func TestGuardFederationIngressOnlyOnBool(t *testing.T) {
	_, err := buildRegistry(reflect.TypeOf(guardIngressIntGroup{}))
	if err == nil {
		t.Fatal(`buildRegistry accepted guard:"federation-ingress" on an int field — the guard needs a bool switch`)
	}
	if want := `guard "federation-ingress" needs a bool field`; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
	built, err := buildRegistry(reflect.TypeOf(guardIngressBoolGroup{}))
	if err != nil || len(built) != 1 || built[0].Guard != GuardFederationIngress {
		t.Fatalf("bool field with the guard: built %v, err %v — want one entry carrying the guard", built, err)
	}
}

// TestNoFederationInboundKeys is the registry pin of design/04 §5.3 D4 and
// §4.7 "Kein zweiter Boden" (F-W9 G9): no federation.inbound_* key exists —
// Rev. 0's tenant-overridable federation.inbound_sensitivity (and
// inbound_log_bodies) are gone; the floor of a foreign prompt is the
// consumer's prompt_sensitivity, lowered only by server-admin. Green at the
// base already: a pin against its return, not a gate of this wave.
func TestNoFederationInboundKeys(t *testing.T) {
	for _, e := range registry() {
		if strings.HasPrefix(e.Key, "federation.inbound_") {
			t.Errorf("registry holds %q — a second sensitivity floor or body policy for foreign prompts (design/04 §4.7, D4)", e.Key)
		}
	}
}
