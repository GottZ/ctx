package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Issue #205: a BlockPreview whose statement did not read created_at must not
// answer the zero time as a date. `omitempty` never drops a struct value, so
// the key was serialized as "0001-01-01T00:00:00Z"; the wanted wire is the
// real time when it was read and no key when it was not.
func TestBlockPreviewCreatedAtWire(t *testing.T) {
	unset, err := json.Marshal(BlockPreview{ID: "x"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(unset), `"created_at"`) {
		t.Errorf("unset created_at is on the wire: %s", unset)
	}

	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	set, err := json.Marshal(BlockPreview{ID: "x", CreatedAt: at})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(set), `"created_at":"2026-03-04T05:06:07Z"`) {
		t.Errorf("set created_at is not on the wire: %s", set)
	}
}
