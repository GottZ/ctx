package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// retirementMarker is the marker every .env.example line that still names one
// of the 29 retired backend tuple vars has to carry. It names the release the
// var disappears in, so the marker doubles as the answer to the only question
// a reader of that line has ("until when does this work?") — a bare "DEPRECATED"
// would pass a gate and tell an operator nothing.
//
// It is spelled here and in cmd/ctxd's retiredMajor; the two cannot share a
// constant (config must not import the daemon, and a doc marker is not config),
// so the pin is this comment plus the fact that a version bump that touches one
// and not the other makes the docs and the boot log name different releases.
const retirementMarker = "retired v5.0.0"

// envExampleRelPath locates the tracked template from this package: config →
// internal → go → repo root. Deliberately the ONE tracked file, never a glob:
// the frozen .deploy-v4.* tag worktrees carry their own .env.example copies of
// older releases, and a gate that swept those would go red on history nobody
// can edit any more.
var envExampleRelPath = filepath.Join("..", "..", "..", ".env.example")

// retiredEnvPattern builds the canonical "29er-Muster" of design/06 §3.0 — the
// alternation of exactly the 29 retired env names, \b-anchored on both ends —
// FROM RetiredEnvNames() instead of transcribing the regex the design wrote
// out. The design's own review history is the argument: two earlier hand-written
// forms of this pattern shipped wrong (one too narrow, missing the whole embed
// tuple and the THINK/NUM_CTX suffixes; one too broad, matching the surviving
// CTX_DREAM_ENABLED/_PARALLELISM/… family and going permanently red). Derived
// from the list, the pattern cannot be either.
//
// The \b anchors carry real weight here: they are what keeps CTX_EMBED_HOST
// from matching inside CTX_DREAM_EMBED_HOST, and what keeps every retired name
// from matching a longer surviving name that merely starts the same way.
func retiredEnvPattern(t *testing.T) *regexp.Regexp {
	t.Helper()
	names := RetiredEnvNames()
	if len(names) != 29 {
		t.Fatalf("RetiredEnvNames() returned %d names, want 29", len(names))
	}
	return regexp.MustCompile(`\b(` + strings.Join(names, "|") + `)\b`)
}

// readEnvExample returns the tracked template's lines.
//
// Caveat for anyone re-running this gate by hand: `go test` caches results per
// package, and a run in which ONLY .env.example changed was observed to serve a
// stale pass once (the file lives outside the module, so it is not part of the
// package's ordinary input hash). Use `go test -count=1 ./internal/config/`
// after editing the template; CI is unaffected — it builds cold.
func readEnvExample(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(envExampleRelPath)
	if err != nil {
		// Fatal, never Skip: a gate that disappears when its subject moves is
		// worse than no gate, because the green run keeps claiming coverage.
		t.Fatalf("read %s: %v", envExampleRelPath, err)
	}
	return strings.Split(string(raw), "\n")
}

// TestEnvExampleRetiredVarsAreMarked is the A06-A2 gate (design/06 §7 A2, using
// the §3.0 pattern): after the deprecation rewrite, every line of .env.example
// that still names one of the 29 retired backend tuple vars is marked as
// retired, and none of them is a live example any more.
//
// Why the template needs a gate of its own and not just a careful edit: example
// material is deployment surface at this project's scale (design/06 §6.3). Every
// fresh install starts as `cp .env.example .env`, so an unmarked assignment in
// here does not merely document the dying surface — it KEEPS CREATING new
// deployments on it, months after the docs said it was going away. The template
// is the one file where a stale line writes itself into other people's systems.
//
// Two assertions, because "marked" and "inert" are different failures:
//
//   - marked: a line that names a retired var without the marker is a line that
//     tells a reader to use it. This is the literal A2 gate ("only retired-marked
//     lines survive; an active unmarked example = red").
//   - inert: a retired var may still be DECLARED here (four of them are, so the
//     compose environment: block keeps interpolating without a warning until it
//     is cut), but it must be declared EMPTY. A non-empty value would re-arm the
//     conditional first-boot seed of A02-W5 — MatchesDefaults compares against
//     the registry defaults, so any deviating value in the template makes a
//     fresh install seed http://localhost:11434 pool rows again instead of
//     staying empty and loud (design/06 §5.2). That is precisely the fail-late
//     mode the whole retirement was designed to avoid, and it would enter
//     through the example file rather than through code.
func TestEnvExampleRetiredVarsAreMarked(t *testing.T) {
	pattern := retiredEnvPattern(t)
	assignment := regexp.MustCompile(`^\s*(CTX_[A-Z0-9_]+)\s*=(.*)$`)

	for i, line := range readEnvExample(t) {
		if !pattern.MatchString(line) {
			continue
		}
		lineNo := i + 1
		if !strings.Contains(line, retirementMarker) {
			t.Errorf(".env.example:%d names a retired backend tuple var without the %q marker: %s",
				lineNo, retirementMarker, strings.TrimSpace(line))
		}
		m := assignment.FindStringSubmatch(line)
		if m == nil {
			continue // comment or prose — the marker check above is the whole rule
		}
		value := strings.TrimSpace(strings.SplitN(m[2], "#", 2)[0])
		if value != "" {
			t.Errorf(".env.example:%d assigns retired var %s a value (%q) — the template must not seed a backend pool; see 'ctx backends seed'",
				lineNo, m[1], value)
		}
	}
}

// TestEnvExampleNamesEveryRetiredVar pins the notice's COMPLETENESS against the
// one list (K4): .env.example spells out all 29 names the boot log names.
//
// The direction matters. The marker gate above only constrains names that ARE
// mentioned; on its own, the cheapest way to make it green forever is to delete
// the deprecation section entirely — and an operator upgrading from a .env he
// wrote two years ago would then find no trace of the vars he is running. This
// test is what makes deletion a red gate instead of a shortcut, and it is why
// the section lists the full set rather than a representative sample.
//
// It is also a drift pin in the other direction: when the tuple set grows or a
// name changes in retired.go, the template goes red until the notice follows.
func TestEnvExampleNamesEveryRetiredVar(t *testing.T) {
	body := strings.Join(readEnvExample(t), "\n")
	for _, name := range RetiredEnvNames() {
		if !regexp.MustCompile(`\b` + name + `\b`).MatchString(body) {
			t.Errorf(".env.example never names retired var %s — the deprecation notice must list all 29", name)
		}
	}
}

// TestEnvExampleNamesTheReplacement keeps the α line honest: this release is
// replacement-first, so the file that announces the retirement has to name the
// path that replaces it. A deprecation notice without a destination is the
// failure mode E13 already ruled out on the wire (404, no hint in the response,
// the destination lives in the docs) — the template is one of the places that
// has to carry it.
func TestEnvExampleNamesTheReplacement(t *testing.T) {
	body := strings.Join(readEnvExample(t), "\n")
	for _, want := range []string{"ctx init", "ctx backends seed", "docs/operations.md"} {
		if !strings.Contains(body, want) {
			t.Errorf(".env.example does not name %q — the retirement notice must point at the replacement path", want)
		}
	}
}
