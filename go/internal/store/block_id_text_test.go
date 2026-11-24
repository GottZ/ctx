package store

import (
	"context"
	"errors"
	"testing"
)

// TestResolveBlockIDAnswersOddIDsWithoutAQuery (issue #547): an id or prefix
// that cannot name a block is answered "not found" before any statement —
// the pool is nil, a query would panic — and a prefix too short keeps its
// wording behind the sentinel.
func TestResolveBlockIDAnswersOddIDsWithoutAQuery(t *testing.T) {
	scopes := []string{"private"}
	for _, id := range []string{
		"%%%%%%%%", "________", "0%%%%%%%", "abcdef0_", "\x00abcdefgh", "abcd\x00efgh",
		"\xff\xfeabcdefgh", "         ", "ABCDEF01", "01234567-89ab-cdef-0123-45678\x00abcdef",
		"01234567-89ab-cdef-0123-4567\xff\xfeabcd",
	} {
		got, matches, err := ResolveBlockID(context.Background(), nil, id, scopes, nil)
		if got != "" || matches != nil || err != nil {
			t.Errorf("ResolveBlockID(%q) = %q, %v, %v; want not found", id, got, matches, err)
		}
	}
	_, _, err := ResolveBlockID(context.Background(), nil, "abc", scopes, nil)
	if !errors.Is(err, ErrIDPrefixTooShort) || err.Error() != "store: id prefix must be at least 8 characters (got 3)" {
		t.Errorf("too short: %v, want ErrIDPrefixTooShort with the wording every surface answers", err)
	}
	full := "01234567-89ab-cdef-0123-456789abcdef"
	if got, _, err := ResolveBlockID(context.Background(), nil, full, scopes, nil); got != full || err != nil {
		t.Errorf("a full id: %q, %v; want it back unchanged", got, err)
	}
}
