package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// repoRootRelPath is the ONE anchor the file-based gates in here hang off:
// config -> internal -> go -> repo root. Both the template reader below and
// the root-script scan of TestEnvExampleNamesEveryRequiredScriptVar derive
// from it, so moving this package breaks in one place instead of two.
var repoRootRelPath = filepath.Join("..", "..", "..")

// envExampleRelPath locates the tracked template from this package: config →
// internal → go → repo root. Deliberately the ONE tracked file, never a glob:
// the frozen .deploy-v4.* tag worktrees carry their own .env.example copies of
// older releases, and a gate that swept those would go red on history nobody
// can edit any more.
var envExampleRelPath = filepath.Join(repoRootRelPath, ".env.example")

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

// TestEnvExampleNamesNoRetiredVar is the A03-W2 gate of the cut (design/03 §7
// W2: "grep … .env.example = 0", using the §3.0 pattern): after the rewrite the
// template does not name a single one of the 29 retired backend tuple vars —
// not as an assignment, not as a commented example, not as prose.
//
// This replaces the two α14 gates that guarded the DEPRECATION WINDOW, and the
// replacement is a deliberate inversion, not a relaxation:
//
//   - TestEnvExampleRetiredVarsAreMarked required every mention to carry the
//     "retired v5.0.0" marker. With zero mentions there is nothing left to
//     mark, and absence is the strictly stronger property — a marked line
//     still passes a name into a fresh .env, an absent one cannot.
//   - TestEnvExampleNamesEveryRetiredVar required the template to spell out
//     all 29, so that deleting the notice could not be the cheap way to green.
//     That pin was right for α (v4.38 still READ the vars: the file a fresh
//     install copies was also the file that configured them). It is wrong for
//     the cut. .env.example is copied verbatim into every new installation, so
//     from v5 on the completeness pin would write 29 dead names into .env files
//     that never had them, to serve a reader who is not there — the operator
//     upgrading from v4 reads his OWN .env, plus the two channels design/03 §6
//     names as the whole documented contract: docs/operations.md (β13) and the
//     release body / tag annotation (β14). Neither is this file.
//
// What survives from the old direction is the destination pin below: the
// template must still point at the replacement path, so the deletion cannot
// turn into a silent hole.
func TestEnvExampleNamesNoRetiredVar(t *testing.T) {
	pattern := retiredEnvPattern(t)
	for i, line := range readEnvExample(t) {
		if m := pattern.FindString(line); m != "" {
			t.Errorf(".env.example:%d names retired backend tuple var %s: %s — the pool is the configuration surface (`ctx backends`), and the full retirement notice lives in docs/operations.md",
				i+1, m, strings.TrimSpace(line))
		}
	}
}

// TestComposeDeclaresNoRetiredVar is the A03-W3 gate (design/03 §7 W3: the
// `environment:` block of the ctx service declares none of the 29 any more)
// in durable form. The design specified it as a one-off shell grep over
// `docker compose config --format json`; written as a test it keeps holding
// after the wave, which is what the cut needs — a re-added declaration is not
// a cosmetic regression but a resurrected configuration channel: it would put
// a name back on the container environment that nothing reads, and the boot
// tombstone owed by β13 would then warn about a var the operator never chose.
//
// It is the negative twin of TestClusterComposeDeclaresEveryKey (gate (ii),
// cluster_c0_test.go) and shares its scanner, so both speak about exactly the
// same block: a knob the container cannot receive is not a knob, and a name
// the container receives for nothing is not a knob either.
//
// Note the asymmetry with the .env.example gate above: this one reads NAMES,
// not lines, because the block's prose is where the cut is explained ("the 29
// CTX_{CHAT,…}_* declarations that stood here are retired in v5.0.0"). A
// comment may say the word; a declaration may not exist.
func TestComposeDeclaresNoRetiredVar(t *testing.T) {
	declared := ctxServiceEnvNames(t)
	for _, name := range RetiredEnvNames() {
		if declared[name] {
			t.Errorf("docker-compose.yml still declares retired var %s in the ctx service environment: block — backend topology lives in the pool (`ctx backends`), not on the container environment",
				name)
		}
	}
}

// Why docs/*.md is NOT pinned by the two gates above — E05-D8, decided B
// ("leave the documentation remnants as unpinned upgrade prose").
//
// Eleven mentions of retired env names live in docs/*.md. Widening the
// retirement gate to them looks like the obvious next ratchet and is wrong at
// the subject: it would go red immediately at docs/operations.md:91, where all
// 29 retired names are spelled out ON PURPOSE. That paragraph is the way OUT
// of the retirement — the operator greps his inherited .env against it — so a
// gate that forbids the names would forbid the documentation of the very cut
// these tests enforce. Carving the upgrade sections out with an allowlist is
// the construction V17 rejects for validations (validate.go, "an allowlist
// ... would have to be maintained in lockstep"): it needs an edit on every
// doc change and goes quietly stale when it does not get one.
//
// A full-name gate would also miss the actual remnants: the env tables in the
// docs write SUFFIXES (_LOCAL_ONLY, _LABEL_BUDGET), never whole names, so the
// \b-anchored 29er pattern does not see them at all.
//
// What carries the obligation instead is per-wave, not standing: every
// retirement wave takes its own documentation line with it, and its gate is a
// grep count inside that wave. That keeps the duty at the place where the
// name actually dies, which is the only place that knows what the prose
// around it has to say afterwards.

// TestEnvExampleNamesTheReplacement keeps the template honest in the direction
// the absence gate cannot: the file that no longer names the retired surface
// has to name the one that replaces it. Written for α as "a deprecation notice
// without a destination", it matters more after the cut, not less — a fresh
// install now finds NO backend configuration in this file at all, and the only
// thing standing between that and a puzzled operator is the pointer.
func TestEnvExampleNamesTheReplacement(t *testing.T) {
	body := strings.Join(readEnvExample(t), "\n")
	for _, want := range []string{"ctx init", "ctx backends seed", "docs/operations.md"} {
		if !strings.Contains(body, want) {
			t.Errorf(".env.example does not name %q — the template must point at the replacement path", want)
		}
	}
}

// requiredScriptVarPattern matches the shell form that makes a variable
// MANDATORY for a root script: ${NAME:?…} — the parameter expansion that
// aborts the script when NAME is unset or empty. ${NAME:-default} is
// deliberately NOT matched: a knob with a fallback needs no template line.
//
// The character class carries the one exception this gate would otherwise
// need a list for: break-glass.sh uses POSITIONAL parameters (${1:?usage …},
// ${2:?name}) — arguments, not environment — and they fall out of
// [A-Z][A-Z0-9_]+ by themselves. Listing them by name would be the allowlist
// construction V17 rejects for validations in validate.go ("an allowlist …
// would have to be maintained in lockstep").
var requiredScriptVarPattern = regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]+):\?`)

// envExampleDeclPattern matches a name the template DECLARES: an assignment,
// live (NAME=value) or commented out (# NAME=value). Prose that merely
// mentions a name does not count, and that is the point — the template's
// contract is `cp .env.example .env` plus filling in the blanks, and a name
// with no assignment line survives that copy as nothing at all.
var envExampleDeclPattern = regexp.MustCompile(`^\s*#?\s*([A-Z][A-Z0-9_]*)\s*=`)

// requiredScriptVarExceptions names every mandatory script variable that is
// deliberately absent from the template, mapped to the reason. It is EMPTY
// today. The test keeps it honest in both directions — an entry without a
// reason and an entry no script demands any more are both errors — so the map
// cannot decay into a silent mute switch for a hole nobody chose.
var requiredScriptVarExceptions = map[string]string{}

// scanRequiredScriptVars returns name → "file:line" sites for every ${NAME:?}
// in the *.sh files of dir.
//
// dir is a PARAMETER, not a constant, for one reason: the red probe inside
// the test runs this exact scanner over a synthetic script in t.TempDir(),
// instead of writing a probe file into the repository root. A probe that
// touches the real root would be a gate that edits its own subject.
//
// The set comes from a glob, never from a hand-written list of script names.
// The first draft of this gate named four scripts and missed state.sh and
// eval-temporal.sh — both of which demand CONTEXT_DB_PASSWORD — which is
// exactly how an allowlist fails: silently, and in the direction of green.
func scanRequiredScriptVars(t *testing.T, dir string) map[string][]string {
	t.Helper()
	pattern := filepath.Join(dir, "*.sh")
	scripts, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("glob %s: %v", pattern, err)
	}
	if len(scripts) == 0 {
		// Fatal, never Skip: an empty scan set is how this gate would go
		// green after a move while claiming to cover the scripts — the same
		// failure mode readEnvExample guards against above.
		t.Fatalf("no *.sh under %s — the scan set must never be empty", pattern)
	}
	sites := map[string][]string{}
	for _, script := range scripts {
		raw, err := os.ReadFile(script)
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			for _, m := range requiredScriptVarPattern.FindAllStringSubmatch(line, -1) {
				sites[m[1]] = append(sites[m[1]], fmt.Sprintf("%s:%d", filepath.Base(script), i+1))
			}
		}
	}
	return sites
}

// TestEnvExampleNamesEveryRequiredScriptVar is the T05-3 ratchet in the
// direction the retirement gates above do not cover: every variable a root
// script REQUIRES has to be declared in the template a fresh installation
// copies. The wave that added it found CONTEXT_API_KEY_ISOTEST (test.sh)
// missing while the key of the abandoned `work` scope — read by nothing in
// the tree — still stood in the template: the two failure directions of an
// env template, both present at once. The dead name is deliberately not
// repeated here; a template gate that reintroduces the string it removed
// would be its own counter-example.
//
// Only this direction is pinned. "Template names something no script reads"
// stays open on purpose: the file is also documentation (commented examples,
// server-side knobs like CTX_DREAM_ENABLED that no shell script touches), and
// a gate against unread names would forbid the documentation half of the file.
//
// The scanner is exercised on a synthetic script in a temp directory in the
// same run (subtest "scanner sees a synthetic script"). Without that probe a
// green result would be ambiguous — a broken pattern and a satisfied template
// look identical from the outside.
func TestEnvExampleNamesEveryRequiredScriptVar(t *testing.T) {
	declared := map[string]bool{}
	for _, line := range readEnvExample(t) {
		if m := envExampleDeclPattern.FindStringSubmatch(line); m != nil {
			declared[m[1]] = true
		}
	}

	required := scanRequiredScriptVars(t, repoRootRelPath)
	names := make([]string, 0, len(required))
	for name := range required {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if reason, excepted := requiredScriptVarExceptions[name]; excepted {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("requiredScriptVarExceptions[%q] carries no reason — an exception without one is an undocumented hole", name)
			}
			continue
		}
		if !declared[name] {
			t.Errorf("%s is mandatory in %s but .env.example declares it nowhere — a fresh `cp .env.example .env` cannot satisfy that script; add the line or name the variable in requiredScriptVarExceptions with a reason",
				name, strings.Join(required[name], ", "))
		}
	}

	for name, reason := range requiredScriptVarExceptions {
		if _, still := required[name]; !still {
			t.Errorf("requiredScriptVarExceptions names %q (%s), but no root script demands it any more — drop the entry", name, reason)
		}
	}

	t.Run("scanner sees a synthetic script", func(t *testing.T) {
		dir := t.TempDir()
		probe := filepath.Join(dir, "ctx-probe.sh")
		body := "#!/usr/bin/env bash\nKEY=\"${CTX_PROBE_X:?probe}\"\nARG=\"${1:?usage}\"\nOPT=\"${CTX_PROBE_OPTIONAL:-fallback}\"\n"
		if err := os.WriteFile(probe, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", probe, err)
		}
		sites := scanRequiredScriptVars(t, dir)
		if got := sites["CTX_PROBE_X"]; len(got) != 1 {
			t.Errorf("scanner found CTX_PROBE_X at %v, want exactly one site — the ${NAME:?} pattern does not reach the scripts it claims to scan", got)
		}
		if len(sites) != 1 {
			// The positional ${1:?} and the optional ${NAME:-…} must both stay
			// out: they are the two forms the character class and the `:?`
			// anchor are supposed to filter, and a hit here would mean the
			// gate demands template lines for arguments and defaults.
			t.Errorf("scanner returned %v, want CTX_PROBE_X alone — positional ${1:?} and optional ${NAME:-…} must not be collected", sites)
		}
	})
}
