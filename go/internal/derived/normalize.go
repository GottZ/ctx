package derived

import (
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/GottZ/ctx/internal/util"
)

// Normalize is the ONE comparison form of this module: NFKC, then case
// folding, then whitespace collapse, then trim. No punctuation stripping.
//
// NFKC and not NFC (masterplan K4, design §4.4.1 change 1). The precedent is
// the echo guard that is already built and running in the label arm
// (topiclabel/guard.go:184-189), whose own comment carries the argument at the
// living code: a title and the text a model builds from it "routinely differ
// in Unicode normalisation alone … Without it the gate is trivially evaded by
// the SUMMARISER — not by an attacker; the model is not adversarial here, it
// is careless."
//
// For a CONTAINMENT gate the direction of the error decides it. NFKC can only
// produce additional MATCHES, never additional rejections: a full-width "ｋｅｙ"
// and "key" are the same text rendered differently, and a byte comparison
// would call them different. A fabricated 32-rune fragment does not hit by
// accident under either form. NFKC therefore lowers the false-reject rate
// without measurably raising the false-accept rate.
//
// Case folding is strings.ToLower and not x/text/cases.Fold, matching the
// built precedent (topiclabel/guard.go:185). K4 names the step "Casefold"; the
// design's own code line spells it strings.ToLower, and the code is the
// authority for a form two gates have to agree on byte for byte.
//
// No punctuation stripping (K4): it would raise the false-ACCEPT rate for no
// gain — a quote that differs from its source only in punctuation is not the
// failure mode this gate exists for.
func Normalize(s string) string {
	s = strings.ToLower(norm.NFKC.String(s))
	return strings.TrimSpace(util.CollapseSpace(s))
}
