package main

// A06-A1 (design/06 §3.4, §7 A1) — what is left of the deprecation window's
// boot sweep on the env side, which is nothing, and the pin that keeps the
// hand-over to β13 visible.
//
// The ROW half needs a database and lives in retiredsources_integration_test.go.
// It is untouched by the cut: it keys off config.RetiredKeyNames(), a static
// list, so it keeps naming leftover context_settings rows in every scope.
//
// The ENV half — warnRetiredEnvVarsBoot and TestWarnRetiredEnvVarsBoot — died
// in β8 together with its subject. The sweep chose its per-key wording from
// c.sources ("this var IS the effective source" vs. "a settings row already
// shadows it"), so it could only ever speak about keys the loader still knew.
// β8 cut the last six of the 29 out of the registry, and design/06 §4 Phase A #1
// had written the hand-over before the first key moved: "im Schnitt wird er
// durch den Tombstone 3.5 ersetzt, weil c.sources die Keys dann nicht mehr
// kennt."
//
// Its coverage partition made the ending explicit rather than gradual. The pin
// asserted one WARN line per STILL-REGISTERED retired env name and not a single
// word about a name whose key had been cut — and it fataled on purpose once the
// live half emptied: "no retired key is registered any more — this sweep is
// spent, β13's tombstone owns the statement now". That fatal is the forcing
// function firing as designed, not a regression, and the honest answer to it is
// to remove the sweep with its test rather than to widen either one.
//
// What is NOT covered between β8 and β13, stated plainly so nobody reads the
// silence as safety: a deployment that still carries a non-empty CTX_CHAT_HOST
// (or any of the other 28) boots without a word about it. fromSources is
// registry-driven, so the var is ignored silently — the fail-open design/06 §5.1
// names as the first break path of the cut. The replacement is specified
// (design/01 §4 W9, design/06 §3.5) and owed by β13; main.go carries the
// specification at the site the code will occupy.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/config"
)

// TestRetiredEnvSweepIsOwedToBeta13 is the reminder pin of the open gap.
//
// It asserts the two halves of the hand-over that must hold while the tombstone
// does not exist yet:
//
//  1. The cut is COMPLETE — no retired key is registered any more. This is what
//     makes a c.sources-driven sweep impossible and is therefore the reason the
//     env half is gone. If this ever fails, a retired name is back in the
//     registry and the far bigger problem is that every stale row on it became
//     effective configuration again (config/retired.go).
//  2. The INGREDIENT survived its consumer. config.RetiredEnvNames() still
//     returns all 29 names, sorted and duplicate-free. It has no caller in this
//     package right now, and an unused list is exactly the kind of thing that
//     decays — a key quietly dropped from retiredSettingKeys would take the
//     tombstone's future name with it, and nothing else in cmd/ctxd would
//     notice.
//
// β13 turns this pin into a real gate: once the tombstone sweep exists, the
// assertions here become its precondition and the sweep's own tests take over
// the behaviour. Until then this is the only line in cmd/ctxd that says the gap
// is known.
func TestRetiredEnvSweepIsOwedToBeta13(t *testing.T) {
	for _, key := range config.RetiredKeyNames() {
		if info, registered := config.KeyByName(key); registered {
			t.Errorf("%s is registered again (env %q) — a retired name back in the registry revives every "+
				"stale row on it; the β8 cut is not complete", key, info.EnvVar)
		}
	}

	names := config.RetiredEnvNames()
	if len(names) != 29 {
		t.Errorf("RetiredEnvNames() returned %d names, want 29 — the β13 tombstone sweeps exactly this list, "+
			"and it has no consumer left to notice it shrinking", len(names))
	}
	seen := map[string]bool{}
	for _, name := range names {
		if !strings.HasPrefix(name, "CTX_") {
			t.Errorf("RetiredEnvNames() carries %q, which is not a CTX_ env var name", name)
		}
		if seen[name] {
			t.Errorf("RetiredEnvNames() lists %s twice", name)
		}
		seen[name] = true
	}
}

// TestRetiredEnvNamesStayOutOfTheLiveEnvSurface is the inverse statement, at the
// boot layer rather than the registry layer: not one of the 29 names may be
// readable as configuration any more.
//
// config/retired_test.go pins the same thing against EnvVars(). This one is
// worth having next to it because it is the layer an operator's .env actually
// meets: it sets every retired name to a distinctive value, boots the env
// config, and requires that none of the values reaches the rendered snapshot.
// A key that lost its struct field but kept an env source — or a future key that
// reintroduced one of the names under a different key — shows up here as a value
// in the render, not as a count mismatch somewhere else.
//
// The scan runs over Redacted(SurfaceBootDump), NOT over BootDumpArgs: the boot
// record renders a CURATED SUBSET of the groups (dumpGroupOrder, six of them),
// so a value landing in any other group would pass a BootDumpArgs scan
// unnoticed. Redacted produces every registry group, which is what makes the
// negative statement worth making. Verified by construction: the control below
// sets a LIVE env key to the same marker and requires it to BE in the render —
// without it, "the marker is absent" would also hold for a render that shows
// nothing at all.
func TestRetiredEnvNamesStayOutOfTheLiveEnvSurface(t *testing.T) {
	resetAllEnv(t)
	const marker = "RETIREDMARKER-must-not-be-read"
	const controlMarker = "CONTROLMARKER-must-be-read"
	for _, name := range config.RetiredEnvNames() {
		t.Setenv(name, marker)
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password-123")
	t.Setenv("CTX_DIGEST_MODE", controlMarker)

	cfg, _ := config.FromEnv()
	rendered := fmt.Sprint(cfg.Redacted(config.SurfaceBootDump))
	if !strings.Contains(rendered, controlMarker) {
		t.Fatalf("the control env var did not reach the render — the scan below would prove nothing:\n%s", rendered)
	}
	if strings.Contains(rendered, marker) {
		t.Errorf("a retired env var reached the effective config:\n%s", rendered)
	}
	for _, key := range config.RetiredKeyNames() {
		if src := cfg.Source(key); src != "" {
			t.Errorf("%s still has a source (%q) — a retired key must be unknown to the loader", key, src)
		}
	}
}
