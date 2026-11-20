package store

import (
	"encoding/json"
	"strings"
	"testing"
)

// Issue #406: a staged update carries its metadata_merge as a merge. The key is
// absent from every canonical form written without one, so each payload_hash a
// client already holds stays confirmable; with a merge, the merge is
// hash-bound, and a null (the removal of a key) survives the JSONB round trip.
func TestCanonicalMetadataMerge(t *testing.T) {
	update := CanonicalWrite{
		Op:            "update",
		ID:            "0198cafe-0000-7000-8000-00000000beef",
		Scope:         "private",
		Content:       "body",
		UpdateFields:  []string{"content"},
		BaseUpdatedAt: "2026-11-20T10:00:00.123456Z",
	}
	_, plain, err := update.PayloadHash()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "metadata_merge") {
		t.Fatalf("an update without a merge canonicalizes with the key present: %s", plain)
	}

	merged := update
	merged.UpdateFields = []string{"content", "metadata_merge"}
	merged.MetadataMerge = map[string]any{"set": "v", "drop": nil}
	mergeHash, canonical, err := merged.PayloadHash()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), `"metadata_merge":{"drop":null,"set":"v"}`) {
		t.Fatalf("the merge is not canonicalized with sorted keys and its null: %s", canonical)
	}
	other := merged
	other.MetadataMerge = map[string]any{"set": "v"}
	otherHash, _, _ := other.PayloadHash()
	if otherHash == mergeHash {
		t.Fatal("the removal of a key is not hash-bound — a confirm could keep a key the card said it removes")
	}

	var back CanonicalWrite
	if err := json.Unmarshal(canonical, &back); err != nil {
		t.Fatal(err)
	}
	v, present := back.MetadataMerge["drop"]
	if !present || v != nil {
		t.Fatalf("round trip lost the removal: %#v", back.MetadataMerge)
	}
}
