package store

import (
	"fmt"
)

// ErrIDPrefixTooShort is ResolveBlockID's refusal of a prefix shorter than
// MinIDPrefixLen — the caller's input, not a fault; its text names the
// length the caller sent (the wording every surface has answered with).
var ErrIDPrefixTooShort = fmt.Errorf("store: id prefix must be at least %d characters", MinIDPrefixLen)

// idIsUUIDText reports whether an id of the full shape (IsFullUUID) is a UUID
// as the database's cast reads it: hex digits in either case around the four
// hyphens. Any other text of that shape names no block (second review of PR
// #554: it reached the cast and came back as a server error, 22P02) — a NUL
// and invalid UTF-8 included, which the database cannot even receive as text
// (22021, issue #547).
func idIsUUIDText(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue // the hyphens IsFullUUID read
		}
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
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
