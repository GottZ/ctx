//go:build integration

// Issue #547, review round 1 (F4, F6): a deadlock (40P01) a write loses
// answers 409 write_conflict on every surface that can meet one — the
// surfaces whose mapping does not pass provenanceRejectOr included, each
// pinned here: the staged MCP store with supersedes and the staged
// store_batch with ONE supersedes item (their stage checks), the MCP issue
// tools, guard-resolve with ids (REST and MCP), dream-link-resolve, the confirmed archive,
// a block batch and a confirmed batch that lose at a scope lock (outside any
// item) and the stage check of a batch outside any item.
//
// Each case: a holder takes a lock, the request starts and waits for it while
// it holds a lock of its own, the holder asks for that one — PostgreSQL ends
// the request (it waited first) with 40P01. Nothing of the blocks or links
// may change, no ERROR line may be written.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run 'TestWriteConflictSurfaces' -v
package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	wcsScopeLock = `SELECT pg_advisory_xact_lock(hashtext('ctx:supersedes:' || $1::text))`
	wcsRowLock   = `SELECT 1 FROM context_blocks WHERE id = $1 FOR UPDATE`
)

// wcsAnswer is a surface's answer: the HTTP status (MCP: 200), the code and
// the message.
type wcsAnswer struct {
	status      int
	code, error string
}

// wcsBlocksAndLinks hashes the rows of the blocks and the dream links.
func wcsBlocksAndLinks(t *testing.T, f *sbFixture) string {
	t.Helper()
	var h string
	if err := f.pool.QueryRow(context.Background(), `SELECT
		(SELECT md5(COALESCE(string_agg(b::text, '|' ORDER BY b.id), '')) FROM context_blocks b) ||
		(SELECT md5(COALESCE(string_agg(l::text, '|' ORDER BY l.source_block_id, l.target_block_id, l.relationship), '')) FROM context_dream_links l)`).Scan(&h); err != nil {
		t.Fatalf("hash: %v", err)
	}
	return h
}

// wcsDeadlock runs one case and checks the answer and what it left.
func wcsDeadlock(t *testing.T, f *sbFixture, holdSQL string, holdArgs []any, secondSQL string, secondArgs []any, req func() wcsAnswer, wantPrefix string) {
	t.Helper()
	ctx := context.Background()
	before := wcsBlocksAndLinks(t, f)
	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	var pid uint32
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, holdSQL, holdArgs...); err != nil {
		t.Fatalf("hold: %v", err)
	}
	logs := installRejectLogCapture(t)
	done := make(chan struct{})
	var got wcsAnswer
	go func() {
		defer close(done)
		got = req()
	}()
	testdb.WaitBlockedBy(t, f.pool, pid, done, "the request", "")
	if _, err := holder.Exec(ctx, secondSQL, secondArgs...); err != nil {
		t.Fatalf("holder: %v (the request should have been the deadlock's victim)", err)
	}
	<-done
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got.code != "write_conflict" || (got.status != http.StatusOK && got.status != http.StatusConflict) {
		t.Errorf("answer beside a deadlock = %+v, want write_conflict", got)
	}
	if !strings.HasPrefix(got.error, wantPrefix) || !strings.Contains(got.error, writeConflictMsg) {
		t.Errorf("message %q, want %q followed by the write_conflict wording", got.error, wantPrefix)
	}
	wcNoErrorLine(t, logs)
	if after := wcsBlocksAndLinks(t, f); after != before {
		t.Errorf("blocks or links changed beside a write that lost")
	}
}

func TestWriteConflictSurfaces(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	ctx := context.Background()
	k := f.key(t, false, "work")
	kc := f.key(t, true, "work")
	h := NewManageHandler(f.pool, f.cfg, nil, backends.NewPool(nil, nil), nil, nil, nil, f.reg)
	mcp := func(key sbKey, tool string, args map[string]any) wcsAnswer {
		a := f.mcpCall(t, key, tool, args)
		if a.RPCError != "" {
			return wcsAnswer{status: -1, error: a.RPCError}
		}
		return wcsAnswer{status: http.StatusOK, code: a.Code, error: a.Text}
	}
	rest := func(st int, m map[string]any) wcsAnswer {
		c, _ := m["code"].(string)
		e, _ := m["error"].(string)
		return wcsAnswer{status: st, code: c, error: e}
	}
	stage := func(tool string, args map[string]any) string {
		a := mcp(kc, tool, args)
		m := sbHashRe.FindStringSubmatch(a.error)
		if m == nil {
			t.Fatalf("%s did not stage: %+v", tool, a)
		}
		return m[1]
	}

	t.Run("staged MCP store with supersedes", func(t *testing.T) { // F4
		p := f.seed(t, "wcs-st-p1", "older")
		wcsDeadlock(t, f, wcsRowLock, []any{p}, wcsScopeLock, []any{"private"}, func() wcsAnswer {
			return mcp(kc, "store", map[string]any{"category": "learnings", "title": "wcs-st-n1", "content": "newer", "supersedes": p})
		}, "")
	})
	t.Run("staged MCP store_batch with one supersedes item", func(t *testing.T) { // F4
		p := f.seed(t, "wcs-st-p2", "older")
		wcsDeadlock(t, f, wcsRowLock, []any{p}, wcsScopeLock, []any{"private"}, func() wcsAnswer {
			return mcp(kc, "store_batch", map[string]any{"items": []any{
				map[string]any{"category": "learnings", "title": "wcs-st-n2", "content": "newer", "supersedes": p}}})
		}, "items[0]: ")
	})
	t.Run("MCP issue_comment", func(t *testing.T) { // F4
		if a := mcp(k, "issue_create", map[string]any{"title": "wcs issue", "content": "an issue"}); a.code != "" || a.status != http.StatusOK {
			t.Fatalf("issue_create: %+v", a)
		}
		var iss string
		if err := f.pool.QueryRow(ctx, `SELECT id::text FROM context_blocks WHERE title LIKE '%wcs issue' AND NOT is_archived`).Scan(&iss); err != nil {
			t.Fatalf("issue: %v", err)
		}
		// The comment holds its issue FOR SHARE and waits for the per-issue
		// sequence lock the holder took; the holder then asks for the issue.
		wcsDeadlock(t, f, `SELECT pg_advisory_xact_lock(hashtext($1))`, []any{"ctx-comment-seq:" + iss}, wcsRowLock, []any{iss}, func() wcsAnswer {
			return mcp(k, "issue_comment", map[string]any{"issue_id": iss, "content": "a comment"})
		}, "")
	})
	t.Run("manage guard-resolve with ids", func(t *testing.T) { // F6
		x := f.seed(t, "wcs-gr", "x")
		wcsDeadlock(t, f, wcsRowLock, []any{x}, wcsScopeLock, []any{"private"}, func() wcsAnswer {
			return rest(wcManage(t, h, k, map[string]any{"action": "guard-resolve", "data": map[string]any{"ids": []string{x}, "resolution": "archive"}}))
		}, "")
	})
	t.Run("MCP guard_resolve", func(t *testing.T) { // F4: the sweep after the review
		x := f.seed(t, "wcs-mgr", "x")
		wcsDeadlock(t, f, wcsRowLock, []any{x}, wcsScopeLock, []any{"private"}, func() wcsAnswer {
			return mcp(k, "guard_resolve", map[string]any{"ids": []string{x}, "resolution": "archive"})
		}, "")
	})
	t.Run("manage dream-link-resolve", func(t *testing.T) { // F6
		src := f.seed(t, "wcs-dl-src", "src")
		tgt := f.seed(t, "wcs-dl-tgt", "tgt")
		if _, err := f.pool.Exec(ctx, `INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, scope)
			VALUES ($1, $2, 'supersedes', 0.9, 'private')`, src, tgt); err != nil {
			t.Fatalf("link: %v", err)
		}
		wcsDeadlock(t, f, `SELECT 1 FROM context_dream_links WHERE source_block_id = $1 FOR UPDATE`, []any{src}, wcsScopeLock, []any{"private"}, func() wcsAnswer {
			return rest(wcManage(t, h, k, map[string]any{"action": "dream-link-resolve", "data": map[string]any{
				"source_id": src, "target_id": tgt, "relationship": "supersedes", "resolution": "delete"}}))
		}, "")
	})
	t.Run("MCP confirm of a staged archive", func(t *testing.T) { // F6
		x := f.seed(t, "wcs-ca", "x")
		hash := stage("archive", map[string]any{"id": x})
		wcsDeadlock(t, f, wcsRowLock, []any{x}, wcsScopeLock, []any{"private"}, func() wcsAnswer {
			return mcp(kc, "confirm", map[string]any{"payload_hash": hash})
		}, confirmArchivePrefix)
	})
	// A batch over two scopes takes their locks in sorted order (private,
	// work); the holder takes work's, the batch waits for it holding
	// private's, the holder asks for private's: the batch loses at a scope
	// lock, outside any item.
	twoScopes := []any{
		map[string]any{"category": "learnings", "title": "wcs-two-a", "content": "a", "scope": "private"},
		map[string]any{"category": "learnings", "title": "wcs-two-b", "content": "b", "scope": "work"},
	}
	t.Run("REST batch outside an item", func(t *testing.T) { // F6
		wcsDeadlock(t, f, wcsScopeLock, []any{"work"}, wcsScopeLock, []any{"private"}, func() wcsAnswer {
			return rest(f.restNoFatal(k, map[string]any{"items": twoScopes}))
		}, writeConflictMsg)
	})
	t.Run("MCP confirm of a staged batch outside an item", func(t *testing.T) { // F6
		hash := stage("store_batch", map[string]any{"items": twoScopes})
		wcsDeadlock(t, f, wcsScopeLock, []any{"work"}, wcsScopeLock, []any{"private"}, func() wcsAnswer {
			return mcp(kc, "confirm", map[string]any{"payload_hash": hash})
		}, confirmStoreBatchPrefix+writeConflictMsg)
	})
	t.Run("MCP stage check of a batch outside an item", func(t *testing.T) { // F6
		u := f.seed(t, "wcs-sc", "to update")
		wcsDeadlock(t, f, wcsScopeLock, []any{"work"}, wcsScopeLock, []any{"private"}, func() wcsAnswer {
			return mcp(kc, "store_batch", map[string]any{"items": []any{
				map[string]any{"id": u, "content": "updated"},
				map[string]any{"category": "learnings", "title": "wcs-sc-b", "content": "b", "scope": "work"}}})
		}, writeConflictMsg)
	})
}
