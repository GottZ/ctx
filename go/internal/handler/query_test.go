package handler

import (
	"testing"

	"github.com/GottZ/ctx/internal/llm"
)

func TestBuildSourceResponses(t *testing.T) {
	rs := 0.5
	orig := 0.008
	sources := []llm.Source{
		{ID: "a", Title: "A", Category: "x", Score: 0.92, AgeDays: 3, RerankScore: &rs, RRFScoreOriginal: &orig},
		{ID: "b", Title: "B", Category: "y", Score: 0.10, AgeDays: 1},
	}
	supersedes := map[string][]string{"a": {"superseder-1", "superseder-2"}}

	out := buildSourceResponses(sources, supersedes)
	if len(out) != 2 {
		t.Fatalf("got %d responses, want 2", len(out))
	}
	// Field mapping + pointers carried through.
	if out[0].ID != "a" || out[0].Title != "A" || out[0].Category != "x" || out[0].Score != 0.92 || out[0].AgeDays != 3 {
		t.Errorf("source[0] fields mismatch: %+v", out[0])
	}
	if out[0].RerankScore == nil || *out[0].RerankScore != 0.5 {
		t.Errorf("source[0] RerankScore not carried: %+v", out[0].RerankScore)
	}
	// First superseder is attached.
	if out[0].SupersededBy == nil || *out[0].SupersededBy != "superseder-1" {
		t.Errorf("source[0] SupersededBy = %v, want superseder-1", out[0].SupersededBy)
	}
	// No supersedes entry → nil.
	if out[1].SupersededBy != nil {
		t.Errorf("source[1] SupersededBy = %v, want nil", *out[1].SupersededBy)
	}
}

func TestBuildSourceResponses_NilMap(t *testing.T) {
	// A nil supersedes map must not panic and yields no SupersededBy.
	out := buildSourceResponses([]llm.Source{{ID: "a", Title: "A"}}, nil)
	if len(out) != 1 || out[0].SupersededBy != nil {
		t.Errorf("nil map: got %+v", out)
	}
}
