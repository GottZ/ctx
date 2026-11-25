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

// The canonical text of a block id (id::text): 32 lowercase hex digits with
// a hyphen after the 8th, 12th, 16th and 20th — the lowest and the highest
// id of that shape.
const (
	idTextLow  = "00000000-0000-0000-0000-000000000000"
	idTextHigh = "ffffffff-ffff-ffff-ffff-ffffffffffff"
)

// idPrefixRange returns the primary-key range [lo, hi] of the ids whose
// canonical text starts with prefix: the prefix completed to the id's shape
// with '0' (lo) and 'f' (hi), hyphens at their places. A uuid orders by its
// bytes, the order of its canonical text, so the range holds exactly the ids
// `id::text LIKE prefix || '%'` admits, and a scan of context_blocks_pkey
// over it reads those ids only (issue #536: the text match had no index path
// and read every visible row of the reader). ok is false when no id's text
// starts with prefix — a hyphen where the text has a digit, a digit where it
// has a hyphen, or more characters than the text has. prefix holds only
// lowercase hex digits and hyphens (idPrefixMatchesNothing answers the rest).
func idPrefixRange(prefix string) (lo, hi string, ok bool) {
	if len(prefix) > len(idTextLow) {
		return "", "", false
	}
	for i := 0; i < len(prefix); i++ {
		if (prefix[i] == '-') != (idTextLow[i] == '-') {
			return "", "", false
		}
	}
	return prefix + idTextLow[len(prefix):], prefix + idTextHigh[len(prefix):], true
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
