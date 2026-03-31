package handler

import (
	"testing"

	"github.com/GottZ/ctx/internal/rrf"
)

func makeResult(id string, score float64) rrf.SearchResult {
	return rrf.SearchResult{ID: id, RRFScore: score}
}

func TestFilterSuperseded_NilMap(t *testing.T) {
	results := []rrf.SearchResult{makeResult("a", 0.1)}
	got := filterSuperseded(results, nil)
	if len(got) != 1 {
		t.Fatalf("expected 1 result, got %d", len(got))
	}
}

func TestFilterSuperseded_EmptyMap(t *testing.T) {
	results := []rrf.SearchResult{makeResult("a", 0.1)}
	got := filterSuperseded(results, map[string][]string{})
	if len(got) != 1 {
		t.Fatalf("expected 1 result, got %d", len(got))
	}
}

func TestFilterSuperseded_SupersederPresent(t *testing.T) {
	// Block "old" is superseded by "new". Both are in results. "old" should be removed.
	results := []rrf.SearchResult{
		makeResult("new", 0.02),
		makeResult("old", 0.01),
	}
	smap := map[string][]string{"old": {"new"}}
	got := filterSuperseded(results, smap)
	if len(got) != 1 {
		t.Fatalf("expected 1 result, got %d", len(got))
	}
	if got[0].ID != "new" {
		t.Errorf("expected 'new', got %q", got[0].ID)
	}
}

func TestFilterSuperseded_SupersederAbsent(t *testing.T) {
	// Block "old" is superseded by "new", but "new" is NOT in results.
	// "old" should be kept (safety rule).
	results := []rrf.SearchResult{
		makeResult("old", 0.01),
		makeResult("other", 0.005),
	}
	smap := map[string][]string{"old": {"new"}}
	got := filterSuperseded(results, smap)
	if len(got) != 2 {
		t.Fatalf("expected 2 results (safety rule), got %d", len(got))
	}
}

func TestFilterSuperseded_MultiTarget(t *testing.T) {
	// Block "old" is superseded by "new1" and "new2". Only "new1" is in results.
	results := []rrf.SearchResult{
		makeResult("new1", 0.03),
		makeResult("old", 0.02),
		makeResult("unrelated", 0.01),
	}
	smap := map[string][]string{"old": {"new1", "new2"}}
	got := filterSuperseded(results, smap)
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}
	if got[0].ID != "new1" || got[1].ID != "unrelated" {
		t.Errorf("expected [new1, unrelated], got [%s, %s]", got[0].ID, got[1].ID)
	}
}

func TestFilterSuperseded_PreservesOrder(t *testing.T) {
	results := []rrf.SearchResult{
		makeResult("a", 0.05),
		makeResult("b", 0.04),
		makeResult("old", 0.03),
		makeResult("c", 0.02),
	}
	smap := map[string][]string{"old": {"a"}}
	got := filterSuperseded(results, smap)
	if len(got) != 3 {
		t.Fatalf("expected 3 results, got %d", len(got))
	}
	for i := 0; i < len(got)-1; i++ {
		if got[i].RRFScore < got[i+1].RRFScore {
			t.Errorf("order not preserved: %f < %f", got[i].RRFScore, got[i+1].RRFScore)
		}
	}
}

func TestFilterSuperseded_AllSuperseded(t *testing.T) {
	// Both blocks supersede each other (shouldn't happen, but handle gracefully).
	results := []rrf.SearchResult{
		makeResult("a", 0.02),
		makeResult("b", 0.01),
	}
	smap := map[string][]string{"a": {"b"}, "b": {"a"}}
	got := filterSuperseded(results, smap)
	// Both are superseded by each other AND both superseder are present → both removed.
	if len(got) != 0 {
		t.Fatalf("expected 0 results (mutual supersedes), got %d", len(got))
	}
}

func TestFilterSuperseded_NoSuperseded(t *testing.T) {
	results := []rrf.SearchResult{
		makeResult("a", 0.03),
		makeResult("b", 0.02),
		makeResult("c", 0.01),
	}
	smap := map[string][]string{"x": {"y"}} // unrelated supersedes
	got := filterSuperseded(results, smap)
	if len(got) != 3 {
		t.Fatalf("expected 3 results, got %d", len(got))
	}
}
