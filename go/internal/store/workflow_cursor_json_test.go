package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Issue #237 point 2 (the class of #205): the board cursor of the
// updated-ordered path carries no created_at, and `omitempty` never drops a
// struct value, so the key went out as "0001-01-01T00:00:00Z" inside the
// opaque after-token and in the cursor object of `manage issue-list`. The
// wanted wire is the real time on the created-ordered path and no key on the
// board path; a token minted before the change still decodes to the same
// cursor.
func TestWorkflowCursorCreatedAtWire(t *testing.T) {
	updated := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	board, err := json.Marshal(WorkflowCursor{Status: "open", UpdatedAt: updated, ID: "x"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(board), `"created_at"`) {
		t.Errorf("unset created_at is on the wire: %s", board)
	}
	if want := `{"status":"open","updated_at":"2026-03-04T05:06:07Z","id":"x"}`; string(board) != want {
		t.Errorf("board cursor wire = %s, want %s", board, want)
	}

	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	byCreated, err := json.Marshal(WorkflowCursor{CreatedAt: created, ID: "y"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(byCreated), `"created_at":"2026-01-02T03:04:05Z"`) {
		t.Errorf("set created_at is not on the wire: %s", byCreated)
	}

	// A token minted before the change carries the zero time; it decodes to
	// the cursor the new wire decodes to.
	var old, cur WorkflowCursor
	if err := json.Unmarshal([]byte(`{"status":"open","updated_at":"2026-03-04T05:06:07Z","id":"x","created_at":"0001-01-01T00:00:00Z"}`), &old); err != nil {
		t.Fatalf("unmarshal old: %v", err)
	}
	if err := json.Unmarshal(board, &cur); err != nil {
		t.Fatalf("unmarshal new: %v", err)
	}
	if old.Status != cur.Status || old.ID != cur.ID || !old.UpdatedAt.Equal(cur.UpdatedAt) ||
		!old.CreatedAt.IsZero() || !cur.CreatedAt.IsZero() {
		t.Errorf("old token decodes to %+v, new to %+v", old, cur)
	}
}
