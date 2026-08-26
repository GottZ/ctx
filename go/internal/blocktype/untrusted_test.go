package blocktype

import (
	"slices"
	"strings"
	"testing"

	"github.com/GottZ/ctx/migrations"
)

// W02-4 probes for retrieval.untrusted — the type-bound flag that tells the
// synthesis presentation layer to frame a source as OBSERVATION DATA rather
// than as knowledge (design/02 §4.6(2)/§7, design/02a §5.4).
//
// The flag is registry DATA, not code: a second foreign-text type inherits the
// framing by carrying it in its config row, with no prompt and no Go change.
// These probes therefore pin the decode/default behaviour and the derived Set
// lookup, and only then the two seeded types.

// TestDecodePolicy_Untrusted is Gate 4a: present-true, present-false and
// absent. Absent must be false — every pre-136 registry row (and every
// operator row written before this wave) omits the key, and a default of true
// would frame the whole knowledge corpus as tool output.
func TestDecodePolicy_Untrusted(t *testing.T) {
	cases := []struct {
		name   string
		config string
		want   bool
	}{
		{"present_true", `{"v":1,"retrieval":{"policy":"damped","damping_factor":0.15,"untrusted":true}}`, true},
		{"present_false", `{"v":1,"retrieval":{"policy":"damped","damping_factor":0.15,"untrusted":false}}`, false},
		{"absent", `{"v":1,"retrieval":{"policy":"damped","damping_factor":0.15}}`, false},
		{"absent_no_retrieval_section", `{"v":1}`, false},
		// An excluded type never reaches a prompt, so the flag is INERT there
		// rather than invalid — validatePolicy deliberately does not reject it
		// (the row stays loadable if a type is later flipped to damped).
		{"inert_on_excluded", `{"v":1,"retrieval":{"policy":"excluded","untrusted":true}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := DecodePolicy("probe-type", globalScope, false, false, []byte(tc.config))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if p.Retrieval.Untrusted != tc.want {
				t.Errorf("retrieval.untrusted = %v, want %v", p.Retrieval.Untrusted, tc.want)
			}
		})
	}
}

// TestDecodePolicy_UntrustedTypoRejected keeps the strict-decode contract: the
// envelope runs with DisallowUnknownFields at every level, so a misspelt key
// must reject with its path instead of silently defaulting to false — the §5.2
// breakage class the whole decoder is built around.
func TestDecodePolicy_UntrustedTypoRejected(t *testing.T) {
	_, err := DecodePolicy("probe-type", globalScope, false, false,
		[]byte(`{"v":1,"retrieval":{"policy":"full-pass","untrused":true}}`))
	if err == nil {
		t.Fatal("a misspelt retrieval key decoded silently")
	}
}

// TestSet_UntrustedLookup is Gate 4b: the derived Set lookup over the builtin
// set. tool-evidence, tool-overview and — since V-W7 — checkpoint carry the
// flag; the knowledge line does not.
//
// WHY CHECKPOINT MOVED (V-W7, design/05 §5 B2): W02-4 read the flag as a
// PROMPT-rendering property, and on an excluded type nothing renders, so
// checkpoint was left at false. A derived layer inverts that reading: a
// distiller that quotes checkpoint prose has to be able to ASK whether its
// source is foreign text, and the only place that answer can live is the
// source type. Checkpoint prose quotes tool output, web content and foreign
// agent prompts, so the fail-closed rule ("a derived type is untrusted unless
// EVERY source class is provably first-party") needs this side of the lookup
// to be true — otherwise the derived layer launders foreign text into
// knowledge. The flag stays INERT here: the type remains retrieval=excluded,
// so no prompt changes today (see the ctx_rrf non-regression probe in
// internal/rrf and the inert_on_excluded case above).
func TestSet_UntrustedLookup(t *testing.T) {
	s, err := NewSet(builtinPolicies())
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}

	for name, want := range map[string]bool{
		"tool-evidence": true,
		"tool-overview": true,
		"knowledge":     false,
		"reference":     false,
		"audit-trail":   false,
		// V-W7: foreign text by construction (transcript prose that quotes
		// tool output and foreign prompts), inert while excluded.
		"checkpoint": true,
		// Unknown names resolve to false, mirroring GuardSameScopeOnly: an
		// unregistered name cannot reach the prompt in the first place (the
		// visibility allowlist comes from the SAME snapshot, fail-closed), and
		// callers outside the registry path (goldbench) carry no type at all —
		// defaulting those to true would move every prompt they build.
		"no-such-type": false,
		"":             false,
	} {
		if got := s.IsUntrusted(name); got != want {
			t.Errorf("IsUntrusted(%q) = %v, want %v", name, got, want)
		}
	}

	got := s.UntrustedTypes()
	want := []string{"checkpoint", "tool-evidence", "tool-overview"}
	if !slices.Equal(got, want) {
		t.Errorf("UntrustedTypes() = %v, want %v (sorted, derived at NewSet)", got, want)
	}
}

// TestMigration138SetsUntrustedFlag is the SQL half of the lockstep, and it is
// deliberately a CONTAINER-FREE OUTPOST, not the authority: the lockstep truth
// is TestRegistryGolden_Integration, which applies the real chain (136 seeds,
// 138 sets) and compares the END STATE of the rows against builtinPolicies().
// This probe reads migration 138 out of migrations.FS — never a test-local copy
// of the statement (design/01 §4.1 R1) — and pins the four things that make it
// the right statement, so a rewrite that still parses goes red in `go test
// -short` instead of two minutes into the container suite.
//
// 136 is not touched by this wave: it is committed, and a landed migration is
// not edited (see the 138 header). Its literals therefore still decode to
// untrusted=false, which TestToolSeedsMatchBuiltin asserts explicitly.
func TestMigration138SetsUntrustedFlag(t *testing.T) {
	body, err := migrations.Section("138_tool_types_untrusted.sql")
	if err != nil {
		t.Fatalf("read 138 from migrations.FS: %v", err)
	}
	// Comment lines are dropped FIRST, then whitespace is collapsed: the
	// statement may be reformatted, never rewritten, and the German header
	// discusses the very tokens asserted below (it names UPDATE, the type names
	// and the guard in prose). Measuring prose would make every assertion here
	// pass on a file whose SQL had been deleted outright.
	var sql []string
	for _, line := range strings.Split(string(body), "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "--") {
			sql = append(sql, t)
		}
	}
	stmt := strings.Join(strings.Fields(strings.Join(sql, " ")), " ")
	if stmt == "" {
		t.Fatal("migration 138 carries no SQL at all — only comments")
	}

	for _, want := range []struct{ what, substr string }{
		// Exactly the two foreign-text types — a widened IN list would reframe
		// knowledge blocks as tool output, which is the failure that matters.
		{"the two type names", `name IN ('tool-evidence', 'tool-overview')`},
		{"the global scope", `scope = '_global'`},
		// The path, not a whole-config rewrite: damping, intent patterns and
		// classify rules are operator-tunable and must survive (M107 doctrine).
		{"the jsonb path", `jsonb_set(config, '{retrieval,untrusted}', 'true'::jsonb)`},
		// The existence guard is what makes the UPDATE idempotent AND keeps an
		// operator's deliberate false — jsonb_set alone would overwrite it.
		{"the existence guard", `NOT (config->'retrieval' ? 'untrusted')`},
		{"the lock timeout", `SET LOCAL lock_timeout = '2s';`},
	} {
		if !strings.Contains(stmt, want.substr) {
			t.Errorf("migration 138 lost %s — want a statement containing %q, got:\n%s",
				want.what, want.substr, stmt)
		}
	}

	// A second UPDATE (or a DELETE/INSERT) would put the end state somewhere
	// this probe does not look. One statement, one effect.
	if n := strings.Count(strings.ToUpper(stmt), "UPDATE "); n != 1 {
		t.Errorf("migration 138 carries %d UPDATE statements, want exactly 1:\n%s", n, stmt)
	}
	for _, forbidden := range []string{"DELETE ", "DROP ", "INSERT "} {
		if strings.Contains(strings.ToUpper(stmt), forbidden) {
			t.Errorf("migration 138 carries a %sstatement — it is registry DATA, one UPDATE:\n%s",
				forbidden, stmt)
		}
	}
}
