// bench_exports.go — Bench-Export-Shims für ctx-goldbench.
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// Diese Datei enthält AUSSCHLIESSLICH exportierte Accessoren auf unexportierte
// Prompts, Builder und Parser dieses Pakets für den Benchmark-Harness
// (internal/goldbench, cmd/ctx-goldbench). Kein Verhaltens-Diff, keine
// Signaturänderung an Bestehendem.
//
// Source: https://github.com/GottZ/ctx
package topiclabel

// BenchSystemPromptFor baut den System-Prompt der cluster-label-Pipeline
// inklusive Nonce-Rule (prompt.go:58).
func BenchSystemPromptFor(lang, nonce string) string { return systemPromptFor(lang, nonce) }

// BenchBuildUser baut die User-Message der cluster-label-Pipeline
// (prompt.go:85). Der interne Typ promptCore bleibt unexportiert; der Shim
// nimmt seine Felder einzeln entgegen und reicht sie unverändert durch.
func BenchBuildUser(nonce string, titles, tags, categories []string) string {
	return buildUser(nonce, promptCore{Titles: titles, Tags: tags, Categories: categories})
}

// BenchParseLabel validiert eine Modell-Antwort strukturell wie parseLabel
// (guard.go:54). ok=true genau dann, wenn das Label den Contract besteht
// (valides JSON mit exakt dem Key "label", nicht leer, ≤120 Runen, kein
// Guard-Marker-Artefakt).
func BenchParseLabel(raw string) (label string, ok bool) {
	l, rej := parseLabel(raw)
	return l, rej == rejectNone
}
