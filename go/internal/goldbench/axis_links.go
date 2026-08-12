package goldbench

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/dream"
)

// blockJSON ist die Block-Repräsentation der Gold-Cases (links, recurrence).
type blockJSON struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Category string `json:"category"`
	Updated  string `json:"updated"`
	Content  string `json:"content"`
}

// toBlockInfo baut die dream.BlockInfo eines Gold-Blocks. UpdatedAt trägt
// das updated-Datum; Sensitivity/Scope bleiben leer (der Bench routet nicht).
func (b blockJSON) toBlockInfo() (dream.BlockInfo, error) {
	updated, err := time.Parse("2006-01-02", b.Updated)
	if err != nil {
		return dream.BlockInfo{}, fmt.Errorf("updated %q: %w", b.Updated, err)
	}
	return dream.BlockInfo{
		ID:        b.ID,
		Title:     b.Title,
		Category:  b.Category,
		Content:   b.Content,
		UpdatedAt: updated,
	}, nil
}

// Achse links — mockt die dream-eval-Pipeline
// (internal/dream/evaluate.go, EvaluateRelationships).

type linksInput struct {
	Source     blockJSON   `json:"source"`
	Candidates []blockJSON `json:"candidates"`
}

type linksGold struct {
	Links []struct {
		TargetID string `json:"target_id"`
		Type     string `json:"type"`
	} `json:"links"`
}

func axisLinks() axisDef {
	return axisDef{
		name: "links",
		build: func(c *Case) ([]ChatRequest, error) {
			var in linksInput
			if err := decodeInto(c.Input, &in, "links input", c.ID); err != nil {
				return nil, err
			}
			source, err := in.Source.toBlockInfo()
			if err != nil {
				return nil, fmt.Errorf("goldbench: links %s: source: %w", c.ID, err)
			}
			candidates := make([]dream.BlockInfo, 0, len(in.Candidates))
			for i, cand := range in.Candidates {
				bi, err := cand.toBlockInfo()
				if err != nil {
					return nil, fmt.Errorf("goldbench: links %s: candidate %d: %w", c.ID, i, err)
				}
				candidates = append(candidates, bi)
			}
			system, user := dream.BenchBuildEvalPrompt(source, candidates)
			return []ChatRequest{{
				System: system,
				User:   user,
				// Sampling wie dream.DreamOptions (evaluate.go:66-73):
				// 0.7 / top_p 0.8 / NumPredict 400. top_k 20 entfällt
				// (OpenAI-API, siehe SamplingOpts-ABWEICHUNG).
				Opts: SamplingOpts{Temperature: 0.7, TopP: 0.8, MaxTokens: 400},
			}}, nil
		},
		score: scoreLinks,
	}
}

// scoreLinks setzt den Achsen-Vertrag um:
//   - gold leer  → Output leer = 1, sonst 0;
//   - gold gesetzt → 1.0 bei exaktem (target_id, type)-Treffer irgendeines
//     Gold-Links, 0.5 bei richtigem target mit falschem type, sonst 0.
//
// Zusätzlich die Typ-Konfusionsmatrix gold-Typ → prädizierter Typ; „none"
// steht für „kein Link", „other-target" für einen Link auf ein falsches Ziel.
func scoreLinks(runs []caseRun) (AxisResult, []CaseScore) {
	var scores []float64
	parsed := 0
	confusion := map[string]map[string]int{}
	bump := func(gold, pred string) {
		if confusion[gold] == nil {
			confusion[gold] = map[string]int{}
		}
		confusion[gold][pred]++
	}
	perCase := make([]CaseScore, 0, len(runs))

	for _, r := range runs {
		var gold linksGold
		_ = json.Unmarshal(r.c.Gold, &gold)

		cs := CaseScore{ID: r.c.ID}
		out := strings.TrimSpace(firstOutput(r))
		// ABWEICHUNG (links): parseLinks wertet "" als validen Leer-Verdict
		// (parse.go:44) — für den Bench ist ein leerer Output aber „kein
		// Response" (Dry-Run, Transport-Fehler), kein Verdict.
		links, _, err := dream.BenchParseLinks(out)
		if err != nil || out == "" {
			scores = append(scores, 0)
			perCase = append(perCase, cs)
			continue
		}
		parsed++
		cs.Parsed = true
		cs.Score = scoreLinksCase(gold, links, bump)
		scores = append(scores, cs.Score)
		perCase = append(perCase, cs)
	}
	return AxisResult{
		N:             len(runs),
		ParseRate:     ratioOrZero(parsed, len(runs)),
		PrimaryMetric: "link_score",
		PrimaryScore:  meanOrZero(scores),
		Confusion:     confusion,
	}, perCase
}

// scoreLinksCase bewertet einen geparsten Fall und pflegt die Konfusionsmatrix.
func scoreLinksCase(gold linksGold, links []dream.Link, bump func(gold, pred string)) float64 {
	if len(gold.Links) == 0 {
		if len(links) == 0 {
			bump("none", "none")
			return 1
		}
		bump("none", links[0].Relationship)
		return 0
	}

	best := 0.0
	for _, g := range gold.Links {
		predType := "none"
		if len(links) > 0 {
			predType = "other-target"
		}
		caseScore := 0.0
		for _, l := range links {
			if l.TargetID != g.TargetID {
				continue
			}
			predType = l.Relationship
			if l.Relationship == g.Type {
				caseScore = 1.0
			} else if caseScore < 0.5 {
				caseScore = 0.5
			}
		}
		bump(g.Type, predType)
		if caseScore > best {
			best = caseScore
		}
	}
	return best
}

// Achse recurrence — mockt die dream-recurrence-Pipeline
// (internal/dream/recurrence.go, confirmRecurrence).

type recurrenceInput struct {
	BlockA blockJSON `json:"block_a"`
	BlockB blockJSON `json:"block_b"`
}

type recurrenceGold struct {
	Verdict string `json:"verdict"`
}

func axisRecurrence() axisDef {
	return axisDef{
		name: "recurrence",
		build: func(c *Case) ([]ChatRequest, error) {
			var in recurrenceInput
			if err := decodeInto(c.Input, &in, "recurrence input", c.ID); err != nil {
				return nil, err
			}
			source, err := in.BlockA.toBlockInfo()
			if err != nil {
				return nil, fmt.Errorf("goldbench: recurrence %s: block_a: %w", c.ID, err)
			}
			// title_sim kommt in ctx aus PG (pg_trgm, recurrence.go:144) und
			// wird hier strukturgleich nachgerechnet — siehe trgmSimilarity.
			sim := trgmSimilarity(in.BlockA.Title, in.BlockB.Title)
			system, user := dream.BenchBuildRecurrencePrompt(source,
				in.BlockB.ID, in.BlockB.Title, in.BlockB.Content, sim)
			return []ChatRequest{{
				System: system,
				User:   user,
				// Sampling wie der Dream-Loop, der DetectRecurrence dieselben
				// DreamOptions reicht wie EvaluateRelationships
				// (evaluate.go:66-73): 0.7 / top_p 0.8 / NumPredict 400.
				Opts: SamplingOpts{Temperature: 0.7, TopP: 0.8, MaxTokens: 400},
			}}, nil
		},
		score: scoreRecurrence,
	}
}

// scoreRecurrence: 3-Klassen-Accuracy (recurrent/supersedes/none) plus
// FP-Rate auf none-Fällen (gold none, Prädiktion recurrent/supersedes).
// Ein Verdict außerhalb der drei Klassen zählt als parsebar, aber falsch —
// exakt wie DetectRecurrence unbekannte Verdicts verwirft (recurrence.go:118).
func scoreRecurrence(runs []caseRun) (AxisResult, []CaseScore) {
	parsed, correct := 0, 0
	noneTotal, noneFP := 0, 0
	perCase := make([]CaseScore, 0, len(runs))
	for _, r := range runs {
		var gold recurrenceGold
		_ = json.Unmarshal(r.c.Gold, &gold)
		if gold.Verdict == "none" {
			noneTotal++
		}

		cs := CaseScore{ID: r.c.ID}
		verdict, _, _, err := dream.BenchParseRecurrenceResponse(firstOutput(r))
		if err != nil {
			perCase = append(perCase, cs)
			continue
		}
		parsed++
		cs.Parsed = true
		if verdict == gold.Verdict {
			correct++
			cs.Score = 1
		}
		if gold.Verdict == "none" && (verdict == "recurrent" || verdict == "supersedes") {
			noneFP++
		}
		perCase = append(perCase, cs)
	}
	return AxisResult{
		N:             len(runs),
		ParseRate:     ratioOrZero(parsed, len(runs)),
		PrimaryMetric: "accuracy_3class",
		PrimaryScore:  ratioOrZero(correct, len(runs)),
		Secondary: map[string]float64{
			"none_fp_rate": ratioOrZero(noneFP, noneTotal),
		},
	}, perCase
}
