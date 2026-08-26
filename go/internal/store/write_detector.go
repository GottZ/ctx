package store

import (
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/sensitivity"
)

// ApplyWriteDetector runs the G40 credentials scanner over content and folds a
// hit into the write intent. It is the ONE implementation of that verdict:
// UpsertBlock calls it on every upsert path, and the handler's
// applyWriteDetector delegates here (only the request-scoped logging stays in
// the handler, which is the sole caller that owns a request id).
//
// On a hit the write becomes credentials with detector provenance
// (sensitivity_source='pattern' in the upsert) and metadata carries the
// secret-free reason — never the matched secret. The verdict shape
// ({"kind","reason"}) is identical to the nachträgliche SQL sweep's
// jsonb_build_object (sensitivity.go:269-270), so a block raised at write time
// and a block raised by the sweep are indistinguishable afterwards.
//
// A hit only ever RAISES: it overrides a too-low manual or default
// classification and leaves an already-credentials block intact (the strict '>'
// on the ON CONFLICT branch, blocks.go:276-292, keeps an existing manual row's
// source). No hit ⇒ inputs returned unchanged, byte for byte.
//
// The returned *sensitivity.Match is nil exactly when there was no hit; it
// exists so a caller can log the kind without scanning a second time.
//
// Wissens-Ebenen V-W8 (design/05 §7 row V-W8, §5 B3): before this the detector
// lived in the handler only, so the four in-process writers (digest.go:146,
// :267, rootmap/run.go:190, dream/synthesize_report.go:334) and the ingest
// block path (handler/ingest.go:223) wrote credentials-bearing content with no
// verdict at all — §5 B3's "Secrets wandern über das Zitat in eine schwächer
// geschützte Klasse" is exactly that gap, and the derived writers of Phase A/B
// go through UpsertBlock too.
func ApplyWriteDetector(content string, sens SensitivityWrite, metadata map[string]any) (SensitivityWrite, map[string]any, *sensitivity.Match) {
	m, hit := sensitivity.Scan(content)
	if !hit {
		return sens, metadata, nil
	}
	// Upgrade-only. The empty value means "no explicit intent" — every
	// in-process writer passes SensitivityWrite{} — and it must be MADE
	// explicit here: backends.sensRank("") is 3, credentials-equivalent
	// (trust.go:38, fail-closed), so a pure rank comparison would leave the
	// value empty, UpsertBlock would then write no sensitivity column at all
	// (blocks.go:265) and the row would take the DDL defaults
	// 'credentials'/'default' — the level right by accident, the PROVENANCE
	// lost. For every non-empty valid level this branch is identical to the
	// rank guard the handler has run since G40.
	if sens.Value == "" || sens.Value.Rank() < backends.SensCredentials.Rank() {
		sens.Value = backends.SensCredentials
	}
	sens.Manual = false
	sens.Detector = true
	if metadata == nil {
		metadata = map[string]any{}
	}
	// In-place on a caller-supplied map, exactly as the handler has always
	// done: the trace belongs to the metadata this write persists, and every
	// caller passes the map it is about to write.
	metadata["sensitivity_detector"] = map[string]any{"kind": m.Kind, "reason": m.Reason}
	return sens, metadata, &m
}
