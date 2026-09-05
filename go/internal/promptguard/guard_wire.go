// guard_wire.go — the XML escaper and the two wiring orders every prompt
// builder in this tree uses for foreign text.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx
package promptguard

import "strings"

// EscapeXML replaces the five XML special characters with their entity
// references.
//
// The order of the five replacements is load-bearing: "&" runs FIRST,
// otherwise the later steps would escape the ampersands this function has
// just written and "<" would render as "&amp;lt;".
//
// ONE place. Until wave T04-6 the same body existed twice — here as the
// package-local escapeXMLAttr and once more as llm.EscapeXml — and the second
// copy was reached from seven wiring functions in llm, dream, rrf and
// goldbench. The escaper lives in promptguard because that is where the
// guard chain lives (clampAttr uses it too); the earlier note that keeping it
// package-local avoids an internal/llm import here is settled, not lost: the
// dependency now runs the other way for everyone.
func EscapeXML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	s = strings.ReplaceAll(s, "'", "&apos;")
	return s
}

// GuardText is the wiring order for every prompt builder that carries foreign
// text in a BLOCK position (design 04 §4.2): Neutralize FIRST, EscapeXML
// second.
//
// Reversed, Neutralize would run against "&lt;|" and never see "<|" — the
// guard would be a silent no-op with nothing turning red, which is why the
// order is pinned by probes and not just by this comment. Four pipelines hold
// one each: TestBuildPrompt_NeutralizeRunsBeforeEscape (llm),
// TestKeywordPrompt_NeutralizeRunsBeforeEscape (dream),
// TestRerankJudgePrompt_NeutralizeRunsBeforeEscape (rrf) and
// TestBuildTitleUser_NeutralizeRunsBeforeEscape (goldbench). They probe one
// body now — four independent pipeline probes of one mechanism are cheaper
// than four mechanisms.
//
// The escape STAYS alongside Neutralize: these are additive wirings. Whether
// Neutralize replaces the escape for content positions is an eval-backed
// decision (design 04 §8-E1), not something a wiring wave settles.
// Consequence worth knowing: wherever both run, the delimiter breakout is
// dead twice over and the text fidelity is exactly today's.
//
// NOT for every position. chat/tools.go guardText deliberately runs Neutralize
// ALONE — its value sits inside a JSON string that the model is meant to read
// back verbatim, and an escape there would rewrite block content.
func GuardText(s string) string {
	n, _ := Neutralize(s)
	return EscapeXML(n)
}

// GuardLine is GuardText for a LINE-BASED position — a metadata line outside
// any block ("Query:", "Doc n [category/title]:", a key/value line), where the
// line break itself carries structure and XML escaping provably does not touch
// it (design 04 §2.3-c2).
//
// ClampLine runs FIRST, the order promptguard uses for its own marker
// attributes: with the newlines already collapsed a turn marker is inert
// regardless, and Neutralize still has to run for the openers that carry no
// newline at all.
//
// NOT for every line position: derived.safeLine and dream.clampField are built
// from the same two steps and end WITHOUT an escape on purpose — their render
// paths have no XML to escape into.
func GuardLine(s string) string {
	n, _ := Neutralize(ClampLine(s))
	return EscapeXML(n)
}
