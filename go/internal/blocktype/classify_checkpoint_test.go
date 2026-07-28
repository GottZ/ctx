package blocktype

import "testing"

// Session-head checkpoint blocks carry the writer title "Compaction
// checkpoint head <session>" — a SECOND stable prefix next to the
// "Compaction source …" parts pinned by M107. Without it in the checkpoint
// classify rules the heads fall through to the default type and enter every
// autonomous pipeline the checkpoint type exists to keep them out of (the
// M107 rationale applies to heads verbatim: they are the anchor of the ID
// chain the guard archive lane broke).
func TestClassify_CheckpointHead(t *testing.T) {
	s := builtinTestSet(t)

	for _, title := range []string{
		"Compaction checkpoint head 20260728_xyz",
		"compaction checkpoint head 20260728_xyz", // MatchesAny is case-insensitive
		"Compaction source part 3/7",              // M107 pattern stays live
	} {
		got, matched := s.Classify(title, nil)
		if !matched || got != "checkpoint" {
			t.Errorf("Classify(%q, nil) = (%q, %v), want (checkpoint, true)", title, got, matched)
		}
	}
}

// The ten audit-trail patterns run at priority 20, ahead of checkpoint's 30 —
// an audit substring inside a head title would silently win the first-match
// race and re-open the pipeline the type closes. None of them matches the
// head shape (live corpus: 0 of 85 heads carry an audit substring); this
// pins that separation against a future pattern addition on either side.
func TestClassify_NoAuditCollision(t *testing.T) {
	s := builtinTestSet(t)

	const title = "Compaction checkpoint head 20260728_xyz"
	got, matched := s.Classify(title, nil)
	if matched && got == "audit-trail" {
		t.Errorf("Classify(%q, nil) = audit-trail — an audit pattern outranks checkpoint (priority 20 < 30)", title)
	}
}
