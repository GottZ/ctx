package main

// A06-A1 / K4 (design/06 §3.4 + §3.5, design/01 §4 + §7 W9) — the ENV half of
// the retirement's boot sweep, and the ingredient list it is built from.
//
// The ROW half needs a database and lives in retiredsources_integration_test.go.
// It is untouched by the cut: it keys off config.RetiredKeyNames(), a static
// list, so it keeps naming leftover context_settings rows in every scope.
//
// The ENV half had a predecessor and an interregnum. warnRetiredEnvVarsBoot
// chose its per-key wording from c.sources ("this var IS the effective source"
// vs. "a settings row already shadows it"), so it could only ever speak about
// keys the loader still knew; β8 cut the last six of the 29 out of the registry
// and the sweep died with its subject, exactly as design/06 §4 Phase A #1 had
// written before the first key moved: "im Schnitt wird er durch den Tombstone
// 3.5 ersetzt, weil c.sources die Keys dann nicht mehr kennt." Between β8 and
// β13 nothing spoke about a still-set CTX_CHAT_HOST at all — a gap this file
// pinned as an open debt rather than letting the silence pass for safety.
//
// β13 discharges it. The tombstone asks the environment directly instead of the
// loader, so it can speak about names no registry knows any more, and the pin
// below turned from a reminder into the sweep's precondition: the cut must stay
// complete, and the ingredient list must stay whole, or the sweep silently
// stops covering part of the retirement.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/config"
)

// needle is the value probe of the name-only rule. Distinctive enough that a
// substring search over a whole log buffer cannot hit it by accident, and
// shaped like the thing that must never be logged: a provider api key.
const needle = "sk-live-NEEDLE-must-never-be-logged-7f3a91"

// TestRetiredEnvSweepPreconditions pins the two facts the tombstone sweep is
// built on. Neither is about the sweep's behaviour — that is the test below —
// and both would break it silently rather than loudly.
//
//  1. The cut is COMPLETE: no retired key is registered any more. This is WHY
//     the env half had to be rebuilt on a static list. If it ever fails, a
//     retired name is back in the registry and the far bigger problem is that
//     every stale row on it became effective configuration again
//     (config/retired.go).
//  2. The INGREDIENT is whole: config.RetiredEnvNames() still returns all 29
//     names, sorted and duplicate-free, and the value-bearing partition the
//     tripwire sweeps is exactly 6 hosts + 6 api keys + 5 models = 17 of them.
//     The partition is asserted by CLASS COUNT, not by a transcript of
//     seventeen strings: a second transcript is the thing config/retired.go
//     exists to prevent, but a suffix rule that started matching a fourth class
//     — or stopped matching one — would otherwise change the sweep's reach
//     without changing a single visible name.
func TestRetiredEnvSweepPreconditions(t *testing.T) {
	for _, key := range config.RetiredKeyNames() {
		if info, registered := config.KeyByName(key); registered {
			t.Errorf("%s is registered again (env %q) — a retired name back in the registry revives every "+
				"stale row on it; the β8 cut is not complete", key, info.EnvVar)
		}
	}

	names := config.RetiredEnvNames()
	if len(names) != 29 {
		t.Errorf("RetiredEnvNames() returned %d names, want 29 — the tombstone tripwire sweeps a subset of "+
			"exactly this list", len(names))
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

	byClass := map[string]int{}
	for _, name := range retiredEnvTripwireNames() {
		switch {
		case strings.HasSuffix(name, "_API_KEY"):
			byClass["api_key"]++
		case strings.HasSuffix(name, "_HOST"):
			byClass["host"]++
		case strings.HasSuffix(name, "_MODEL"):
			byClass["model"]++
		default:
			t.Errorf("tripwire sweeps %q, which belongs to none of the three value-bearing classes", name)
		}
	}
	for class, want := range map[string]int{"host": 6, "api_key": 6, "model": 5} {
		if byClass[class] != want {
			t.Errorf("tripwire sweeps %d %s vars, want %d (design/01 §4 W9: 6 hosts, 6 api_keys, 5 models)",
				byClass[class], class, want)
		}
	}
	if got := len(retiredEnvTripwireNames()); got != 17 {
		t.Errorf("tripwire sweeps %d of the 29 names, want 17 — the twelve value-less ones "+
			"(protocols, timeout, num_ctx, think) must stay out", got)
	}
}

// TestWarnRetiredEnvVarsBoot is the K4 gate (design/01 §7 W9): one WARN naming
// the var and the way out on a set, non-empty, value-bearing legacy var — and
// silence in every one of the four cases that would otherwise make this sweep
// noise instead of signal.
//
// The negative arms carry the weight. This tripwire runs on a cohort that did
// NOT update its compose file, which is precisely the cohort whose compose file
// materialises all 29 vars as `${VAR:-}` — a sweep without the value filter
// would print up to 29 lines on every boot of every such installation, and the
// seventeen lines that can mean a dead host would drown in them.
func TestWarnRetiredEnvVarsBoot(t *testing.T) {
	t.Run("set non-empty legacy var warns by name with the way out", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		t.Setenv("CTX_CHAT_HOST", "x")

		warnRetiredEnvVarsBoot()

		out := buf.String()
		if !strings.Contains(out, "level=WARN") {
			t.Errorf("log = %q, want a WARN — a silently ignored backend host is not an INFO", out)
		}
		if !strings.Contains(out, "CTX_CHAT_HOST") {
			t.Errorf("log = %q, want the var NAME — the operator has to know which line of his .env is dead", out)
		}
		// Without a next step this is an alarm, not an instruction: the value
		// moved somewhere, and the line has to say where.
		if !strings.Contains(out, "ctx backends") {
			t.Errorf("log = %q, want the 'ctx backends' pointer to the new home of the value", out)
		}
		if !strings.Contains(out, retiredMajor) {
			t.Errorf("log = %q, want the release the key disappeared in (%s)", out, retiredMajor)
		}
		if !strings.Contains(out, "deprecation="+deprecationRetiredEnv) {
			t.Errorf("log = %q, want the deprecation label — it is how the whole window greps out of a JSON boot log", out)
		}
		if n := strings.Count(out, "level=WARN"); n != 1 {
			t.Errorf("log = %q, want exactly 1 WARN for 1 set var, got %d", out, n)
		}
	})

	// Negative 1: nothing set at all. A tripwire that fires on a clean
	// environment reports its own existence, not a problem.
	t.Run("negative 1: no legacy var set is silent", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)

		warnRetiredEnvVarsBoot()

		if out := buf.String(); out != "" {
			t.Errorf("log = %q on a clean environment, want silence", out)
		}
	})

	// Negative 2: the compose-scaffold case, and the reason the filter is on
	// the VALUE rather than on existence. `${CTX_CHAT_HOST:-}` in an unmodified
	// v4 compose file reaches the process as set-and-empty; the loader treats
	// empty env as unset (load.go:296) and so must this.
	t.Run("negative 2: set-but-empty is not set", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		for _, name := range retiredEnvTripwireNames() {
			t.Setenv(name, "")
		}

		warnRetiredEnvVarsBoot()

		if out := buf.String(); out != "" {
			t.Errorf("log = %q with all 17 vars set-but-empty, want silence — this is the state every "+
				"unmodified v4 compose file produces", out)
		}
	})

	// Negative 3: the two scaffold DEFAULTS. Same cohort, but these two arrive
	// non-empty because their compose declaration shipped a default value, so
	// the value filter alone would not stop them.
	t.Run("negative 3: scaffold default values are exempt", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		for name, def := range retiredEnvScaffoldDefaults {
			t.Setenv(name, def)
		}

		warnRetiredEnvVarsBoot()

		if out := buf.String(); out != "" {
			t.Errorf("log = %q on the untouched rerank scaffold defaults, want silence", out)
		}
	})

	// The exemption is value-scoped, not name-scoped — the control that makes
	// negative 3 mean something. An operator who pointed rerank at his own host
	// made a real choice, and that choice is exactly what dies silently.
	t.Run("a scaffold-default var with a different value still warns", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		t.Setenv("CTX_RERANK_HOST", "http://rerank.internal:9000")

		warnRetiredEnvVarsBoot()

		if out := buf.String(); !strings.Contains(out, "CTX_RERANK_HOST") {
			t.Errorf("log = %q, want the WARN — only the scaffold VALUE is exempt, not the name", out)
		}
	})

	// The twelve value-less names, as a class. Their compose defaults are
	// hard-wired non-empty, so they pass the value filter and are stopped only
	// by the partition.
	t.Run("value-less legacy vars stay out of the sweep", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		swept := map[string]bool{}
		for _, name := range retiredEnvTripwireNames() {
			swept[name] = true
		}
		var quiet []string
		for _, name := range config.RetiredEnvNames() {
			if !swept[name] {
				quiet = append(quiet, name)
				t.Setenv(name, "some-non-empty-value")
			}
		}
		if len(quiet) != 12 {
			t.Fatalf("expected 12 value-less names outside the sweep, got %d (%v)", len(quiet), quiet)
		}

		warnRetiredEnvVarsBoot()

		if out := buf.String(); out != "" {
			t.Errorf("log = %q, want silence — protocols/timeout/num_ctx/think carry no topology and their "+
				"compose scaffold defaults are non-empty on every unmodified v4 install", out)
		}
	})

	// Negative 4, the needle: NO value ever reaches the log. Six of the
	// seventeen names are api_key vars and a boot log travels into aggregators
	// and support bundles, so a sweep that echoed what it found would turn a
	// deprecation notice into a credential leak.
	t.Run("negative 4: no value is ever logged (needle)", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		t.Setenv("CTX_CHAT_API_KEY", needle)

		warnRetiredEnvVarsBoot()

		out := buf.String()
		if !strings.Contains(out, "CTX_CHAT_API_KEY") {
			t.Fatalf("log = %q, want the var name — without the positive half the needle scan proves nothing", out)
		}
		if strings.Contains(out, needle) {
			t.Errorf("the api key VALUE reached the boot log:\n%s", out)
		}
	})

	// Every value-bearing name gets a voice: one line each, none swallowed.
	t.Run("all 17 value-bearing vars are reported", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		names := retiredEnvTripwireNames()
		for _, name := range names {
			t.Setenv(name, "non-default-value")
		}

		warnRetiredEnvVarsBoot()

		out := buf.String()
		for _, name := range names {
			if !strings.Contains(out, "env="+name) {
				t.Errorf("log = %q, missing %s", out, name)
			}
		}
		if n := strings.Count(out, "level=WARN"); n != len(names) {
			t.Errorf("got %d WARN lines for %d set vars", n, len(names))
		}
	})
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
