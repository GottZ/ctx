//go:build integration

// Issue #507, round 1 (P1): the title floor compares the question in the form
// people type it — a question mark or a full stop at its end, typographic
// quotes, another Unicode normal form — while punctuation inside it stays
// distinctive. Corpus and request path as in query_title_floor_integration_test.go.

package handler

import (
	"strings"
	"testing"
)

func TestQueryTitleFloorFoldedQuestion(t *testing.T) {
	e := titleFloorSetup(t)
	for _, q := range []string{
		"Orbit nightly report 2026-08-31?",
		"orbit nightly report 2026-08-31.",
		"Orbit nightly report 2026-08-31 !",
		"Orbit nightly report 2026-08-31:",
	} {
		if p := position(e.ask(t, q, 10, false).ids(), e.target); p == 0 {
			t.Errorf("%q: the named block is not among the 10 sources", q)
		}
	}

	// Punctuation inside the question is not folded: a date written with
	// dots names no title, and the floor moves nothing.
	const inner = "Orbit nightly report 2026.08.31"
	ten := e.ask(t, inner, 10, false).ids()
	twenty := e.ask(t, inner, 20, false).ids()
	if strings.Join(ten, ",") != strings.Join(twenty[:10], ",") {
		t.Errorf("%q: the ten sources are not the first ten of twenty:\n ten %v\n 20  %v", inner, ten, twenty[:10])
	}
	if p := position(ten, e.target); p != 0 {
		t.Errorf("%q: the block of a title that differs inside was placed at %d", inner, p)
	}
}
