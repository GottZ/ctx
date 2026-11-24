package store

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrIDPrefixTooShort is ResolveBlockID's refusal of a prefix shorter than
// MinIDPrefixLen — the caller's input, not a fault; its text names the
// length the caller sent (the wording every surface has answered with).
var ErrIDPrefixTooShort = fmt.Errorf("store: id prefix must be at least %d characters", MinIDPrefixLen)

// idTextUnsendable reports an id the database cannot receive as text: a NUL
// character or invalid UTF-8 (22021). Such an id names no block (issue #547:
// it reached the resolve's query and came back as a server error).
func idTextUnsendable(s string) bool {
	return strings.IndexByte(s, 0) >= 0 || !utf8.ValidString(s)
}

// idPrefixMatchesNothing reports a prefix with a character no block id's
// text carries — id::text is lowercase hex digits and hyphens — so it
// matches no block. Answered without the query (issue #547): there a '%' or
// '_' was a LIKE wildcard, and "%%%%%%%%" resolved to a block the caller had
// not named; a NUL or invalid UTF-8 failed the query.
func idPrefixMatchesNothing(prefix string) bool {
	for i := 0; i < len(prefix); i++ {
		c := prefix[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && c != '-' {
			return true
		}
	}
	return false
}
