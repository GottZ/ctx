// Package rrf — Bench-Export-Shims für ctx-goldbench.
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// Diese Datei enthält AUSSCHLIESSLICH exportierte Accessoren auf unexportierte
// Prompts, Builder und Parser dieses Pakets für den Benchmark-Harness
// (internal/goldbench, cmd/ctx-goldbench). Kein Verhaltens-Diff, keine
// Signaturänderung an Bestehendem.
//
// Source: https://github.com/GottZ/ctx
package rrf

// BenchRerankSystemPrompt liefert den nackten rerank-judge-System-Prompt OHNE
// die Nonce-Rule (rerank.go:80). Der vollständige System-Prompt inklusive
// Rule entsteht über BenchBuildRerankJudgePrompt.
func BenchRerankSystemPrompt() string { return rerankSystemPrompt }

// BenchBuildRerankJudgePrompt baut System- und User-Prompt des LLM-Judge
// (rerank.go:205) — inklusive promptguard-Nonce, exakt wie in Produktion.
func BenchBuildRerankJudgePrompt(query string, docsToRerank []SearchResult) (system, user string) {
	return buildRerankJudgePrompt(query, docsToRerank)
}

// BenchParseRerankScores parst das Integer-Score-Array des Judge mit
// Längen-Match gegen expectedCount (rerank.go:349).
func BenchParseRerankScores(content string, expectedCount int) ([]float64, error) {
	return parseRerankScores(content, expectedCount)
}
