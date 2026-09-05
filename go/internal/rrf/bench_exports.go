// Package rrf — the retrieval pipeline of ctx: it drives the SQL-side 4-arm
// RRF fusion (ctx_rrf) through a strategy selector and applies the post-RRF
// stages — temporal gravity, dream-graph expansion, cluster boost, rerank.
//
// bench_exports.go — Bench-Export-Shims für ctx-goldbench.
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// Diese Datei enthält AUSSCHLIESSLICH exportierte Accessoren auf unexportierte
// Prompts, Builder und Parser dieses Pakets für den Benchmark-Harness
// (internal/goldbench, cmd/ctx-goldbench). Kein Verhaltens-Diff, keine
// Signaturänderung an Bestehendem.
//
// Source: https://github.com/GottZ/ctx
package rrf

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
