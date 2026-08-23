// Language gates of issue #34: the map's fixed scaffolding and its number
// format follow the corpus language (dream.language), with the empty tag and
// every German tag byte-frozen on the legacy German table.
//
// The frozen half is proven NEXT DOOR, not here: TestRenderGolden and the ~40
// German assertions across rootmap_test / super_test / run_test all run with
// Input.Language unset and must keep passing unchanged. If any of them had to be
// touched, this wave would have rewritten live artefacts.
package rootmap_test

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/rootmap"
)

// goldenInputEN is the golden fixture in English. Two fields move and no others:
// the language tag, and the operational rationale — which the renderer takes as
// DATA (the write path resolves it through the same two-table split, pinned by
// TestOperationalRationaleFollowsLanguage below).
func goldenInputEN(t *testing.T, lang string) rootmap.Input {
	t.Helper()
	in := goldenInput(t)
	in.Language = lang
	in.OperationalRationale = "hermes compaction artefacts"
	return in
}

const goldenTextEN = `ctx Root Map v1 | scope:private | 3/29 topics | 1,155/1,190 blocks listed | Q=0.877 γ=1.0
cluster state 2026-08-04T07:54Z · last attempt 2026-08-04T13:54Z · expected cadence ~6h (1 tenant) · labels: llm

Coverage: 1,190 of 1,215 knowledge blocks clustered (97.9%).
Operational, deliberately outside the topic map: checkpoint — 5,881 blocks (type policy, not a gap: hermes compaction artefacts).
Raw corpus count: 7,096 active blocks in the read window.
Outside the cluster cut per type policy: issue, comment, system-meta.
Anything newer than the cluster state is NOT contained here.

## Topics
019fd1a2-4c7b-7e11-9a30-2f6b81c40d55 Retrieval-Architektur & Vektor-Stack 133 learnings 50 · decisions 30 · infrastructure 22 repr=019c3269-7427-75ef-ae61-12a9dae82098
019fd1a3-9e02-7b84-8c17-71ad3fe0912c Sicherheitsvorfälle & CVE-Mitigation 92 infrastructure 39 · learnings 32 · projects 15 repr=019d4e26-e9d4-7d41-affb-5f053e804f2b scopes=private,shared
019fd1a4-1122-7c33-8d44-91be5ac07711 Multi-Tenant-Scope-Modell 930 decisions 500 · reference 430 repr=019d25d8-b8aa-7f02-8ad0-e0bba7b7cfcf

## Not listed individually
26 clusters with ≤2 blocks (35 blocks) — link-poor, no topic of their own.
0 further clusters (0 blocks) cut by the line budget.
`

// TestRenderGoldenEN is the English byte contract, the twin of TestRenderGolden.
// It covers head, cadence, the coverage branch with a known operational count,
// the rationale line, topic rows, footer — and the number swap: thousands groups
// become "1,190" and the share becomes "97.9%" without the German space.
//
// The TOPIC LABELS stay German in this fixture on purpose. They are corpus data
// produced by the label pipeline (which reads the same key), not scaffolding —
// a renderer that touched them would be translating the corpus.
func TestRenderGoldenEN(t *testing.T) {
	got, err := rootmap.Render(goldenInputEN(t, "en"))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got.Text != goldenTextEN {
		t.Errorf("EN golden mismatch:\n--- got ---\n%s\n--- want ---\n%s", got.Text, goldenTextEN)
	}
}

// TestRenderRegionalVariantStaysGerman is gate (a): only the PRIMARY SUBTAG
// decides, so a regional variant must not fall out of its language's branch —
// the same guarantee the report and the label prompt give.
//
// RED PROBE: switch phrasesFor to an exact "de" match ⇒ de-CH renders the
// English table and every Swiss install rewrites all its maps.
func TestRenderRegionalVariantStaysGerman(t *testing.T) {
	for _, tag := range []string{"de", "de-CH", "DE-de", "  de-DE  "} {
		in := goldenInput(t)
		in.Language = tag
		got, err := rootmap.Render(in)
		if err != nil {
			t.Fatalf("Render(%q): %v", tag, err)
		}
		if got.Text != goldenText {
			t.Errorf("%q did not render the frozen German map:\n%s", tag, got.Text)
		}
	}
}

// TestRenderUnknownTagRendersEnglish is gate (b): exactly two scaffolding tables
// exist, so an unlabelled third language gets a CONSISTENT English map rather
// than German scaffolding around its own labels — which is the defect #34
// reports, not the fallback.
func TestRenderUnknownTagRendersEnglish(t *testing.T) {
	for _, tag := range []string{"fr", "tr", "ja", "en-GB"} {
		got, err := rootmap.Render(goldenInputEN(t, tag))
		if err != nil {
			t.Fatalf("Render(%q): %v", tag, err)
		}
		if got.Text != goldenTextEN {
			t.Errorf("%q did not render the English table:\n%s", tag, got.Text)
		}
	}
}

// germanScaffolding are words that can only come from the German table. The list
// deliberately reaches past head/coverage/footer into writeCapBlock, headParts,
// staleNote, renderSuper and label — those five writers are the ones a threading
// pass forgets, and a forgotten one produces a mixed-language artefact in
// production rather than a compile error.
var germanScaffolding = []string{
	"Themen", "Blöcke", "Deckung", "Nicht einzeln", "geführt",
	"Kandidaten", "Cluster-Stand", "eingefroren", "übersprungen", "unbekannt",
	"Scopes", "Scope ", "Erfolgs-Stempel", "weitere", "Gruppen", "Meta-Ebene",
	"(ohne Titel)", "Lücke", "Rohzahl", "Korpusgröße", "Tenants", "Kadenz",
	"letzter Versuch", "link-arm", "Typ-Policy", "Rebuild gelaufen", "Keine Cluster",
}

// enVariants renders the English map through every writer the package has, one
// fixture per branch. Row labels are neutralised because the grep below is about
// SCAFFOLDING — a German topic label is corpus data and must survive verbatim.
func enVariants(t *testing.T) map[string]string {
	t.Helper()
	base := func() rootmap.Input {
		in := goldenInputEN(t, "en")
		for i := range in.Rows {
			in.Rows[i].Label = "Topic " + string(rune('A'+i))
		}
		return in
	}
	// Same reason as the row labels: a group name is the lead TOPIC's name,
	// corpus data the renderer passes through untouched.
	groups := func(n int) []rootmap.SuperRow {
		rows := superRows(n)
		for i := range rows {
			rows[i].Label = "Group " + pad(i)
		}
		return rows
	}
	empty := func(mut func(*rootmap.Input)) rootmap.Input {
		in := base()
		in.Rows = nil
		in.Coverage.ClusterTotal, in.Coverage.ClusteredBlocks = 0, 0
		in.Coverage.SmallClusterN, in.Coverage.SmallClusterSize = 0, 0
		in.Freshness.ClusterN = 0
		mut(&in)
		return in
	}

	inputs := map[string]rootmap.Input{}
	inputs["plain"] = base()

	for _, reason := range []string{"node-cap", "timeout", "error", "disabled",
		"registry-unwired", "empty-node-cut", "quota-exotic"} {
		in := base()
		in.Freshness.SkipReason = reason
		inputs["freeze/"+reason] = in
	}
	in := base()
	in.Freshness.SkipReason = "node-cap"
	in.Freshness.ComputedAt, in.Freshness.LastAttemptAt = nil, nil
	inputs["freeze/never-built"] = in

	for name, stale := range map[string][]string{
		"stale/one":  {"gone"},
		"stale/many": {"gone", "older"},
	} {
		in := base()
		in.Freshness.StaleScopes = stale
		inputs[name] = in
	}

	in = base()
	in.Freshness.TenantCount = 20
	inputs["cadence/plural"] = in

	in = base()
	in.Coverage.ActiveKnown = false
	in.Coverage.ActiveBlocks = 0
	inputs["coverage/no-denominator"] = in

	in = base()
	in.Coverage.OperationalKnown = false
	inputs["coverage/raw"] = in

	in = base()
	in.Rows[0].StableID = ""
	in.Rows[1].Label, in.Rows[1].ReprTitle = "", ""
	inputs["rows/placeholder-and-unstable"] = in

	in = base()
	in.SuperRows = groups(4)
	in.Freshness.SuperKnown, in.Freshness.SuperN = true, 4
	in.Freshness.SuperResolution = 0.45
	inputs["super/rendered"] = in

	in = base()
	in.SuperRows = groups(400)
	in.Freshness.SuperKnown, in.Freshness.SuperN = true, 400
	in.Freshness.SuperResolution = 0.2
	in.BudgetBytes = 4096
	inputs["super/cut"] = in

	in = base()
	in.Freshness.SuperKnown, in.Freshness.SuperN = true, 0
	inputs["super/capped"] = in

	inputs["empty/built"] = empty(func(*rootmap.Input) {})
	inputs["empty/never-built"] = empty(func(i *rootmap.Input) { i.Freshness.ComputedAt = nil })
	inputs["empty/rebuilding"] = empty(func(i *rootmap.Input) { i.Freshness.ClusterN = 59 })
	inputs["empty/stale-only"] = empty(func(i *rootmap.Input) {
		i.Freshness.ComputedAt = nil
		i.Freshness.StaleScopes = []string{"gone"}
	})

	squeeze := bulkInput(t, 500, 4096, 512)
	squeeze.Language = "en"
	squeeze.OperationalRationale = "hermes compaction artefacts"
	for i := range squeeze.Rows {
		squeeze.Rows[i].Label = "Topic " + pad(i)
	}
	inputs["budget/squeeze"] = squeeze

	out := make(map[string]string, len(inputs))
	for name, v := range inputs {
		got, err := rootmap.Render(v)
		if err != nil {
			t.Fatalf("Render(%s): %v", name, err)
		}
		out[name] = got.Text
	}
	return out
}

// TestEnglishMapCarriesNoGermanScaffolding is gate (c): a writer that was not
// threaded fails HERE instead of in a live map.
func TestEnglishMapCarriesNoGermanScaffolding(t *testing.T) {
	for name, text := range enVariants(t) {
		for _, word := range germanScaffolding {
			if strings.Contains(text, word) {
				t.Errorf("%s: German scaffolding %q survived into the English map:\n%s", name, word, text)
			}
		}
	}
}

// TestEnglishFreezeKeepsOperatorTokens is gate (d): translating the clause must
// not translate the GREP HANDLES. The raw skip_reason and the config key names
// are how an operator connects a map to a log line and a settings row — prose
// around them may move, they may not.
func TestEnglishFreezeKeepsOperatorTokens(t *testing.T) {
	for reason, token := range map[string]string{
		"node-cap":         "node-cap",
		"timeout":          "rebuild_timeout",
		"disabled":         "graph_overview.enabled=false",
		"registry-unwired": "type registry not wired",
		"empty-node-cut":   "type policy",
		"quota-exotic":     "quota-exotic", // unknown reason ⇒ named verbatim
	} {
		in := goldenInputEN(t, "en")
		in.Freshness.SkipReason = reason
		got, err := rootmap.Render(in)
		if err != nil {
			t.Fatalf("Render(%s): %v", reason, err)
		}
		head := got.Text
		if len(head) > 200 {
			head = head[:200]
		}
		if !strings.Contains(head, "!! Partition frozen") {
			t.Errorf("%s: no English cap warning in the first 200 bytes:\n%s", reason, head)
		}
		if !strings.Contains(got.Text, token) {
			t.Errorf("%s: operator token %q lost in translation:\n%s", reason, token, got.Text)
		}
		if !strings.Contains(got.Text, "is OLD.") {
			t.Errorf("%s: English cap block does not mark the cluster state as old", reason)
		}
	}

	// The capped meta level names its key in English too — a cap that cannot be
	// tied back to root_map.super_max_nodes is a cap nobody can act on.
	in := goldenInputEN(t, "en")
	in.Freshness.SuperKnown, in.Freshness.SuperN = true, 0
	got, err := rootmap.Render(in)
	if err != nil {
		t.Fatalf("Render(super-capped): %v", err)
	}
	if !strings.Contains(got.Text, "Meta level: skipped") || !strings.Contains(got.Text, "root_map.super_max_nodes") {
		t.Errorf("English capped meta level lost its cap or its key:\n%s", got.Text)
	}
}

// TestEnglishEmptyAndStalePaths is gate (e): emptyStatement and staleNote are the
// two writers outside the main render path, and therefore the two easiest to
// leave behind.
func TestEnglishEmptyAndStalePaths(t *testing.T) {
	in := goldenInputEN(t, "en")
	in.Freshness.StaleScopes = []string{"gone", "older"}
	got, err := rootmap.Render(in)
	if err != nil {
		t.Fatalf("Render(stale): %v", err)
	}
	if !strings.Contains(got.Text, "Note: 2 scopes without live cluster rows but with an old success stamp (gone, older) — neither coverage nor gap.") {
		t.Errorf("English stale note missing or malformed:\n%s", got.Text)
	}
	in.Freshness.StaleScopes = []string{"gone"}
	got, err = rootmap.Render(in)
	if err != nil {
		t.Fatalf("Render(stale-singular): %v", err)
	}
	if !strings.Contains(got.Text, "Note: 1 scope without live cluster rows") {
		t.Errorf("English stale note does not use the singular:\n%s", got.Text)
	}

	base := func() rootmap.Input {
		i := goldenInputEN(t, "en")
		i.Rows = nil
		i.Coverage.ClusterTotal, i.Coverage.ClusteredBlocks = 0, 0
		i.Coverage.SmallClusterN, i.Coverage.SmallClusterSize = 0, 0
		i.Freshness.ClusterN = 0
		return i
	}
	for _, tc := range []struct {
		name string
		mut  func(*rootmap.Input)
		want string
	}{
		{"built-empty", func(*rootmap.Input) {}, "Rebuild ran (2026-08-04T07:54Z), no clusters in this scope."},
		{"never-built", func(i *rootmap.Input) { i.Freshness.ComputedAt = nil }, "No clusters built yet — this map is empty, not incomplete."},
		{"rebuilding", func(i *rootmap.Input) { i.Freshness.ClusterN = 59 }, "and reports 59 topics"},
		{"stale-only", func(i *rootmap.Input) {
			i.Freshness.ComputedAt = nil
			i.Freshness.StaleScopes = []string{"gone"}
		}, "old success stamps without a partition."},
	} {
		in := base()
		tc.mut(&in)
		got, err := rootmap.Render(in)
		if err != nil {
			t.Fatalf("Render(%s): %v", tc.name, err)
		}
		if !strings.Contains(got.Text, tc.want) {
			t.Errorf("%s: English empty statement missing %q:\n%s", tc.name, tc.want, got.Text)
		}
	}
}

// TestBudgetHoldsInBothLanguages is gate (f). The measuring loop counts BYTES,
// and the two tables have different lengths — so the budget proof has to be
// re-stated per language rather than inherited from the German one.
//
// The footer gets its own assertion: Render REFUSES a map whose footer outgrows
// FooterReserveBytes, so an overlong translation there would not degrade, it
// would take the artefact away entirely.
func TestBudgetHoldsInBothLanguages(t *testing.T) {
	for _, lang := range []string{"", "de", "en", "fr"} {
		in := goldenInput(t)
		in.Language = lang
		got, err := rootmap.Render(in)
		if err != nil {
			t.Fatalf("Render(%q): %v", lang, err)
		}
		if len(got.Text) > in.BudgetBytes {
			t.Errorf("%q: golden render %d B over budget %d B", lang, len(got.Text), in.BudgetBytes)
		}

		squeeze := bulkInput(t, 500, 4096, 512)
		squeeze.Language = lang
		got, err = rootmap.Render(squeeze)
		if err != nil {
			t.Fatalf("Render(squeeze, %q): %v", lang, err)
		}
		if len(got.Text) > squeeze.BudgetBytes {
			t.Errorf("%q: squeeze render %d B over budget %d B", lang, len(got.Text), squeeze.BudgetBytes)
		}
		if got.Coverage.CappedClusterN <= 0 {
			t.Errorf("%q: squeeze fixture did not cut anything — the pin proves nothing", lang)
		}

		heading := "\n## Nicht einzeln geführt\n"
		if lang == "en" || lang == "fr" {
			heading = "\n## Not listed individually\n"
		}
		i := strings.Index(got.Text, heading)
		if i < 0 {
			t.Fatalf("%q: no footer in the rendered map:\n%s", lang, got.Text)
		}
		if n := len(got.Text) - i; n >= squeeze.FooterReserveBytes {
			t.Errorf("%q: footer is %d B against a %d B reserve — Render would refuse the map",
				lang, n, squeeze.FooterReserveBytes)
		}
	}
}

// TestOperationalRationaleFollowsLanguage pins the ONE rendered string that does
// not live in the phrases table: the operational clause is derived from the
// block-type registry on the write path, and the issue's own inventory missed
// that it lands inside the coverage line.
func TestOperationalRationaleFollowsLanguage(t *testing.T) {
	set := blocktype.NewRegistry().Snapshot()
	for tag, want := range map[string]string{
		"":      "hermes-Compaction-Artefakte",
		"de-DE": "hermes-Compaction-Artefakte",
		"en":    "hermes compaction artefacts",
		"fr":    "hermes compaction artefacts",
	} {
		types, rationale := rootmap.OperationalTypes(set, tag)
		if len(types) != 1 || types[0] != "checkpoint" {
			t.Fatalf("%q: operational types = %v, want [checkpoint]", tag, types)
		}
		if rationale != want {
			t.Errorf("%q: rationale = %q, want %q", tag, rationale, want)
		}
	}
}
