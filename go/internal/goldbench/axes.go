package goldbench

import (
	"encoding/json"
	"fmt"
)

// caseRun ist das Ergebnis der LLM-Calls eines Falls, Input für den Scorer.
// Im Dry-Run bleiben die outputs leere Strings (Parse schlägt fehl,
// parse_rate 0 — der Lauf validiert Daten + Prompt-Bau ohne HTTP).
type caseRun struct {
	c          *Case
	outputs    []string // ein Eintrag pro ChatRequest der Achse
	callErr    error    // erster Transport-Fehler (nil im Dry-Run)
	contextErr bool     // mind. ein Call an der Context-Grenze abgelehnt
	truncated  int      // Calls mit finish_reason "length" (Output-Budget gerissen)
	thinkStrip int      // Calls mit client-seitig entferntem <think>-Block
}

// CaseScore ist das per-Case-Ergebnis für den Verbose-Report.
type CaseScore struct {
	ID     string  `json:"id"`
	Parsed bool    `json:"parsed"`
	Score  float64 `json:"score"`
	Note   string  `json:"note,omitempty"`
}

// AxisResult ist das aggregierte Ergebnis einer Achse.
type AxisResult struct {
	N               int                       `json:"n"`
	ParseRate       float64                   `json:"parse_rate"`
	PrimaryMetric   string                    `json:"primary_metric"`
	PrimaryScore    float64                   `json:"primary_score"`
	CI95Low         float64                   `json:"ci95_low"`
	CI95High        float64                   `json:"ci95_high"`
	Secondary       map[string]float64        `json:"secondary,omitempty"`
	Confusion       map[string]map[string]int `json:"confusion,omitempty"` // nur links: gold-Typ → prädizierter Typ
	SilverShare     float64                   `json:"silver_share"`
	LabelQuality    string                    `json:"label_quality"` // "gold" | "silver" (>50 % silver-Cases)
	TransportErrors int                       `json:"transport_errors"`
	ContextErrors   int                       `json:"context_errors"`    // Fälle, vom Server an der Context-Grenze abgelehnt
	TruncatedOutputs int                      `json:"truncated_outputs"` // Fälle mit finish_reason "length" (max_tokens gerissen)
	ThinkStripped   int                       `json:"think_stripped,omitempty"` // Fälle mit entferntem <think>-Block
	Prospective     bool                      `json:"prospective,omitempty"` // Achse ohne echte ctx-Pipeline
	PerCase         []CaseScore               `json:"per_case,omitempty"`
}

// axisDef bindet eine Achse an ihren Prompt-Bau und ihren Scorer.
//
// build liefert die ChatRequests eines Falls (meist 1, sensitivity 2) —
// System-Prompt, User-Prompt und Sampling exakt wie die gemockte Pipeline.
// score konsumiert alle caseRuns der Achse und aggregiert die Metriken;
// parse_rate, silver_share und transport_errors ergänzt der Runner generisch.
type axisDef struct {
	name        string
	prospective bool
	build       func(c *Case) ([]ChatRequest, error)
	score       func(runs []caseRun) (AxisResult, []CaseScore)
}

// axisRegistry mappt Achsen-Namen auf ihre Definition.
func axisRegistry() map[string]axisDef {
	defs := []axisDef{
		axisTemporalBlock(),
		axisTemporalQuery(),
		axisKeywords(),
		axisTagging(),
		axisTitle(),
		axisLinks(),
		axisRecurrence(),
		axisSensitivity(),
		axisClusterLabel(),
		axisRerank(),
		axisSynthesis(),
		axisTranslate(),
	}
	m := make(map[string]axisDef, len(defs))
	for _, d := range defs {
		m[d.name] = d
	}
	return m
}

// decodeInto dekodiert ein Roh-JSON-Feld in das Achsen-Schema.
func decodeInto(raw []byte, v any, what, id string) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("goldbench: %s %s: %w", what, id, err)
	}
	return nil
}
