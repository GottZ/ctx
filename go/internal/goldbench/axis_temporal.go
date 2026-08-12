package goldbench

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/llm"
)

// Achse temporal-block — mockt die dream-temporal-Pipeline
// (internal/dream/validate_temporal.go, Phase 2).

type temporalBlockInput struct {
	Title        string `json:"title"`
	Content      string `json:"content"`
	BlockCreated string `json:"block_created"`
}

type temporalBlockGold struct {
	Dates []string `json:"dates"`
}

func axisTemporalBlock() axisDef {
	return axisDef{
		name: "temporal-block",
		build: func(c *Case) ([]ChatRequest, error) {
			var in temporalBlockInput
			if err := decodeInto(c.Input, &in, "temporal-block input", c.ID); err != nil {
				return nil, err
			}
			created, err := time.Parse("2006-01-02", in.BlockCreated)
			if err != nil {
				return nil, fmt.Errorf("goldbench: temporal-block %s: block_created: %w", c.ID, err)
			}
			bi := &dream.BlockInfo{Title: in.Title, Content: in.Content, CreatedAt: created}
			return []ChatRequest{{
				System: dream.BenchTemporalValidationPrompt(),
				User:   dream.BenchBuildTemporalReviewPrompt(bi),
				// Sampling wie validate_temporal.go:114-124 (0.1 / NumPredict 1000).
				Opts: SamplingOpts{Temperature: 0.1, MaxTokens: 1000},
			}}, nil
		},
		score: scoreTemporalBlock,
	}
}

// scoreTemporalBlock: Set-F1 der Datumswerte gegen gold.dates, micro-aggregiert.
// Parse wie das Original: strikter json.Unmarshal in TemporalReview
// (validate_temporal.go:159), Datumswerte per time.Parse validiert und
// nicht-parsebare Einträge übersprungen (validate_temporal.go:167-171).
func scoreTemporalBlock(runs []caseRun) (AxisResult, []CaseScore) {
	var tp, fp, fn, parsed int
	perCase := make([]CaseScore, 0, len(runs))
	for _, r := range runs {
		var gold temporalBlockGold
		_ = json.Unmarshal(r.c.Gold, &gold)
		goldSet := map[string]bool{}
		for _, d := range gold.Dates {
			goldSet[d] = true
		}

		var review dream.TemporalReview
		cs := CaseScore{ID: r.c.ID}
		if err := json.Unmarshal([]byte(strings.TrimSpace(firstOutput(r))), &review); err != nil {
			// Parse-Fehler: alle Gold-Daten fehlen (FN), keine Prädiktionen.
			fn += len(goldSet)
			perCase = append(perCase, cs)
			continue
		}
		parsed++
		cs.Parsed = true
		predSet := map[string]bool{}
		for _, f := range review.Dates {
			if _, err := time.Parse("2006-01-02", f.Date); err == nil {
				predSet[f.Date] = true
			}
		}
		ctp, cfp, cfn := setCounts(predSet, goldSet)
		tp, fp, fn = tp+ctp, fp+cfp, fn+cfn
		_, _, cs.Score = microF1(ctp, cfp, cfn)
		if len(goldSet) == 0 && len(predSet) == 0 {
			cs.Score = 1 // Leermengen-Korrektheit
		}
		perCase = append(perCase, cs)
	}
	prec, rec, f1 := microF1(tp, fp, fn)
	return AxisResult{
		N:             len(runs),
		ParseRate:     ratioOrZero(parsed, len(runs)),
		PrimaryMetric: "date_set_micro_f1",
		PrimaryScore:  f1,
		Secondary: map[string]float64{
			"micro_precision": prec,
			"micro_recall":    rec,
		},
	}, perCase
}

// Achse temporal-query — mockt die query-temporal-Pipeline
// (internal/llm/temporal.go, NormalizeTemporal).

type temporalQueryInput struct {
	Query string `json:"query"`
	Today string `json:"today"`
}

type temporalQueryGoldDate struct {
	Date string  `json:"date"`
	End  *string `json:"end"`
	Dir  string  `json:"dir"`
}

type temporalQueryGold struct {
	Dates []temporalQueryGoldDate `json:"dates"`
}

func axisTemporalQuery() axisDef {
	return axisDef{
		name: "temporal-query",
		build: func(c *Case) ([]ChatRequest, error) {
			var in temporalQueryInput
			if err := decodeInto(c.Input, &in, "temporal-query input", c.ID); err != nil {
				return nil, err
			}
			today, err := time.Parse("2006-01-02", in.Today)
			if err != nil {
				return nil, fmt.Errorf("goldbench: temporal-query %s: today: %w", c.ID, err)
			}
			// System-Prompt = Template mit V2-Kalender gefüllt, exakt wie
			// NormalizeTemporal (temporal.go:251-252); User = rohe Query.
			system := fmt.Sprintf(llm.BenchTemporalPromptTemplate(), llm.BenchBuildCalendar(today))
			return []ChatRequest{{
				System: system,
				User:   in.Query,
				// Sampling wie llm.TemporalOptions (temporal.go:75-84): 0.1 / 300.
				Opts: SamplingOpts{Temperature: 0.1, MaxTokens: 300},
			}}, nil
		},
		score: scoreTemporalQuery,
	}
}

// temporalTuple bildet den Vergleichsschlüssel: (date, dir), bei dir=="range"
// zusätzlich end.
func temporalTuple(date, dir string, end *string) string {
	key := date + "|" + dir
	if dir == "range" && end != nil {
		key += "|" + *end
	}
	return key
}

// scoreTemporalQuery: micro-F1 über (date,dir)-Tupel; Leermengen-Korrektheit
// (gold.dates leer UND Output leer) zählt als Treffer (TP+1).
// Parse mit dem Original-Parser parseTemporalResponse (temporal.go:282) —
// (nil, nil) ist die valide Leer-Antwort.
func scoreTemporalQuery(runs []caseRun) (AxisResult, []CaseScore) {
	var tp, fp, fn, parsed int
	perCase := make([]CaseScore, 0, len(runs))
	for _, r := range runs {
		var gold temporalQueryGold
		_ = json.Unmarshal(r.c.Gold, &gold)
		var in temporalQueryInput
		_ = json.Unmarshal(r.c.Input, &in)

		goldSet := map[string]bool{}
		for _, d := range gold.Dates {
			goldSet[temporalTuple(d.Date, d.Dir, d.End)] = true
		}

		cs := CaseScore{ID: r.c.ID}
		res, err := llm.BenchParseTemporalResponse(strings.TrimSpace(firstOutput(r)), in.Query)
		if err != nil {
			fn += len(goldSet)
			perCase = append(perCase, cs)
			continue
		}
		parsed++
		cs.Parsed = true
		predSet := map[string]bool{}
		if res != nil {
			for _, d := range res.Dates {
				predSet[temporalTuple(d.Date, d.Dir, d.End)] = true
			}
		}
		ctp, cfp, cfn := setCounts(predSet, goldSet)
		if len(goldSet) == 0 && len(predSet) == 0 {
			ctp = 1 // Leermengen-Korrektheit als Treffer werten
		}
		tp, fp, fn = tp+ctp, fp+cfp, fn+cfn
		_, _, cs.Score = microF1(ctp, cfp, cfn)
		perCase = append(perCase, cs)
	}
	prec, rec, f1 := microF1(tp, fp, fn)
	return AxisResult{
		N:             len(runs),
		ParseRate:     ratioOrZero(parsed, len(runs)),
		PrimaryMetric: "tuple_micro_f1",
		PrimaryScore:  f1,
		Secondary: map[string]float64{
			"micro_precision": prec,
			"micro_recall":    rec,
		},
	}, perCase
}

// firstOutput liefert den ersten LLM-Output eines caseRun ("" wenn keiner).
func firstOutput(r caseRun) string {
	if len(r.outputs) == 0 {
		return ""
	}
	return r.outputs[0]
}
