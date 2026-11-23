//go:build integration

// Issue #507, round 1 (P2, P3): the block a question names has to reach the
// synthesis prompt, also when the answer is low_confidence and the prompt
// carries only the first query.low_confidence_max_sources sources; and the
// retrieval-only label and the synthesis label of one answer agree. A source
// carries citation_index exactly when it stood in the prompt.

package handler

import (
	"testing"

	"github.com/GottZ/ctx/internal/config"
)

const titleFloorQuestion = "Orbit nightly report 2026-08-31"

func TestQueryTitleFloorInPrompt(t *testing.T) {
	// A threshold above every score of the corpus: the answer is low_confidence.
	e := titleFloorSetupWith(t, func(c *config.Config) { c.Query.ConfidentThreshold = 0.05 })
	r := e.askSynth(t, titleFloorQuestion, 10)
	if r.Confidence != "low_confidence" {
		t.Fatalf("confidence %q, want low_confidence (the floor must not lift the label)", r.Confidence)
	}
	inPrompt := 0
	for _, s := range r.Sources {
		if s.CitationIndex != nil {
			inPrompt++
		}
		if s.ID == e.target {
			if s.CitationIndex == nil {
				t.Errorf("the named block is a source but not in the synthesis prompt")
			}
			return
		}
	}
	t.Errorf("the named block is not among the sources (%d in the prompt)", inPrompt)
}

func TestQueryTitleFloorLimitOneLabels(t *testing.T) {
	e := titleFloorSetup(t)
	ro := e.ask(t, titleFloorQuestion, 1, false)
	sy := e.askSynth(t, titleFloorQuestion, 1)
	if len(ro.Sources) != 1 || ro.Sources[0].ID != e.target {
		t.Fatalf("limit 1: sources %v, want the named block", ro.ids())
	}
	if ro.Confidence != sy.Confidence {
		t.Errorf("limit 1: retrieval-only says %q, the synthesis %q for the same single source", ro.Confidence, sy.Confidence)
	}
}
