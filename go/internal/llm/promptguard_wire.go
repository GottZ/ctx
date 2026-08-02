package llm

import "github.com/GottZ/ctx/internal/promptguard"

// guardText is the wiring order for every llm prompt builder that carries
// foreign text (design 04 §4.2): Neutralize FIRST, EscapeXml second.
//
// Reversed, Neutralize would run against "&lt;|" and never see "<|" — the
// guard would be a silent no-op with nothing turning red, which is why the
// order is pinned by a probe (TestBuildPrompt_NeutralizeRunsBeforeEscape) and
// not just by this comment. Twin of dream.guardText; the two packages keep
// their own copy because the dependency runs llm ← dream, not the other way.
//
// EscapeXml STAYS: the wiring waves are additive. Whether Neutralize replaces
// it for content positions is an eval-backed decision (design 04 §8-E1), not
// something a wiring wave settles. Consequence worth knowing: wherever both
// run, the delimiter breakout is dead twice over and the text fidelity is
// exactly today's.
func guardText(s string) string {
	n, _ := promptguard.Neutralize(s)
	return EscapeXml(n)
}
