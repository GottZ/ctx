//go:build integration

package handler

// Issue #547, review round 1 (F3): an id that cannot name a block is the
// caller's unknown id on every surface that resolves one — never a server
// error with an ERROR line, never a write on a block the caller did not name.
//
// store.ResolveBlockID is the one place that resolves an id or a prefix. An
// update item of a block batch with a NUL in its id answered 500 with an
// ERROR line (the database refuses NUL in text, 22021), where the batch had
// answered 422 unknown_block; and a prefix of LIKE wildcards ("%%%%%%%%")
// matched a block the caller had not named on every surface that resolves
// prefixes — get, update, delete, the MCP tools and the batch.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run 'TestBlockIDOdd' -count=1 -v

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// oddBlockIDs are ids no block carries. An item without an id is a store
// item (its refusal is the missing fields); every other one is an update
// item whose id names no block.
var oddBlockIDs = []struct{ name, id string }{
	{"NUL first", "\x00abcdefgh"},
	{"NUL inside", "abcd\x00efgh"},
	{"NUL in a full-id shape", "01234567-89ab-cdef-0123-45678\x00abcdef"},
	{"invalid UTF-8", "\xff\xfeabcdefgh"},
	{"too long", "0123456789abcdef0123456789abcdef0123456789abcdef"},
	{"too short", "0123"},
	{"empty", ""},
	{"whitespace", "         "},
	{"LIKE percent", "%%%%%%%%"},
	{"LIKE underscore", "________"},
	{"hex then percent", "0%%%%%%%"},
}

// oddErrorLines counts the records at ERROR or above.
func oddErrorLines(c *rejectLogCapture) int {
	n := 0
	for _, r := range c.all() {
		if r.level >= slog.LevelError {
			n++
		}
	}
	return n
}

// TestBlockIDOddUpdateItemIsUnknown: every odd id in an update item of a
// block batch is the item's refusal — 422 unknown_block, a prefix too short
// included; an item without an id is a store item missing its fields — with
// no ERROR line and nothing written.
func TestBlockIDOddUpdateItemIsUnknown(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	k := f.key(t, false)
	a := f.seed(t, "odd-a", "content a")
	b := f.seed(t, "odd-b", "content b")
	for _, c := range oddBlockIDs {
		t.Run(c.name, func(t *testing.T) {
			before := f.counts(t, k)
			logs := installRejectLogCapture(t)
			wantStatus, wantCode, wantText := http.StatusUnprocessableEntity, "unknown_block", "Block not found"
			switch c.id {
			case "":
				wantStatus, wantCode, wantText = http.StatusBadRequest, "missing_fields", "Missing required fields"
			case "0123":
				wantText = "prefix must be at least"
			}
			st, m := f.restNoFatal(k, sbItems(sbuUpdate(c.id, "content", "written through an odd id")))
			if st != wantStatus || m["code"] != wantCode {
				t.Errorf("REST batch: %d %v, want %d %s", st, m, wantStatus, wantCode)
			}
			if strings.IndexByte(c.id, 0) >= 0 {
				// A NUL in a tool argument is refused before the tool runs,
				// naming the field (issue #519); the store's own answer for
				// such an id is what the REST call above reads.
				wantText = "id " + nulFieldMessage
			}
			ans, err := f.mcpCallNoFatal(k, "store_batch", map[string]any{"items": []any{map[string]any{"id": c.id, "content": "written through an odd id"}}})
			if err != nil || !ans.IsError || !strings.Contains(ans.Text, wantText) {
				t.Errorf("MCP store_batch: %+v %v, want an error saying %q", ans, err, wantText)
			}
			if n := oddErrorLines(logs); n != 0 {
				t.Errorf("%d ERROR lines for a refused id", n)
			}
			if after := f.counts(t, k); after != before {
				t.Errorf("counts %+v → %+v: a refused batch wrote", before, after)
			}
			if f.content(t, a) != "content a" || f.content(t, b) != "content b" {
				t.Errorf("a block the caller did not name was written")
			}
		})
	}
}

// TestBlockIDOddPrefixNamesNoBlock: a prefix of LIKE wildcards (or a NUL)
// names no block on the surfaces that resolve prefixes: the MCP get, update
// and archive tools, manage get, update and delete (in this order: on the
// base the delete archived the block).
func TestBlockIDOddPrefixNamesNoBlock(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	k := f.key(t, false)
	ar := AuthResultFromContext(k.ctx)
	h := NewManageHandler(f.pool, nil, nil, backends.NewPool(nil, nil), nil, nil, nil, nil)
	only := f.seed(t, "odd-only", "the only block of the key")
	archived := func() bool {
		var gone bool
		if err := f.pool.QueryRow(context.Background(), `SELECT is_archived FROM context_blocks WHERE id = $1`, only).Scan(&gone); err != nil {
			t.Fatalf("read block: %v", err)
		}
		return gone
	}
	for _, id := range []string{"%%%%%%%%", "________", "%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%", "\x00\x00\x00\x00\x00\x00\x00\x00"} {
		logs := installRejectLogCapture(t)
		for _, tool := range []struct {
			name string
			args map[string]any
		}{
			{"get", map[string]any{"id": id}},
			{"update", map[string]any{"id": id, "content": "written through a wildcard"}},
			{"archive", map[string]any{"id": id}},
		} {
			ans, err := f.mcpCallNoFatal(k, tool.name, tool.args)
			if err != nil || !ans.IsError || strings.Contains(ans.Text, only) {
				t.Errorf("MCP %s with %q: %+v %v, want an error naming no block", tool.name, id, ans, err)
			}
		}
		for _, action := range []map[string]any{
			{"action": "get", "id": id},
			{"action": "update", "id": id, "data": map[string]any{"content": "written through a wildcard"}},
			{"action": "delete", "id": id},
		} {
			st, resp := kaManage(t, h, ar, "odd-prefix", action)
			msg, _ := resp["error"].(string)
			if resp["success"] == true || resp["block"] != nil || (st == http.StatusOK && msg != "Block not found") {
				t.Errorf("manage %v with %q: %d %v, want no block", action["action"], id, st, resp)
			}
		}
		if n := oddErrorLines(logs); n != 0 {
			t.Errorf("%q: %d ERROR lines", id, n)
		}
	}
	if f.content(t, only) != "the only block of the key" || archived() {
		t.Fatalf("the only block of the key was written or archived through an id that does not name it")
	}
}
