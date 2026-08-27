// Wave W01-6, the W01-4 review's finding #1: both by-source renderers iterated a
// hardcoded four-name list and dropped every other class on the TTY path — the
// 'derived' class (migration 144) among them, which the server has been counting
// since W01-4. The list is now ONE source, and a class the list does not know is
// appended instead of swallowed.
//
//	go test ./internal/cli/ -run TestFormatBySource -count=1 -v
package cli

import "testing"

func TestFormatBySourceShowsDerived(t *testing.T) {
	// The CLI-Gate of the briefing: a derived row exists, so it must be visible.
	got := formatBySource(map[string]int{"default": 4, "manual": 1, "derived": 2})
	want := "default=4  manual=1  derived=2"
	if got != want {
		t.Errorf("formatBySource = %q, want %q — the derived class must not fall off the TTY path", got, want)
	}
}

func TestFormatBySourceNonRegression(t *testing.T) {
	// Non-regression half: a map WITHOUT a derived row renders byte-identically
	// to what the four-name list produced.
	cases := []struct {
		name string
		in   map[string]int
		want string
	}{
		{"all four, canonical order", map[string]int{
			"manual": 1, "pattern": 2, "llm-audit": 3, "default": 4},
			"default=4  llm-audit=3  pattern=2  manual=1"},
		{"a subset keeps the order", map[string]int{"pattern": 7, "default": 9},
			"default=9  pattern=7"},
		{"a zero count still renders", map[string]int{"default": 0},
			"default=0"},
		{"empty map renders empty", map[string]int{}, ""},
		{"nil map renders empty", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatBySource(tc.in); got != tc.want {
				t.Errorf("formatBySource = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatBySourceKeepsUnknownClasses(t *testing.T) {
	// The structural half of finding #1: the bug was not "derived is missing
	// from the list", it was "a class outside the list disappears without a
	// trace". A future migration must not be able to reopen it. Unknown classes
	// come after the known ones, sorted, so the output stays deterministic.
	got := formatBySource(map[string]int{
		"default": 1, "derived": 2, "zeta-future": 3, "alpha-future": 4})
	want := "default=1  derived=2  alpha-future=4  zeta-future=3"
	if got != want {
		t.Errorf("formatBySource = %q, want %q — an unknown class must be appended, never swallowed", got, want)
	}
}

func TestBySourceOrderCoversEveryConstraintClass(t *testing.T) {
	// bySourceOrder is the wire-level mirror of the sensitivity_source CHECK
	// constraint (113_baseline.sql + migration 144). If a migration adds a class
	// and nobody adds it here, the class still renders (previous test), but it
	// renders after the known ones — this pin says which set is "known".
	want := []string{"default", "llm-audit", "pattern", "manual", "derived"}
	if len(bySourceOrder) != len(want) {
		t.Fatalf("bySourceOrder = %v, want %v", bySourceOrder, want)
	}
	for i, w := range want {
		if bySourceOrder[i] != w {
			t.Errorf("bySourceOrder[%d] = %q, want %q", i, bySourceOrder[i], w)
		}
	}
}
