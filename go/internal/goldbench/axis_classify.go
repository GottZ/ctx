package goldbench

import (
	"encoding/json"

	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/promptguard"
	"github.com/GottZ/ctx/internal/topiclabel"
)

// Achse sensitivity — mockt die sensitivity-audit-Pipeline
// (internal/llm/classify.go, ClassifyBlockBool). ZWEI Calls pro Fall:
// Frage credentials, Frage personal — exakt wie der Scheduler-Audit.

type sensitivityGold struct {
	Credentials bool `json:"credentials"`
	Personal    bool `json:"personal"`
}

func axisSensitivity() axisDef {
	return axisDef{
		name: "sensitivity",
		build: func(c *Case) ([]ChatRequest, error) {
			var in titleContentInput
			if err := decodeInto(c.Input, &in, "sensitivity input", c.ID); err != nil {
				return nil, err
			}
			// Sampling wie llm.ClassifyOptions (classify.go:52-61): 0 / 32,
			// Format json wie ClassifyBlockBool (classify.go:181).
			opts := SamplingOpts{Temperature: 0, MaxTokens: 32, JSONFormat: true}
			system := llm.BenchClassifySystemPrompt()
			return []ChatRequest{
				{System: system, User: llm.BenchBuildClassifyUser(llm.QuestionCredentials, in.Title, in.Content), Opts: opts},
				{System: system, User: llm.BenchBuildClassifyUser(llm.QuestionPersonal, in.Title, in.Content), Opts: opts},
			}, nil
		},
		score: scoreSensitivity,
	}
}

// scoreSensitivity: Accuracy je Frage; kritische Metrik ist die FN-Rate auf
// positiven Fällen (gold true, Antwort false) — ein übersehenes Credential
// ist der teure Fehler. Primär: Gesamt-Accuracy über beide Fragen.
// parse_rate verlangt BEIDE Antworten parsebar (ParseClassifyAnswer,
// classify.go:78 — kein Verdict, nie ein Default).
func scoreSensitivity(runs []caseRun) (AxisResult, []CaseScore) {
	var credCorrect, credTotal, persCorrect, persTotal int
	var credFN, credPos, persFN, persPos int
	parsed := 0
	perCase := make([]CaseScore, 0, len(runs))

	for _, r := range runs {
		var gold sensitivityGold
		_ = json.Unmarshal(r.c.Gold, &gold)

		cs := CaseScore{ID: r.c.ID}
		outputs := r.outputs
		for len(outputs) < 2 {
			outputs = append(outputs, "")
		}
		credAns, credErr := llm.ParseClassifyAnswer(outputs[0])
		persAns, persErr := llm.ParseClassifyAnswer(outputs[1])
		if credErr == nil && persErr == nil {
			parsed++
			cs.Parsed = true
		}

		caseHits := 0
		if credErr == nil {
			credTotal++
			if credAns == gold.Credentials {
				credCorrect++
				caseHits++
			}
			if gold.Credentials && !credAns {
				credFN++
			}
		}
		if persErr == nil {
			persTotal++
			if persAns == gold.Personal {
				persCorrect++
				caseHits++
			}
			if gold.Personal && !persAns {
				persFN++
			}
		}
		if gold.Credentials {
			credPos++
		}
		if gold.Personal {
			persPos++
		}
		cs.Score = float64(caseHits) / 2
		perCase = append(perCase, cs)
	}

	// Accuracy über ALLE Fragen (nicht nur parsebare): eine unparsebare
	// Antwort ist kein Verdict und damit kein Treffer.
	total := 2 * len(runs)
	return AxisResult{
		N:             len(runs),
		ParseRate:     ratioOrZero(parsed, len(runs)),
		PrimaryMetric: "accuracy_both_questions",
		PrimaryScore:  ratioOrZero(credCorrect+persCorrect, total),
		Secondary: map[string]float64{
			"accuracy_credentials": ratioOrZero(credCorrect, len(runs)),
			"accuracy_personal":    ratioOrZero(persCorrect, len(runs)),
			"fn_rate_credentials":  ratioOrZero(credFN, credPos),
			"fn_rate_personal":     ratioOrZero(persFN, persPos),
			"answered_credentials": ratioOrZero(credTotal, len(runs)),
			"answered_personal":    ratioOrZero(persTotal, len(runs)),
		},
	}, perCase
}

// Achse cluster-label — mockt die topiclabel-Pipeline
// (internal/topiclabel/prompt.go + guard.go), Sprache "de".

type clusterLabelInput struct {
	Titles        []string `json:"titles"`
	TopTags       []string `json:"top_tags"`
	TopCategories []string `json:"top_categories"`
}

type clusterLabelGold struct {
	Label string `json:"label"`
}

func axisClusterLabel() axisDef {
	return axisDef{
		name: "cluster-label",
		build: func(c *Case) ([]ChatRequest, error) {
			var in clusterLabelInput
			if err := decodeInto(c.Input, &in, "cluster-label input", c.ID); err != nil {
				return nil, err
			}
			// Eine Nonce pro Prompt-Bau, System + User zusammen — wie der
			// topiclabel-Run-Loop (topiclabel.go:631-634 nutzt beide Builder
			// mit derselben Nonce).
			nonce := promptguard.NewNonce()
			return []ChatRequest{{
				System: topiclabel.BenchSystemPromptFor("de", nonce),
				User:   topiclabel.BenchBuildUser(nonce, in.Titles, in.TopTags, in.TopCategories),
				// Sampling wie topiclabel.go:634: 0.2 / 128.
				Opts: SamplingOpts{Temperature: 0.2, MaxTokens: 128},
			}}, nil
		},
		score: scoreClusterLabel,
	}
}

// scoreClusterLabel: primär Constraint-Pass (parseLabel-Contract: parsebar,
// nicht leer, ≤120 Runen — guard.go:54), informativ Token-Overlap-F1 gegen
// gold.label.
//
// ABWEICHUNG (cluster-label): Der Echo-/Scan-Screen (screenLabel,
// topiclabel/guard.go:89) braucht die sensitiven Kern-Titel eines
// Live-Clusters und bleibt außen vor — gemessen wird nur der parseLabel-Contract.
func scoreClusterLabel(runs []caseRun) (AxisResult, []CaseScore) {
	pass := 0
	var f1s []float64
	perCase := make([]CaseScore, 0, len(runs))
	for _, r := range runs {
		var gold clusterLabelGold
		_ = json.Unmarshal(r.c.Gold, &gold)

		cs := CaseScore{ID: r.c.ID}
		label, ok := topiclabel.BenchParseLabel(firstOutput(r))
		if ok {
			pass++
			cs.Parsed = true
			cs.Score = 1
			f1s = append(f1s, tokenF1(label, gold.Label))
		}
		perCase = append(perCase, cs)
	}
	return AxisResult{
		N:             len(runs),
		ParseRate:     ratioOrZero(pass, len(runs)),
		PrimaryMetric: "constraint_pass_rate",
		PrimaryScore:  ratioOrZero(pass, len(runs)),
		Secondary: map[string]float64{
			"token_f1_vs_gold": meanOrZero(f1s),
		},
	}, perCase
}
