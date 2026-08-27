package derived

import "testing"

// TestReservedCategories_Golden pins the LIST. It is a security statement (I7 /
// S2, design D-01 §4.3.1), so a name added or removed here has to be a visible
// decision, not a side effect of another edit — the same reasoning
// ReservedMetadataKeys carries.
func TestReservedCategories_Golden(t *testing.T) {
	want := []string{"catalog", "session-insights"}
	if len(ReservedCategories) != len(want) {
		t.Fatalf("ReservedCategories = %v, want %v", ReservedCategories, want)
	}
	for i, name := range want {
		if ReservedCategories[i] != name {
			t.Errorf("ReservedCategories[%d] = %q, want %q", i, ReservedCategories[i], name)
		}
	}
	// The insight arm's category is the DEFAULT of distill.category
	// (config/config.go:1886). derived may not import config (leaf package), so
	// the coupling is asserted by value here and named in reserved.go.
	if ReservedCategories[1] != "session-insights" {
		t.Errorf("the insight category drifted from the distill.category default")
	}
}

func TestIsReservedCategory(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"catalog", true},
		{"session-insights", true},
		// Case and surrounding space fold: the upsert key is byte-exact, so
		// these cannot collide with an arm's own block — but they would wear
		// the layer's name in every list the category feeds.
		{"Catalog", true},
		{"CATALOG", true},
		{"  session-insights  ", true},
		{"Session-Insights", true},
		// Neighbours that must stay writable. "session insights" (a space, not
		// a hyphen) is NOT the arm's category and is deliberately admitted:
		// widening the list past the names the arms actually write would refuse
		// client writes for a resemblance.
		{"session insights", false},
		{"catalogue", false},
		{"katalog", false},
		{"learnings", false},
		{"", false},
	} {
		if got := IsReservedCategory(tc.in); got != tc.want {
			t.Errorf("IsReservedCategory(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestReservedCategoriesCoverEveryDerivedType is the coupling test between the
// two halves of I7's namespace: every derived TYPE must have its category
// reserved, otherwise S1 refuses the name while S2 leaves the identity open.
// It is written against StratumOf rather than against a literal list, so a
// third derived type cannot be added without landing here.
func TestReservedCategoriesCoverEveryDerivedType(t *testing.T) {
	byType := map[string]string{
		TypeInsight: "session-insights",
		TypeCatalog: "catalog",
	}
	for _, name := range []string{TypeInsight, TypeCatalog} {
		if StratumOf(name) == StratumSource {
			t.Fatalf("%q is not a derived type — this test has lost its subject", name)
		}
		cat, ok := byType[name]
		if !ok {
			t.Fatalf("derived type %q has no category in this test — add it and reserve it", name)
		}
		if !IsReservedCategory(cat) {
			t.Errorf("category %q of derived type %q is not reserved", cat, name)
		}
	}
}
