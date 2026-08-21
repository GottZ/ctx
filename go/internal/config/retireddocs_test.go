package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

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
