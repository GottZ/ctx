package llm

import "testing"

// Issue #528, point 6: one label for one answer on both paths, read on the
// final score the synthesis has always classified — the highest Score among
// the sources at or above the score threshold. rrf_score_original (the score
// before a stage behind the fusion rescaled it) is not read.
func TestClassifySourcesReadsTheFinalScore(t *testing.T) {
	s := SynthesisSettings{ScoreThreshold: 0.001, ConfidentThreshold: 0.008}
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name    string
		sources []Source
		want    string
	}{
		{"a stage lifted the top across the threshold",
			[]Source{{Score: 0.0098, RRFScoreOriginal: f(0.0075)}, {Score: 0.006}}, ConfidenceConfident},
		{"the original alone would carry it",
			[]Source{{Score: 0.0050, RRFScoreOriginal: f(0.0090)}, {Score: 0.004}}, ConfidenceLow},
		{"exactly at the confident threshold",
			[]Source{{Score: 0.008}, {Score: 0.002}}, ConfidenceConfident},
		{"a source at the score threshold is read",
			[]Source{{Score: 0.001}, {Score: 0.0009}}, ConfidenceLow},
		{"a source below the score threshold is not read",
			[]Source{{Score: 0.0009}}, ConfidenceNoRelevant},
		{"no sources", nil, ConfidenceNoRelevant},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifySources(c.sources, s); got != c.want {
				t.Errorf("label %q, want %q", got, c.want)
			}
			// The synthesis' own classification of the same list, as it read
			// before issue #528: the maximum FilterByScore finds.
			_, max := FilterByScore(c.sources, s)
			if got, syn := ClassifySources(c.sources, s), ClassifyConfidence(max, s); got != syn {
				t.Errorf("label %q, the synthesis' classification %q", got, syn)
			}
		})
	}
}
