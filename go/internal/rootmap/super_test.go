// Renderer gates of wave W-F (Cluster-Topic-Map, design/02 §4.7 step 5): the
// meta-cluster section of the root map. Pure, like the rest of the package.
package rootmap_test

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/rootmap"
)

func superRows(n int) []rootmap.SuperRow {
	rows := make([]rootmap.SuperRow, 0, n)
	for i := range n {
		rows = append(rows, rootmap.SuperRow{
			ID:    "019fd2a0-0000-7000-8000-00000000000" + string(rune('0'+i%10)),
			Label: "Themengruppe " + string(rune('A'+i%26)),
			Size:  500 - i, TopicN: 20 - i%17,
		})
	}
	return rows
}

// The shipped default is silence: without an attempted level the map is BYTE
// IDENTICAL to what W-C/W-D produce. That is the whole "dunkel ausliefern"
// claim, and it is the only form of it that can be checked.
func TestSuperSectionAbsentWhenNotAttempted(t *testing.T) {
	in := goldenInput(t)
	base, err := rootmap.Render(in)
	if err != nil {
		t.Fatalf("render baseline: %v", err)
	}

	// Rows present but the level never attempted — the section must still not
	// appear, because SuperKnown is what says whether a level exists.
	in.SuperRows = superRows(5)
	got, err := rootmap.Render(in)
	if err != nil {
		t.Fatalf("render with rows: %v", err)
	}
	if got.Text != base.Text {
		t.Error("map changed although no meta level was attempted — the section is not dark")
	}
	if got.SuperRows != 0 {
		t.Errorf("SuperRows = %d without an attempted level", got.SuperRows)
	}
}

// A BUILT level renders above the topic list, names its γ, and reports how many
// lines it printed.
func TestSuperSectionRenders(t *testing.T) {
	in := goldenInput(t)
	in.SuperRows = superRows(4)
	in.Freshness.SuperKnown, in.Freshness.SuperN = true, 4
	in.Freshness.SuperResolution = 0.45

	got, err := rootmap.Render(in)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(got.Text, "## Themen-Gruppen (Meta-Ebene, γ=0.45)") {
		t.Errorf("section heading missing or without γ:\n%s", got.Text)
	}
	if got.SuperRows != 4 {
		t.Errorf("SuperRows = %d, want 4", got.SuperRows)
	}
	iSuper := strings.Index(got.Text, "## Themen-Gruppen")
	iTopics := strings.Index(got.Text, "\n## Themen\n")
	if iSuper < 0 || iTopics < 0 || iSuper > iTopics {
		t.Errorf("meta section must sit ABOVE the topic list (super=%d topics=%d)", iSuper, iTopics)
	}
	if !strings.Contains(got.Text, "Themengruppe A 500 Blöcke · 20 Themen") {
		t.Errorf("group line missing its two numbers:\n%s", got.Text)
	}
	if len(got.Text) > in.BudgetBytes {
		t.Errorf("rendered %d B over budget %d B", len(got.Text), in.BudgetBytes)
	}
}

// A CAPPED level is a cap and must read like one. Rendering it as absence would
// be the invisible freeze that migration 123 exists to abolish — one level up.
func TestSuperSectionCapIsNamed(t *testing.T) {
	in := goldenInput(t)
	in.Freshness.SuperKnown, in.Freshness.SuperN = true, 0

	got, err := rootmap.Render(in)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(got.Text, "Meta-Ebene: übersprungen") ||
		!strings.Contains(got.Text, "super_max_nodes") {
		t.Errorf("a capped meta level renders as silence:\n%s", got.Text)
	}
	if strings.Contains(got.Text, "## Themen-Gruppen") {
		t.Error("capped level printed a group heading")
	}
	if !strings.Contains(got.Text, "## Themen\n") {
		t.Error("the topic list disappeared over a capped meta level")
	}
}

// THE HALF RULE: the coarse level may summarise the map, never replace it. With
// far more groups than budget the section is cut, says so, and the topic list
// still gets lines.
//
// Rot gegen eine Fassung ohne die Deckelung: the section would eat the whole
// budget and the topic list would be empty.
func TestSuperSectionNeverStarvesTheTopicList(t *testing.T) {
	in := goldenInput(t)
	in.SuperRows = superRows(400)
	in.Freshness.SuperKnown, in.Freshness.SuperN = true, 400
	in.Freshness.SuperResolution = 0.2
	in.BudgetBytes = 4096

	got, err := rootmap.Render(in)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(got.Text) > in.BudgetBytes {
		t.Fatalf("rendered %d B over budget %d B", len(got.Text), in.BudgetBytes)
	}
	if got.SuperRows == 0 || got.SuperRows >= 400 {
		t.Errorf("SuperRows = %d — the section was neither rendered nor cut", got.SuperRows)
	}
	if !strings.Contains(got.Text, "weitere Gruppen (Zeilenbudget)") {
		t.Errorf("the cut section does not say it was cut:\n%s", got.Text)
	}
	if got.Coverage.RenderedRows == 0 {
		t.Error("no topic line survived the meta section — the half rule did not hold")
	}
	// The section must stay at or below half the space the topic lines would
	// otherwise have; measuring it against the whole budget is the loose form
	// that would pass even without the rule.
	sec := got.Text[strings.Index(got.Text, "\n## Themen-Gruppen"):strings.Index(got.Text, "\n## Themen\n")]
	if len(sec)*2 > in.BudgetBytes {
		t.Errorf("meta section is %d B of a %d B budget — more than its half share", len(sec), in.BudgetBytes)
	}
}

// The bucket invariants of §4.5 step 5 are not weakened by the new section:
// clusters still fall into exactly one of rendered/small/cut.
func TestSuperSectionKeepsBucketInvariants(t *testing.T) {
	in := goldenInput(t)
	in.SuperRows = superRows(30)
	in.Freshness.SuperKnown, in.Freshness.SuperN = true, 30
	in.Freshness.SuperResolution = 0.3
	in.BudgetBytes = 3072

	got, err := rootmap.Render(in)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	c := got.Coverage
	if c.RenderedRows+c.SmallClusterN+c.CappedClusterN != c.ClusterTotal {
		t.Errorf("cluster buckets %d+%d+%d != %d", c.RenderedRows, c.SmallClusterN, c.CappedClusterN, c.ClusterTotal)
	}
	if c.RenderedBlocks+c.SmallClusterSize+c.CappedBlocks != c.ClusteredBlocks {
		t.Errorf("block buckets %d+%d+%d != %d", c.RenderedBlocks, c.SmallClusterSize, c.CappedBlocks, c.ClusteredBlocks)
	}
}

// Determinism: identical input, identical bytes — including the section. The map
// is compared byte for byte against the stored one before every write, so a
// wobbling section would turn every cycle into a write plus an embedding
// invalidation.
func TestSuperSectionDeterministic(t *testing.T) {
	in := goldenInput(t)
	in.SuperRows = superRows(12)
	in.Freshness.SuperKnown, in.Freshness.SuperN = true, 12
	in.Freshness.SuperResolution = 0.6

	first, err := rootmap.Render(in)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for i := range 49 {
		got, err := rootmap.Render(in)
		if err != nil {
			t.Fatalf("render %d: %v", i+2, err)
		}
		if got.Text != first.Text {
			t.Fatalf("run %d differs", i+2)
		}
	}
}

// A group whose lead topic lost its label falls back to the lead's title, and
// then to an explicit placeholder — the line always has a name, exactly like a
// topic line (D0→D2).
func TestSuperRowNameFallback(t *testing.T) {
	in := goldenInput(t)
	in.SuperRows = []rootmap.SuperRow{
		{ID: "019fd2a0-0000-7000-8000-000000000001", Title: "Nur-Titel-Gruppe", Size: 40, TopicN: 3},
		{ID: "019fd2a0-0000-7000-8000-000000000002", Size: 20, TopicN: 2},
	}
	in.Freshness.SuperKnown, in.Freshness.SuperN = true, 2
	in.Freshness.SuperResolution = 0.5

	got, err := rootmap.Render(in)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(got.Text, "Nur-Titel-Gruppe 40 Blöcke") {
		t.Errorf("label fallback to the lead title missing:\n%s", got.Text)
	}
	if !strings.Contains(got.Text, "(ohne Titel) 20 Blöcke") {
		t.Errorf("nameless group has no placeholder:\n%s", got.Text)
	}
}
