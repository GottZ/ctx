// Issue #226: the low-confidence cut is the settings key
// query.low_confidence_max_sources, not a compile-time constant. Measured on
// the wire like the H12 probes next door — the assertions read the body the
// backend received, not a value the code computed on the way.
package llm

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// lowConfSources builds n short sources whose best score sits between the
// two thresholds of lowConfSettings, so Synthesize classifies the set
// low_confidence. Short content on purpose: the H12 budget must never be the
// reason a source is missing from the prompt here.
func lowConfSources(n int) []Source {
	out := make([]Source, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, Source{
			ID: "id" + itoa(i), Title: "t", Category: "c",
			Score:       0.005 - 0.0001*float64(i),
			Content:     "MARKER" + itoa(i) + " body",
			Sensitivity: backends.SensPublic,
		})
	}
	return out
}

func lowConfSettings(cut int) SynthesisSettings {
	return SynthesisSettings{
		ScoreThreshold: 0.001, ConfidentThreshold: 0.008,
		PromptVersion:           PromptVersionV52,
		LowConfidenceMaxSources: cut,
	}
}

// TestSynthesizeLowConfidenceCutFollowsSetting: the number of sources a
// low_confidence prompt carries is the setting, and the zero value of a
// literally built SynthesisSettings keeps today's cut of two.
//
// Falsifying implementations: the constant left in place (case "four" carries
// two markers), and a zero value read as "no cut" (case "zero value" carries
// five) — the second would change every bench harness and test that builds the
// struct by hand.
func TestSynthesizeLowConfidenceCutFollowsSetting(t *testing.T) {
	cases := []struct {
		name string
		cut  int
		want int // markers in the wire body
	}{
		{"zero value keeps the cut of two", 0, LowConfidenceMaxSources},
		{"one", 1, 1},
		{"four", 4, 4},
		{"at or above the source count is no cut", MaxPromptSources, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newBudgetRecorder(t)
			res, err := runBudgetSynthesize(t, budgetBackend(rec.srv.URL, 32768), lowConfSettings(tc.cut), lowConfSources(5))
			if err != nil {
				t.Fatalf("Synthesize: %v", err)
			}
			if res.Confidence != ConfidenceLow {
				t.Fatalf("confidence = %q, want %q — the fixture must reach the low-confidence cut", res.Confidence, ConfidenceLow)
			}
			body := rec.lastBody()
			got := 0
			for i := 1; i <= 5; i++ {
				if strings.Contains(body, "MARKER"+itoa(i)+" ") {
					got++
					if i > tc.want {
						t.Errorf("source %d reached the prompt past a cut of %d", i, tc.want)
					}
				}
			}
			if got != tc.want {
				t.Errorf("prompt carries %d sources, want %d", got, tc.want)
			}
			// The response lists every source either way; the ones the cut
			// kept out carry no citation index (V-W1).
			if len(res.Sources) != 5 {
				t.Fatalf("response lists %d sources, want all 5", len(res.Sources))
			}
			for i, s := range res.Sources {
				if inPrompt := i < tc.want; inPrompt != (s.CitationIndex != nil) {
					t.Errorf("source %d: citation_index set = %v, want %v", i+1, s.CitationIndex != nil, inPrompt)
				}
			}
		})
	}
}
