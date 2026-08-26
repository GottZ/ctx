// Package redact owns the literal marker strings that the ctx pipeline writes
// INTO text it later reads back — the cross-scope redaction replacement and the
// prompt truncation suffix — so that every writer and every reader of a marker
// names the SAME constant.
//
// Why a package and not a literal per call site (design/05 §4.4, review F-12):
// the markers cross a trust boundary in both directions. store writes
// '[redacted]' into a guard listing, the prompt builders write "[... truncated]"
// into model input, and internal/derived reads both back as a NEGATIVE list —
// a quote that consists of pipeline marks proves nothing about its source, so
// the citation gate rejects it. As unsupervised string copies that negative
// list is fail-OPEN at a write-time gate: a new marker introduced by some other
// writer is simply not on the list, and every quote carrying it passes.
//
// The register closes that by construction plus one test: no marker literal may
// exist anywhere in go/ outside this package (registry_test.go walks the tree
// and goes red on any other occurrence), so a new marker cannot be introduced
// without being added here — and adding it here adds it to Markers.
//
// This package is a LEAF on purpose: it imports nothing, not even from the
// standard library, so every layer (store, llm, promptguard, derived) can
// depend on it without any import-cycle risk.
package redact

const (
	// Redacted is the exact token that REPLACES a value the caller may not
	// see, rather than deleting it — store's cross-scope guard listing
	// (blocks.go, matched_title) and the audit-trigger redaction in
	// migrations/113_baseline.sql both emit these bytes verbatim. Because it
	// replaces, it is a genuine substring of the text a later reader sees.
	Redacted = "[redacted]"

	// RedactedPrefix is the OPEN form of Redacted, and it is what a reader
	// matches on: it covers "[redacted]", the upper-case "[REDACTED]" written
	// by the ctx_checkpoint plugin after case folding, and any future
	// "[redacted: <reason>]" a writer may add. A reader that matched the
	// closed form would miss all but the first.
	RedactedPrefix = "[redacted"

	// Truncated is the suffix that keeps a shortening VISIBLE to the model —
	// it must know it is reading an excerpt, not a whole block. Emitted by
	// promptguard.Assemble, the synthesis prompt builder and the classify
	// prompt builder, all with these exact bytes.
	Truncated = "[... truncated]"
)

// Markers is the negative list for readers that must decide whether a piece of
// text is evidence or merely pipeline residue (internal/derived G4).
//
// The entries are in NORMALISED form: lower case, single ASCII spaces, NFKC
// stable. Readers fold the candidate text the same way before comparing, so an
// upper-case or full-width spelling of a marker cannot slip past. Entries are
// PREFIXES, not whole tokens — see RedactedPrefix.
//
// Read-only. It is a slice rather than an array so that consumers can range
// over it without copying; nothing may write to it.
var Markers = []string{RedactedPrefix, Truncated}
