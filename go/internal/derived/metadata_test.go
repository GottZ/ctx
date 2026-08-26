package derived

import (
	"reflect"
	"testing"
)

// TestGate1_StripReservedRemovesGuardCheckedAt is gate 1 of §7 W01-1.
//
// guard_checked_at is the pending mark of the duplicate guard
// (guard/guard.go:67): a block that carries it is permanently out of the guard
// queue. A derived writer that passes a caller's metadata through unchanged
// hands that lever to whoever produced the map.
//
// Red probe: replace the filter in StripReserved with a no-op — the reserved
// key survives and this test fails.
func TestGate1_StripReservedRemovesGuardCheckedAt(t *testing.T) {
	in := map[string]any{
		"guard_checked_at": "2026-08-26T18:00:00Z",
		"guard_status":     "duplicate",
		"is_meta":          true,
		"topic":            "retrieval",
	}
	out := StripReserved(in)

	for _, k := range []string{"guard_checked_at", "guard_status", "is_meta"} {
		if _, ok := out[k]; ok {
			t.Errorf("StripReserved kept reserved key %q", k)
		}
	}
	if out["topic"] != "retrieval" {
		t.Errorf("StripReserved dropped a legitimate key: got %v", out["topic"])
	}
	if _, ok := in["guard_checked_at"]; !ok {
		t.Error("StripReserved mutated the caller's map; it must return a copy")
	}
}

// TestReservedMetadataKeysGolden pins the list byte for byte against §3.2.
// A key added or renamed here is a security decision, so it has to be a
// visible diff in this test and not a quiet edit in a var block.
func TestReservedMetadataKeysGolden(t *testing.T) {
	want := []string{
		"guard_checked_at",
		"guard_status",
		"guard_similarity",
		"guard_matched_id",
		"guard_is_cross_scope",
		"guard_is_temporal",
		"guard_threshold_duplicate",
		"guard_threshold_review",
		"guard_repair",
		"guard_resolution",
		"guard_resolved_at",
		"is_meta",
		"sensitivity_audit",
		"sensitivity_detector",
	}
	if len(want) != 14 {
		t.Fatalf("the golden list itself is wrong: %d keys, §3.2 names 14", len(want))
	}
	if !reflect.DeepEqual(ReservedMetadataKeys, want) {
		t.Errorf("ReservedMetadataKeys drifted from §3.2\n got: %q\nwant: %q", ReservedMetadataKeys, want)
	}
}

// TestStripReservedNilStaysNil — "no metadata" must not become "{}" on the
// wire.
func TestStripReservedNilStaysNil(t *testing.T) {
	if got := StripReserved(nil); got != nil {
		t.Errorf("StripReserved(nil) = %v, want nil", got)
	}
}

// TestStripReservedRemovesEveryReservedKey walks the whole list, so a key that
// is in the golden list but not in the lookup set cannot pass unnoticed.
func TestStripReservedRemovesEveryReservedKey(t *testing.T) {
	in := map[string]any{"keep": 1}
	for _, k := range ReservedMetadataKeys {
		in[k] = "x"
	}
	out := StripReserved(in)
	if len(out) != 1 {
		t.Fatalf("StripReserved left %d keys, want 1 (only \"keep\")", len(out))
	}
	if out["keep"] != 1 {
		t.Errorf("StripReserved dropped the wrong key: %v", out)
	}
}
