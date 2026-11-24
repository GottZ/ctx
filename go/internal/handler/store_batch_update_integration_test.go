//go:build integration

// Issue #211, second half: update items (by id, the update tool's form) and a
// parent (parent_id, parent_item) in a batch. The probes run over the real
// surfaces — REST POST /api/store/batch through its handler, the MCP
// store_batch tool by name over the real transport, and the staged form
// through the confirm tool — and check every refusal with what it must leave
// behind: the block rows, the write log (the write budget) and the open stage
// rows as they were, and the item written ahead of the refused one rolled
// back.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run 'TestStoreBatchUpdate|TestStoreBatchParent|TestStoreParent' -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/store"
)

// sbuRow is what an update item writes and the tests compare.
type sbuRow struct {
	title, content, tags, metadata, scope string
}

func (f *sbFixture) row(t *testing.T, id string) sbuRow {
	t.Helper()
	var r sbuRow
	if err := f.pool.QueryRow(context.Background(),
		`SELECT title, content, array_to_string(tags, ','), COALESCE(metadata, '{}'::jsonb)::text, scope
		   FROM context_blocks WHERE id = $1`, id).Scan(&r.title, &r.content, &r.tags, &r.metadata, &r.scope); err != nil {
		t.Fatalf("row %s: %v", id, err)
	}
	return r
}

// seedIn writes a block into scope through the production upsert.
func (f *sbFixture) seedIn(t *testing.T, scope, title, content string) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), f.pool, "learnings", title, content, nil, nil, scope, true,
		store.SensitivityWrite{Value: backends.SensInternal}, "")
	if err != nil {
		t.Fatalf("seed %s/%s: %v", scope, title, err)
	}
	return b.ID
}

func (f *sbFixture) parentOf(t *testing.T, id string) string {
	t.Helper()
	var p string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COALESCE(parent_id::text, '') FROM context_blocks WHERE id = $1`, id).Scan(&p); err != nil {
		t.Fatalf("parent of %s: %v", id, err)
	}
	return p
}

func (f *sbFixture) idOf(t *testing.T, title string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id::text FROM context_blocks WHERE title = $1 AND NOT is_archived`, title).Scan(&id); err != nil {
		t.Fatalf("id of %s: %v", title, err)
	}
	return id
}

// restStoreNoFatal posts one /api/store body (it runs in a goroutine).
func (f *sbFixture) restStoreNoFatal(k sbKey, body any) (int, map[string]any) {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/store", bytes.NewReader(raw)).WithContext(k.ctx)
	rec := httptest.NewRecorder()
	NewStoreHandler(f.pool, f.cfg, f.reg).HandleStore(rec, req)
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		return rec.Code, map[string]any{"decode": err.Error(), "raw": rec.Body.String()}
	}
	return rec.Code, m
}

func sbuUpdate(id string, kv ...any) map[string]any {
	m := map[string]any{"id": id}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

// TestStoreBatchUpdateWrites: a batch that mixes store and update items writes
// all of them in one answer on both surfaces — a whole new content, edits with
// a metadata merge, tags — leaves an update item that changes nothing as it
// is, and books one write per written item.
func TestStoreBatchUpdateWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	for _, surface := range []string{"REST", "MCP"} {
		t.Run(surface, func(t *testing.T) {
			k := f.key(t, false)
			p := strings.ToLower(surface)
			a := f.seed(t, "sbu-w-a-"+p, "alpha body")
			b := f.seed(t, "sbu-w-b-"+p, "beta body, beta tail")
			same := f.seed(t, "sbu-w-same-"+p, "same body")
			before := f.counts(t, k)
			body := sbItems(
				sbItem("sbu-w-new-"+p, "new body"),
				sbuUpdate(a, "content", "alpha rewritten", "tags", []string{"x", "y"}),
				sbuUpdate(b, "edits", []map[string]any{{"old": "beta tail", "new": "BETA TAIL"}},
					"metadata_merge", map[string]any{"source": "sbu"}),
				sbuUpdate(same, "content", "same body"),
			)
			if surface == "REST" {
				status, resp := f.rest(t, k, body)
				if status != http.StatusOK || resp["success"] != true {
					t.Fatalf("status %d body %v", status, resp)
				}
				items, _ := resp["items"].([]any)
				if len(items) != 4 {
					t.Fatalf("items %v", resp["items"])
				}
				upd, _ := items[1].(map[string]any)
				if blk, _ := upd["block"].(map[string]any); upd["action"] != "update" || blk["content"] != "alpha rewritten" {
					t.Errorf("items[1] = %v; want the manage-update answer of the written block", upd)
				}
				noop, _ := items[3].(map[string]any)
				if noop["action"] != "noop" || noop["existing_id"] != same {
					t.Errorf("items[3] = %v; want the noop answer naming %s", noop, same)
				}
			} else {
				ans := f.mcpCall(t, k, "store_batch", body)
				if ans.IsError {
					t.Fatalf("store_batch: %+v", ans)
				}
				lines := strings.Split(strings.TrimSpace(ans.Text), "\n")
				if len(lines) != 5 || lines[0] != "Batch written in one transaction: 1 stored, 2 updated, 1 unchanged." ||
					!strings.HasPrefix(lines[2], "items[1]: Updated: sbu-w-a-mcp (id: "+a) ||
					!strings.HasPrefix(lines[4], "items[3]: No change needed") {
					t.Fatalf("answer:\n%s", ans.Text)
				}
			}
			if r := f.row(t, a); r.content != "alpha rewritten" || r.tags != "x,y" {
				t.Errorf("a = %+v", r)
			}
			if r := f.row(t, b); r.content != "beta body, BETA TAIL" || !strings.Contains(r.metadata, `"source": "sbu"`) {
				t.Errorf("b = %+v", r)
			}
			after := f.counts(t, k)
			if after.blocks != before.blocks+1 || after.budget != 3 || after.writes != before.writes+3 {
				t.Fatalf("before %+v after %+v; want one block more and three writes booked (the unchanged item costs nothing)",
					before, after)
			}
		})
	}
}

// TestStoreBatchUpdateRefusalsWriteNothing is the all-or-none contract for
// update items: each refusal names the item with the single update's text and
// a code, and leaves nothing written, logged or spent — items[0], an update of
// `existing` that the batch writes first, is rolled back with it.
func TestStoreBatchUpdateRefusalsWriteNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	ctx := context.Background()

	existing := f.seed(t, "sbu-0-existing", "original body")
	target := f.seed(t, "sbu-target", "target body")
	f.seed(t, "sbu-taken", "taken body")
	foreign := f.seedIn(t, "sbu-foreign", "sbu-foreign-block", "foreign body")
	if _, err := store.UpsertBlock(ctx, f.pool, "learnings", "sbu-lowered", "lowered body", nil, nil, "private", false,
		store.SensitivityWrite{Value: backends.SensPersonal, Manual: true}, ""); err != nil {
		t.Fatal(err)
	}
	lowered := f.idOf(t, "sbu-lowered")
	md := map[string]any{derived.MetadataKey: map[string]any{
		"v": derived.ContractVersion, "stratum": int(derived.StratumDerived), "arm": "i211",
	}}
	if _, err := store.UpsertBlock(ctx, f.pool, "learnings", "sbu-derivative", "derived body", nil, md,
		"private", true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true}, derived.TypeCatalog); err != nil {
		t.Fatalf("server-path derivative: %v", err)
	}
	derivative := f.idOf(t, "sbu-derivative")
	first := sbuUpdate(existing, "content", "rewritten body")

	cases := []struct {
		name     string
		body     map[string]any
		code     string
		text     string
		restOnly bool
		mcpOnly  bool
	}{
		{"unknown id", sbItems(first, sbuUpdate(sbUnknownID, "content", "x")),
			"unknown_block", "items[1]: Block not found (or not in a writable scope of this key)", false, false},
		{"block outside the writable scopes", sbItems(first, sbuUpdate(foreign, "content", "x")),
			"unknown_block", "items[1]: Block not found (or not in a writable scope of this key)", false, false},
		{"derivative in the transaction", sbItems(first, sbuUpdate(derivative, "content", "x")),
			"provenance_protected", "items[1]: block carries derived provenance", false, false},
		{"edit that does not match in the transaction", sbItems(first, sbuUpdate(target, "edits", []map[string]any{{"old": "nope", "new": "x"}})),
			"edit_mismatch", "items[1]: edits[0]: old matches 0 times", false, false},
		{"identity taken in the transaction", sbItems(first, sbuUpdate(target, "title", "sbu-taken")),
			"constraint", "items[1]: another active block in this scope already has this category and title", false, false},
		{"one block twice", sbItems(first, sbuUpdate(target, "content", "x"), sbuUpdate(target, "tags", []string{"t"})),
			"constraint", "items[2]: same block as items[1] — a batch writes each block once", false, false},
		{"a store item and an update item on one block", sbItems(first, sbItem("sbu-target", "y"), sbuUpdate(target, "content", "x")),
			"constraint", "items[2]: same block as items[1] — a batch writes each block once", false, false},
		{"an update onto a store item's identity", sbItems(first, sbItem("sbu-fresh", "y"), sbuUpdate(target, "title", "sbu-fresh")),
			"constraint", "items[2]: same category, title and scope as items[1] — a batch writes each block once", false, false},
		{"reserved metadata in a merge", sbItems(first, sbuUpdate(target, "metadata_merge", md)),
			"reserved_metadata", "items[1]: metadata:", false, false},
		{"unknown type", sbItems(first, sbuUpdate(target, "type", "no-such-type")),
			"unknown_type", "items[1]: type: unknown block type", false, false},
		{"nothing to update", sbItems(first, sbuUpdate(target)),
			"missing_fields", "items[1]: nothing to update", false, false},
		{"content beside edits", sbItems(first, sbuUpdate(target, "content", "x", "edits", []map[string]any{{"old": "target", "new": "x"}})),
			"invalid_body", "items[1]: content and edits are mutually exclusive", false, false},
		{"a store item with edits", sbItems(first, sbItem("sbu-e", "y", "edits", []map[string]any{{"old": "y", "new": "z"}})),
			"invalid_body", "items[1]: edits and metadata_merge belong to an update item", false, false},
		{"a scope move the key may not write", sbItems(first, sbuUpdate(target, "scope", "sbu-foreign")),
			"scope_denied", "items[1]: Cannot set scope to requested value", true, false},
		{"a sensitivity downgrade without its confirm", sbItems(first, sbuUpdate(lowered, "sensitivity", "public")),
			"missing_fields", "items[1]: sensitivity downgrade personal → public", true, false},
		{"a parent on an update item", sbItems(first, sbuUpdate(target, "content", "x", "parent_id", existing)),
			"invalid_body", "items[1]: supersedes, parent_id and parent_item are not part of an update item", true, false},
		{"a scope on an MCP update item", sbItems(first, sbuUpdate(target, "scope", "private")),
			"invalid_body", "items[1]: sensitivity, scope and supersedes are not part of an update item", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := f.key(t, false)
			before := f.counts(t, k)
			var restText any
			if !c.mcpOnly {
				status, resp := f.rest(t, k, c.body)
				restText = resp["error"]
				if status < 400 || resp["code"] != c.code || !strings.HasPrefix(fmt.Sprint(resp["error"]), c.text) {
					t.Errorf("REST: status %d body %v; want code %s and text %q", status, resp, c.code, c.text)
				}
				if after := f.counts(t, k); after != before {
					t.Errorf("REST refusal changed the store: before %+v after %+v", before, after)
				}
			}
			if !c.restOnly {
				a := f.mcpCall(t, k, "store_batch", c.body)
				if !a.IsError || a.Code != c.code || !strings.HasPrefix(a.Text, c.text) || (restText != nil && a.Text != restText) {
					t.Errorf("MCP: %+v; want code %s and text %q (REST %q)", a, c.code, c.text, restText)
				}
				if after := f.counts(t, k); after != before {
					t.Errorf("MCP refusal changed the store: before %+v after %+v", before, after)
				}
			}
			if got := f.content(t, existing); got != "original body" {
				t.Fatalf("items[0] survived the rollback: content %q", got)
			}
			if got := f.content(t, target); got != "target body" {
				t.Fatalf("the target changed: content %q", got)
			}
		})
	}

	t.Run("the budget counts every written update item", func(t *testing.T) {
		k := f.key(t, false)
		for i := 0; i < 3; i++ {
			if a := f.mcpCall(t, k, "store", sbItem(fmt.Sprintf("sbu-budget-single-%d", i), "x")); a.IsError {
				t.Fatalf("single store %d: %+v", i, a)
			}
		}
		ids := []string{f.seed(t, "sbu-bu-1", "a"), f.seed(t, "sbu-bu-2", "b"), f.seed(t, "sbu-bu-3", "c"), f.seed(t, "sbu-bu-4", "d")}
		before := f.counts(t, k)
		four := sbItems(sbuUpdate(ids[0], "content", "a2"), sbuUpdate(ids[1], "content", "b2"),
			sbuUpdate(ids[2], "content", "c2"), sbuUpdate(ids[3], "content", "d2"))
		a := f.mcpCall(t, k, "store_batch", four)
		if !a.IsError || a.Code != "rate_limit" {
			t.Fatalf("3 used + 4 > %d: %+v; want the rate_limit refusal", sbRateLimit, a)
		}
		if after := f.counts(t, k); after != before || f.content(t, ids[0]) != "a" {
			t.Fatalf("budget refusal changed the store: before %+v after %+v", before, after)
		}
		three := sbItems(sbuUpdate(ids[0], "content", "a2"), sbuUpdate(ids[1], "content", "b2"), sbuUpdate(ids[2], "content", "c"))
		if a := f.mcpCall(t, k, "store_batch", three); a.IsError {
			t.Fatalf("3 used + 2 writes: %+v; want it written", a)
		}
		if got := f.counts(t, k); got.budget != 5 {
			t.Fatalf("after 3 used + 2 written update items and one unchanged: budget %d, want 5", got.budget)
		}
	})
}

// TestStoreBatchUpdateStaged: a confirm_writes key stages a mixed batch as ONE
// card, the confirm writes every item in one transaction, and a block that
// changed between stage and confirm — in any update item, not only the first
// — refuses the whole confirm and keeps the token.
func TestStoreBatchUpdateStaged(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	stage := func(t *testing.T, k sbKey, body map[string]any) string {
		t.Helper()
		a := f.mcpCall(t, k, "store_batch", body)
		m := sbHashRe.FindStringSubmatch(a.Text)
		if !a.IsError || a.Code != "" || !strings.HasPrefix(a.Text, "STAGED — NOT saved yet.") || m == nil {
			t.Fatalf("stage: %+v; want an uncoded STAGED answer with a payload_hash", a)
		}
		return m[1]
	}

	t.Run("stage then confirm", func(t *testing.T) {
		k := f.key(t, true)
		a := f.seed(t, "sbu-st-a", "alpha body")
		b := f.seed(t, "sbu-st-b", "beta body")
		hash := stage(t, k, sbItems(sbItem("sbu-st-new", "new"), sbuUpdate(a, "content", "alpha staged"),
			sbuUpdate(b, "edits", []map[string]any{{"old": "beta", "new": "BETA"}})))
		if f.content(t, a) != "alpha body" {
			t.Fatalf("the stage wrote")
		}
		ans := f.mcpCall(t, k, "confirm", map[string]any{"payload_hash": hash})
		if ans.IsError {
			t.Fatalf("confirm: %+v", ans)
		}
		if f.content(t, a) != "alpha staged" || f.content(t, b) != "BETA body" {
			t.Fatalf("after the confirm a = %q, b = %q", f.content(t, a), f.content(t, b))
		}
	})

	for _, drifted := range []int{0, 1} {
		t.Run(fmt.Sprintf("drift in items[%d]", drifted), func(t *testing.T) {
			k := f.key(t, true)
			other := f.key(t, false)
			ids := []string{f.seed(t, fmt.Sprintf("sbu-dr-%d-a", drifted), "a body"), f.seed(t, fmt.Sprintf("sbu-dr-%d-b", drifted), "b body")}
			hash := stage(t, k, sbItems(sbuUpdate(ids[0], "content", "a staged"), sbuUpdate(ids[1], "content", "b staged")))
			if ans := f.mcpCall(t, other, "update", map[string]any{"id": ids[drifted], "tags": []string{"moved"}}); ans.IsError {
				t.Fatalf("drift: %+v", ans)
			}
			before := f.counts(t, k)
			ans := f.mcpCall(t, k, "confirm", map[string]any{"payload_hash": hash})
			want := fmt.Sprintf("items[%d]: confirm rejected: block %s changed since this update was staged", drifted, ids[drifted])
			if !ans.IsError || !strings.HasPrefix(ans.Text, want) {
				t.Fatalf("confirm: %+v; want %q", ans, want)
			}
			if after := f.counts(t, k); after != before || f.content(t, ids[0]) != "a body" || f.content(t, ids[1]) != "b body" {
				t.Fatalf("the refused confirm changed the store or spent the token: before %+v after %+v", before, after)
			}
		})
	}
}

// TestStoreBatchParent: a store item of a REST batch names its parent — an
// existing block (parent_id) or an earlier item of the same batch
// (parent_item) — and the link is written in the batch's transaction. A
// forward or self reference, a parent the item cannot hang under and a parent
// that would close a cycle refuse the batch and write nothing.
func TestStoreBatchParent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)

	t.Run("index and record in one batch", func(t *testing.T) {
		k := f.key(t, false)
		status, resp := f.rest(t, k, sbItems(sbItem("sbp-index", "the index"), sbItem("sbp-record", "the record", "parent_item", 0)))
		if status != http.StatusOK {
			t.Fatalf("status %d body %v", status, resp)
		}
		if got, want := f.parentOf(t, f.idOf(t, "sbp-record")), f.idOf(t, "sbp-index"); got != want {
			t.Fatalf("record's parent %q, want the index %q", got, want)
		}
	})

	t.Run("an existing parent", func(t *testing.T) {
		k := f.key(t, false)
		p := f.seed(t, "sbp-existing-parent", "parent body")
		status, resp := f.rest(t, k, sbItems(sbItem("sbp-child", "child", "parent_id", p)))
		if status != http.StatusOK {
			t.Fatalf("status %d body %v", status, resp)
		}
		if got := f.parentOf(t, f.idOf(t, "sbp-child")); got != p {
			t.Fatalf("child's parent %q, want %q", got, p)
		}
	})

	existing := f.seed(t, "sbp-0-existing", "original body")
	foreign := f.seedIn(t, "sbp-foreign", "sbp-foreign-parent", "foreign body")
	// A cycle: c's parent is p. A batch that hangs p under c closes it.
	p := f.seed(t, "sbp-cycle-p", "p body")
	k0 := f.key(t, false)
	if status, resp := f.rest(t, k0, sbItems(sbItem("sbp-cycle-c", "c body", "parent_id", p))); status != http.StatusOK {
		t.Fatalf("cycle fixture: %d %v", status, resp)
	}
	c := f.idOf(t, "sbp-cycle-c")
	first := sbItem("sbp-0-existing", "rewritten body")

	for _, tc := range []struct {
		name, code, text string
		body             map[string]any
	}{
		{"a forward reference", "invalid_body", "items[1]: parent_item must name an earlier item of this batch",
			sbItems(first, sbItem("sbp-f1", "a", "parent_item", 2), sbItem("sbp-f2", "b"))},
		{"a self reference", "invalid_body", "items[1]: parent_item must name an earlier item of this batch",
			sbItems(first, sbItem("sbp-s1", "a", "parent_item", 1))},
		{"both parent fields", "invalid_body", "items[1]: parent_id and parent_item are mutually exclusive",
			sbItems(first, sbItem("sbp-b1", "a", "parent_item", 0, "parent_id", existing))},
		{"a parent in another scope", "unknown_parent", "items[1]: parent_id: no parent of this write's scope",
			sbItems(first, sbItem("sbp-x1", "a", "parent_id", foreign))},
		{"an unknown parent", "unknown_parent", "items[1]: parent_id: no parent of this write's scope",
			sbItems(first, sbItem("sbp-u1", "a", "parent_id", sbUnknownID))},
		{"a parent that closes a cycle", "unknown_parent", "items[1]: parent_id: no parent of this write's scope",
			sbItems(first, sbItem("sbp-cycle-p", "p rewritten", "parent_id", c))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := f.key(t, false)
			before := f.counts(t, k)
			status, resp := f.rest(t, k, tc.body)
			if status < 400 || resp["code"] != tc.code || !strings.HasPrefix(fmt.Sprint(resp["error"]), tc.text) {
				t.Errorf("status %d body %v; want code %s and text %q", status, resp, tc.code, tc.text)
			}
			if after := f.counts(t, k); after != before {
				t.Errorf("refusal changed the store: before %+v after %+v", before, after)
			}
			if got := f.content(t, existing); got != "original body" {
				t.Fatalf("items[0] survived the rollback: content %q", got)
			}
			if f.parentOf(t, p) != "" {
				t.Fatalf("p got a parent")
			}
		})
	}
}

// TestStoreParentCycleRefused: POST /api/store refused the shortest parent
// cycle (a block as its own parent) and nothing longer: p under c, then c
// under p wrote a two-block cycle. The parent check of the store and the batch
// is one function, and it refuses every cycle with the one unknown_parent
// answer, writing nothing.
func TestStoreParentCycleRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	k := f.key(t, false)
	p := f.seed(t, "sbp-single-p", "p body")
	if status, resp := f.restStoreNoFatal(k, sbItem("sbp-single-c", "c body", "parent_id", p)); status != http.StatusOK {
		t.Fatalf("c under p: %d %v", status, resp)
	}
	c := f.idOf(t, "sbp-single-c")
	before := f.counts(t, k)
	status, resp := f.restStoreNoFatal(k, sbItem("sbp-single-p", "p rewritten", "parent_id", c))
	if status != http.StatusUnprocessableEntity || resp["code"] != "unknown_parent" {
		t.Errorf("p under c: %d %v; want the unknown_parent refusal", status, resp)
	}
	// The single store books its write after the commit, in the background:
	// the block count is what the refused write must leave as it was.
	if f.parentOf(t, p) != "" || f.content(t, p) != "p body" || f.counts(t, k).blocks != before.blocks {
		t.Fatalf("the refused write changed the store: p's parent %q content %q", f.parentOf(t, p), f.content(t, p))
	}
}

// TestStoreParentWrittenWithTheBlock: POST /api/store with parent_id writes the
// block and its parent link in ONE transaction. A parent deleted after the
// pre-check and before the link — here by a transaction that holds the parent
// row while the store runs, and deletes it — leaves nothing written: the block
// does not stand without the parent it named.
func TestStoreParentWrittenWithTheBlock(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	ctx := context.Background()
	k := f.key(t, false)
	p := f.seed(t, "sbp-gone-parent", "parent body")

	gate, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Rollback(ctx) }()
	if _, err := gate.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1 FOR UPDATE`, p); err != nil {
		t.Fatal(err)
	}
	actor := f.actor(t, "sbp-single-store")
	before := f.counts(t, k)
	done := make(chan sbRestResult, 1)
	go func() {
		on := &sbFixture{pool: actor, cfg: f.cfg, reg: f.reg}
		status, resp := on.restStoreNoFatal(k, sbItem("sbp-orphan", "orphan body", "parent_id", p))
		done <- sbRestResult{status, resp}
	}()
	if !f.waitLockOrDone(t, "sbp-single-store", done) {
		t.Fatalf("the store did not wait for the parent row: %v", <-done)
	}
	if _, err := gate.Exec(ctx, `DELETE FROM context_blocks WHERE id = $1`, p); err != nil {
		t.Fatal(err)
	}
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	res := sbAwait(t, "store", done)
	if res.status < 400 || res.body["code"] != "unknown_parent" {
		t.Errorf("store: %d %v; want the unknown_parent refusal", res.status, res.body)
	}
	after := f.counts(t, k)
	if after.blocks != before.blocks-1 {
		t.Fatalf("blocks before %d after %d; want only the deleted parent gone, the orphan not written", before.blocks, after.blocks)
	}
}

// sbuSnapshot is every block of the fixture's database (all columns but ids
// and times, its parent and supersedes head as titles), the temporal rows per
// block and dimension and the block-count deltas: the end state the
// difference probe compares.
func (f *sbFixture) sbuSnapshot(t *testing.T) string {
	t.Helper()
	var doc string
	if err := f.pool.QueryRow(context.Background(), `
		WITH dropped AS (
		  SELECT array_agg(column_name::text) AS cols FROM information_schema.columns
		   WHERE table_name = 'context_blocks' AND table_schema = current_schema()
		     AND (data_type IN ('uuid', 'timestamp with time zone', 'timestamp without time zone') OR column_name = 'content_times'))
		SELECT jsonb_build_object(
		  'blocks', (SELECT jsonb_agg((to_jsonb(b) - d.cols) || jsonb_build_object(
		       'superseded_by', (SELECT x.title FROM context_blocks x WHERE x.id = b.superseded_by),
		       'parent', (SELECT x.title FROM context_blocks x WHERE x.id = b.parent_id))
		     ORDER BY b.title, b.scope) FROM context_blocks b, dropped d),
		  'temporal', (SELECT jsonb_agg(jsonb_build_object('block', b.title, 'dimension', tm.dimension, 'rows', tm.n)
		     ORDER BY b.title, tm.dimension)
		     FROM (SELECT block_id, dimension, count(*) AS n FROM context_temporal GROUP BY 1, 2) tm
		     JOIN context_blocks b ON b.id = tm.block_id),
		  'count_deltas', (SELECT jsonb_agg(to_jsonb(c) ORDER BY c) FROM (
		     SELECT scope, category, is_archived, guard_status, sum(n) AS n FROM context_block_count_deltas
		      GROUP BY 1, 2, 3, 4 HAVING sum(n) <> 0) c))::text`,
	).Scan(&doc); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return doc
}

// TestStoreBatchUpdateMatchesSingleUpdates is the difference probe over the
// MCP surface: one sequence of updates — edits with a merge that names a
// guard key, a new content with tags, a rename with a type and a metadata
// replacement, an update that changes nothing — once as single `update`
// calls, once as one `store_batch`, each in its own database seeded alike.
// The rows, the temporal rows the afterwork writes and the count deltas end
// the same.
func TestStoreBatchUpdateMatchesSingleUpdates(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	type world struct {
		f   *sbFixture
		k   sbKey
		ids map[string]string
	}
	// Seeded through the store tool, afterwork included, so every block holds
	// the temporal rows its write gave it.
	seed := func(t *testing.T) world {
		f := newSBFixture(t)
		k := f.key(t, false)
		ids := map[string]string{}
		for _, title := range []string{"sbd-a", "sbd-b", "sbd-c", "sbd-e"} {
			if a := f.mcpCall(t, k, "store", sbItem(title, title+" body, first line")); a.IsError {
				t.Fatalf("seed %s: %+v", title, a)
			}
			ids[title] = f.idOf(t, title)
		}
		if _, err := f.pool.Exec(context.Background(),
			`UPDATE context_blocks SET metadata = '{"source":"seed","guard_status":"needs_review"}'::jsonb WHERE id = $1`, ids["sbd-a"]); err != nil {
			t.Fatal(err)
		}
		return world{f: f, k: f.key(t, false), ids: ids}
	}
	updates := func(ids map[string]string) []map[string]any {
		return []map[string]any{
			sbuUpdate(ids["sbd-a"], "edits", []map[string]any{{"old": "first line", "new": "edited line"}},
				"metadata_merge", map[string]any{"k": "v", "source": nil, "guard_status": "kept"}),
			sbuUpdate(ids["sbd-b"], "content", "sbd-b new content on 2026-03-04", "tags", []string{"x", "y"}),
			sbuUpdate(ids["sbd-c"], "title", "sbd-c renamed", "type", "reference", "metadata", map[string]any{"source": "replaced"}),
			sbuUpdate(ids["sbd-e"], "content", "sbd-e body, first line"),
		}
	}
	singles := seed(t)
	for _, u := range updates(singles.ids) {
		if a := singles.f.mcpCall(t, singles.k, "update", u); a.IsError {
			t.Fatalf("single update %v: %+v", u, a)
		}
	}
	batched := seed(t)
	if a := batched.f.mcpCall(t, batched.k, "store_batch", sbItems(updates(batched.ids)...)); a.IsError ||
		!strings.HasPrefix(a.Text, "Batch written in one transaction: 0 stored, 3 updated, 1 unchanged.") {
		t.Fatalf("batch: %+v", a)
	}
	if got, want := batched.f.sbuSnapshot(t), singles.f.sbuSnapshot(t); got != want {
		t.Fatalf("the batch left another state than the single updates:\nbatch:   %s\nsingles: %s", got, want)
	}
}

// TestStoreBatchAfterworkFaultIsPerItem: the afterwork of a written batch —
// classify and the temporal rows — runs per block after the commit and is
// non-fatal. A fault in one block's afterwork (here the temporal insert of
// one update item refused by a trigger) leaves the other items and their
// afterwork as they are, rolls nothing back and books nothing twice: the
// batch answers success, every item is written, the budget holds one write
// per written item.
func TestStoreBatchAfterworkFaultIsPerItem(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	ctx := context.Background()
	k := f.key(t, false)
	a := f.seed(t, "sbaw-a", "a body")
	b := f.seed(t, "sbaw-b", "b body")
	if _, err := f.pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION sbaw_fault() RETURNS trigger LANGUAGE plpgsql AS $f$
		BEGIN
		  IF NEW.block_id = '%s'::uuid THEN RAISE EXCEPTION 'injected afterwork fault'; END IF;
		  RETURN NEW;
		END $f$;
		CREATE TRIGGER sbaw_fault BEFORE INSERT ON context_temporal FOR EACH ROW EXECUTE FUNCTION sbaw_fault();`, b)); err != nil {
		t.Fatalf("fault trigger: %v", err)
	}
	before := f.counts(t, k)
	status, resp := f.rest(t, k, sbItems(sbItem("sbaw-new", "new body"),
		sbuUpdate(a, "content", "a rewritten"), sbuUpdate(b, "content", "b rewritten")))
	if status != http.StatusOK || resp["success"] != true {
		t.Fatalf("status %d body %v; want the batch answered as written", status, resp)
	}
	after := f.counts(t, k)
	if f.content(t, a) != "a rewritten" || f.content(t, b) != "b rewritten" || after.blocks != before.blocks+1 ||
		after.budget != 3 || after.writes != before.writes+3 {
		t.Fatalf("before %+v after %+v; want three items written and three writes booked", before, after)
	}
	withRows := map[string]bool{}
	rows, err := f.pool.Query(ctx, `SELECT b.title, count(t.block_id) > 0 FROM context_blocks b
		LEFT JOIN context_temporal t ON t.block_id = b.id WHERE b.title LIKE 'sbaw-%' GROUP BY b.title`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var title string
		var has bool
		if err := rows.Scan(&title, &has); err != nil {
			t.Fatal(err)
		}
		withRows[title] = has
	}
	rows.Close()
	if !withRows["sbaw-new"] || !withRows["sbaw-a"] || withRows["sbaw-b"] {
		t.Fatalf("temporal rows %v; want the faulted item without, the other two with theirs", withRows)
	}
}

// mcpCallNoFatal is mcpCall without t (it runs in a goroutine).
func (f *sbFixture) mcpCallNoFatal(k sbKey, tool string, args map[string]any) (sbAnswer, error) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	req := httptest.NewRequestWithContext(k.ctx, http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	f.mcp.ServeHTTP(rec, req)
	var resp struct {
		Result *struct {
			IsError bool                    `json:"isError"`
			Content []struct{ Text string } `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return sbAnswer{}, fmt.Errorf("decode %q: %w", rec.Body.String(), err)
	}
	if resp.Result == nil {
		return sbAnswer{}, fmt.Errorf("no result in %q", rec.Body.String())
	}
	a := sbAnswer{IsError: resp.Result.IsError}
	for _, c := range resp.Result.Content {
		a.Text += c.Text
	}
	return a, nil
}

// TestStoreBatchUpdateReviewCases are the cases the review of #211 ran: a
// chain or a swap of renames is refused whatever the order of the titles, a
// declaration whose target an update item moves out of the scope is refused,
// a NUL character in an update item is the caller's 400, a staged batch whose
// items refuse each other gets no card, and a parent named in capitals is the
// same block.
func TestStoreBatchUpdateReviewCases(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	refused := func(t *testing.T, k sbKey, body map[string]any, code, text string) {
		t.Helper()
		before := f.counts(t, k)
		status, resp := f.rest(t, k, body)
		if status < 400 || resp["code"] != code || !strings.HasPrefix(fmt.Sprint(resp["error"]), text) {
			t.Errorf("status %d body %v; want code %s and text %q", status, resp, code, text)
		}
		if after := f.counts(t, k); after != before {
			t.Errorf("refusal changed the store: before %+v after %+v", before, after)
		}
	}
	for _, titles := range [][3]string{{"sbr-1", "sbr-2", "sbr-3"}, {"sbr-9", "sbr-8", "sbr-7"}} {
		a := f.seed(t, titles[0], "a body")
		b := f.seed(t, titles[1], "b body")
		for _, order := range []string{"ab", "ba"} {
			t.Run("a chain of renames "+titles[0]+" "+order, func(t *testing.T) {
				k := f.key(t, false)
				ua := sbuUpdate(a, "title", titles[1])
				ub := sbuUpdate(b, "title", titles[2])
				body := sbItems(ua, ub)
				if order == "ba" {
					body = sbItems(ub, ua)
				}
				refused(t, k, body, "constraint", "items[1]: same category, title and scope as items[0]")
			})
		}
		t.Run("a swap of titles "+titles[0], func(t *testing.T) {
			k := f.key(t, false)
			refused(t, k, sbItems(sbuUpdate(a, "title", titles[1]), sbuUpdate(b, "title", titles[0])),
				"constraint", "items[1]: same category, title and scope as items[0]")
		})
	}

	t.Run("a declaration whose target an update item moves away", func(t *testing.T) {
		k := f.key(t, false, "sbr-team")
		x := f.seed(t, "sbr-x", "x body")
		refused(t, k, sbItems(sbItem("sbr-a", "a body", "supersedes", x), sbuUpdate(x, "scope", "sbr-team")),
			"constraint", "items[1]: same block as items[0]")
	})

	t.Run("a NUL character in an update item", func(t *testing.T) {
		k := f.key(t, false)
		x := f.seed(t, "sbr-nul", "nul body")
		refused(t, k, sbItems(sbuUpdate(x, "content", "a\x00b")), "invalid_body", "items[0]: content carries a NUL character")
		if a := f.mcpCall(t, k, "store_batch", sbItems(sbuUpdate(x, "metadata_merge", map[string]any{"k": "a\x00b"}))); !a.IsError ||
			a.Code != "invalid_body" || !strings.HasPrefix(a.Text, "items[0]: metadata_merge") || !strings.Contains(a.Text, " carries a NUL character") {
			t.Errorf("MCP: %+v", a)
		}
	})

	t.Run("a staged batch whose items refuse each other gets no card", func(t *testing.T) {
		k := f.key(t, true)
		x := f.seed(t, "sbr-staged", "staged body")
		before := f.counts(t, k)
		a := f.mcpCall(t, k, "store_batch", sbItems(sbItem("sbr-staged", "other body"), sbuUpdate(x, "content", "x2")))
		if !a.IsError || a.Code != "constraint" || !strings.HasPrefix(a.Text, "items[1]: same block as items[0]") {
			t.Errorf("stage: %+v; want the constraint refusal and no card", a)
		}
		if after := f.counts(t, k); after != before {
			t.Errorf("the refused stage changed the store: before %+v after %+v", before, after)
		}
	})

	t.Run("a parent named in capitals", func(t *testing.T) {
		k := f.key(t, false)
		p := f.seed(t, "sbr-caps-parent", "parent body")
		if status, resp := f.restStoreNoFatal(k, sbItem("sbr-caps-child", "child", "parent_id", strings.ToUpper(p))); status != http.StatusOK {
			t.Fatalf("/api/store: %d %v", status, resp)
		}
		if got := f.parentOf(t, f.idOf(t, "sbr-caps-child")); got != p {
			t.Fatalf("parent %q, want %q", got, p)
		}
	})
}

// TestStoreBatchParentOfAnotherScopeIsNotWaitedFor: a parent_id that names a
// block of a scope the item does not write is refused without the batch
// touching that block — no lock taken on it and none waited for while another
// transaction holds it, so the time of the refusal says no more than its text.
func TestStoreBatchParentOfAnotherScopeIsNotWaitedFor(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	ctx := context.Background()
	k := f.key(t, false)
	foreign := f.seedIn(t, "sbr-hidden", "sbr-hidden-block", "hidden body")
	gate, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Rollback(ctx) }()
	if _, err := gate.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1 FOR UPDATE`, foreign); err != nil {
		t.Fatal(err)
	}
	actor := f.actor(t, "sbr-foreign-parent")
	done := make(chan sbRestResult, 1)
	go func() {
		on := &sbFixture{pool: actor, cfg: f.cfg, reg: f.reg}
		status, resp := on.restNoFatal(k, sbItems(sbItem("sbr-child", "child", "parent_id", foreign)))
		done <- sbRestResult{status, resp}
	}()
	if f.waitLockOrDone(t, "sbr-foreign-parent", done) {
		t.Fatalf("the batch waits for the row of a block outside its scope")
	}
	res := sbAwait(t, "batch", done)
	if res.status != http.StatusUnprocessableEntity || res.body["code"] != "unknown_parent" {
		t.Fatalf("batch: %d %v; want unknown_parent", res.status, res.body)
	}
}

// TestStoreBatchUpdateDriftAfterTheConsume: a staged batch's update item whose
// block another writer changes after the confirm's drift check and before the
// batch writes it — the writer holds the row while the confirm's lock pass
// waits, then commits — refuses the whole batch: the token is spent, the
// answer names the item and the drift, and nothing of the batch is written.
func TestStoreBatchUpdateDriftAfterTheConsume(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	ctx := context.Background()
	k := f.key(t, true)
	a := f.seed(t, "sbx-a", "a body")
	b := f.seed(t, "sbx-b", "b body")
	staged := f.mcpCall(t, k, "store_batch", sbItems(sbuUpdate(a, "content", "a staged"), sbuUpdate(b, "content", "b staged")))
	m := sbHashRe.FindStringSubmatch(staged.Text)
	if m == nil {
		t.Fatalf("stage: %+v", staged)
	}
	gate, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Rollback(ctx) }()
	if _, err := gate.Exec(ctx, `UPDATE context_blocks SET tags = '{moved}', updated_at = clock_timestamp() WHERE id = $1`, b); err != nil {
		t.Fatal(err)
	}
	before := f.counts(t, k)
	done := make(chan sbAnswer, 1)
	go func() {
		ans, err := f.mcpCallNoFatal(k, "confirm", map[string]any{"payload_hash": m[1]})
		if err != nil {
			ans.Text = err.Error()
		}
		done <- ans
	}()
	for deadline := time.Now().Add(20 * time.Second); ; {
		var waiting int
		if err := f.pool.QueryRow(ctx, `SELECT count(*)::int FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the confirm never waited for the held row")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	ans := sbAwait(t, "confirm", done)
	want := "items[1]: confirmed update not executed, the stage token is consumed: block " + b + " changed while this update was confirmed"
	if !ans.IsError || !strings.HasPrefix(ans.Text, want) {
		t.Fatalf("confirm: %+v; want %q", ans, want)
	}
	after := f.counts(t, k)
	if f.content(t, a) != "a body" || f.content(t, b) != "b body" || after.openStages != before.openStages-1 || after.writes != before.writes {
		t.Fatalf("before %+v after %+v, a %q b %q; want nothing written and the token spent", before, after, f.content(t, a), f.content(t, b))
	}
}
