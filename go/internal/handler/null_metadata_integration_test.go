//go:build integration

// A block whose metadata is SQL NULL carries no provenance key, so the S3
// derivative exclusion of the by-id client writes must not apply to it. Before
// the fix `NOT (metadata ? 'provenance')` evaluated to NULL for such a row: the
// statement matched nothing, a direct archive or update answered "not found"
// for a block the key can read and write, and a staged archive or update was
// carded (its stage read sees the row) and then failed at confirm with the
// token spent.
//
// Fixture: no production write path stores NULL — UpsertBlock and the issue
// and chunk inserts all bind a non-nil map — but the column is nullable
// (DEFAULT '{}'), so rows from an older writer, a restore or a manual SQL fix
// can carry it. The block is therefore written through the MCP store tool and
// its metadata set to NULL with one direct UPDATE, the only way to reach the
// state.
//
// Run with:
//
//	go test -tags=integration -p 1 ./internal/handler/ -run TestNullMetadataIsNotDerived -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNullMetadataIsNotDerived(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI201Fixture(t)
	ctx := context.Background()

	nullBlock := func(t *testing.T, title string) string {
		t.Helper()
		id := f.store(t, f.direct, title)
		if _, err := f.pool.Exec(ctx, `UPDATE context_blocks SET metadata = NULL WHERE id = $1`, id); err != nil {
			t.Fatalf("null the metadata of %s: %v", title, err)
		}
		return id
	}
	h := NewManageHandler(f.pool, nil, nil, nil, nil, nil, nil, f.cfg.Blocktypes)
	manage := func(t *testing.T, body map[string]any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		h.HandleManage(rec, httptest.NewRequestWithContext(f.direct, http.MethodPost, "/api/manage", bytes.NewReader(raw)))
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode manage %v: %v (%s)", body["action"], err, rec.Body.String())
		}
		return resp
	}
	content := func(t *testing.T, id string) string {
		t.Helper()
		var c string
		if err := f.pool.QueryRow(ctx, `SELECT content FROM context_blocks WHERE id = $1`, id).Scan(&c); err != nil {
			t.Fatalf("read content: %v", err)
		}
		return c
	}
	confirmStaged := func(t *testing.T, a i201Answer) i201Answer {
		t.Helper()
		m := i201Hash.FindStringSubmatch(a.Text)
		if m == nil {
			t.Fatalf("no staged card: %+v", a)
		}
		return f.call(t, f.flagged, "confirm", map[string]any{"payload_hash": m[1]})
	}

	t.Run("MCP archive", func(t *testing.T) {
		id := nullBlock(t, "i201-null-archive")
		if a := f.call(t, f.direct, "archive", map[string]any{"id": id}); a.IsError || !strings.HasPrefix(a.Text, "Archived: ") {
			t.Errorf("archive = %+v", a)
		}
		if !f.archived(t, id) {
			t.Errorf("the block is live")
		}
	})

	t.Run("staged MCP archive and confirm", func(t *testing.T) {
		id := nullBlock(t, "i201-null-staged-archive")
		if c := confirmStaged(t, f.call(t, f.flagged, "archive", map[string]any{"id": id})); c.IsError || !strings.HasPrefix(c.Text, "Confirmed and archived: ") {
			t.Errorf("confirm = %+v", c)
		}
		if !f.archived(t, id) {
			t.Errorf("the block is live")
		}
	})

	t.Run("REST manage delete", func(t *testing.T) {
		id := nullBlock(t, "i201-null-delete")
		if resp := manage(t, map[string]any{"action": "delete", "id": id}); resp["success"] != true {
			t.Errorf("manage delete = %v", resp)
		}
		if !f.archived(t, id) {
			t.Errorf("the block is live")
		}
	})

	t.Run("REST guard-resolve archive", func(t *testing.T) {
		id := nullBlock(t, "i201-null-guard")
		if resp := manage(t, map[string]any{"action": "guard-resolve", "id": id, "data": map[string]any{"resolution": "archive"}}); resp["success"] != true {
			t.Errorf("guard-resolve archive = %v", resp)
		}
		if !f.archived(t, id) {
			t.Errorf("the block is live")
		}
	})

	t.Run("MCP update", func(t *testing.T) {
		id := nullBlock(t, "i201-null-update")
		if a := f.call(t, f.direct, "update", map[string]any{"id": id, "content": "updated"}); a.IsError || !strings.HasPrefix(a.Text, "Updated: ") {
			t.Errorf("update = %+v", a)
		}
		if got := content(t, id); got != "updated" {
			t.Errorf("content = %q, want updated", got)
		}
	})

	t.Run("staged MCP update and confirm", func(t *testing.T) {
		id := nullBlock(t, "i201-null-staged-update")
		if c := confirmStaged(t, f.call(t, f.flagged, "update", map[string]any{"id": id, "content": "confirmed"})); c.IsError || !strings.HasPrefix(c.Text, "Confirmed and updated: ") {
			t.Errorf("confirm = %+v", c)
		}
		if got := content(t, id); got != "confirmed" {
			t.Errorf("content = %q, want confirmed", got)
		}
	})
}
