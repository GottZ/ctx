// bench_exports.go — Bench-Export-Shims für ctx-goldbench.
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// Diese Datei enthält AUSSCHLIESSLICH exportierte Accessoren auf unexportierte
// Prompts, Builder und Parser dieses Pakets, damit der Benchmark-Harness
// (internal/goldbench, cmd/ctx-goldbench) die echten ctx-Prompts byte-identisch
// abspielen und die echten Parser nutzen kann. Kein Verhaltens-Diff, keine
// Signaturänderung an Bestehendem.
//
// Source: https://github.com/GottZ/ctx
package dream

// BenchTemporalValidationPrompt liefert den System-Prompt der
// dream-temporal-Pipeline (validate_temporal.go:40).
func BenchTemporalValidationPrompt() string { return temporalValidationPrompt }

// BenchBuildTemporalReviewPrompt baut den User-Prompt der dream-temporal-
// Pipeline (validate_temporal.go:216).
func BenchBuildTemporalReviewPrompt(block *BlockInfo) string {
	return buildTemporalReviewPrompt(block)
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

// BenchDreamSystemPrompt liefert den nackten dream-eval-System-Prompt OHNE
// die Nonce-Rule (evaluate.go:48). Für den vollständigen System-Prompt
// inklusive Rule ist BenchBuildEvalPrompt die Quelle.
func BenchDreamSystemPrompt() string { return dreamSystemPrompt }

// BenchBuildEvalPrompt baut System- und User-Prompt der dream-eval-Pipeline
// (evaluate.go:210) — inklusive promptguard-Nonce, exakt wie in Produktion.
func BenchBuildEvalPrompt(source BlockInfo, candidates []BlockInfo) (system, user string) {
	return buildEvalPrompt(source, candidates)
}

// BenchParseLinks parst eine dream-eval-Antwort mit allen produktiven
// Drift-Formen (parse.go:40). Rückgabe wie parseLinks: (links, format, err).
func BenchParseLinks(raw string) ([]Link, string, error) { return parseLinks(raw) }

// BenchRecurrenceSystemPrompt liefert den nackten dream-recurrence-System-
// Prompt OHNE die Nonce-Rule (recurrence.go:38).
func BenchRecurrenceSystemPrompt() string { return recurrenceSystemPrompt }

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
