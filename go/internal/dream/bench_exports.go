// bench_exports.go — Bench-Export-Shims für ctx-goldbench.
// Part of ctx by GottZ and Damien Moon — The memory your LLM pretends to have.
//
// Diese Datei enthält AUSSCHLIESSLICH exportierte Accessoren auf unexportierte
// Prompts, Builder und Parser dieses Pakets, damit der Benchmark-Harness
// (internal/goldbench, cmd/ctx-goldbench) die echten ctx-Prompts byte-identisch
// abspielen und die echten Parser nutzen kann. Kein Verhaltens-Diff, keine
// Signaturänderung an Bestehendem.
//
// Source: https://github.com/GottZ/ctx
package dream

import "github.com/GottZ/ctx/internal/llm"

// BenchTemporalValidationPrompt liefert den System-Prompt der
// dream-temporal-Pipeline (validate_temporal.go:40).
func BenchTemporalValidationPrompt() string { return temporalValidationPrompt }

// BenchBuildTemporalReviewPrompt baut den User-Prompt der dream-temporal-
// Pipeline (validate_temporal.go:216).
func BenchBuildTemporalReviewPrompt(block *BlockInfo) string {
	return buildTemporalReviewPrompt(block)
}

// BenchParseTemporalReview parst eine Phase-2-Antwort wie die dream-temporal-
// Pipeline (validate_temporal.go, parseTemporalReview) — inklusive der
// Fence-Toleranz über llm.StripJSONFence. Der Harness darf diese Antwort nicht
// selbst dekodieren: eine eigene Kopie driftet, sobald der Produktions-Parser
// sich ändert (so geschehen mit der Fence-Toleranz, entdeckt 2026-09-18).
func BenchParseTemporalReview(raw string) (*TemporalReview, error) {
	return parseTemporalReview(raw)
}

// BenchKeywordSystemPrompt liefert den System-Prompt der dream-keywords-
// Pipeline (keywords.go:35).
func BenchKeywordSystemPrompt() string { return keywordSystemPrompt }

// BenchBuildKeywordPrompt baut den User-Prompt der dream-keywords-Pipeline
// (keywords.go:150).
func BenchBuildKeywordPrompt(title, content string) string {
	return buildKeywordPrompt(title, content)
}

// BenchParseKeywords parst eine Keyword-Antwort wie die dream-keywords-
// Pipeline (keywords.go:169).
func BenchParseKeywords(raw string) ([]string, error) { return parseKeywords(raw) }

// BenchBuildEvalPrompt baut System- und User-Prompt der dream-eval-Pipeline
// (evaluate.go:210) — inklusive promptguard-Nonce, exakt wie in Produktion.
func BenchBuildEvalPrompt(source BlockInfo, candidates []BlockInfo) (system, user string) {
	return buildEvalPrompt(source, candidates)
}

// BenchParseLinks parst eine dream-eval-Antwort mit allen produktiven
// Drift-Formen (parse.go:40). Rückgabe wie parseLinks: (links, format, err).
func BenchParseLinks(raw string) ([]Link, string, error) { return parseLinks(raw) }

// BenchBuildRecurrencePrompt baut System- und User-Prompt der
// dream-recurrence-Pipeline (recurrence.go:235). Der interne Typ
// recurrenceCandidate ist aus einem Gold-Case nicht direkt konstruierbar,
// deshalb nimmt der Shim seine Felder einzeln entgegen und reicht sie
// unverändert durch — der Prompt-Bau selbst bleibt das Original.
func BenchBuildRecurrencePrompt(source BlockInfo, targetID, targetTitle, targetText string, titleSim float64) (system, user string) {
	return buildRecurrencePrompt(source, recurrenceCandidate{
		TargetID:    targetID,
		TargetTitle: targetTitle,
		TargetText:  targetText,
		TitleSim:    titleSim,
	})
}

// BenchParseRecurrenceResponse parst eine dream-recurrence-Antwort
// (recurrence.go:254). Der interne Typ recurrenceVerdict bleibt unexportiert;
// der Shim gibt seine Felder einzeln zurück.
func BenchParseRecurrenceResponse(raw string) (verdict, pattern string, confidence float64, err error) {
	v, err := parseRecurrenceResponse(raw)
	return v.Verdict, v.Pattern, v.Confidence, err
}

// Decide-Modus (2026-09-20): Shims für die goldbench-Achsen links-decide und
// recurrence-decide, damit die Achse exakt die Produktions-Abbildung misst.

// BenchBuildDecideEvalPrompt baut das paarweise Decide-Prompt der dream-eval-
// Pipeline (decide_eval.go, buildDecideEvalPrompt) — inklusive Nonce, exakt
// wie in Produktion.
func BenchBuildDecideEvalPrompt(source, cand BlockInfo) (system, user string) {
	return buildDecideEvalPrompt(source, cand)
}

// BenchDecideLinkLabels liefert das Antwort-Vokabular des Decide-Eval-Prompts
// in Prompt-Reihenfolge (A..E), für llm.DecideChoice.
func BenchDecideLinkLabels() []string { return append([]string(nil), decideLinkLabels...) }

// BenchDecideLinks bildet je Kandidat die Entscheidung auf höchstens einen
// Link ab (decisionToLink) und fährt danach dieselben Post-Parse-Constraints
// wie die Produktion (finishDecideLinks: supersedes-Richtung, Kandidatenfilter
// mit Typ-Gate, Tie-Distanz, Hard-Cap). decisions ist parallel zu candidates;
// ein Nil-Eintrag (Fallback-Signal) ergibt keinen Link. tieOdds ist
// dream.decide_tie_odds (≤ 0 = Regel aus).
func BenchDecideLinks(source BlockInfo, candidates []BlockInfo, decisions []*llm.Decision, tieOdds float64) []Link {
	links := make([]Link, 0, len(candidates))
	for i, c := range candidates {
		if i >= len(decisions) || decisions[i] == nil {
			continue
		}
		if l, ok := decisionToLink(c.ID, *decisions[i]); ok {
			links = append(links, l)
		}
	}
	return finishDecideLinks(source, candidates, links, tieOdds)
}

// BenchDecideLinkArgmax liefert den Relationship-Typ der Entscheidung ohne
// Gate ("none" für die No-Link-Antwort) — die Rohsicht neben der gegateten.
func BenchDecideLinkArgmax(d llm.Decision) string {
	if l, ok := decisionToLink("", d); ok {
		return l.Relationship
	}
	return "none"
}

// BenchBuildDecideRecurrencePrompt baut das Decide-Prompt des Recurrence-
// Confirms (decide_recurrence.go), Felder wie BenchBuildRecurrencePrompt.
func BenchBuildDecideRecurrencePrompt(source BlockInfo, targetID, targetTitle, targetText string, titleSim float64) (system, user string) {
	return buildDecideRecurrencePrompt(source, recurrenceCandidate{
		TargetID: targetID, TargetTitle: targetTitle, TargetText: targetText, TitleSim: titleSim,
	})
}

// BenchDecideRecurrenceLabels liefert das Antwort-Vokabular (A..C).
func BenchDecideRecurrenceLabels() []string {
	return append([]string(nil), decideRecurrenceLabels...)
}

// BenchDecideRecurrenceVerdict bildet die Entscheidung auf (verdict,
// confidence) ab wie decisionToRecurrenceVerdict; gated meldet, ob der
// DetectRecurrence-Loop den Verdict schreiben würde (Decide-Gate
// DecideRecurrenceFloor auf 1−P(none); "none" ist nie ein Link).
func BenchDecideRecurrenceVerdict(d llm.Decision) (verdict string, confidence float64, gated bool) {
	v := decisionToRecurrenceVerdict(d)
	switch v.Verdict {
	case "recurrent", "supersedes":
		return v.Verdict, v.Confidence, v.Confidence >= recurrenceWriteFloor(true, v.Verdict)
	default:
		return v.Verdict, v.Confidence, false
	}
}
