// text.go — the three text primitives that six packages held privately and
// identically (design D-04 §4.5, Naht 9). Each one is a pure function of its
// argument over the standard library alone; none of them reaches for config,
// a store or a clock. That is what let them be copied in the first place, and
// it is why one copy is enough.
package util

import (
	"strings"
	"time"
	"unicode"
)

// CollapseSpace reduces every run of unicode.IsSpace to a single U+0020 and
// drops leading and trailing runs entirely. It does NOT trim in the
// strings.TrimSpace sense as a separate step — a leading run produces no
// output byte because nothing has been written yet, and a trailing run is
// never flushed, so the result carries no edge spaces either way.
//
// unicode.IsSpace and not a byte test: NBSP (U+00A0), NEL (U+0085), EM SPACE
// (U+2003) and IDEOGRAPHIC SPACE (U+3000) are what a paste out of a PDF, an
// office document or a CJK source actually carries, and a comparison form that
// leaves them intact reports two identical texts as different. ZERO WIDTH
// SPACE (U+200B) is deliberately NOT space by that definition and survives —
// it is a joiner, not a separator.
func CollapseSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// PrimaryLanguageSubtag reduces a BCP-47-ish language value to its PRIMARY
// SUBTAG: trim + lower, then everything before the first "-" ("de-DE" → "de",
// "zh-Hant-TW" → "zh"). The primary subtag is what a language surface switches
// on — a regional variant must not silently fall out of its language's branch.
//
// Total and parameter-pure on purpose. config.Validate (V14) already
// normalizes and shape-checks the stored dream.language, but the surfaces that
// switch on it have callers which bypass the config path entirely (schedulers,
// handlers passing an Input through, tests); doing the reduction again at the
// point of use is what keeps those callers honest rather than lucky.
func PrimaryLanguageSubtag(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if i := strings.IndexByte(lang, '-'); i >= 0 {
		lang = lang[:i]
	}
	return lang
}

// MonthDE maps a lowercased German month name to its time.Month. "maerz" is
// carried next to "märz" because the umlaut-less spelling is what a keyboard
// without a German layout produces, and the extraction regexes upstream match
// both.
//
// READ ONLY. Package-level maps cannot be const in Go; this one is shared by
// the temporal rules of the LLM path and the date extraction of the store
// path, and a write from either would corrupt the other silently.
var MonthDE = map[string]time.Month{
	"januar": time.January, "februar": time.February, "märz": time.March, "maerz": time.March,
	"april": time.April, "mai": time.May, "juni": time.June,
	"juli": time.July, "august": time.August, "september": time.September,
	"oktober": time.October, "november": time.November, "dezember": time.December,
}

// MonthEN maps a lowercased English month name to its time.Month.
//
// READ ONLY, for the same reason as MonthDE.
var MonthEN = map[string]time.Month{
	"january": time.January, "february": time.February, "march": time.March,
	"april": time.April, "may": time.May, "june": time.June,
	"july": time.July, "august": time.August, "september": time.September,
	"october": time.October, "november": time.November, "december": time.December,
}
