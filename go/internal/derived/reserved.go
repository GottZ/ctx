package derived

import "strings"

// The write lock I7 (design D-01 §1.3, §4.3.1) rests on three names that must
// be owned by CODE and not by configuration, a registry row or a convention:
// the two derived TYPE names (StratumOf, derived.go), the metadata key that
// marks a block as a derivative (MetadataKey, provenance.go — S3 keys on its
// PRESENCE, never on its shape), and the two derived CATEGORIES below.
//
// Why the list lives here and not in handler: §4.3.1 puts it in this package
// because three write surfaces plus two update surfaces consult it, and a list
// that each surface keeps its own copy of is a convention. This package is the
// leaf every one of them may import (see the package doc: derived imports only
// promptguard and sensitivity, so store and handler can both depend on it).

// ReservedCategories are the block categories the derived layer writes into.
// Client writes into them are refused with 403 (S2, §4.3.1).
//
//   - "catalog" is the catalogue arm's category (design D-01 §4.7.1, D-03).
//   - "session-insights" is the DEFAULT of distill.category (config.go:1831),
//     which is the insight arm's category and half of its upsert identity.
//
// The second entry is the one honest gap in this list, and it is named rather
// than papered over: distill.category is an operator-settable key, so an
// operator who moves the insight arm to another category moves it OUT of this
// reservation. Following the key here would make a security list configurable,
// which §4.3.1 rejects ("Mechanismus, nicht Politik"); the arm wave (D-02) is
// where the key and the reservation have to be reconciled — either by pinning
// the key to this value or by refusing a distill.category outside this list.
var ReservedCategories = []string{
	"catalog",
	"session-insights",
}

// HasProvenance reports whether a metadata map carries the derived layer's
// provenance key. It is the ONE predicate behind both halves of S3b — the
// handler gate that answers 403 reserved_metadata and the store-side sentinel
// on the issue domain — so the two can never disagree about what "carries
// provenance" means.
//
// PRESENCE only, never shape: the sperr side (`metadata ? 'provenance'` in SQL)
// tests exactly the same thing, and a predicate that judged the value would
// admit keys the SQL side then locks on.
func HasProvenance(metadata map[string]any) bool {
	if metadata == nil {
		return false
	}
	_, ok := metadata[MetadataKey]
	return ok
}

// IsReservedCategory reports whether a client write into category must be
// refused.
//
// The comparison folds case and surrounding space. The upsert key
// (category, title, scope) is byte-exact, so "Catalog" could not collide with
// the arm's own block — but it would carry the layer's name into
// sources[].category and into every list the category feeds, and a reservation
// that a shift key walks around is a convention rather than a mechanism. The
// direction of the error is the safe one: it can only refuse more, never less.
func IsReservedCategory(category string) bool {
	c := strings.ToLower(strings.TrimSpace(category))
	for _, r := range ReservedCategories {
		if c == r {
			return true
		}
	}
	return false
}
