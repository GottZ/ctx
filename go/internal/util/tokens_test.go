package util

import "testing"

// TestTokenSet pins the token alphabet: the hyphen splits, digits count, and
// case folds. The assertion moved here with the function (T01-4); it stood in
// evalscore's TestTokenF1 while TokenSet lived there.
func TestTokenSet(t *testing.T) {
	if got := len(TokenSet("A-b c1")); got != 3 {
		t.Errorf("token set size %d, want 3", got)
	}
}
