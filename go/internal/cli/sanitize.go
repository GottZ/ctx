package cli

import (
	"strings"
	"unicode/utf8"
)

// sanitizeTerminal defuses attacker-controlled text (issue/comment titles,
// bodies, labels — design/03 §5.4) before it is printed to a terminal, agent log
// or CI output. It is an ALLOWLIST, not a blocklist: a bare \x1b filter is too
// thin — the C1 CSI single byte 0x9b, DCS 0x90, OSC 0x9d and a lone \r (line
// overwrite) all drive escape sequences into foreign terminals without ever
// carrying an ESC. So EVERYTHING is dropped except printable text plus the two
// whitespace controls \n and \t:
//
//   - every C0 control (0x00–0x1f) except \n (0x0a) and \t (0x09) — this covers
//     ESC, BEL, and \r;
//   - DEL (0x7f);
//   - every C1 control (0x80–0x9f), WHETHER it arrives as a raw single byte
//     (invalid UTF-8, e.g. the CSI 0x9b) or as a proper code point U+0080–U+009F;
//   - any other lone/invalid UTF-8 byte (never safe to emit raw).
//
// Valid multibyte UTF-8 (accents, dashes, emoji) is preserved byte-for-byte.
func sanitizeTerminal(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c < 0x80 { // ASCII fast path (incl. all C0 controls + DEL)
			if c == '\n' || c == '\t' || (c >= 0x20 && c != 0x7f) {
				b.WriteByte(c)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			// Lone/invalid byte — includes the raw C1 controls (0x80–0x9f) such as
			// the CSI 0x9b, DCS 0x90, OSC 0x9d. Never emitted.
		case r >= 0x80 && r <= 0x9f:
			// C1 control as a proper code point (U+0080–U+009F). Dropped.
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}
