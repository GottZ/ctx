//go:build integration

// Issue #238 §5: every metadata key with the prefix guard_ is the duplicate
// guard's record about a block — the verdict applyDecision writes
// (guard/guard.go), the trace GuardResolve writes, the repair marks of the
// migrations. GuardList orders a scope's review queue by guard_similarity and
// the status rollup ages it by guard_checked_at, for every reader of the
// scope. A key that may write one block of the scope must therefore neither
// set these keys nor change or remove the ones the guard wrote.
//
// Each subtest drives one client write surface through its production handler
// (REST handler, MCP tool over the MCP transport, manage dispatcher, project
// issue router, chat stage runner) and reads the row straight from
// context_blocks. The contract:
//
//  1. a NEW block carries no guard_ key of the request;
//  2. a block the GUARD flagged keeps every guard_ key it holds, value for
//     value — on a replacement, on a merge that sets a guard_ key or nulls
//     one, on an identical content; on a content change as well, except
//     guard_checked_at, which the change drops (the guard checks again);
//  3. the request's other keys ("topic") land as before.
//
// And for every flagged block after its write: GuardList, as the scope's
// readers call it, lists the block with the guard's similarity.
//
// Every request carries several guard_ keys with values the guard never
// writes (gkForged). Each subtest owns a scope, so a write that breaks
// GuardList in one scope cannot fail another subtest.
//
//	subtest                               surface                        store writer
//	rest_store_*                          POST /api/store                IdenticalContentMatch, UpsertBlock (prepareUpsert)
//	rest_store_batch, mcp_store_batch     POST /api/store/batch, MCP     UpsertBlocks (lockBatchMatch, prepareUpsert)
//	mcp_store                             MCP store                      IdenticalContentMatch, UpsertBlock
//	ingest_block_mode                     POST /api/ingest               IdenticalContentMatch, UpsertBlock
//	ingest_chunk_mode                     POST /api/ingest (source_id)   InsertChunk
//	manage_update_*, mcp_update_*         manage update, MCP update      UpdateBlock (updateMetadataExpr)
//	staged_update_confirmed_over_mcp      MCP update + MCP confirm       UpdateBlock
//	staged_store_confirmed                MCP store + HTTP/MCP confirm   UpsertBlock
//	chat_staged_confirmed_over_http       chat stage runner + /api/confirm  UpdateBlock, UpsertBlock
//	manage_issue_family                   manage issue-create, -comment-create, -update
//	                                                                     InsertIssueBlock, InsertCommentBlock, UpdateIssueBlock
//	mcp_issue_tools                       MCP issue_create, issue_comment  InsertIssueBlock, InsertCommentBlock
//	project_issue_routes                  POST/PATCH /api/project/{id}/issues…  the same three
//
// Run:
//
//	go test -tags=integration -p 1 ./internal/handler/ -run TestGuardKeysWritePaths -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
)

// gkForged is the metadata the writes below send: guard_ keys with values the
// guard never writes — a similarity that is no number, a check time in the
// future, a verdict, a resolution, a matched id of no block, a key no writer
// has — and one ordinary key.
func gkForged(topic string) map[string]any {
	return map[string]any{
		"guard_similarity": "x",
		"guard_checked_at": "2999-01-01T00:00:00Z",
		"guard_status":     "clean",
		"guard_resolution": "keep",
		"guard_matched_id": "00000000-0000-7000-8000-000000000238",
		"guard_custom":     map[string]any{"forged": true},
		"topic":            topic,
	}
}

// gkMergeNulls is a metadata_merge that names stored guard_ keys with null
// (remove) and sets an ordinary key.
func gkMergeNulls(topic string) map[string]any {
	return map[string]any{
		"guard_similarity":       nil,
		"guard_checked_at":       nil,
		"guard_status":           nil,
		"guard_matched_id":       nil,
		"guard_threshold_review": nil,
		"topic":                  topic,
	}
}

// gkMergeMixed sets forged guard_ keys and nulls stored ones in one merge.
func gkMergeMixed(topic string) map[string]any {
	m := gkForged(topic)
	m["guard_checked_at"] = nil
	m["guard_threshold_review"] = nil
	return m
}

// gkFlagged is a block the guard flagged and the guard_ keys it holds after
// the guard pass, each value as jsonb prints it (canonical, so equal text is
// an equal JSON value).
type gkFlagged struct {
	id, scope string
	guard     map[string]string
}

// The scopes of the subtests, one each. The ones in gkPairScopes get a pair of
// blocks the guard flags (needs_review) before the writes.
const (
	gkRestNew           = "gk-rest-new"
	gkRestSame          = "gk-rest-same"
	gkRestChanged       = "gk-rest-changed"
	gkRestBatch         = "gk-rest-batch"
	gkMCPStore          = "gk-mcp-store"
	gkMCPBatch          = "gk-mcp-batch"
	gkIngestBlock       = "gk-ingest-block"
	gkIngestChunk       = "gk-ingest-chunk"
	gkManageMeta        = "gk-manage-meta"
	gkManageMergeSet    = "gk-manage-merge-set"
	gkManageMergeNull   = "gk-manage-merge-null"
	gkManageMetaContent = "gk-manage-meta-content"
	gkMCPUpdateMeta     = "gk-mcp-update-meta"
	gkMCPUpdateMerge    = "gk-mcp-update-merge"
	gkStagedUpdate      = "gk-staged-update"
	gkStagedStore       = "gk-staged-store"
	gkChatStaged        = "gk-chat-staged"
	gkIssueManage       = "gk-issue-manage"
	gkIssueMCP          = "gk-issue-mcp"
	gkProjectSlug       = "gkproj" // w6SeedProject: scope gkproj:repo in a tenant of its own
)

var gkPairScopes = []string{
	gkRestSame, gkRestChanged, gkRestBatch, gkMCPStore, gkMCPBatch, gkIngestBlock,
	gkManageMeta, gkManageMergeSet, gkManageMergeNull, gkManageMetaContent,
	gkMCPUpdateMeta, gkMCPUpdateMerge, gkStagedUpdate, gkStagedStore, gkChatStaged,
}

func TestGuardKeysWritePaths(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	ctx := context.Background()
	mh := NewManageHandler(f.pool, f.cfg, nil, nil, nil, nil, nil, f.reg)

	// --- keys and drivers --------------------------------------------------

	// keyIn is a fresh key whose home scope is scope: every surface without a
	// scope parameter writes there.
	keyIn := func(t *testing.T, scope, tenantID string, confirm bool) sbKey {
		t.Helper()
		f.n++
		row, plain, err := store.CreateApiKey(ctx, f.pool, fmt.Sprintf("gk-key-%d", f.n), scope, nil, tenantID)
		if err != nil {
			t.Fatalf("create key in %s: %v", scope, err)
		}
		if confirm {
			if _, err := f.pool.Exec(ctx, `UPDATE context_api_keys SET confirm_writes = true WHERE id = $1`, row.ID); err != nil {
				t.Fatalf("confirm_writes on %s: %v", row.ID, err)
			}
		}
		return f.auth(t, row.ID, plain)
	}
	restStore := func(t *testing.T, k sbKey, body map[string]any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/store", bytes.NewReader(raw)).WithContext(k.ctx)
		rec := httptest.NewRecorder()
		NewStoreHandler(f.pool, f.cfg, f.reg).HandleStore(rec, req)
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK || resp["success"] != true {
			t.Fatalf("/api/store: status %d body %s", rec.Code, rec.Body.String())
		}
		return resp
	}
	restIngest := func(t *testing.T, k sbKey, chunks ...map[string]any) []string {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"source": "gk238", "chunks": chunks})
		req := httptest.NewRequest(http.MethodPost, "/api/ingest", bytes.NewReader(raw)).WithContext(k.ctx)
		rec := httptest.NewRecorder()
		NewIngestHandler(f.pool, f.cfg, f.reg).HandleIngest(rec, req)
		var resp IngestResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK || len(resp.Results) != len(chunks) {
			t.Fatalf("/api/ingest: status %d body %s", rec.Code, rec.Body.String())
		}
		ids := make([]string, len(resp.Results))
		for i, r := range resp.Results {
			if r.Status != "inserted" && r.Status != "updated" {
				t.Fatalf("/api/ingest chunk %d: %+v, want it written", i, r)
			}
			ids[i] = r.ID
		}
		return ids
	}
	manage := func(t *testing.T, k sbKey, action, id string, data map[string]any) {
		t.Helper()
		code, resp := callManage(t, mh, AuthResultFromContext(k.ctx), action, id, data)
		if code != http.StatusOK || resp["success"] != true {
			t.Fatalf("manage %s: status %d body %v", action, code, resp)
		}
	}
	mcpOK := func(t *testing.T, k sbKey, tool string, args map[string]any) {
		t.Helper()
		if a := f.mcpCall(t, k, tool, args); a.RPCError != "" || a.IsError {
			t.Fatalf("MCP %s: %+v", tool, a)
		}
	}
	mcpStage := func(t *testing.T, k sbKey, tool string, args map[string]any) string {
		t.Helper()
		a := f.mcpCall(t, k, tool, args)
		m := sbHashRe.FindStringSubmatch(a.Text)
		if !a.IsError || a.Code != "" || !strings.HasPrefix(a.Text, "STAGED — NOT ") || m == nil {
			t.Fatalf("MCP %s with a confirm_writes key: %+v; want a STAGED card", tool, a)
		}
		return m[1]
	}
	httpConfirm := func(t *testing.T, k sbKey, hash string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"payload_hash": hash})
		req := httptest.NewRequest(http.MethodPost, "/api/confirm", bytes.NewReader(raw)).WithContext(k.ctx)
		rec := httptest.NewRecorder()
		NewConfirmHandler(f.pool, f.reg).HandleConfirm(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("/api/confirm: status %d body %s", rec.Code, rec.Body.String())
		}
	}
	project := func(t *testing.T, k sbKey, method, path string, body map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := w7Do(t, f.pool, f.reg, f.cfg, AuthResultFromContext(k.ctx), method, path, string(raw))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: status %d body %s", method, path, rec.Code, rec.Body.String())
		}
	}

	// --- readers -----------------------------------------------------------

	metaOf := func(t *testing.T, id string) map[string]json.RawMessage {
		t.Helper()
		var raw []byte
		if err := f.pool.QueryRow(ctx, `SELECT metadata FROM context_blocks WHERE id = $1::uuid`, id).Scan(&raw); err != nil {
			t.Fatalf("read metadata of %s: %v", id, err)
		}
		m := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode metadata of %s (%s): %v", id, raw, err)
		}
		return m
	}
	guardPart := func(m map[string]json.RawMessage) map[string]string {
		g := map[string]string{}
		for k, v := range m {
			if strings.HasPrefix(k, "guard_") {
				g[k] = string(v)
			}
		}
		return g
	}
	titleOf := func(t *testing.T, id string) string {
		t.Helper()
		var title string
		if err := f.pool.QueryRow(ctx, `SELECT title FROM context_blocks WHERE id = $1::uuid`, id).Scan(&title); err != nil {
			t.Fatalf("read title of %s: %v", id, err)
		}
		return title
	}
	// idByContent finds the one live block of scope with this content (every
	// content below is unique).
	idByContent := func(t *testing.T, scope, content string) string {
		t.Helper()
		rows, err := f.pool.Query(ctx,
			`SELECT id::text FROM context_blocks WHERE scope = $1 AND content = $2 AND NOT is_archived`, scope, content)
		if err != nil {
			t.Fatalf("find %q in %s: %v", content, scope, err)
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			ids = append(ids, id)
		}
		if len(ids) != 1 {
			t.Fatalf("blocks in %s with content %q: %v, want exactly one", scope, content, ids)
		}
		return ids[0]
	}
	checkTopic := func(t *testing.T, id string, m map[string]json.RawMessage, topic string) {
		t.Helper()
		want, _ := json.Marshal(topic)
		if got, ok := m["topic"]; !ok || string(got) != string(want) {
			t.Errorf("block %s: topic = %s, want %s (the request's other keys land)", id, got, want)
		}
	}
	// checkNew: contract 1 and 3 on a block the write created.
	checkNew := func(t *testing.T, id, topic string) {
		t.Helper()
		m := metaOf(t, id)
		got := guardPart(m)
		keys := gkSortedKeys(got)
		for _, k := range keys {
			t.Errorf("new block %s carries %s = %s from the request; a client write stores no guard_ key", id, k, got[k])
		}
		checkTopic(t, id, m, topic)
	}
	// checkListed: GuardList, as the scope's readers call it, lists the block
	// with the guard's similarity.
	checkListed := func(t *testing.T, fb gkFlagged) {
		t.Helper()
		items, err := store.GuardList(ctx, f.pool, []string{fb.scope}, "", "", nil, 50)
		if err != nil {
			t.Errorf("GuardList for the readers of %s: %v", fb.scope, err)
			return
		}
		for _, it := range items {
			if it.ID != fb.id {
				continue
			}
			if it.Similarity == nil || *it.Similarity != fb.guard["guard_similarity"] {
				t.Errorf("GuardList of %s: block %s has similarity %v, want the guard's %s", fb.scope, fb.id, deref(it.Similarity), fb.guard["guard_similarity"])
			}
			return
		}
		t.Errorf("GuardList of %s (%d items) does not list the flagged block %s", fb.scope, len(items), fb.id)
	}
	// checkKept: contract 2 and 3 on a block the guard flagged, then GuardList.
	checkKept := func(t *testing.T, fb gkFlagged, topic string, contentChanged bool) {
		t.Helper()
		m := metaOf(t, fb.id)
		got := guardPart(m)
		want := maps.Clone(fb.guard)
		if contentChanged {
			delete(want, "guard_checked_at")
		}
		for _, k := range gkSortedKeys(want) {
			g, ok := got[k]
			switch {
			case !ok:
				t.Errorf("flagged block %s: the guard's %s = %s is gone after the client write", fb.id, k, want[k])
			case g != want[k]:
				t.Errorf("flagged block %s: the guard's %s = %s became %s", fb.id, k, want[k], g)
			}
		}
		for _, k := range gkSortedKeys(got) {
			if _, ok := want[k]; ok {
				continue
			}
			if k == "guard_checked_at" && contentChanged {
				t.Errorf("flagged block %s: guard_checked_at = %s after a content change; the change drops it", fb.id, got[k])
				continue
			}
			t.Errorf("flagged block %s carries %s = %s, which the guard did not write", fb.id, k, got[k])
		}
		checkTopic(t, fb.id, m, topic)
		checkListed(t, fb)
	}

	// --- fixtures: scopes, flagged pairs, one guard pass --------------------

	allScopes := append([]string{gkRestNew, gkIngestChunk, gkIssueManage, gkIssueMCP}, gkPairScopes...)
	for _, s := range allScopes {
		be5SeedScope(t, f.pool, s, store.DefaultTenantID)
	}
	pid, projScope := w6SeedProject(t, f.pool, gkProjectSlug)
	var projTenant string
	if err := f.pool.QueryRow(ctx, `SELECT tenant_id::text FROM context_projects WHERE id = $1::uuid`, pid).Scan(&projTenant); err != nil {
		t.Fatalf("project tenant: %v", err)
	}
	projBase := "/api/project/" + pid

	// Each pair lies at cosine 0.96 on two axes of its own: the guard flags
	// both blocks of a pair needs_review (asserted below), and no pair is near
	// another.
	axis := 0
	nextPair := func() [2][]float32 {
		v := [2][]float32{agingVec(axis, 1.0, 0, 0), agingVec(axis, 0.96, axis+1, 0.28)}
		axis += 2
		return v
	}
	embed := func(t *testing.T, id string, v []float32) {
		t.Helper()
		if err := store.StoreEmbedding(ctx, f.pool, id, "gk-model", v); err != nil {
			t.Fatalf("StoreEmbedding %s: %v", id, err)
		}
	}
	pairs := map[string][2]string{}
	for _, s := range gkPairScopes {
		v := nextPair()
		pairs[s] = [2]string{
			writeAgingBlock(t, f.pool, s, s+" a", v[0]),
			writeAgingBlock(t, f.pool, s, s+" b", v[1]),
		}
	}
	// Issue pairs over the issue surfaces themselves.
	{
		seed := keyIn(t, gkIssueManage, store.DefaultTenantID, false)
		v := nextPair()
		var ids [2]string
		for i, c := range []string{"gk manage issue body a", "gk manage issue body b"} {
			manage(t, seed, "issue-create", "", map[string]any{"title": fmt.Sprintf("gk manage issue %d", i), "content": c})
			ids[i] = idByContent(t, gkIssueManage, c)
			embed(t, ids[i], v[i])
		}
		pairs[gkIssueManage] = ids
	}
	{
		seed := keyIn(t, projScope, projTenant, false)
		v := nextPair()
		var ids [2]string
		for i, c := range []string{"gk project issue body a", "gk project issue body b"} {
			project(t, seed, http.MethodPost, projBase+"/issues", map[string]any{"title": fmt.Sprintf("gk project issue %d", i), "content": c})
			ids[i] = idByContent(t, projScope, c)
			embed(t, ids[i], v[i])
		}
		pairs[projScope] = ids
	}

	for round := 0; ; round++ {
		n, err := guard.RunGuardBatch(ctx, f.pool, f.reg.Snapshot(), 100, f.cfg.Snapshot().GuardSearch())
		if err != nil {
			t.Fatalf("RunGuardBatch: %v", err)
		}
		if n == 0 {
			break
		}
		if round > 5 {
			t.Fatalf("the guard still picks blocks after %d rounds", round)
		}
	}
	flagged := map[string][2]gkFlagged{}
	for scope, ids := range pairs {
		var fb [2]gkFlagged
		for i, id := range ids {
			var status string
			if err := f.pool.QueryRow(ctx, `SELECT COALESCE(guard_status, '') FROM context_blocks WHERE id = $1::uuid`, id).Scan(&status); err != nil {
				t.Fatalf("read guard_status of %s: %v", id, err)
			}
			g := guardPart(metaOf(t, id))
			if status != "needs_review" || g["guard_similarity"] == "" || g["guard_checked_at"] == "" || g["guard_matched_id"] == "" {
				t.Fatalf("block %s in %s: guard_status %q, guard keys %v; the fixture did not reach a block the guard flagged", id, scope, status, g)
			}
			fb[i] = gkFlagged{id: id, scope: scope, guard: g}
			checkListed(t, fb[i]) // premise: listed before any client write
		}
		flagged[scope] = fb
	}

	// --- the upsert family: prepareUpsert / identicalContentCompare ---------

	t.Run("rest_store_new", func(t *testing.T) {
		k := keyIn(t, gkRestNew, store.DefaultTenantID, false)
		const content = "gk rest store, a new block"
		restStore(t, k, map[string]any{"category": "learnings", "title": "gk rest new", "content": content,
			"metadata": gkForged("rest-new")})
		checkNew(t, idByContent(t, gkRestNew, content), "rest-new")
	})

	t.Run("rest_store_flagged_identical_content", func(t *testing.T) {
		fb := flagged[gkRestSame][0]
		k := keyIn(t, fb.scope, store.DefaultTenantID, false)
		body := map[string]any{"category": "learnings", "title": titleOf(t, fb.id), "content": f.content(t, fb.id),
			"metadata": gkForged("rest-same")}
		restStore(t, k, body)
		checkKept(t, fb, "rest-same", false)

		// The same request again changes nothing the write would store, so it
		// is the identical-content noop (issue #64) — the comparison drops the
		// guard_ keys of the request as the write does (identicalContentCompare).
		// A client that writes back what it read sends the guard's keys on
		// every repeat save.
		if resp := restStore(t, k, body); resp["action"] != "noop" {
			t.Errorf("the repeated request answered %v, want the identical-content noop", resp)
		}
		checkKept(t, fb, "rest-same", false)
	})

	t.Run("rest_store_flagged_changed_content", func(t *testing.T) {
		fb := flagged[gkRestChanged][0]
		k := keyIn(t, fb.scope, store.DefaultTenantID, false)
		restStore(t, k, map[string]any{"category": "learnings", "title": titleOf(t, fb.id), "content": f.content(t, fb.id) + " (rewritten)",
			"metadata": gkForged("rest-changed")})
		checkKept(t, fb, "rest-changed", true)
	})

	t.Run("rest_store_batch", func(t *testing.T) {
		fb := flagged[gkRestBatch]
		k := keyIn(t, gkRestBatch, store.DefaultTenantID, false)
		const content = "gk rest batch, a new block"
		status, resp := f.rest(t, k, sbItems(
			sbItem("gk rest batch new", content, "metadata", gkForged("batch-new")),
			sbItem(titleOf(t, fb[0].id), f.content(t, fb[0].id), "metadata", gkForged("batch-same")),
			sbItem(titleOf(t, fb[1].id), f.content(t, fb[1].id)+" (rewritten)", "metadata", gkForged("batch-changed")),
		))
		if status != http.StatusOK {
			t.Fatalf("/api/store/batch: status %d body %v", status, resp)
		}
		checkNew(t, idByContent(t, gkRestBatch, content), "batch-new")
		checkKept(t, fb[0], "batch-same", false)
		checkKept(t, fb[1], "batch-changed", true)
	})

	t.Run("mcp_store", func(t *testing.T) {
		fb := flagged[gkMCPStore][0]
		k := keyIn(t, gkMCPStore, store.DefaultTenantID, false)
		const content = "gk mcp store, a new block"
		mcpOK(t, k, "store", sbItem("gk mcp store new", content, "metadata", gkForged("mcp-new")))
		checkNew(t, idByContent(t, gkMCPStore, content), "mcp-new")
		mcpOK(t, k, "store", sbItem(titleOf(t, fb.id), f.content(t, fb.id)+" (rewritten)", "metadata", gkForged("mcp-changed")))
		checkKept(t, fb, "mcp-changed", true)
	})

	t.Run("mcp_store_batch", func(t *testing.T) {
		fb := flagged[gkMCPBatch]
		k := keyIn(t, gkMCPBatch, store.DefaultTenantID, false)
		const content = "gk mcp batch, a new block"
		mcpOK(t, k, "store_batch", sbItems(
			sbItem("gk mcp batch new", content, "metadata", gkForged("mbatch-new")),
			sbItem(titleOf(t, fb[0].id), f.content(t, fb[0].id), "metadata", gkForged("mbatch-same")),
			sbItem(titleOf(t, fb[1].id), f.content(t, fb[1].id)+" (rewritten)", "metadata", gkForged("mbatch-changed")),
		))
		checkNew(t, idByContent(t, gkMCPBatch, content), "mbatch-new")
		checkKept(t, fb[0], "mbatch-same", false)
		checkKept(t, fb[1], "mbatch-changed", true)
	})

	t.Run("ingest_block_mode", func(t *testing.T) {
		fb := flagged[gkIngestBlock][0]
		k := keyIn(t, gkIngestBlock, store.DefaultTenantID, false)
		ids := restIngest(t, k,
			map[string]any{"category": "learnings", "title": "gk ingest new", "content": "gk ingest block mode, a new block",
				"metadata": gkForged("ingest-new")},
			map[string]any{"category": "learnings", "title": titleOf(t, fb.id), "content": f.content(t, fb.id) + " (rewritten)",
				"metadata": gkForged("ingest-changed")},
		)
		checkNew(t, ids[0], "ingest-new")
		if ids[1] != fb.id {
			t.Fatalf("the ingest of the flagged block's identity wrote %s, want %s", ids[1], fb.id)
		}
		checkKept(t, fb, "ingest-changed", true)
	})

	t.Run("ingest_chunk_mode", func(t *testing.T) {
		// Chunk rows (lifecycle_state 'chunk') are never picked by the guard
		// (guardPendingIndexWhere), so this path has only contract 1 and 3: on
		// the first write of the slot and on its rewrite (InsertChunk's ON
		// CONFLICT branch).
		k := keyIn(t, gkIngestChunk, store.DefaultTenantID, false)
		var sourceID string
		if err := f.pool.QueryRow(ctx,
			`INSERT INTO context_sources (file_path, file_hash, scope) VALUES ('/gk238/chunks.md', 'gk238', $1) RETURNING id::text`,
			gkIngestChunk).Scan(&sourceID); err != nil {
			t.Fatalf("seed source: %v", err)
		}
		chunk := func(content, topic string) map[string]any {
			return map[string]any{"category": "learnings", "title": "gk chunk 0", "content": content,
				"metadata": gkForged(topic), "source_id": sourceID, "chunk_index": 0}
		}
		first := restIngest(t, k, chunk("gk ingest chunk mode, first text", "chunk-new"))[0]
		checkNew(t, first, "chunk-new")
		again := restIngest(t, k, chunk("gk ingest chunk mode, second text", "chunk-rewrite"))[0]
		if again != first {
			t.Fatalf("the rewrite of chunk slot 0 wrote %s, want the slot's row %s", again, first)
		}
		checkNew(t, again, "chunk-rewrite")
	})

	// --- UpdateBlock: updateMetadataExpr ------------------------------------

	t.Run("manage_update_metadata", func(t *testing.T) {
		fb := flagged[gkManageMeta][0]
		k := keyIn(t, fb.scope, store.DefaultTenantID, false)
		manage(t, k, "update", fb.id, map[string]any{"metadata": gkForged("mu-meta")})
		checkKept(t, fb, "mu-meta", false)
	})

	t.Run("manage_update_metadata_merge_set", func(t *testing.T) {
		fb := flagged[gkManageMergeSet][0]
		k := keyIn(t, fb.scope, store.DefaultTenantID, false)
		manage(t, k, "update", fb.id, map[string]any{"metadata_merge": gkForged("mu-merge-set")})
		checkKept(t, fb, "mu-merge-set", false)
	})

	t.Run("manage_update_metadata_merge_null", func(t *testing.T) {
		fb := flagged[gkManageMergeNull][0]
		k := keyIn(t, fb.scope, store.DefaultTenantID, false)
		manage(t, k, "update", fb.id, map[string]any{"metadata_merge": gkMergeNulls("mu-merge-null")})
		checkKept(t, fb, "mu-merge-null", false)
	})

	t.Run("manage_update_metadata_and_content", func(t *testing.T) {
		fb := flagged[gkManageMetaContent][0]
		k := keyIn(t, fb.scope, store.DefaultTenantID, false)
		manage(t, k, "update", fb.id, map[string]any{"metadata": gkForged("mu-meta-content"),
			"content": f.content(t, fb.id) + " (rewritten)"})
		checkKept(t, fb, "mu-meta-content", true)
	})

	t.Run("mcp_update_metadata", func(t *testing.T) {
		fb := flagged[gkMCPUpdateMeta][0]
		k := keyIn(t, fb.scope, store.DefaultTenantID, false)
		mcpOK(t, k, "update", map[string]any{"id": fb.id, "metadata": gkForged("mcpu-meta")})
		checkKept(t, fb, "mcpu-meta", false)
	})

	t.Run("mcp_update_metadata_merge", func(t *testing.T) {
		fb := flagged[gkMCPUpdateMerge][0]
		k := keyIn(t, fb.scope, store.DefaultTenantID, false)
		mcpOK(t, k, "update", map[string]any{"id": fb.id, "metadata_merge": gkMergeMixed("mcpu-merge")})
		checkKept(t, fb, "mcpu-merge", false)
	})

	// --- staged writes and their confirm ------------------------------------

	t.Run("staged_update_confirmed_over_mcp", func(t *testing.T) {
		fb := flagged[gkStagedUpdate][0]
		k := keyIn(t, fb.scope, store.DefaultTenantID, true)
		hash := mcpStage(t, k, "update", map[string]any{"id": fb.id, "metadata": gkForged("staged-update")})
		mcpOK(t, k, "confirm", map[string]any{"payload_hash": hash})
		checkKept(t, fb, "staged-update", false)
	})

	t.Run("staged_store_confirmed", func(t *testing.T) {
		fb := flagged[gkStagedStore][0]
		k := keyIn(t, fb.scope, store.DefaultTenantID, true)
		hash := mcpStage(t, k, "store", sbItem(titleOf(t, fb.id), f.content(t, fb.id)+" (rewritten)", "metadata", gkForged("staged-store")))
		httpConfirm(t, k, hash)
		checkKept(t, fb, "staged-store", true)

		const content = "gk staged store, a new block"
		hash = mcpStage(t, k, "store", sbItem("gk staged store new", content, "metadata", gkForged("staged-store-new")))
		mcpOK(t, k, "confirm", map[string]any{"payload_hash": hash})
		checkNew(t, idByContent(t, gkStagedStore, content), "staged-store-new")
	})

	t.Run("chat_staged_confirmed_over_http", func(t *testing.T) {
		fb := flagged[gkChatStaged]
		k := keyIn(t, gkChatStaged, store.DefaultTenantID, false)
		runner := &chatStageRunner{pool: f.pool, cfg: f.cfg, blocktypes: f.reg}

		staged, reject, err := runner.StageUpdate(k.ctx, fb[0].id, nil, nil, nil, nil, gkForged("chat-update"))
		if err != nil || reject != "" || staged == nil {
			t.Fatalf("chat StageUpdate: err=%v reject=%q", err, reject)
		}
		httpConfirm(t, k, staged.PayloadHash)
		checkKept(t, fb[0], "chat-update", false)

		staged, reject, err = runner.StageWrite(k.ctx, "learnings", titleOf(t, fb[1].id), f.content(t, fb[1].id)+" (rewritten)", nil, gkForged("chat-store"))
		if err != nil || reject != "" || staged == nil {
			t.Fatalf("chat StageWrite: err=%v reject=%q", err, reject)
		}
		httpConfirm(t, k, staged.PayloadHash)
		checkKept(t, fb[1], "chat-store", true)
	})

	// --- the issue domain: InsertIssueBlock / InsertCommentBlock / UpdateIssueBlock

	t.Run("manage_issue_family", func(t *testing.T) {
		fb := flagged[gkIssueManage]
		k := keyIn(t, gkIssueManage, store.DefaultTenantID, false)

		const issueBody = "gk manage issue-create, a new issue"
		manage(t, k, "issue-create", "", map[string]any{"title": "gk manage new issue", "content": issueBody, "metadata": gkForged("mi-create")})
		checkNew(t, idByContent(t, gkIssueManage, issueBody), "mi-create")

		const commentBody = "gk manage issue-comment-create, a new comment"
		manage(t, k, "issue-comment-create", "", map[string]any{"parent_id": fb[1].id, "content": commentBody, "metadata": gkForged("mi-comment")})
		checkNew(t, idByContent(t, gkIssueManage, commentBody), "mi-comment")

		manage(t, k, "issue-update", fb[0].id, map[string]any{"metadata": gkForged("mi-update")})
		checkKept(t, fb[0], "mi-update", false)
	})

	t.Run("mcp_issue_tools", func(t *testing.T) {
		k := keyIn(t, gkIssueMCP, store.DefaultTenantID, false)
		const issueBody = "gk mcp issue_create, a new issue"
		mcpOK(t, k, "issue_create", map[string]any{"title": "gk mcp new issue", "content": issueBody, "metadata": gkForged("mcpi-create")})
		issueID := idByContent(t, gkIssueMCP, issueBody)
		checkNew(t, issueID, "mcpi-create")

		const commentBody = "gk mcp issue_comment, a new comment"
		mcpOK(t, k, "issue_comment", map[string]any{"issue_id": issueID, "content": commentBody, "metadata": gkForged("mcpi-comment")})
		checkNew(t, idByContent(t, gkIssueMCP, commentBody), "mcpi-comment")
	})

	t.Run("project_issue_routes", func(t *testing.T) {
		fb := flagged[projScope]
		k := keyIn(t, projScope, projTenant, false)

		const issueBody = "gk project POST issues, a new issue"
		project(t, k, http.MethodPost, projBase+"/issues", map[string]any{"title": "gk project new issue", "content": issueBody, "metadata": gkForged("pi-create")})
		checkNew(t, idByContent(t, projScope, issueBody), "pi-create")

		const commentBody = "gk project POST comments, a new comment"
		project(t, k, http.MethodPost, projBase+"/issues/"+fb[1].id+"/comments", map[string]any{"content": commentBody, "metadata": gkForged("pi-comment")})
		checkNew(t, idByContent(t, projScope, commentBody), "pi-comment")

		project(t, k, http.MethodPatch, projBase+"/issues/"+fb[0].id, map[string]any{"metadata": gkForged("pi-patch"),
			"content": f.content(t, fb[0].id) + " (rewritten)"})
		checkKept(t, fb[0], "pi-patch", true)
	})
}

func gkSortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
