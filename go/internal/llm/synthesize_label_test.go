package llm

import "testing"

// Issue #528, point 6: one label for one answer on both paths, read on the
// fusion's scale — rrf_score_original where a stage behind the fusion kept it.
func TestClassifySourcesReadsTheFusionScale(t *testing.T) {
	s := SynthesisSettings{ScoreThreshold: 0.001, ConfidentThreshold: 0.008}
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name    string
		sources []Source
		want    string
	}{
		{"gravity lifted the top across the threshold",
			[]Source{{Score: 0.0098, RRFScoreOriginal: f(0.0075)}, {Score: 0.006}}, ConfidenceLow},
		{"the fusion score alone carries it",
			[]Source{{Score: 0.0050, RRFScoreOriginal: f(0.0090)}, {Score: 0.004}}, ConfidenceConfident},
		{"no original: the score",
			[]Source{{Score: 0.0085}, {Score: 0.002}}, ConfidenceConfident},
		{"a source at the score threshold is read",
			[]Source{{Score: 0.001, RRFScoreOriginal: f(0.0090)}, {Score: 0.0009}}, ConfidenceConfident},
		{"a source below the score threshold is not read",
			[]Source{{Score: 0.0005, RRFScoreOriginal: f(0.0090)}, {Score: 0.003}}, ConfidenceLow},
		{"nothing at or above the score threshold",
			[]Source{{Score: 0.0005, RRFScoreOriginal: f(0.0090)}}, ConfidenceNoRelevant},
		{"no sources", nil, ConfidenceNoRelevant},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifySources(c.sources, s); got != c.want {
				t.Errorf("label %q, want %q", got, c.want)
			}
		})
	}
}

func TestLabelScore(t *testing.T) {
	o := 0.004
	if got := LabelScore(Source{Score: 0.009, RRFScoreOriginal: &o}); got != o {
		t.Errorf("with an original: %v, want %v", got, o)
	}
	if got := LabelScore(Source{Score: 0.009}); got != 0.009 {
		t.Errorf("without: %v, want 0.009", got)
	}
}
