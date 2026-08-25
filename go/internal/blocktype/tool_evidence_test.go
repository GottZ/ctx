package blocktype

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/GottZ/ctx/migrations"
)

// ── Gate 1: envelope ────────────────────────────────────────────────────────.

// migration136File is the fixed source of both seed configs. The test reads
// the REAL migration body (never a test-local JSON copy, design/01 §4.1 R1) —
// a config edit in the SQL is measured here, not mirrored here.
const migration136File = "136_tool_evidence_block_types.sql"

// seedRowRe extracts (name, config-literal) pairs from the migration's INSERT
// VALUES list. The seed rows follow the 072/084/107 shape:
//
//	('<name>', '_global', 'Display', true, false, '{ … }'::jsonb)
var seedRowRe = regexp.MustCompile(`\('([a-z0-9-]+)',\s*'_global',[^']*'[^']*',\s*true,\s*false,\s*'(\{(?s:.*?)\})'::jsonb\)`)

// migration136Configs returns the raw config JSON of both seeded types, keyed
// by type name.
func migration136Configs(t *testing.T) map[string][]byte {
	t.Helper()
	body, err := migrations.Section(migration136File)
	if err != nil {
		t.Fatalf("read %s from migrations.FS: %v", migration136File, err)
	}
	out := map[string][]byte{}
	for _, m := range seedRowRe.FindAllSubmatch(body, -1) {
		out[string(m[1])] = m[2]
	}
	if len(out) != 2 {
		t.Fatalf("extracted %d seed configs from %s, want 2 (%v)", len(out), migration136File, keysOf(out))
	}
	return out
}

func keysOf(m map[string][]byte) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	return names
}

// stripEnvelope removes the "v" key from a config object — the negative
// probe's mutation.
func stripEnvelope(t *testing.T, raw []byte) []byte {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("config is not a JSON object: %v", err)
	}
	if _, ok := obj["v"]; !ok {
		t.Fatalf("config carries no %q key — the envelope probe would be vacuous", "v")
	}
	delete(obj, "v")
	out, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return out
}

// TestMigration136Envelope is Gate 1 of wave W02-2. DecodePolicy rejects a
// config without the v=1 envelope BEFORE validatePolicy is ever reached
// (policy.go: the envelope check precedes the default fill), so a seed row
// that forgot "v" would be a corrupt-config event at every registry reload and
// the type would never load at all. The probe runs the REAL seed config twice:
// once with the envelope deleted (must be red with the envelope error class),
// once as shipped (must decode AND validate).
func TestMigration136Envelope(t *testing.T) {
	const wantErr = "unsupported or missing envelope version"
	for name, raw := range migration136Configs(t) {
		t.Run(name+"/no_envelope_rejected", func(t *testing.T) {
			_, err := DecodePolicy(name, globalScope, true, false, stripEnvelope(t, raw))
			if err == nil {
				t.Fatalf("config without %q decoded — the envelope gate is gone", "v")
			}
			if !strings.Contains(err.Error(), wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), wantErr)
			}
		})
		t.Run(name+"/shipped_config_decodes_and_validates", func(t *testing.T) {
			p, err := DecodePolicy(name, globalScope, true, false, raw)
			if err != nil {
				t.Fatalf("shipped seed config does not decode: %v", err)
			}
			// DecodePolicy runs validatePolicy last, so reaching this point is
			// the validation green. These assertions pin WHAT was validated.
			if p.Retrieval.Kind != RetrievalDamped {
				t.Errorf("retrieval.policy = %q, want damped", p.Retrieval.Kind)
			}
			if f := p.Retrieval.DampingFactor; f <= 0 || f > 1 {
				t.Errorf("damping_factor = %v, outside (0,1]", f)
			}
			if p.Parent.Mode != ParentModeNone {
				t.Errorf("parent.mode = %q, want none", p.Parent.Mode)
			}
		})
	}
	// The two factors are the whole point of splitting the axis in two types —
	// pin them against a copy-paste that gives both the same weight.
	cfgs := migration136Configs(t)
	want := map[string]float64{"tool-evidence": 0.15, "tool-overview": 0.35}
	for name, f := range want {
		raw, ok := cfgs[name]
		if !ok {
			t.Fatalf("migration 136 seeds no type %q", name)
		}
		p, err := DecodePolicy(name, globalScope, true, false, raw)
		if err != nil {
			t.Fatalf("decode %q: %v", name, err)
		}
		if p.Retrieval.DampingFactor != f {
			t.Errorf("%s damping_factor = %v, want %v", name, p.Retrieval.DampingFactor, f)
		}
	}
}

// ── Gate 3 + 5: classify ────────────────────────────────────────────────────.

// TestClassify_ToolEvidenceAndOverview is Gate 3 (the plain index/overview
// titles) plus Gate 5 (the adversarial root). The tool types run at priority
// 19/18, BELOW audit-trail's 20 and checkpoint's 30: Set.Classify walks
// ascending and takes the first match, so a root session name carrying
// "session"/"audit"/"baseline"/"reset" would otherwise hand the block to
// audit-trail — which guards AND dreams, exactly the two pipelines these types
// exist to close. The reverse direction cannot collide: no real audit-trail or
// checkpoint title contains "compaction checkpoint tool index" or "compaction
// tool overview", and the last four cases pin that.
func TestClassify_ToolEvidenceAndOverview(t *testing.T) {
	s := builtinTestSet(t)
	cases := []struct {
		name  string
		title string
		want  string
	}{
		{"evidence index", "Compaction checkpoint tool index x 0123456789abcdef part 001", "tool-evidence"},
		{"evidence lowercase", "compaction checkpoint tool index x 0123456789abcdef part 001", "tool-evidence"},
		{"overview axis", "Compaction tool overview <root> tools", "tool-overview"},
		// Gate 5, adversarial root: the {root} placeholder carries an
		// audit-trail pattern as a substring.
		{"adversarial root session", "Compaction checkpoint tool index session-test 0123456789abcdef part 001", "tool-evidence"},
		{"adversarial root audit", "Compaction checkpoint tool index audit-fix 0123456789abcdef part 001", "tool-evidence"},
		{"adversarial root baseline-reset", "Compaction tool overview baseline-reset tools", "tool-overview"},
		// The reverse: the bestand classification must not move.
		{"real audit title stays audit-trail", "Session 27 Handover", "audit-trail"},
		{"checkpoint head stays checkpoint", "Compaction checkpoint head 2026-08-25_abcdef", "checkpoint"},
		{"checkpoint source stays checkpoint", "Compaction source candidate-x 00ff 00ff part 001 of 002", "checkpoint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, matched := s.Classify(tc.title, nil)
			if !matched || got != tc.want {
				t.Errorf("Classify(%q, nil) = (%q, %v), want (%q, true)", tc.title, got, matched, tc.want)
			}
		})
	}
}

// ── Gate 4: false-lift measurement ──────────────────────────────────────────.

const evalQuestionsFixture = "testdata/eval_questions_2026-08-25.txt"

// loadEvalQuestions reads the fixture copy of eval.sh's question column.
func loadEvalQuestions(t *testing.T) []string {
	t.Helper()
	f, err := os.Open(evalQuestionsFixture)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	if len(out) != 47 {
		t.Fatalf("fixture holds %d questions, want 47 (eval.sh define_test_cases, 2026-08-25)", len(out))
	}
	return out
}

// liftRate counts the questions for which typeName leaves the damping arrays
// (rrf.MatchesAny hit ⇒ full weight 1.0 downstream) and returns count + share.
func liftRate(s *Set, typeName string, questions []string) (int, float64) {
	lifted := 0
	for _, q := range questions {
		names, _ := s.DampedTypesFor(q)
		found := false
		for _, n := range names {
			if n == typeName {
				found = true
				break
			}
		}
		if !found {
			lifted++
		}
	}
	return lifted, float64(lifted) / float64(len(questions))
}

// probeSetWithPatterns builds a Set = builtins + one extra damped type
// carrying the candidate intent patterns. It is the measurement rig for a
// pattern list that is NOT (or not yet) in the builtin set.
func probeSetWithPatterns(t *testing.T, name string, patterns []string) *Set {
	t.Helper()
	p := Policy{
		Name: name, Scope: globalScope,
		Retrieval: RetrievalPolicy{Kind: RetrievalDamped, DampingFactor: 0.5, IntentPatterns: patterns},
		Guard:     GuardPolicy{Mode: GuardModeArchive, Candidates: GuardCandidatesAll},
		Parent:    ParentPolicy{Mode: ParentModeNone},
		Classify:  ClassifyRules{Priority: DefaultClassifyPriority},
	}
	s, err := NewSet(append(builtinPolicies(), p))
	if err != nil {
		t.Fatalf("probe set %q: %v", name, err)
	}
	return s
}

// maxFalseLift is the wave's target rate (design/02 §7 W02-2 gate 4): fewer
// than 10 % of the eval questions may lift a tool type out of the damping.
const maxFalseLift = 0.10

// TestFalseLiftRate_ToolTypes is Gate 4. Set.DampedTypesFor lifts a type
// COMPLETELY (factor 1.0, not partially) as soon as rrf.MatchesAny hits, and
// MatchesAny is a case-insensitive SUBSTRING test — so a generic single-word
// pattern silently disables the damping for a large share of ordinary
// knowledge queries, and the near-duplicate index population would enter
// candidate sets at full weight. The shipped lists are multi-word and
// domain-specific for exactly that reason; this measures it instead of
// asserting it.
func TestFalseLiftRate_ToolTypes(t *testing.T) {
	questions := loadEvalQuestions(t)
	s := builtinTestSet(t)
	for _, name := range []string{"tool-evidence", "tool-overview"} {
		p, ok := s.Resolve(name)
		if !ok {
			t.Fatalf("builtin set carries no type %q — Gate 4 has no subject", name)
		}
		if p.Retrieval.Kind != RetrievalDamped {
			t.Fatalf("%s retrieval.policy = %q, want damped", name, p.Retrieval.Kind)
		}
		n, rate := liftRate(s, name, questions)
		t.Logf("false-lift %-14s %2d/%d = %.1f %%", name, n, len(questions), rate*100)
		if rate >= maxFalseLift {
			t.Errorf("%s false-lift rate %d/%d = %.1f %% >= %.0f %% (intent_patterns too generic)",
				name, n, len(questions), rate*100, maxFalseLift*100)
		}
	}
}

// rev1GenericPatterns is the counterfactual: the generic single-word list of
// design/02 revision 1, retired by review finding #12 ("intent_patterns zu
// generisch"). rev1OverviewGenericPatterns is design/02a's counterfactual for
// the overview axis.
var (
	rev1GenericPatterns         = []string{"tool", "output", "exit", "command", "shell", "terminal", "failed"}
	rev1OverviewGenericPatterns = []string{"tool", "datei", "fehler"}
)

// instrumentPatterns is a deliberately over-broad list used ONLY to prove the
// measurement itself can go red. Without it a false-lift rate of 0 % would be
// indistinguishable from a broken counter.
var instrumentPatterns = []string{"what", "was ", "wie "}

// TestFalseLiftRate_Counterfactuals runs the same measurement over the retired
// generic lists and over an instrument probe. The instrument probe MUST exceed
// the threshold — that is what makes the 0 % of the shipped lists a
// measurement and not an artefact of a dead counter. The two generic rates are
// reported, not asserted: see the wave report — the 47 eval questions contain
// none of those tokens either, so on THIS corpus the retired list is not
// distinguishable from the shipped one.
func TestFalseLiftRate_Counterfactuals(t *testing.T) {
	questions := loadEvalQuestions(t)

	for _, c := range []struct {
		name     string
		patterns []string
	}{
		{"rev1-generic-evidence", rev1GenericPatterns},
		{"rev1-generic-overview", rev1OverviewGenericPatterns},
	} {
		s := probeSetWithPatterns(t, "lift-probe", c.patterns)
		n, rate := liftRate(s, "lift-probe", questions)
		t.Logf("false-lift %-22s %2d/%d = %.1f %% (counterfactual, reported not asserted)",
			c.name, n, len(questions), rate*100)
	}

	s := probeSetWithPatterns(t, "lift-probe", instrumentPatterns)
	n, rate := liftRate(s, "lift-probe", questions)
	t.Logf("false-lift %-22s %2d/%d = %.1f %% (instrument probe)", "instrument", n, len(questions), rate*100)
	if rate <= maxFalseLift {
		t.Errorf("instrument probe lifted only %d/%d = %.1f %% — the false-lift counter cannot go red, "+
			"so the 0 %% of the shipped lists proves nothing", n, len(questions), rate*100)
	}
}

// TestToolPatternsDriveEngine is the positive control of Gate 4: every shipped
// intent pattern must actually lift its own type through the shared engine.
// Twin of TestBuiltinPatternsDriveEngine for the audit-trail list.
func TestToolPatternsDriveEngine(t *testing.T) {
	s := builtinTestSet(t)
	for _, name := range []string{"tool-evidence", "tool-overview"} {
		p, ok := s.Resolve(name)
		if !ok {
			t.Fatalf("builtin set carries no type %q", name)
		}
		if len(p.Retrieval.IntentPatterns) == 0 {
			t.Fatalf("%s carries no intent patterns", name)
		}
		for _, probe := range p.Retrieval.IntentPatterns {
			names, _ := s.DampedTypesFor("xx " + probe + " yy")
			for _, n := range names {
				if n == name {
					t.Errorf("pattern %q does not lift %s via the engine (damped=%v)", probe, name, names)
				}
			}
		}
	}
}

// TestToolSeedsMatchBuiltin is the unit-level half of the lockstep gate: the
// migration's config literals must decode to exactly the compiled-in policies.
// The integration golden test does the same against a real DB (and covers the
// whole registry); this one runs in `go test -short` and needs no container,
// so a drift is caught before the container suite is even started.
//
// SCOPE, since W02-4: 136 is the SEED state, not the end state. Migration 138
// adds retrieval.untrusted to both rows afterwards, so exactly one field is
// allowed to differ between these literals and builtin.go — and only in one
// direction (seed false, builtin true). Everything else still has to match byte
// for byte. The end state after the WHOLE chain is what
// TestRegistryGolden_Integration compares, and that is the lockstep truth;
// this probe covers the container-free part of it plus the 138 assertions in
// TestMigration138SetsUntrustedFlag.
func TestToolSeedsMatchBuiltin(t *testing.T) {
	cfgs := migration136Configs(t)
	s := builtinTestSet(t)
	for name, raw := range cfgs {
		seed, err := DecodePolicy(name, globalScope, true, false, raw)
		if err != nil {
			t.Fatalf("decode seed %q: %v", name, err)
		}
		got, ok := s.Resolve(name)
		if !ok {
			t.Fatalf("builtin set carries no %q (migration/builtin drift)", name)
		}
		// The one carve-out, asserted rather than assumed: the seed must NOT
		// carry the flag and the builtin MUST — if either side moves, the
		// carve-out itself is wrong and this is where it goes red, instead of
		// silently widening into "untrusted never matters here".
		if seed.Retrieval.Untrusted {
			t.Errorf("migration 136 seed %q already carries retrieval.untrusted — "+
				"138 would be a no-op and this normalisation is now hiding a real drift", name)
		}
		if !got.Retrieval.Untrusted {
			t.Errorf("builtin %q lost retrieval.untrusted — migration 138 sets it, so the "+
				"registry golden would go red against the compiled-in set", name)
		}
		got.Retrieval.Untrusted = false

		if diff := policyDiff(seed, got); diff != "" {
			t.Errorf("seed/builtin drift for %q (retrieval.untrusted normalised away — "+
				"migration 138 owns that field):\n%s", name, diff)
		}
	}
}

// policyDiff renders a compact field-level difference of two policies.
func policyDiff(a, b Policy) string {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if bytes.Equal(ja, jb) {
		return ""
	}
	return fmt.Sprintf("  seed:    %s\n  builtin: %s", ja, jb)
}
