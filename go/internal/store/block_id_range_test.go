package store

import (
	"strings"
	"testing"
)

// TestIDPrefixRange: a prefix of an id's canonical text maps to the range of
// the ids it starts, completed to the id's shape with '0' and 'f'; a prefix
// no canonical text starts with has no range (issue #536).
func TestIDPrefixRange(t *testing.T) {
	const id = "01a2105f-d5fa-7893-834f-ee2a496b8fda"
	for n := 0; n < len(id); n++ {
		lo, hi, ok := idPrefixRange(id[:n])
		if !ok {
			t.Fatalf("prefix %q: no range", id[:n])
		}
		if !strings.HasPrefix(lo, id[:n]) || !strings.HasPrefix(hi, id[:n]) || len(lo) != len(id) || len(hi) != len(id) {
			t.Fatalf("prefix %q: range %q..%q", id[:n], lo, hi)
		}
		if lo > id || id > hi {
			t.Errorf("prefix %q: %s not in %s..%s", id[:n], id, lo, hi)
		}
		for i := n; i < len(id); i++ {
			want := byte('0')
			if idTextLow[i] == '-' {
				want = '-'
			}
			if lo[i] != want || (hi[i] != 'f' && idTextLow[i] != '-') {
				t.Fatalf("prefix %q: %q..%q at %d", id[:n], lo, hi, i)
			}
		}
	}
	for _, c := range []struct{ in, lo, hi string }{
		{"01a2105f", "01a2105f-0000-0000-0000-000000000000", "01a2105f-ffff-ffff-ffff-ffffffffffff"},
		{"01a2105f-", "01a2105f-0000-0000-0000-000000000000", "01a2105f-ffff-ffff-ffff-ffffffffffff"},
		{"01a2105f-d5fa-7", "01a2105f-d5fa-7000-0000-000000000000", "01a2105f-d5fa-7fff-ffff-ffffffffffff"},
		{"01a2105f-d5fa-7893-834f-ee2a496b8fd", "01a2105f-d5fa-7893-834f-ee2a496b8fd0", "01a2105f-d5fa-7893-834f-ee2a496b8fdf"},
	} {
		lo, hi, ok := idPrefixRange(c.in)
		if !ok || lo != c.lo || hi != c.hi {
			t.Errorf("idPrefixRange(%q) = %q, %q, %v; want %q, %q", c.in, lo, hi, ok, c.lo, c.hi)
		}
	}
	for _, in := range []string{
		"01a2105fd5fa",                          // the first hyphen left out
		"01a2105-f",                             // a hyphen where the text has a digit
		"01a2105f-d5fa7893",                     // the second hyphen left out
		"01a2105f-d5fa-7893-834f-ee2a496b8fda0", // longer than an id
		"--------",
	} {
		if lo, hi, ok := idPrefixRange(in); ok {
			t.Errorf("idPrefixRange(%q) = %q, %q: a range for a text no id starts with", in, lo, hi)
		}
	}
}
