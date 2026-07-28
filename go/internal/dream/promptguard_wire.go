package dream

import (
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/promptguard"
)

// guardText is the wiring order for every dream prompt builder that carries
// foreign block text (design 04 §4.2): Neutralize FIRST, EscapeXml second.
//
// Reversed, Neutralize would run against "&lt;|" and never see "<|" — the
// guard would be a silent no-op with nothing turning red, which is why the
// order is pinned by a probe (TestKeywordPrompt_NeutralizeRunsBeforeEscape)
// and not just by this comment.
//
// EscapeXml STAYS: the wiring waves are additive. Whether Neutralize replaces
// it for content positions is an eval-backed decision (design 04 §8-E1), not
// something a wiring wave settles. Consequence worth knowing: wherever both
// run, the delimiter breakout is dead twice over and the text fidelity is
// exactly today's — this wave changes what survives, not how it reads.
func guardText(s string) string {
	n, _ := promptguard.Neutralize(s)
	return llm.EscapeXml(n)
}

// guardLine is guardText for a LINE-BASED position — a metadata line outside
// any block, where the line break itself carries structure and XML escaping
// does not touch it (design 04 §2.3-c2: the line-based key/value position is
// the one EscapeXml provably does not cover).
//
// ClampLine runs FIRST, which is the order promptguard uses for its own marker
// attributes: with the newlines already collapsed, a turn marker is inert
// regardless, and Neutralize still has to run for the openers that carry no
// newline at all.
func guardLine(s string) string {
	n, _ := promptguard.Neutralize(promptguard.ClampLine(s))
	return llm.EscapeXml(n)
}
