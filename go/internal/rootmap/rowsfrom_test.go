package rootmap

import (
	"reflect"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

// TestRowsFromCarriesTopicIdentity ist das Naht-Gate zwischen W7 (Store liefert
// TopicID/Label/LabelSource) und W-D (rowsFrom projiziert auf Renderer-Rows).
// Beide Wellen bauten parallel; die W-D-Fassung ließ StableID/Label leer, und
// die Karte druckte „Themen ohne stabile ID" plus D2-Titel, obwohl die
// Identität längst auf dem Read-Pfad lag.
//
// ROT-BELEG (Vor-Fix-Stand): StableID = "" und Label = "" trotz gefüllter
// Store-Node — genau der gemeldete Seam-Gap.
func TestRowsFromCarriesTopicIdentity(t *testing.T) {
	nodes := []store.OverviewNode{{
		ClusterID:   "019c0000-0000-7000-9000-000000000001",
		TopicID:     "8f0d2a4e-3c1b-4e5f-9a70-000000000aaa",
		Label:       "Retrieval-Architektur",
		LabelSource: "llm",
		Size:        133,
		ReprID:      "019c0000-0000-7000-9000-0000000000ff",
		ReprTitle:   "repr",
		ScopeMix:    []string{"private"},
	}, {
		// Legacy-/Übergangszeile: keine Identität ⇒ Felder bleiben leer (D2/D3).
		ClusterID: "019c0000-0000-7000-9000-000000000002",
		Size:      7,
		ReprID:    "019c0000-0000-7000-9000-0000000000fe",
		ReprTitle: "repr2",
		ScopeMix:  []string{"private"},
	}}

	rows := rowsFrom(nodes)
	if rows[0].StableID != nodes[0].TopicID {
		t.Errorf("StableID = %q, want the store TopicID %q — the W7 identity is dropped at the seam", rows[0].StableID, nodes[0].TopicID)
	}
	if rows[0].Label != "Retrieval-Architektur" {
		t.Errorf("Label = %q, want the axis-01 label — the map renders D2 although D0 is available", rows[0].Label)
	}
	// Provenienz-Vokabular: DB none/fallback/llm/manual → Renderer ""/heuristic/llm/manual.
	if rows[0].LabelSource != "llm" {
		t.Errorf("LabelSource = %q, want %q", rows[0].LabelSource, "llm")
	}
	if rows[1].StableID != "" || rows[1].Label != "" || rows[1].LabelSource != "" {
		t.Errorf("identity-free node must stay empty (D2/D3): %+v", rows[1])
	}
	for dbSrc, want := range map[string]string{"none": "", "fallback": "heuristic", "llm": "llm", "manual": "manual"} {
		got := rowsFrom([]store.OverviewNode{{TopicID: "t", Label: "x", LabelSource: dbSrc, Size: 1}})[0].LabelSource
		if got != want {
			t.Errorf("LabelSource mapping %q = %q, want %q", dbSrc, got, want)
		}
	}
	// Die bestehende Projektion bleibt unangetastet.
	if !reflect.DeepEqual(rows[0].TopCats, nodes[0].TopCategories) || rows[0].Size != 133 {
		t.Errorf("base projection drifted: %+v", rows[0])
	}
}
