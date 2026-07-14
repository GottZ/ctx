// Graph-structural GA6/W1 unit gates (design/01 §4.6, no DB needed):
// W1-G3 pins the class-filter input invariant, plus the displayClasses bind
// semantic that W1-G1 (integration) proves against live SQL.
package store_test

import (
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

// TestNormalizeClassFilters_W1G3: a class filter is all-or-nothing across
// both vocabularies — filtering exactly one side turns the other side into
// non-nil-EMPTY ("none"), never leaves it nil ("all"). Red probe: removing
// either normalization arm in a test double turns its case red.
func TestNormalizeClassFilters_W1G3(t *testing.T) {
	cases := []struct {
		name                 string
		link, structIn       []string
		wantLink, wantStruct []string
	}{
		{"both nil stay nil (no filter at all)", nil, nil, nil, nil},
		{"dream filtered → struct becomes none", []string{"topical"}, nil, []string{"topical"}, []string{}},
		{"struct filtered → dream becomes none", nil, []string{"references"}, []string{}, []string{"references"}},
		{"both set stay untouched", []string{"topical"}, []string{"references"}, []string{"topical"}, []string{"references"}},
		{"non-nil-empty dream counts as filtered", []string{}, nil, []string{}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := store.EgoParams{LinkClasses: tc.link, StructClasses: tc.structIn}
			store.NormalizeClassFiltersForTest(&p)
			assertClassSlice(t, "LinkClasses", p.LinkClasses, tc.wantLink)
			assertClassSlice(t, "StructClasses", p.StructClasses, tc.wantStruct)
		})
	}
}

// assertClassSlice distinguishes nil from non-nil-empty — the whole point of
// the §4.6 semantic (nil = all, non-nil-empty = none).
func assertClassSlice(t *testing.T, field string, got, want []string) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Fatalf("%s nil-ness = %v, want %v (nil means ALL, non-nil-empty means NONE)", field, got == nil, want == nil)
	}
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", field, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", field, got, want)
		}
	}
}

// TestDisplayClasses_BindSemantic: nil passes through as nil (SQL NULL, "no
// filter"); non-nil — including EMPTY — passes as the array (empty matches
// nothing). The nilIfEmpty collapse is the red-proven W1-G1 bug path.
func TestDisplayClasses_BindSemantic(t *testing.T) {
	if got := store.DisplayClassesForTest(nil); got != nil {
		t.Errorf("displayClasses(nil) = %v, want nil (SQL NULL)", got)
	}
	if got := store.DisplayClassesForTest([]string{}); got == nil {
		t.Error("displayClasses(non-nil-empty) = nil — empty filter collapsed to \"all five\" (W1-G1 bug path)")
	}
	if got := store.DisplayClassesForTest([]string{"topical"}); len(got) != 1 || got[0] != "topical" {
		t.Errorf("displayClasses([topical]) = %v", got)
	}
}
