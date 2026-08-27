package blocktype

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/migrations"
)

// Wave W01-Seed (design D-01 §3.3/§3.4/§4.2/§7 W01-2+W01-3, masterplan K2):
// ONE migration seeds BOTH derived registry rows, insight and catalog. These
// are the container-free halves of the wave's gates — the authority for the
// lockstep is TestDerivedTypes_Integration against the real chain, but a drift
// that still parses goes red here, in `go test -short`, instead of two minutes
// into the container suite. Construction copied from tool_evidence_test.go
// (M136) and checkpoint_untrusted_test.go (M141).
//
// START VISIBILITY IS `excluded`, NOT `damped`. D-01 §3.3/§3.4 proposed
// damped at 0.50/0.60; the conflict resolution K7 of 00-masterplan.md — user
// confirmed on the decision board as E-4 — overrides exactly those two fields:
// both types start `excluded` until the pilots (X-W4/X-W5), then a Registry
// DATA update sets the swept factor (M-W8 over {0.25…1.0}). "Die Zahlen
// 0,35/0,50/0,60 sind Sweep-Kandidaten, keine Startwerte." Every other field
// is D-01 §3.3/§3.4 verbatim. The intent patterns are seeded anyway: they are
// inert while the type is excluded (only damped types enter Set.damped), and
// seeding them makes the later visibility switch a one-field data change over
// a pattern list whose false-lift rate has already been measured — which is
// what TestFalseLiftRate_DerivedPatterns below does, ahead of the flip.
const migration143File = "143_derived_block_types.sql"

// migration143Configs returns the raw config JSON of both seeded types, keyed
// by type name. It reads the REAL migration body out of migrations.FS — never
// a test-local copy of the literal (design/01 §4.1 R1).
func migration143Configs(t *testing.T) map[string][]byte {
	t.Helper()
	body, err := migrations.Section(migration143File)
	if err != nil {
		t.Fatalf("read %s from migrations.FS: %v", migration143File, err)
	}
	out := map[string][]byte{}
	for _, m := range seedRowRe.FindAllSubmatch(body, -1) {
		out[string(m[1])] = m[2]
	}
	if len(out) != 2 {
		t.Fatalf("extracted %d seed configs from %s, want 2 (%v)", len(out), migration143File, keysOf(out))
	}
	for _, name := range []string{derived.TypeInsight, derived.TypeCatalog} {
		if _, ok := out[name]; !ok {
			t.Fatalf("%s seeds no type %q — K2 requires BOTH rows in ONE migration", migration143File, name)
		}
	}
	return out
}

// migration143Statement returns the migration with every comment and blank line
// removed and the remainder collapsed to one whitespace-normal line. Comments
// go FIRST: the German header discusses the very tokens asserted below, so
// measuring prose would let every assertion pass on a file whose SQL had been
// deleted outright (construction from migration141Statement).
func migration143Statement(t *testing.T) string {
	t.Helper()
	body, err := migrations.Section(migration143File)
	if err != nil {
		t.Fatalf("read %s from migrations.FS: %v", migration143File, err)
	}
	var sql []string
	for _, line := range strings.Split(string(body), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "--") {
			sql = append(sql, trimmed)
		}
	}
	stmt := strings.Join(strings.Fields(strings.Join(sql, " ")), " ")
	if stmt == "" {
		t.Fatalf("%s carries no SQL at all — only comments", migration143File)
	}
	return stmt
}

// TestMigration143Shape pins WHAT the migration is: registry DATA, two INSERTs,
// idempotent, no schema object. A wave that reaches for an UPDATE/ALTER here
// would be editing state this file does not own.
func TestMigration143Shape(t *testing.T) {
	stmt := migration143Statement(t)

	for _, want := range []struct{ what, substr string }{
		{"the lock timeout", `SET LOCAL lock_timeout = '2s';`},
		{"the idempotency clause", `ON CONFLICT (name, scope) DO NOTHING`},
		{"the insight row", `('insight', '_global', 'Session-Insight', true, false,`},
		{"the catalog row", `('catalog', '_global', 'Cluster-Katalog', true, false,`},
	} {
		if !strings.Contains(stmt, want.substr) {
			t.Errorf("%s lost %s — want a statement containing %q, got:\n%s",
				migration143File, want.what, want.substr, stmt)
		}
	}

	upper := strings.ToUpper(stmt)
	if n := strings.Count(upper, "INSERT "); n != 2 {
		t.Errorf("%s carries %d INSERT statements, want exactly 2 (K2: both rows, one migration):\n%s",
			migration143File, n, stmt)
	}
	if n := strings.Count(upper, "ON CONFLICT (NAME, SCOPE) DO NOTHING"); n != 2 {
		t.Errorf("%s carries %d ON CONFLICT clauses, want 2 — an unguarded INSERT overwrites "+
			"nothing but fails a re-run, which is what M107's idempotency doctrine forbids:\n%s",
			migration143File, n, stmt)
	}
	for _, forbidden := range []string{"UPDATE ", "DELETE ", "DROP ", "CREATE ", "ALTER "} {
		if strings.Contains(upper, forbidden) {
			t.Errorf("%s carries a %sstatement — it is registry DATA, two INSERTs and nothing else:\n%s",
				migration143File, forbidden, stmt)
		}
	}
	// The field D-01 §7's wave brief forbids explicitly: retrieval.
	// shadow_measurable does not exist on this base (it arrives with M-W2), so
	// a row carrying it would be unloadable — DecodePolicy has
	// DisallowUnknownFields on every level.
	if strings.Contains(stmt, "shadow_measurable") {
		t.Errorf("%s carries shadow_measurable — the field does not exist in this build's "+
			"policy vocabulary and DisallowUnknownFields would make both rows unloadable", migration143File)
	}
}

// TestMigration143Envelope is the envelope gate, negatively probed: DecodePolicy
// checks v=1 BEFORE any other validation (policy.go), so a seed row without it
// would be a corrupt-config event at every registry reload — the type would
// never load and /health would fall back to builtin-fallback. Both configs run
// twice: once with the envelope deleted (must be red), once as shipped.
func TestMigration143Envelope(t *testing.T) {
	const wantErr = "unsupported or missing envelope version"
	for name, raw := range migration143Configs(t) {
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
			if _, err := DecodePolicy(name, globalScope, true, false, raw); err != nil {
				t.Fatalf("shipped seed config does not decode: %v", err)
			}
		})
	}
}

// TestMigration143StartsExcluded is the K7/E-4 gate. It asserts the decision as
// two independent facts, because one alone is not the decision: the policy is
// `excluded`, AND no damping_factor is present in the literal. A row that
// carried a factor while excluded would be inert today and would silently
// become the START value the moment somebody flips the policy — which is
// exactly the "Startwert" K7 refused.
func TestMigration143StartsExcluded(t *testing.T) {
	for name, raw := range migration143Configs(t) {
		p, err := DecodePolicy(name, globalScope, true, false, raw)
		if err != nil {
			t.Fatalf("decode %q: %v", name, err)
		}
		if p.Retrieval.Kind != RetrievalExcluded {
			t.Errorf("%s retrieval.policy = %q, want excluded (K7/E-4: excluded until the pilots, "+
				"then a swept factor as DATA)", name, p.Retrieval.Kind)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatalf("%s config is not a JSON object: %v", name, err)
		}
		var retr map[string]json.RawMessage
		if err := json.Unmarshal(obj["retrieval"], &retr); err != nil {
			t.Fatalf("%s retrieval section is not an object: %v", name, err)
		}
		if _, ok := retr["damping_factor"]; ok {
			t.Errorf("%s carries retrieval.damping_factor while excluded — K7 calls 0.35/0.50/0.60 "+
				"Sweep-Kandidaten, not Startwerte; the factor arrives with the M-W8 sweep", name)
		}
	}
}

// TestMigration143UntrustedSplit pins the one retrieval field that DOES differ
// between the two rows (§4.2). insight distils transcript and tool material —
// M138's doctrine verbatim, "summarising attacker-shapable output does not
// launder it" — so it is foreign text. catalog distils corpus blocks somebody
// wrote as knowledge, so its DEFAULT is false; a single untrusted source is
// handled per block via the inheritance clause (§4.8.3), not by flipping the
// type. Both values are written EXPLICITLY: an explicit false is what a later
// blanket untrusted backfill of the M138/M141 shape (`NOT (config->'retrieval'
// ? 'untrusted')`) would have to step over deliberately.
func TestMigration143UntrustedSplit(t *testing.T) {
	cfgs := migration143Configs(t)
	for name, want := range map[string]bool{derived.TypeInsight: true, derived.TypeCatalog: false} {
		var retr struct {
			Untrusted *bool `json:"untrusted"`
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(cfgs[name], &obj); err != nil {
			t.Fatalf("%s config is not a JSON object: %v", name, err)
		}
		if err := json.Unmarshal(obj["retrieval"], &retr); err != nil {
			t.Fatalf("%s retrieval section is not an object: %v", name, err)
		}
		if retr.Untrusted == nil {
			t.Errorf("%s omits retrieval.untrusted — absent decodes to false either way, but the "+
				"explicit key is what an existence-guarded backfill (M138/M141 shape) respects", name)
			continue
		}
		if *retr.Untrusted != want {
			t.Errorf("%s retrieval.untrusted = %v, want %v (§4.2)", name, *retr.Untrusted, want)
		}
	}
}

// TestDerivedSeedsMatchBuiltin is the unit-level half of the lockstep gate: the
// migration's config literals must decode to EXACTLY the compiled-in policies.
// Unlike TestToolSeedsMatchBuiltin there is no carve-out here — 143 seeds the
// END state, so a single field of drift in either direction is a bug, not a
// later migration's business.
func TestDerivedSeedsMatchBuiltin(t *testing.T) {
	s := builtinTestSet(t)
	for name, raw := range migration143Configs(t) {
		seed, err := DecodePolicy(name, globalScope, true, false, raw)
		if err != nil {
			t.Fatalf("decode seed %q: %v", name, err)
		}
		got, ok := s.Resolve(name)
		if !ok {
			t.Fatalf("builtin set carries no %q — migration/builtin drift (a builtin.go entry "+
				"without a migration makes the registry golden red, and the reverse leaves the "+
				"compiled-in fallback blind to a type the DB serves)", name)
		}
		if diff := policyDiff(seed, got); diff != "" {
			t.Errorf("seed/builtin drift for %q:\n%s", name, diff)
		}
	}
}

// setWithoutDerived builds the builtin set MINUS the two derived policies — the
// registry as it stood before this wave. It is the "before" half of the
// two-step classify gate and stays in the tree permanently: it is what proves
// the new priorities do the work, rather than the titles happening to be
// unclaimed.
func setWithoutDerived(t *testing.T) *Set {
	t.Helper()
	var kept []Policy
	for _, p := range builtinPolicies() {
		if derived.IsDerivedType(p.Name) {
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) != len(builtinPolicies())-2 {
		t.Fatalf("stripped %d policies, want 2 — derived.IsDerivedType and builtinPolicies disagree "+
			"about which names are derived", len(builtinPolicies())-len(kept))
	}
	s, err := NewSet(kept)
	if err != nil {
		t.Fatalf("pre-wave set: %v", err)
	}
	return s
}

// TestClassify_DerivedAnchors is the two-step gate (D-01 §7 W01-2 gate 3, both
// types). Step one: against the pre-wave registry the two title anchors
// classify to their IST types — "Session insights …" carries the substring
// "session", which is auditPatterns[0], so audit-trail claims it at priority
// 20 and hands the block to guard AND dream; "Katalog #…" matches nothing and
// falls through to the default. Measured on the unchanged tree, 2026-08-27.
// Step two: with priorities 17 and 16 — both BELOW audit-trail's 20, and
// Set.Classify takes the FIRST match ascending — the anchors land on their own
// types. Without both steps it is not shown that the priority is what moved
// them.
func TestClassify_DerivedAnchors(t *testing.T) {
	const (
		insightTitle = "Session insights 019d25d8b8aa7f028ad0e0bba7b7cfcf ab #1000"
		catalogTitle = "Katalog #0123456789abcdef0123456789abcdef"
	)

	t.Run("step1_pre_wave_ist", func(t *testing.T) {
		s := setWithoutDerived(t)
		got, matched := s.Classify(insightTitle, nil)
		if got != "audit-trail" || !matched {
			t.Errorf("pre-wave Classify(%q) = (%q, %v), want (audit-trail, true) — the IST this "+
				"wave moves; if it changed, the two-step is measuring something else", insightTitle, got, matched)
		}
		got, matched = s.Classify(catalogTitle, nil)
		if matched || got != "knowledge" {
			t.Errorf("pre-wave Classify(%q) = (%q, matched=%v), want (knowledge, false) — the catalog "+
				"anchor was unclaimed before this wave", catalogTitle, got, matched)
		}
	})

	t.Run("step2_post_wave", func(t *testing.T) {
		s := builtinTestSet(t)
		cases := []struct {
			name  string
			title string
			want  string
		}{
			{"insight anchor", insightTitle, derived.TypeInsight},
			{"insight lowercase", strings.ToLower(insightTitle), derived.TypeInsight},
			{"catalog anchor", catalogTitle, derived.TypeCatalog},
			{"catalog lowercase", strings.ToLower(catalogTitle), derived.TypeCatalog},
			// Adversarial root/topic payloads: the identity part of the title is
			// attacker- and operator-shapable, and audit-trail's patterns are
			// ordinary words. Priority 17/16 must win over 20 regardless.
			{"insight adversarial root", "Session insights session-audit-baseline-reset ab #7", derived.TypeInsight},
			{"catalog adversarial topic", "Katalog #deadbeefsessionaudithandoverbase", derived.TypeCatalog},
			// The reverse direction: the Bestand classification must not move.
			// "session insights " (trailing space) and "katalog #" occur in no
			// real audit-trail, checkpoint or tool title.
			{"real audit title stays audit-trail", "Session 27 Handover", "audit-trail"},
			{"dream v title stays audit-trail", "Dream v3 performance report", "audit-trail"},
			{"checkpoint head stays checkpoint", "Compaction checkpoint head 2026-08-25_abcdef", "checkpoint"},
			{"checkpoint source stays checkpoint", "Compaction source candidate-x 00ff 00ff part 001 of 002", "checkpoint"},
			{"tool index stays tool-evidence", "Compaction checkpoint tool index x 0123456789abcdef part 001", "tool-evidence"},
			{"tool overview stays tool-overview", "Compaction tool overview <root> tools", "tool-overview"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, matched := s.Classify(tc.title, nil)
				if !matched || got != tc.want {
					t.Errorf("Classify(%q, nil) = (%q, %v), want (%q, true)", tc.title, got, matched, tc.want)
				}
			})
		}
	})
}

// TestClassify_DerivedHasNoMetadataRule pins §4.2: neither derived type carries
// classify.metadata_flags. is_meta is the ONLY metadata classify rule in the
// system and it belongs to system-meta; a second one would be a second silent
// re-typing path — and for these types specifically, a block that fell into
// system-meta would go retrieval=excluded and lose its embedding without a
// single error (bruchpfad B9).
func TestClassify_DerivedHasNoMetadataRule(t *testing.T) {
	s := builtinTestSet(t)
	for _, name := range []string{derived.TypeInsight, derived.TypeCatalog} {
		p, ok := s.Resolve(name)
		if !ok {
			t.Fatalf("builtin set carries no %q", name)
		}
		if len(p.Classify.MetadataFlags) != 0 {
			t.Errorf("%s carries classify.metadata_flags = %v, want none (§4.2)", name, p.Classify.MetadataFlags)
		}
		if len(p.Classify.SourcePrefixes) != 0 {
			t.Errorf("%s carries classify.source_prefixes = %v, want none (§4.2)", name, p.Classify.SourcePrefixes)
		}
		if len(p.StructuralLinkClasses) != 0 {
			t.Errorf("%s carries structural_link_classes = %v, want none (§4.2)", name, p.StructuralLinkClasses)
		}
	}
	// Priorities, pinned as the mechanism rather than as a number: both must sit
	// strictly below audit-trail, which is what makes step 2 above possible.
	audit, _ := s.Resolve("audit-trail")
	for name, want := range map[string]int{derived.TypeInsight: 17, derived.TypeCatalog: 16} {
		p, _ := s.Resolve(name)
		if p.Classify.Priority != want {
			t.Errorf("%s classify.priority = %d, want %d (§4.2)", name, p.Classify.Priority, want)
		}
		if p.Classify.Priority >= audit.Classify.Priority {
			t.Errorf("%s priority %d is not below audit-trail's %d — first match wins ascending, so "+
				"audit-trail would claim the anchor and send the block into guard AND dream",
				name, p.Classify.Priority, audit.Classify.Priority)
		}
	}
}

// TestFalseLiftRate_DerivedPatterns is gate 5, measured ahead of the visibility
// switch. The types ship `excluded` (K7), so they are not in Set.damped and
// DampedTypesFor cannot see them — the measurement therefore runs on the rig
// tool_evidence_test.go already built for exactly this case: a probe type
// carrying the candidate patterns. That the rig can go red at all is proven by
// TestFalseLiftRate_Counterfactuals' instrument probe, so a 0 % here is a
// measurement and not a dead counter.
//
// What it buys: when E-4 flips the policy to damped, the pattern list has
// already been measured against the 47 eval questions. Generic single words
// ("katalog" alone hits "Katalogisierung", "insight" hits every English
// "insights") would lift the type out of the damping for ordinary queries —
// DampedTypesFor lifts COMPLETELY on the first hit, there is no partial lift.
func TestFalseLiftRate_DerivedPatterns(t *testing.T) {
	questions := loadEvalQuestions(t)
	s := builtinTestSet(t)
	for _, name := range []string{derived.TypeInsight, derived.TypeCatalog} {
		p, ok := s.Resolve(name)
		if !ok {
			t.Fatalf("builtin set carries no type %q — gate 5 has no subject", name)
		}
		if len(p.Retrieval.IntentPatterns) == 0 {
			t.Fatalf("%s carries no intent_patterns — the list is seeded now so the later "+
				"visibility switch is a one-field data change over a MEASURED list", name)
		}
		probe := probeSetWithPatterns(t, "lift-probe", p.Retrieval.IntentPatterns)
		n, rate := liftRate(probe, "lift-probe", questions)
		t.Logf("false-lift %-8s %2d/%d = %.1f %%", name, n, len(questions), rate*100)
		if rate >= maxFalseLift {
			t.Errorf("%s false-lift rate %d/%d = %.1f %% >= %.0f %% (intent_patterns too generic)",
				name, n, len(questions), rate*100, maxFalseLift*100)
		}
	}
}

// TestDerivedPatternsSingleWordCensus reports — it does NOT assert — which
// shipped patterns are single words. It exists because D-01 contradicts itself
// on exactly this point: §4.2 names "katalog" as THE counter-example of a
// generic single word that would make the damping useless in a technical corpus
// ("katalog allein trifft Katalogisierung"), and §3.4's literal list, which
// this wave seeds verbatim, opens with "katalog". On the 47 eval questions the
// two are indistinguishable (0/47 either way), so there is nothing to decide
// here on evidence — the same situation TestFalseLiftRate_Counterfactuals
// already records for the retired rev1 lists. The census keeps the open
// question in the tree rather than only in a wave report: it is due before the
// E-4 visibility switch, when the list stops being inert.
func TestDerivedPatternsSingleWordCensus(t *testing.T) {
	s := builtinTestSet(t)
	for _, name := range []string{derived.TypeInsight, derived.TypeCatalog} {
		p, _ := s.Resolve(name)
		var single []string
		for _, pattern := range p.Retrieval.IntentPatterns {
			if !strings.ContainsAny(pattern, " -") {
				single = append(single, pattern)
			}
		}
		t.Logf("single-word intent patterns of %-8s %d/%d %v (reported, not asserted — due before "+
			"the E-4 visibility switch, D-01 §4.2 vs §3.4)", name, len(single), len(p.Retrieval.IntentPatterns), single)
	}
}

// TestDerivedPatternsDriveEngine is the positive control of gate 5: every
// shipped pattern must actually lift its own type through the shared engine.
// Twin of TestToolPatternsDriveEngine — without it a 0 % false-lift rate could
// also mean the patterns match nothing at all.
func TestDerivedPatternsDriveEngine(t *testing.T) {
	s := builtinTestSet(t)
	for _, name := range []string{derived.TypeInsight, derived.TypeCatalog} {
		p, ok := s.Resolve(name)
		if !ok {
			t.Fatalf("builtin set carries no type %q", name)
		}
		probe := probeSetWithPatterns(t, "lift-probe", p.Retrieval.IntentPatterns)
		for _, pattern := range p.Retrieval.IntentPatterns {
			lifted := true
			names, _ := probe.DampedTypesFor("xx " + pattern + " yy")
			for _, n := range names {
				if n == "lift-probe" {
					lifted = false
				}
			}
			if !lifted {
				t.Errorf("pattern %q of %s does not lift through the engine (damped=%v)", pattern, name, names)
			}
		}
	}
}

// TestDerivedTypesOutOfEveryPipeline is gate 8 on the compiled-in set: both
// types are out of guard (both directions), dream, digest and overview, and —
// while K7's excluded start holds — out of retrieval visibility too.
//
// Each list is asserted with the REASON, because the four flags are not
// interchangeable: guard.candidate=false keeps a derivative from ARCHIVING an
// original (B1, silent data loss on the original), guard.check=false keeps it
// from archiving ITSELF and orphaning its regeneration (B2), dream.linkable=
// false keeps it out of context_dream_links — which is the ONLY input Louvain
// reads, so a linkable derivative would shape the very partition it is derived
// from — and overview.include=false keeps it out of the topic map (§0/K1).
func TestDerivedTypesOutOfEveryPipeline(t *testing.T) {
	s := builtinTestSet(t)
	for _, name := range []string{derived.TypeInsight, derived.TypeCatalog} {
		for _, l := range []struct {
			what string
			list []string
			why  string
		}{
			{"GuardCheckTypes", s.GuardCheckTypes(), "B2: a derivative that guards itself archives its own regeneration"},
			{"GuardCandidateTypes", s.GuardCandidateTypes(), "B1: a derivative admitted as a guard candidate archives the ORIGINAL"},
			{"DreamLinkableTypes", s.DreamLinkableTypes(), "a linkable derivative shapes the Louvain partition it is derived from"},
			{"DigestTypes", s.DigestTypes(), "digest scans context_blocks without LIMIT or cursor (digest.go:307-334)"},
			{"OverviewTypes", s.OverviewTypes(), "§0/K1: a derivative in the topic map derives the topic map from itself"},
			{"AggregateTypes", s.AggregateTypes(), "§3.6: aggregate-to-parent needs ONE parent; a derivative has N sources"},
			{"VisibleTypes", s.VisibleTypes(), "K7/E-4: excluded until the pilots"},
		} {
			for _, n := range l.list {
				if n == name {
					t.Errorf("%s appears in %s() = %v — %s", name, l.what, l.list, l.why)
				}
			}
		}
	}
	// The Bestand lists must be byte-identical to before the wave: the four
	// flags are all false, so nothing this wave adds may show up anywhere.
	pre := setWithoutDerived(t)
	for _, l := range []struct {
		what      string
		got, want []string
	}{
		{"VisibleTypes", s.VisibleTypes(), pre.VisibleTypes()},
		{"GuardCheckTypes", s.GuardCheckTypes(), pre.GuardCheckTypes()},
		{"GuardCandidateTypes", s.GuardCandidateTypes(), pre.GuardCandidateTypes()},
		{"DreamLinkableTypes", s.DreamLinkableTypes(), pre.DreamLinkableTypes()},
		{"DigestTypes", s.DigestTypes(), pre.DigestTypes()},
		{"OverviewTypes", s.OverviewTypes(), pre.OverviewTypes()},
		{"AggregateTypes", s.AggregateTypes(), pre.AggregateTypes()},
	} {
		if strings.Join(l.got, ",") != strings.Join(l.want, ",") {
			t.Errorf("%s() changed: %v, want the pre-wave %v — this wave adds two rows and moves "+
				"no pipeline", l.what, l.got, l.want)
		}
	}
	// The one list that DOES change, and only for insight (§4.2).
	if !s.IsUntrusted(derived.TypeInsight) {
		t.Error("IsUntrusted(insight) = false — insight distils transcript and tool material, " +
			"and M138's doctrine is that summarising attacker-shapable output does not launder it")
	}
	if s.IsUntrusted(derived.TypeCatalog) {
		t.Error("IsUntrusted(catalog) = true — catalog distils corpus blocks somebody wrote as " +
			"knowledge; a single untrusted SOURCE is framed per block (§4.8.3), not by flipping the type")
	}
}

// TestOverviewGate_NoDerivedInIntersect is gate 6, the §0/K1 guard, and it is
// deliberately stronger than the design's formulation. D-01 §7 W01-3 asks for
// intersect(VisibleTypes, OverviewTypes) to hold no type with StratumOf > 0.
// Under K7's excluded start that intersection is empty for our types no matter
// what overview.include says — the assertion would be vacuous. So the gate
// asserts BOTH:
//
//	(a) OverviewTypes() itself carries no derived type — red on
//	    overview.include=true alone, i.e. red today, and
//	(b) the intersection invariant as designed — the form that keeps guarding
//	    after E-4 flips the visibility.
//
// Both halves are negatively probed below, and the probes are the two mutations
// that would each break exactly one half.
func TestOverviewGate_NoDerivedInIntersect(t *testing.T) {
	assertGate := func(t *testing.T, s *Set) []string {
		t.Helper()
		var found []string
		for _, o := range s.OverviewTypes() {
			if derived.StratumOf(o) > derived.StratumSource {
				found = append(found, "OverviewTypes:"+o)
			}
		}
		overview := map[string]bool{}
		for _, o := range s.OverviewTypes() {
			overview[o] = true
		}
		for _, v := range s.VisibleTypes() {
			if overview[v] && derived.StratumOf(v) > derived.StratumSource {
				found = append(found, "intersect:"+v)
			}
		}
		return found
	}

	if found := assertGate(t, builtinTestSet(t)); len(found) != 0 {
		t.Errorf("derived types in the overview path: %v — a catalogue that enters the topic map "+
			"is derived from a partition it helped produce (§0/K1)", found)
	}

	// Negative probe (a): overview.include=true on catalog, policy untouched.
	// This is the exact mutation D-01 §7 W01-3 names, and under the excluded
	// start ONLY half (a) of the gate catches it.
	t.Run("negative_probe_overview_include", func(t *testing.T) {
		mutated := mutateDerivedPolicy(t, derived.TypeCatalog, func(p *Policy) { p.Overview.Include = true })
		found := assertGate(t, mutated)
		if len(found) == 0 {
			t.Error("a registry copy with catalog.overview.include=true passed the gate — the gate " +
				"cannot go red and proves nothing")
		}
		if !containsString(found, "OverviewTypes:"+derived.TypeCatalog) {
			t.Errorf("probe found %v, want the OverviewTypes half to fire — that is the half that "+
				"works while the type is excluded", found)
		}
	})

	// Negative probe (b): the post-E-4 world. catalog damped AND in the overview
	// set — now the intersection half has to fire too, which is what keeps the
	// gate alive after the visibility switch.
	t.Run("negative_probe_visible_and_overview", func(t *testing.T) {
		mutated := mutateDerivedPolicy(t, derived.TypeCatalog, func(p *Policy) {
			p.Overview.Include = true
			p.Retrieval.Kind = RetrievalDamped
			p.Retrieval.DampingFactor = 0.6
		})
		found := assertGate(t, mutated)
		if !containsString(found, "intersect:"+derived.TypeCatalog) {
			t.Errorf("probe found %v, want the intersect half to fire — without it the gate stops "+
				"guarding the moment E-4 flips the visibility", found)
		}
	})
}

// mutateDerivedPolicy returns the builtin set with one named policy rewritten by
// mutate — the "Registry-Kopie" the negative probes need.
func mutateDerivedPolicy(t *testing.T, name string, mutate func(*Policy)) *Set {
	t.Helper()
	pols := builtinPolicies()
	hit := false
	for i := range pols {
		if pols[i].Name == name {
			mutate(&pols[i])
			hit = true
		}
	}
	if !hit {
		t.Fatalf("no policy %q to mutate — the negative probe has no subject", name)
	}
	s, err := NewSet(pols)
	if err != nil {
		t.Fatalf("mutated set: %v", err)
	}
	return s
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// TestDerivedRegistryCoversEveryStratumName is the B15 half that can be checked
// without a container: every name derived.StratumOf maps above 0 must exist in
// the registry. The sperre S1 (wave W01-2a) and the invariant I7 hang on a
// STRING; type_name has no FK, and sweepOrphans only warns. If a name ever
// exists in Go without a registry row, Resolve does not know it — IsUntrusted
// answers false and VisibleTypes drops it — and nothing goes red on its own.
func TestDerivedRegistryCoversEveryStratumName(t *testing.T) {
	s := builtinTestSet(t)
	for _, name := range []string{derived.TypeInsight, derived.TypeCatalog} {
		if derived.StratumOf(name) <= derived.StratumSource {
			t.Fatalf("derived.StratumOf(%q) = %d — this probe is asserting the wrong names",
				name, derived.StratumOf(name))
		}
		if _, ok := s.Resolve(name); !ok {
			t.Errorf("derived type %q has no registry row — I7 and the write lock hang on this "+
				"string, and type_name carries no FK", name)
		}
	}
}
