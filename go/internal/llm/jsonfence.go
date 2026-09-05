// jsonfence.go — the one lenient markdown-fence strip for LLM JSON answers.
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// Three packages carried a byte-similar copy of this trim (dream/parse.go,
// dream/recurrence.go, goldbench/axis_keywords.go) and the copies had drifted
// apart in exactly the way copies do: one tested the prefix on the untrimmed
// input, one on the trimmed one, and one tested nothing at all. This file is
// the single lenient variant; the drift is documented on the function.
//
// Source: https://github.com/GottZ/ctx
package llm

import "strings"

// StripJSONFence peels a leading ```json (or bare ```) markdown fence and its
// matching trailing ``` off an LLM answer and returns the body. It is
// idempotent on plain JSON: an answer that carries no fence comes back with
// nothing removed but its surrounding whitespace.
//
// The HasPrefix test runs on the TRIMMED string, and the doc comment says so
// deliberately. The dream variant this helper replaces tested the UNTRIMMED
// input and therefore silently depended on every caller trimming first — a
// contract its own comment never stated, and one a new caller had no way to
// read off the signature.
//
// A trailing ``` is removed ONLY when the answer actually opens a fence. An
// answer that merely ends in ``` without opening one is model content, not
// fence syntax, and stays untouched.
//
// Three neighbouring fence handlers stay separate on purpose — they answer
// different questions and must not collapse into this one:
//
//   - topiclabel.stripCodeFence is STRICT: it unwraps only a fence that spans
//     the ENTIRE input and returns the input unchanged otherwise, so a
//     fenced-plus-commentary answer still fails the structural label gate.
//     Making it lenient would widen that gate.
//   - llm.jsonFenceRe (temporal.go) EXTRACTS an embedded {...} object out of
//     arbitrary surrounding prose. It is a search, not a trim.
//   - rrf.jsonArrayPattern extracts a JSON integer array from free text and is
//     not about fences at all.
func StripJSONFence(raw string) string {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
