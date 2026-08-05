// Wave W-C gates (Cluster-Topic-Map, design/02 §7 "W-C"). Pure unit tests — the
// renderer touches no database, so every gate runs under -short in
// milliseconds, which is what makes 50-run determinism checks affordable.
package rootmap_test

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/rootmap"
)

func ts(t *testing.T, s string) *time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad fixture timestamp %q: %v", s, err)
	}
	return &v
}

// goldenInput is the normative fixture (§4.3), adapted in exactly two places
// and in no others:
//
//   - Three topic lines instead of 33: the design fixture elides its rows with
//     "…" and is therefore not literally reproducible. The MECHANICS are kept —
//     the sum invariants 3 + 26 + 0 = 29 clusters and 1.155 + 35 + 0 = 1.190
//     blocks are the same arithmetic the doc checks, so a miscalculation in the
//     example still reddens this test.
//   - The coverage section follows amendment A02-2 (decision E11-02):
//     checkpoints are NOT a gap. The coverage figure is measured against the
//     knowledge corpus, the operational types get their own line, and the raw
//     corpus number stays visible next to it.
func goldenInput(t *testing.T) rootmap.Input {
	t.Helper()
	return rootmap.Input{
		Scope: "private",
		Rows: []rootmap.Row{
			{
				StableID: "019fd1a2-4c7b-7e11-9a30-2f6b81c40d55", Label: "Retrieval-Architektur & Vektor-Stack",
				LabelSource: "llm", Size: 133,
				TopCats: []string{"learnings", "decisions", "infrastructure"}, CatCounts: []int{50, 30, 22},
				ReprID: "019c3269-7427-75ef-ae61-12a9dae82098", ReprTitle: "RRF-Gen15",
				ScopeMix: []string{"private"},
			},
			{
				StableID: "019fd1a3-9e02-7b84-8c17-71ad3fe0912c", Label: "Sicherheitsvorfälle & CVE-Mitigation",
				LabelSource: "llm", Size: 92,
				TopCats: []string{"infrastructure", "learnings", "projects"}, CatCounts: []int{39, 32, 15},
				ReprID: "019d4e26-e9d4-7d41-affb-5f053e804f2b", ReprTitle: "GHSA-Runbook",
				ScopeMix: []string{"private", "shared"},
			},
			{
				StableID: "019fd1a4-1122-7c33-8d44-91be5ac07711", Label: "Multi-Tenant-Scope-Modell",
				LabelSource: "llm", Size: 930,
				TopCats: []string{"decisions", "reference"}, CatCounts: []int{500, 430},
				ReprID: "019d25d8-b8aa-7f02-8ad0-e0bba7b7cfcf", ReprTitle: "Modell C",
				ScopeMix: []string{"private"},
			},
		},
		Coverage: rootmap.Coverage{
			ClusterTotal: 29, ClusteredBlocks: 1190,
			SmallClusterN: 26, SmallClusterSize: 35,
			CandidateBlocks: 1191,
			ActiveBlocks:    7096, ActiveKnown: true,
			OperationalTypes: []string{"checkpoint"}, OperationalBlocks: 5881, OperationalKnown: true,
			ExcludedTypes: []string{"issue", "comment", "system-meta"},
		},
		Freshness: rootmap.Freshness{
			ComputedAt: ts(t, "2026-08-04T07:54:14Z"), LastAttemptAt: ts(t, "2026-08-04T13:54:02Z"),
			ClusterN: 29, CandidateN: 1191,
			Interval: 6 * time.Hour, TenantCount: 1,
			Modularity: 0.8768136, Resolution: 1.0,
		},
		BudgetBytes: 15360, FooterReserveBytes: 512, SmallClusterMax: 2,
		OperationalRationale: "hermes-Compaction-Artefakte",
	}
}

const goldenText = `ctx Root Map v1 | scope:private | 3/29 Themen | 1.155/1.190 Blöcke geführt | Q=0.877 γ=1.0
Cluster-Stand 2026-08-04T07:54Z · letzter Versuch 2026-08-04T13:54Z · erwartete Kadenz ~6h (1 Tenant) · Labels: llm

Deckung: 1.190 von 1.215 Wissens-Blöcken geclustert (97,9 %).
Operativ, bewusst außerhalb der Themenkarte: checkpoint — 5.881 Blöcke (Typ-Policy, keine Lücke: hermes-Compaction-Artefakte).
Korpus-Rohzahl: 7.096 aktive Blöcke im Lesefenster.
Außerhalb des Cluster-Schnitts per Typ-Policy: issue, comment, system-meta.
Alles nach dem Cluster-Stand ist hier NICHT enthalten.

## Themen
019fd1a2-4c7b-7e11-9a30-2f6b81c40d55 Retrieval-Architektur & Vektor-Stack 133 learnings 50 · decisions 30 · infrastructure 22 repr=019c3269-7427-75ef-ae61-12a9dae82098
019fd1a3-9e02-7b84-8c17-71ad3fe0912c Sicherheitsvorfälle & CVE-Mitigation 92 infrastructure 39 · learnings 32 · projects 15 repr=019d4e26-e9d4-7d41-affb-5f053e804f2b scopes=private,shared
019fd1a4-1122-7c33-8d44-91be5ac07711 Multi-Tenant-Scope-Modell 930 decisions 500 · reference 430 repr=019d25d8-b8aa-7f02-8ad0-e0bba7b7cfcf

## Nicht einzeln geführt
26 Cluster mit ≤2 Blöcken (35 Blöcke) — link-arm, kein eigenes Thema.
0 weitere Cluster (0 Blöcke) wegen Zeilenbudget gekappt.
`

// TestRenderGolden is gate 1: the format is a byte contract, not a suggestion.
// Single-space separation (column alignment cannot be pinned byte-exactly),
// both identifiers in full 36 chars (an 8-char prefix cannot be resolved to a
// `ctx get`), and the sum invariants visible in the numbers themselves.
func TestRenderGolden(t *testing.T) {
	got, err := rootmap.Render(goldenInput(t))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got.Text != goldenText {
		t.Errorf("golden mismatch:\n--- got ---\n%s\n--- want ---\n%s", got.Text, goldenText)
	}
	// The sums the doc checks by hand, checked by machine.
	c := got.Coverage
	if c.RenderedRows+c.SmallClusterN+c.CappedClusterN != c.ClusterTotal {
		t.Errorf("cluster buckets do not add up: %+v", c)
	}
	if c.RenderedBlocks+c.SmallClusterSize+c.CappedBlocks != c.ClusteredBlocks {
		t.Errorf("block buckets do not add up: %+v", c)
	}
	if got.Empty {
		t.Error("a map with three topic lines reported Empty")
	}
}

// bulkInput builds n synthetic clusters with a size mix, plus consistent
// totals — the shape of gate 2 and gate 10.
func bulkInput(t *testing.T, n, budget, reserve int) rootmap.Input {
	t.Helper()
	in := goldenInput(t)
	in.BudgetBytes = budget
	in.FooterReserveBytes = reserve
	in.Rows = nil
	total, small, smallBlocks, blocks := 0, 0, 0, 0
	for i := range n {
		size := 3 + (n-i)%40 // > SmallClusterMax, descending-ish
		if i%5 == 4 {
			size = 2 // small: never a topic line
			small++
			smallBlocks += size
		}
		in.Rows = append(in.Rows, rootmap.Row{
			StableID:  "019fd200-0000-7000-9000-" + pad(i),
			Label:     "Thema " + pad(i),
			Size:      size,
			TopCats:   []string{"learnings", "decisions"},
			CatCounts: []int{size / 2, size - size/2},
			ReprID:    "019fd300-0000-7000-9000-" + pad(i),
		})
		total++
		blocks += size
	}
	// Rows must be descending by size for the "top-N" semantics to hold.
	for i := 1; i < len(in.Rows); i++ {
		for j := i; j > 0 && in.Rows[j].Size > in.Rows[j-1].Size; j-- {
			in.Rows[j], in.Rows[j-1] = in.Rows[j-1], in.Rows[j]
		}
	}
	in.Coverage.ClusterTotal = total
	in.Coverage.ClusteredBlocks = blocks
	in.Coverage.SmallClusterN = small
	in.Coverage.SmallClusterSize = smallBlocks
	in.Freshness.ClusterN = total
	return in
}

func pad(i int) string {
	s := "000000000000" + string(rune('0'+i%10))
	digits := ""
	for v := i; v > 0; v /= 10 {
		digits = string(rune('0'+v%10)) + digits
	}
	if digits == "" {
		digits = "0"
	}
	return s[:12-len(digits)] + digits
}

// TestRenderBudgetAndDisjointness is gate 2 plus BP-5: 500 clusters into a 1 KB
// budget. The output stays inside the budget, both footer buckets are printed,
// the cut bucket is non-zero, and the two sum invariants hold.
//
// RED PROBE 1 (no measuring loop): append all rows unconditionally ⇒ ~40 KB of
// output against a 1024 B budget.
// RED PROBE 2 (two overlapping footer buckets, the pre-revision design): count
// a cluster that is BOTH small and cut in both lines ⇒ the bucket sum exceeds
// ClusterTotal and assertBuckets reddens.
func TestRenderBudgetAndDisjointness(t *testing.T) {
	// Two budgets: 1 KB is the BP-5 probe from the design (the skeleton barely
	// fits, so the reserve is dialled down to its own footer size — reserve is a
	// config knob, the measuring loop is not), 4 KB is the shape where topic
	// lines actually render and the ordering rule is observable.
	for _, tc := range []struct{ budget, reserve int }{{1024, 256}, {4096, 512}} {
		in := bulkInput(t, 500, tc.budget, tc.reserve)
		got, err := rootmap.Render(in)
		if err != nil {
			t.Fatalf("Render(budget=%d): %v", tc.budget, err)
		}
		if len(got.Text) > in.BudgetBytes {
			t.Errorf("budget overrun: %d B > %d B", len(got.Text), in.BudgetBytes)
		}
		if !strings.Contains(got.Text, "## Nicht einzeln geführt") {
			t.Error("footer header missing")
		}
		if !strings.Contains(got.Text, "link-arm, kein eigenes Thema.") {
			t.Error("small-cluster line missing")
		}
		if !strings.Contains(got.Text, "wegen Zeilenbudget gekappt.") {
			t.Error("capped line missing")
		}
		if got.Coverage.CappedClusterN <= 0 {
			t.Errorf("500 clusters into %d B: CappedClusterN = %d, want > 0", tc.budget, got.Coverage.CappedClusterN)
		}
		c := got.Coverage
		if c.RenderedRows+c.SmallClusterN+c.CappedClusterN != c.ClusterTotal ||
			c.RenderedBlocks+c.SmallClusterSize+c.CappedBlocks != c.ClusteredBlocks {
			t.Errorf("buckets not disjoint/complete: %+v", c)
		}
		// Topic lines stay in descending size order.
		prev := 1 << 30
		for _, line := range topicLines(got.Text) {
			size := sizeOf(t, line)
			if size > prev {
				t.Errorf("topic lines not descending: %d after %d", size, prev)
			}
			if size <= in.SmallClusterMax {
				t.Errorf("small cluster rendered as topic line: %q", line)
			}
			prev = size
		}
		if tc.budget == 4096 && got.Coverage.RenderedRows == 0 {
			t.Error("4 KB budget rendered no topic line at all — the ordering probe proves nothing")
		}
	}
}

// TestRenderBucketAssertionFires proves the invariant is an assertion, not a
// comment: inconsistent inputs (rows outside the counted window) produce an
// ERROR, not a map with numbers that do not add up.
func TestRenderBucketAssertionFires(t *testing.T) {
	in := goldenInput(t)
	in.Coverage.ClusterTotal = 2 // three rows will render — one more than exists
	if _, err := rootmap.Render(in); err == nil {
		t.Fatal("inconsistent buckets rendered a map instead of an error")
	}
}

// TestRenderFooterReserve is gate 3: an undersized reserve is an ERROR, never a
// silently truncated footer. Same reflex as the rebuild refusing to run on an
// empty type allowlist — a wiring mistake fails loudly.
func TestRenderFooterReserve(t *testing.T) {
	in := goldenInput(t)
	in.FooterReserveBytes = 8
	if _, err := rootmap.Render(in); err == nil {
		t.Fatal("footer over its reserve was accepted; want an error")
	}
	// And a budget too small for the skeleton fails before writing anything.
	in = goldenInput(t)
	in.BudgetBytes = 200
	if _, err := rootmap.Render(in); err == nil {
		t.Fatal("200 B budget rendered a map; want an error")
	}
}

// TestRenderRuneSafety is gate 4: labels are cut at RUNE boundaries. Byte
// slicing splits a multi-byte character and PostgreSQL rejects the write with
// 22021 — the trap the old title truncation fell into.
func TestRenderRuneSafety(t *testing.T) {
	for _, label := range []string{
		strings.Repeat("🌱", 80),
		strings.Repeat("森", 80),
		strings.Repeat("ä", 59) + "🌱🌱",
		// Byte 60 lands INSIDE a rune here — the shape byte slicing breaks and
		// an all-emoji label (4 bytes each) would never expose.
		strings.Repeat("a", 59) + "äöü",
	} {
		in := goldenInput(t)
		in.Rows = in.Rows[:1]
		in.Rows[0].Label = label
		in.Coverage.ClusterTotal = 27 // 1 + 26 small
		in.Coverage.ClusteredBlocks = 133 + 35
		got, err := rootmap.Render(in)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		if !utf8.ValidString(got.Text) {
			t.Errorf("invalid UTF-8 for label %q", label[:12])
		}
		for _, line := range topicLines(got.Text) {
			if !utf8.ValidString(line) || strings.ContainsRune(line, utf8.RuneError) {
				t.Errorf("topic line broken by byte slicing: %q", line)
			}
		}
	}
}

// TestRenderDeterminism is gate 5: identical input, identical bytes, 50 times.
func TestRenderDeterminism(t *testing.T) {
	in := bulkInput(t, 200, 4096, 512)
	first, err := rootmap.Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for i := range 50 {
		got, err := rootmap.Render(in)
		if err != nil {
			t.Fatalf("Render run %d: %v", i, err)
		}
		if got.Text != first.Text {
			t.Fatalf("run %d differs from run 0", i)
		}
	}
}

// TestRenderCapLine is gate 6: every FREEZE reason produces a warning inside
// the first 200 bytes (rule R2 — a truncated context must still carry it), and
// advisory-lock produces NONE.
//
// RED PROBE: render any non-empty skip_reason as a cap ⇒ the advisory-lock case
// grows a cap line for a partition that is being rebuilt successfully right now.
func TestRenderCapLine(t *testing.T) {
	for _, reason := range []string{"node-cap", "timeout", "error", "disabled", "registry-unwired"} {
		in := goldenInput(t)
		in.Freshness.SkipReason = reason
		got, err := rootmap.Render(in)
		if err != nil {
			t.Fatalf("Render(%s): %v", reason, err)
		}
		head := got.Text
		if len(head) > 200 {
			head = head[:200]
		}
		if !strings.Contains(head, "!! Partition eingefroren") {
			t.Errorf("%s: no cap warning in the first 200 bytes:\n%s", reason, head)
		}
		if !strings.Contains(got.Text, "ist ALT.") {
			t.Errorf("%s: cap block does not mark the cluster state as old", reason)
		}
	}

	in := goldenInput(t)
	in.Freshness.SkipReason = "advisory-lock"
	in.Freshness.Contended = true
	got, err := rootmap.Render(in)
	if err != nil {
		t.Fatalf("Render(advisory-lock): %v", err)
	}
	if strings.Contains(got.Text, "Partition eingefroren") {
		t.Error("advisory-lock rendered a cap line — contention is not a freeze")
	}
}

// TestRenderCadenceHonesty is gate 7: the head prints the EFFECTIVE cadence
// (interval × tenants) plus the tenant count, never the raw config value. At 20
// tenants a printed "6h" is a freshness promise over a partition that is five
// days old on average.
func TestRenderCadenceHonesty(t *testing.T) {
	in := goldenInput(t)
	in.Freshness.TenantCount = 20
	got, err := rootmap.Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	line := strings.Split(got.Text, "\n")[1]
	if strings.Contains(line, "~6h") {
		t.Errorf("raw config interval printed as the cadence: %q", line)
	}
	if !strings.Contains(line, "~5d") || !strings.Contains(line, "(20 Tenants)") {
		t.Errorf("effective cadence or tenant count missing: %q", line)
	}
	if !strings.Contains(line, "letzter Versuch") {
		t.Errorf("last attempt missing beside the cluster state: %q", line)
	}

	// No tenant count ⇒ no cadence claim at all, rather than a raw value.
	in.Freshness.TenantCount = 0
	got, err = rootmap.Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(got.Text, "Kadenz") {
		t.Error("cadence claimed without a tenant count")
	}
}

// TestRenderEmptyStates is gate 8 plus the D4 revision: "built, empty" is a
// LIVE state (scope='work' carries computed_at with cluster_n = 0) and must not
// fall through every branch into a freshness head over an empty topic list.
func TestRenderEmptyStates(t *testing.T) {
	base := func() rootmap.Input {
		in := goldenInput(t)
		in.Rows = nil
		in.Coverage.ClusterTotal, in.Coverage.ClusteredBlocks = 0, 0
		in.Coverage.SmallClusterN, in.Coverage.SmallClusterSize = 0, 0
		in.Freshness.ClusterN = 0
		return in
	}

	// (a) built, empty — computed_at set.
	got, err := rootmap.Render(base())
	if err != nil {
		t.Fatalf("Render(built-empty): %v", err)
	}
	if !got.Empty {
		t.Error("built-empty map not flagged Empty")
	}
	if !strings.Contains(got.Text, "keine Cluster in diesem Scope") {
		t.Errorf("built-empty map has no empty statement:\n%s", got.Text)
	}

	// (b) never built — computed_at nil. This is the ONLY case that may be
	// written over an existing map.
	in := base()
	in.Freshness.ComputedAt = nil
	got, err = rootmap.Render(in)
	if err != nil {
		t.Fatalf("Render(never-built): %v", err)
	}
	if !strings.Contains(got.Text, "Noch keine Cluster gebaut") {
		t.Errorf("never-built map has no empty statement:\n%s", got.Text)
	}
	if !rootmap.AllowsUpsert(in, got) {
		t.Error("a genuinely fresh tenant may not write its first map")
	}
}

// TestAllowsUpsertBP6 is the BP-6 rule: an empty or stale map NEVER replaces a
// good one.
//
// RED PROBE: drop the ComputedAt half of the condition ⇒ the TRUNCATE-window
// case (a) passes and a working map is replaced by an empty one; drop the stale
// half ⇒ case (c) does the same for a partition that no longer exists.
func TestAllowsUpsertBP6(t *testing.T) {
	empty := func(mut func(*rootmap.Input)) (rootmap.Input, rootmap.Rendered) {
		in := goldenInput(t)
		in.Rows = nil
		in.Coverage.ClusterTotal, in.Coverage.ClusteredBlocks = 0, 0
		in.Coverage.SmallClusterN, in.Coverage.SmallClusterSize = 0, 0
		mut(&in)
		got, err := rootmap.Render(in)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		return in, got
	}

	// (a) mid-rebuild TRUNCATE window: no clusters visible, meta still reports
	// the OLD partition. Writing here destroys a good map.
	in, got := empty(func(i *rootmap.Input) { i.Freshness.ClusterN = 59 })
	if rootmap.AllowsUpsert(in, got) {
		t.Error("empty map allowed over a partition that reports 59 clusters (BP-6)")
	}

	// (b) built-empty partition: computed_at set, cluster_n = 0. Still no write
	// — a previously good map must survive it.
	in, got = empty(func(i *rootmap.Input) { i.Freshness.ClusterN = 0 })
	if rootmap.AllowsUpsert(in, got) {
		t.Error("empty map allowed although the partition was built before (BP-6)")
	}

	// (c) stale-only window: the meta rows are old success stamps without a
	// partition. Neither coverage nor gap — and never a write.
	in, got = empty(func(i *rootmap.Input) {
		i.Freshness.ComputedAt = nil
		i.Freshness.ClusterN = 0
		i.Freshness.StaleScopes = []string{"gone"}
	})
	if rootmap.AllowsUpsert(in, got) {
		t.Error("empty map allowed on a stale-only window (P0 teardown follow-up)")
	}
	if !strings.Contains(got.Text, "alte Erfolgs-Stempel") {
		t.Errorf("stale-only window does not say what it saw:\n%s", got.Text)
	}

	// A non-empty map is always writable.
	full, err := rootmap.Render(goldenInput(t))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !rootmap.AllowsUpsert(goldenInput(t), full) {
		t.Error("a full map was refused")
	}
}

// TestRenderStaleScopeIsNeitherCoverageNorGap is the P0-merge follow-up gate on
// the renderer side: a scope whose meta row outlived its partition is NAMED,
// counted as neither coverage nor gap, and does not turn the map into an
// escalation. The read side (store.OverviewMeta) already keeps its numbers out
// of every sum; here the map has to say so.
func TestRenderStaleScopeIsNeitherCoverageNorGap(t *testing.T) {
	in := goldenInput(t)
	in.Freshness.StaleScopes = []string{"gone", "older"}
	got, err := rootmap.Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(got.Text, "2 Scopes ohne aktive Cluster-Zeilen mit altem Erfolgs-Stempel (gone, older) — weder Deckung noch Lücke.") {
		t.Errorf("stale scopes not named as neither coverage nor gap:\n%s", got.Text)
	}
	// The coverage arithmetic is untouched by them.
	if !strings.Contains(got.Text, "Deckung: 1.190 von 1.215 Wissens-Blöcken geclustert (97,9 %).") {
		t.Errorf("stale scopes moved the coverage figure:\n%s", got.Text)
	}
	if strings.Contains(got.Text, "!!") {
		t.Error("stale scopes escalated into a cap line")
	}
}

// TestRenderTimeoutDenominator is gate 9: when the capped corpus count times
// out the map prints NEITHER a denominator NOR a percentage. No estimate — a
// number that cannot be scope-filtered has no place in a persisted artefact.
func TestRenderTimeoutDenominator(t *testing.T) {
	in := goldenInput(t)
	in.Coverage.ActiveKnown = false
	in.Coverage.ActiveBlocks = 0
	got, err := rootmap.Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(got.Text, "Korpusgröße in dieser Runde nicht ermittelt.") {
		t.Errorf("no honest statement about the missing denominator:\n%s", got.Text)
	}
	if strings.Contains(got.Text, "%") {
		t.Error("a percentage was printed without a known denominator")
	}
	if strings.Contains(got.Text, "Korpus-Rohzahl") {
		t.Error("a raw corpus number was printed although the count timed out")
	}
}

// TestRenderKnowledgeCorpusCoverage is amendment A02-2 (decision E11-02): the
// coverage figure describes the KNOWLEDGE corpus, checkpoints appear as an
// operational line rather than a gap, and the raw number stays visible.
//
// RED PROBE: divide by the raw corpus ⇒ the map prints 16,8 % and reads as
// "83 % missing", which is precisely the re-escalation this wording prevents.
func TestRenderKnowledgeCorpusCoverage(t *testing.T) {
	got, err := rootmap.Render(goldenInput(t))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(got.Text, "von 1.215 Wissens-Blöcken geclustert (97,9 %)") {
		t.Errorf("coverage is not measured against the knowledge corpus:\n%s", got.Text)
	}
	if !strings.Contains(got.Text, "Operativ, bewusst außerhalb der Themenkarte: checkpoint — 5.881 Blöcke (Typ-Policy, keine Lücke: hermes-Compaction-Artefakte).") {
		t.Errorf("operational types are not declared as a decision:\n%s", got.Text)
	}
	if !strings.Contains(got.Text, "Korpus-Rohzahl: 7.096 aktive Blöcke") {
		t.Errorf("raw corpus number missing beside the coverage figure:\n%s", got.Text)
	}
	// Unknown operational count ⇒ raw denominator, explicitly labelled as raw.
	in := goldenInput(t)
	in.Coverage.OperationalKnown = false
	got, err = rootmap.Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(got.Text, "Rohzahl inkl. operativer Typen") {
		t.Errorf("unknown operational count not disclosed:\n%s", got.Text)
	}
}

// TestRenderDegradationLadder covers D1–D3: heuristic labels, no label at all,
// and no stable identity. Each step still produces a valid, self-describing
// line — the map never needs an LLM to exist.
func TestRenderDegradationLadder(t *testing.T) {
	// D1/D2: heuristic label, then none at all (fall back to repr_title).
	in := goldenInput(t)
	in.Rows[0].LabelSource = "heuristic"
	in.Rows[1].Label, in.Rows[1].LabelSource = "", ""
	got, err := rootmap.Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(strings.Split(got.Text, "\n")[1], "Labels: llm, heuristic, repr_title") {
		t.Errorf("label provenance not disclosed:\n%s", got.Text)
	}
	if !strings.Contains(got.Text, "GHSA-Runbook") {
		t.Error("label-less row did not fall back to its representative title")
	}

	// D3: no stable id — the repr handle stays, the head says so.
	in = goldenInput(t)
	for i := range in.Rows {
		in.Rows[i].StableID = ""
	}
	got, err = rootmap.Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(got.Text, "Themen ohne stabile ID") {
		t.Errorf("missing stable ids not disclosed:\n%s", got.Text)
	}
	for _, line := range topicLines(got.Text) {
		if !strings.Contains(line, "repr=") {
			t.Errorf("row without any handle at all: %q", line)
		}
	}
}

// TestNodeLimitFor pins the pessimistic query limit (§4.5): it must fetch
// rather too many clusters than too few, and never zero.
func TestNodeLimitFor(t *testing.T) {
	if n := rootmap.NodeLimitFor(15360, 512); n < 95 {
		t.Errorf("NodeLimitFor(15360, 512) = %d, want >= 95 (pessimistic)", n)
	}
	if n := rootmap.NodeLimitFor(1024, 512); n < 1 {
		t.Errorf("NodeLimitFor(1024, 512) = %d, want >= 1", n)
	}
}

func topicLines(text string) []string {
	_, rest, ok := strings.Cut(text, "## Themen\n")
	if !ok {
		return nil
	}
	body, _, _ := strings.Cut(rest, "\n## Nicht einzeln geführt")
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// sizeOf extracts the size field of a topic line: it follows the label and
// precedes the category mix. Parsed from the RIGHT so labels containing spaces
// cannot confuse it — the field before the first "cat n" pair.
func sizeOf(t *testing.T, line string) int {
	t.Helper()
	fields := strings.Fields(line)
	for i, f := range fields {
		if i+1 < len(fields) && isNum(f) && !isNum(fields[i+1]) && strings.Contains(line, " repr=") {
			// first purely numeric field followed by a category name
			return atoi(f)
		}
	}
	t.Fatalf("no size field in %q", line)
	return 0
}

func isNum(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r == '.' {
			continue
		}
		n = n*10 + int(r-'0')
	}
	return n
}
