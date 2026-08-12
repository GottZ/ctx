// Package llm — Bench-Export-Shims für ctx-goldbench.
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// Diese Datei enthält AUSSCHLIESSLICH exportierte Accessoren auf unexportierte
// Prompts, Builder und Parser dieses Pakets für den Benchmark-Harness
// (internal/goldbench, cmd/ctx-goldbench). Kein Verhaltens-Diff, keine
// Signaturänderung an Bestehendem.
//
// Source: https://github.com/GottZ/ctx
package llm

import "time"

// BenchTemporalPromptTemplate liefert das System-Prompt-Template der
// query-temporal-Pipeline (temporal.go:89). Der %s-Platzhalter wird — wie in
// NormalizeTemporal (temporal.go:252) — mit BenchBuildCalendar gefüllt.
func BenchTemporalPromptTemplate() string { return temporalPromptTemplate }

// BenchBuildCalendar baut den dynamischen V2-Kalender der query-temporal-
// Pipeline (temporal.go:119).
func BenchBuildCalendar(now time.Time) string { return buildCalendar(now) }

// BenchParseTemporalResponse parst eine query-temporal-Antwort inkl.
// Fence-Stripping und DimensionWeights-Derivation (temporal.go:282).
// Rückgabe (nil, nil) = valide Antwort ohne temporale Referenzen.
func BenchParseTemporalResponse(raw, query string) (*TemporalResult, error) {
	return parseTemporalResponse(raw, query)
}

// BenchClassifySystemPrompt liefert den System-Prompt der
// sensitivity-audit-Pipeline (classify.go:43).
func BenchClassifySystemPrompt() string { return classifySystemPrompt }

// BenchBuildClassifyUser baut die User-Message einer Audit-Frage
// (classify.go:151) — Frage + Separator + promptguard-gewrappter Block.
func BenchBuildClassifyUser(question, title, content string) string {
	return buildClassifyUser(question, title, content)
}

// BenchSystemPromptV52 liefert den v5.2-Synthesis-System-Prompt
// (synthesize.go:123) OHNE Nonce-Rule. Der vollständige Prompt inklusive
// Rule entsteht über das exportierte BuildPrompt.
func BenchSystemPromptV52() string { return systemPromptV52 }

// BenchTranslationSystemPrompt liefert den System-Prompt der
// query-translate-Pipeline (translate.go:39).
func BenchTranslationSystemPrompt() string { return translationSystemPrompt }

// BenchValidateTranslation prüft die Sicherheits-Constraints einer
// Übersetzung wie TranslateQuery (translate.go:137).
func BenchValidateTranslation(translated, original string) bool {
	return validateTranslation(translated, original)
}

// BenchStripSQLMeta wendet denselben SQL-Metazeichen-Stripper an, den
// TranslateQuery VOR validateTranslation ausführt (translate.go:54,126).
func BenchStripSQLMeta(s string) string { return sqlStripper.Replace(s) }
