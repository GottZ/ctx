// Package llm — Temporal Normalization
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// GottZ Temporal Gravity: Physics-inspired temporal scoring where knowledge
// blocks are masses in time-space with gravitational fields. Novel approach
// combining asymmetric Gaussian decay, cognitive-science-calibrated windows
// (Rubin & Baddeley 0.4d/d), specificity-weighted mass, and semantic coupling.
// No prior art combines all dimensions — confirmed via 22-agent literature review.
//
// GottZ Cyclic Phase Model: Multi-dimensional temporal retrieval where each
// cyclic time structure (weekday, month, quarter, year, daily) is an independent
// dimension with normalized phase [0,1) and Gaussian decay. Queries activate
// specific dimensions — "immer dienstags" activates weekday:1.0 while
// "am letzten Dienstag" activates linear:0.6 + weekday:0.4.
//
// Source: https://github.com/GottZ/ctx
// Contributors: https://github.com/GottZ/ctx/graphs/contributors
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	// TemporalTimeout is the HTTP timeout for temporal normalization.
	TemporalTimeout = 15 * time.Second
)

// TemporalDate is a single resolved temporal reference.
type TemporalDate struct {
	Ref  string  `json:"ref"`
	Date string  `json:"date"`
	End  *string `json:"end"`
	Dir  string  `json:"dir"`
}

// TemporalResult is the output of LLM temporal normalization.
type TemporalResult struct {
	Dates []TemporalDate `json:"dates"`
	Query string         `json:"query"`
}

// TemporalOptions returns Ollama options for temporal normalization.
func TemporalOptions() Options {
	return Options{
		Temperature: 0.1,
		NumPredict:  300,
	}
}

// temporalPromptTemplate is the system prompt for temporal normalization.
// The %s placeholder is filled with the dynamic calendar.
const temporalPromptTemplate = `You are a temporal reference resolver. Output raw JSON only — no markdown, no code fences, no explanation.

%s

DIRECTION RULES (in priority order):
1. VERB TENSE always wins over defaults:
   Past (war/ging/hatte/wollte/habe..gemacht/bin..gewesen/musste) → BACKWARD
   Future (will/werde/gehe/mache/muss/soll) → FORWARD
2. Explicit markers override tense: letzten/last → BACKWARD, nächsten/next → FORWARD
3. bis/until/by/deadline/frist = ALWAYS FORWARD
4. seit/since = start in past, end=today
5. ONLY when NO verb tense AND NO explicit marker: bare weekday → nearest forward occurrence.
6. No temporal reference at all → empty dates.
7. Vague memory (nochmal, irgendwann, damals) without specific time → empty dates.

JSON: {"dates":[{"ref":"matched text","date":"YYYY-MM-DD","end":"YYYY-MM-DD or null","dir":"past|future|today|range"}],"query":"original with dates inserted"}`

// weekdayDE maps Go's time.Weekday to German weekday names.
var weekdayNameDE = [7]string{"Sonntag", "Montag", "Dienstag", "Mittwoch", "Donnerstag", "Freitag", "Samstag"}

// weekdayEN maps Go's time.Weekday to English weekday names.
var weekdayNameEN = [7]string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}

// buildCalendar generates the dynamic calendar section for the temporal prompt.
func buildCalendar(now time.Time) string {
	// Find Monday of this week (ISO: Mon=1).
	wd := int(now.Weekday())
	if wd == 0 {
		wd = 7
	}
	thisMonday := now.AddDate(0, 0, -(wd - 1))
	lastMonday := thisMonday.AddDate(0, 0, -7)
	nextMonday := thisMonday.AddDate(0, 0, 7)
	weekAfterMonday := thisMonday.AddDate(0, 0, 14)

	f := func(t time.Time) string { return t.Format("Mon 2006-01-02") }
	fDE := func(t time.Time) string {
		return weekdayNameDE[t.Weekday()] + " " + t.Format("2006-01-02")
	}

	// Last/this/next weekend (Sat+Sun)
	thisSat := thisMonday.AddDate(0, 0, 5)
	thisSun := thisMonday.AddDate(0, 0, 6)
	lastSat := lastMonday.AddDate(0, 0, 5)
	lastSun := lastMonday.AddDate(0, 0, 6)
	nextSat := nextMonday.AddDate(0, 0, 5)
	nextSun := nextMonday.AddDate(0, 0, 6)

	return fmt.Sprintf(`TODAY: %s %s.
CALENDAR:
  vorgestern/day-before-yesterday: %s
  gestern/yesterday: %s
  heute/today: %s
  morgen/tomorrow: %s
  übermorgen/day-after-tomorrow: %s
  Last week:    %s .. %s
  This week:    %s .. %s
  Next week:    %s .. %s
  Week after:   %s .. %s
  Last weekend: %s + %s
  This weekend: %s + %s
  Next weekend: %s + %s`,
		weekdayNameEN[now.Weekday()], now.Format("2006-01-02"),
		fDE(now.AddDate(0, 0, -2)),
		fDE(now.AddDate(0, 0, -1)),
		fDE(now),
		fDE(now.AddDate(0, 0, 1)),
		fDE(now.AddDate(0, 0, 2)),
		f(lastMonday), f(lastMonday.AddDate(0, 0, 6)),
		f(thisMonday), f(thisMonday.AddDate(0, 0, 6)),
		f(nextMonday), f(nextMonday.AddDate(0, 0, 6)),
		f(weekAfterMonday), f(weekAfterMonday.AddDate(0, 0, 6)),
		f(lastSat), f(lastSun),
		f(thisSat), f(thisSun),
		f(nextSat), f(nextSun),
	)
}

// temporalIntentWords are words that indicate a query has temporal intent.
// Used for cheap pre-filtering before the LLM call.
var temporalIntentWords = []string{
	// German
	"heute", "gestern", "morgen", "vorgestern", "übermorgen",
	"woche", "monat", "jahr",
	"montag", "dienstag", "mittwoch", "donnerstag", "freitag", "samstag", "sonntag",
	"wochenende", "neulich", "kürzlich", "demnächst", "bald",
	"letzten", "nächsten", "vorigen", "kommenden",
	"seit", "bis", "vor", "anfang", "ende", "mitte",
	"heut", "abend",
	// German months (BUG-5: "im März" was missed)
	"januar", "februar", "märz", "maerz", "april", "mai", "juni",
	"juli", "august", "september", "oktober", "november", "dezember",
	// English
	"today", "yesterday", "tomorrow",
	"week", "month", "year",
	"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday",
	"weekend", "recently", "soon", "last", "next", "ago", "since", "until", "by",
}

// isoDateRe detects ISO dates in queries (BUG-1 fix).
var isoDateRe = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)

// HasTemporalIntent returns true if the query likely contains temporal references.
// This is a cheap pre-filter — false positives are OK (the LLM will return empty dates).
func HasTemporalIntent(query string) bool {
	// Check for ISO dates first (BUG-1: "was war am 2026-03-27?" was missed).
	if isoDateRe.MatchString(query) {
		return true
	}
	lower := strings.ToLower(query)
	for _, w := range temporalIntentWords {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// jsonFenceRe strips markdown code fences from LLM output.
var jsonFenceRe = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?})\\s*```")

// NormalizeTemporal uses the LLM to resolve temporal references in a query.
// Returns nil if no temporal references are found or if the LLM call fails.
func NormalizeTemporal(ctx context.Context, host, model, query string, now time.Time) (*TemporalResult, error) {
	calendar := buildCalendar(now)
	systemPrompt := fmt.Sprintf(temporalPromptTemplate, calendar)

	resp, err := Chat(ctx, host, model, systemPrompt, query, TemporalOptions(), TemporalTimeout)
	if err != nil {
		return nil, fmt.Errorf("llm: temporal: %w", err)
	}

	raw := strings.TrimSpace(resp.Message.Content)

	// Strip markdown fences if present.
	if m := jsonFenceRe.FindStringSubmatch(raw); len(m) > 1 {
		raw = m[1]
	}

	var result TemporalResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, fmt.Errorf("llm: temporal: invalid JSON: %w: %s", err, raw)
	}

	// Empty dates = no temporal references found.
	if len(result.Dates) == 0 {
		return nil, nil
	}

	return &result, nil
}

// TemporalToFTSExpansion converts resolved dates to a websearch_to_tsquery OR string.
func TemporalToFTSExpansion(dates []TemporalDate) string {
	if len(dates) == 0 {
		return ""
	}

	seen := make(map[string]bool)
	var terms []string

	addDate := func(isoDate string) {
		t, err := time.Parse("2006-01-02", isoDate)
		if err != nil {
			return
		}
		key := t.Format("2006-01-02")
		if seen[key] {
			return
		}
		seen[key] = true
		terms = append(terms,
			weekdayNameDE[t.Weekday()],
			weekdayNameEN[t.Weekday()],
			key,
		)
	}

	for _, d := range dates {
		addDate(d.Date)
		if d.End != nil {
			addDate(*d.End)
		}
	}

	return strings.Join(terms, " OR ")
}

// TemporalToEmbedPrefix builds a compact date prefix for embedding augmentation.
func TemporalToEmbedPrefix(dates []TemporalDate) string {
	if len(dates) == 0 {
		return ""
	}

	seen := make(map[string]bool)
	var parts []string

	for _, d := range dates {
		t, err := time.Parse("2006-01-02", d.Date)
		if err != nil {
			continue
		}
		key := t.Format("2006-01-02")
		if seen[key] {
			continue
		}
		seen[key] = true
		parts = append(parts, fmt.Sprintf("%s %s", weekdayNameDE[t.Weekday()], key))
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ") + "."
}
