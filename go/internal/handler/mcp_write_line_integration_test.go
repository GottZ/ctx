//go:build integration

package handler

// Issue #453: an MCP write tool writes one server line per call — tool, the
// block it wrote, write_outcome, duration, client_gone — and the failure
// lines of the remaining write paths name the write's outcome
// (write_outcome): MCP store, update, archive, guard_resolve, the staged
// store_batch, REST guard-resolve with ids and ingest in block mode.
//
// The MCP calls run through the production chain (RequestID, Logger, Auth,
// NewMCPHandler) on a real httptest server over a real database. A write
// tool's context does not end when its client leaves (go-sdk hides the
// cancellation, mcp_cancel.go), so the tool runs to its end: the client-gone
// case below is a write that commits after the client left. A failed write
// is made with a trigger that raises on the fixture's rows.
//
//	go test -tags=integration ./internal/handler/ -run 'TestMCPWriteLine|TestWriteOutcomeFollowups' -count=1 -v

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// mcpLineFixture is a database, a booted registry and the MCP wiring the
// production server passes.
type mcpLineFixture struct {
	pool  *pgxpool.Pool
	reg   *blocktype.Registry
	cfg   MCPConfig
	plain string
}

func newMCPLineFixture(t *testing.T) *mcpLineFixture {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	cfgStore := staticConfigStore{cfg: &config.Config{
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute, BatchMaxItems: 100},
	}}
	return &mcpLineFixture{
		pool: pool, reg: reg, plain: abortKey(t, pool),
		cfg: MCPConfig{Pool: pool, Cfg: cfgStore, Blocktypes: reg},
	}
}

// serve serves the production MCP handler behind RequestID, Logger and auth;
// ar nil = the production Auth middleware, else ar injected as Auth does.
// reqCtx receives the request context of every request.
func (f *mcpLineFixture) serve(t *testing.T, ar *auth.AuthResult, reqCtx chan<- context.Context) (string, <-chan struct{}) {
	t.Helper()
	h := NewMCPHandler(f.cfg)
	if ar == nil {
		h = Auth(f.pool)(h)
	} else {
		inner := h
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inner.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authResultKey, ar)))
		})
	}
	if reqCtx != nil {
		inner := h
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case reqCtx <- r.Context():
			default:
			}
			inner.ServeHTTP(w, r)
		})
	}
	srv, done := abortServer(t, h, nil)
	return srv.URL, done
}

// mcpToolRequest builds the tools/call POST of tool with args.
func mcpToolRequest(t *testing.T, ctx context.Context, url, plain, tool string, args map[string]any) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if plain != "" {
		req.Header.Set("X-Context-Key", plain)
	}
	return req
}

// callTool makes one tools/call and returns the result's text and isError.
func callTool(t *testing.T, url, plain, tool string, args map[string]any) (string, bool) {
	t.Helper()
	resp, err := http.DefaultClient.Do(mcpToolRequest(t, context.Background(), url, plain, tool, args))
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &rpc); err != nil || rpc.Error != nil {
		t.Fatalf("%s: answer %d %s (%v)", tool, resp.StatusCode, raw, err)
	}
	text := ""
	if len(rpc.Result.Content) > 0 {
		text = rpc.Result.Content[0].Text
	}
	return text, rpc.Result.IsError
}

// toolLine returns the one tool line of the capture.
func toolLine(t *testing.T, logs *rejectLogCapture) rejectLogRecord {
	t.Helper()
	recs := logs.withMsg(toolWriteLineMsg)
	if len(recs) != 1 {
		t.Fatalf("want one %q line, got %d: %+v", toolWriteLineMsg, len(recs), logs.all())
	}
	return recs[0]
}

// failWritesOn makes every INSERT or UPDATE of table whose row matches when
// raise (SQLSTATE P0001) — the database refusing the write mid-transaction.
func failWritesOn(t *testing.T, pool *pgxpool.Pool, name, table, when string) {
	t.Helper()
	for _, q := range []string{
		fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN RAISE EXCEPTION 'i453 probe fault'; END $$`, name),
		fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT OR UPDATE ON %s FOR EACH ROW WHEN (%s) EXECUTE FUNCTION %s()`,
			name, table, when, name),
	} {
		if _, err := pool.Exec(context.Background(), q); err != nil {
			t.Fatalf("fault fixture: %v", err)
		}
	}
}

func blockIDByTitle(t *testing.T, pool *pgxpool.Pool, title string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`SELECT id::text FROM context_blocks WHERE title = $1 AND NOT is_archived`, title).Scan(&id); err != nil {
		t.Fatalf("read block %q: %v", title, err)
	}
	return id
}

func storeArgs(title, content string) map[string]any {
	return map[string]any{"category": "i453", "title": title, "content": content}
}

// TestMCPWriteLineStore — the direct store: one line per call, the outcome of
// the write, the client that left.
func TestMCPWriteLineStore(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newMCPLineFixture(t)

	t.Run("a written block", func(t *testing.T) {
		logs := installRejectLogCapture(t)
		url, done := f.serve(t, nil, nil)
		if text, isErr := callTool(t, url, f.plain, "store", storeArgs("i453 line written", "written once")); isErr {
			t.Fatalf("store answered an error: %s", text)
		}
		waitDone(t, done)
		line, req := toolLine(t, logs), requestLogOf(t, logs)
		id := blockIDByTitle(t, f.pool, "i453 line written")
		if line.level != slog.LevelInfo || line.attrs["tool"] != "store" || line.attrs["block_id"] != id ||
			line.attrs["write_outcome"] != writeCommitted || line.attrs["client_gone"] != nil {
			t.Errorf("tool line = %+v, want INFO tool=store block_id=%s write_outcome=committed without client_gone", line, id)
		}
		if _, ok := line.attrs["duration_ms"].(int64); !ok || line.attrs["request_id"] != req.attrs["request_id"] {
			t.Errorf("tool line = %+v, want duration_ms and the request line's request_id %v", line.attrs, req.attrs["request_id"])
		}
		for _, k := range []string{"title", "content", "category"} {
			if _, ok := line.attrs[k]; ok {
				t.Errorf("tool line carries %s: %+v", k, line.attrs)
			}
		}
	})

	t.Run("an unchanged block writes nothing", func(t *testing.T) {
		logs := installRejectLogCapture(t)
		url, done := f.serve(t, nil, nil)
		callTool(t, url, f.plain, "store", storeArgs("i453 line written", "written once"))
		waitDone(t, done)
		line := toolLine(t, logs)
		if line.attrs["write_outcome"] != writeNone || line.attrs["block_id"] != blockIDByTitle(t, f.pool, "i453 line written") {
			t.Errorf("tool line = %+v, want write_outcome=none with the unchanged block's id", line.attrs)
		}
	})

	t.Run("a refused write writes nothing", func(t *testing.T) {
		logs := installRejectLogCapture(t)
		url, done := f.serve(t, nil, nil)
		args := storeArgs("i453 line refused", "refused")
		args["scope"] = "i453-not-a-scope-of-the-key"
		if _, isErr := callTool(t, url, f.plain, "store", args); !isErr {
			t.Fatal("fixture: a foreign scope was written")
		}
		waitDone(t, done)
		if line := toolLine(t, logs); line.attrs["write_outcome"] != writeNone {
			t.Errorf("tool line = %+v, want write_outcome=none", line.attrs)
		}
	})

	t.Run("a failed write is an ERROR line with its outcome", func(t *testing.T) {
		failWritesOn(t, f.pool, "i453_fail_store", "context_blocks", `NEW.title = 'i453 fault store'`)
		logs := installRejectLogCapture(t)
		url, done := f.serve(t, nil, nil)
		if _, isErr := callTool(t, url, f.plain, "store", storeArgs("i453 fault store", "fails in the database")); !isErr {
			t.Fatal("fixture: the trigger did not refuse the write")
		}
		waitDone(t, done)
		errs := logs.withMsg("mcp: store error")
		if len(errs) != 1 || errs[0].level != slog.LevelError || errs[0].attrs["write_outcome"] != writeRolledBack {
			t.Errorf("store error line = %+v, want one ERROR line with write_outcome=rolled_back", errs)
		}
		if line := toolLine(t, logs); line.attrs["write_outcome"] != writeRolledBack {
			t.Errorf("tool line = %+v, want write_outcome=rolled_back", line.attrs)
		}
	})

	t.Run("the client leaves while the write waits: it commits, and the line says so", func(t *testing.T) {
		logs := installRejectLogCapture(t)
		reqCtx := make(chan context.Context, 1)
		url, done := f.serve(t, nil, reqCtx)
		release := lockedTx(t, f.pool, `LOCK TABLE context_blocks IN SHARE MODE`)
		cctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() {
			resp, err := http.DefaultClient.Do(mcpToolRequest(t, cctx, url, f.plain, "store", storeArgs("i453 line client gone", "the client left")))
			if err == nil {
				resp.Body.Close()
			}
			errc <- err
		}()
		waitLockWaiters(t, f.pool, 1)
		cancel()
		if err := <-errc; err == nil {
			t.Fatal("client: the call was answered while the write waited")
		}
		// The server has seen the client go before the write may go on.
		select {
		case rc := <-reqCtx:
			<-rc.Done()
		case <-time.After(10 * time.Second):
			t.Fatal("no request context")
		}
		release()
		waitDone(t, done)
		line := toolLine(t, logs)
		id := blockIDByTitle(t, f.pool, "i453 line client gone")
		if line.attrs["client_gone"] != true || line.attrs["write_outcome"] != writeCommitted || line.attrs["block_id"] != id {
			t.Errorf("tool line = %+v, want client_gone=true write_outcome=committed block_id=%s", line.attrs, id)
		}
		req := requestLogOf(t, logs)
		if statusOf(t, req) != clientClosedStatus || req.attrs["handler_status"] != int64(handlerWroteNothing) {
			t.Errorf("request line = %+v, want 499 with handler_status 0", req.attrs)
		}
		if errs := errorLines(logs); len(errs) != 0 {
			t.Errorf("a write that committed after its client left produced ERROR lines: %+v", errs)
		}
	})
}

// TestWriteOutcomeFollowups — write_outcome on the failure lines the
// comment on #453 lists: MCP update, archive, guard_resolve and the staged
// store_batch (a write the database refuses mid-transaction), REST
// guard-resolve with ids and ingest in block mode (a client that leaves while
// the write waits inside its transaction).
func TestWriteOutcomeFollowups(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newMCPLineFixture(t)
	ctx := context.Background()
	seed := func(title string) string {
		t.Helper()
		b, err := store.UpsertBlock(ctx, f.pool, "i453", title, "seeded: "+title, nil, nil, "private", false, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("seed %q: %v", title, err)
		}
		return b.ID
	}
	// guard_status is the Guard's own flag; no client path sets it.
	flag := func(id string) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, `UPDATE context_blocks SET guard_status = 'needs_review' WHERE id = $1::uuid`, id); err != nil {
			t.Fatalf("flag %s: %v", id, err)
		}
	}
	updateID, archiveID, guardID := seed("i453 fault update"), seed("i453 fault archive"), seed("i453 fault guard")
	flag(guardID)
	restGuardID := seed("i453 guard rest")
	flag(restGuardID)
	failWritesOn(t, f.pool, "i453_fail_blocks", "context_blocks", `NEW.title LIKE 'i453 fault%'`)

	mcpFault := func(t *testing.T, tool string, args map[string]any, errMsg, blockID string, count int) {
		t.Helper()
		logs := installRejectLogCapture(t)
		url, done := f.serve(t, nil, nil)
		if text, isErr := callTool(t, url, f.plain, tool, args); !isErr {
			t.Fatalf("fixture: %s answered %q, want the refused write's error", tool, text)
		}
		waitDone(t, done)
		recs := logs.withMsg(errMsg)
		if len(recs) != 1 || recs[0].level != slog.LevelError || recs[0].attrs["write_outcome"] != writeRolledBack {
			t.Errorf("%q = %+v, want one ERROR line with write_outcome=rolled_back", errMsg, recs)
		}
		line := toolLine(t, logs)
		if line.attrs["tool"] != tool || line.attrs["write_outcome"] != writeRolledBack {
			t.Errorf("tool line = %+v, want tool=%s write_outcome=rolled_back", line.attrs, tool)
		}
		if blockID != "" && line.attrs["block_id"] != blockID {
			t.Errorf("tool line block_id = %v, want %s", line.attrs["block_id"], blockID)
		}
		if count > 0 && line.attrs["count"] != int64(count) {
			t.Errorf("tool line count = %v, want %d", line.attrs["count"], count)
		}
	}

	t.Run("MCP update", func(t *testing.T) {
		mcpFault(t, "update", map[string]any{"id": updateID, "content": "refused by the database"}, "mcp: update error", updateID, 0)
	})
	t.Run("MCP archive", func(t *testing.T) {
		mcpFault(t, "archive", map[string]any{"id": archiveID}, "mcp: archive error", archiveID, 0)
	})
	t.Run("MCP guard_resolve", func(t *testing.T) {
		mcpFault(t, "guard_resolve", map[string]any{"ids": []string{guardID}, "resolution": "keep"}, "mcp: guard_resolve error", "", 1)
	})

	t.Run("MCP store_batch staged", func(t *testing.T) {
		failWritesOn(t, f.pool, "i453_fail_stage", "context_pending_writes", `NEW.op = 'store_batch'`)
		ar, err := auth.AuthenticateRaw(ctx, f.pool, f.plain)
		if err != nil || !ar.IsValid {
			t.Fatalf("authenticate fixture key: %v", err)
		}
		ar.ConfirmWrites = true
		logs := installRejectLogCapture(t)
		url, done := f.serve(t, ar, nil)
		args := map[string]any{"items": []map[string]any{storeArgs("i453 staged one", "one"), storeArgs("i453 staged two", "two")}}
		if _, isErr := callTool(t, url, "", "store_batch", args); !isErr {
			t.Fatal("fixture: the stage was not refused")
		}
		waitDone(t, done)
		recs := logs.withMsg("mcp: stage pending batch failed")
		if len(recs) != 1 || recs[0].level != slog.LevelError || recs[0].attrs["write_outcome"] != writeRolledBack {
			t.Errorf("stage line = %+v, want one ERROR line with write_outcome=rolled_back", recs)
		}
		line := toolLine(t, logs)
		if line.attrs["write_outcome"] != writeRolledBack || line.attrs["staged"] != true || line.attrs["count"] != int64(2) {
			t.Errorf("tool line = %+v, want write_outcome=rolled_back staged=true count=2", line.attrs)
		}
	})

	t.Run("REST guard-resolve with ids", func(t *testing.T) {
		mh := mcHandler(t, f.pool, false, false)
		logs := installRejectLogCapture(t)
		srv, done := abortRoute(t, f.pool, mh.HandleManage)
		release := lockedTx(t, f.pool, `SELECT 1 FROM context_blocks WHERE id = $1::uuid FOR UPDATE`, restGuardID)
		body := fmt.Sprintf(`{"action":"guard-resolve","data":{"resolution":"keep","ids":[%q]}}`, restGuardID)
		abortAfter(t, srv.URL+"/api/manage", f.plain, body, func() { waitLockWaiters(t, f.pool, 1) })
		waitDone(t, done)
		release()
		assertClientGone(t, logs)
		if got := writeOutcomeOf(t, logs, "manage: guard-resolve batch error"); got != writeRolledBack {
			t.Errorf("write_outcome = %q, want %s", got, writeRolledBack)
		}
	})

	t.Run("ingest in block mode", func(t *testing.T) {
		ih := NewIngestHandler(f.pool, nil, f.reg)
		logs := installRejectLogCapture(t)
		srv, done := abortRoute(t, f.pool, ih.HandleIngest)
		release := lockedTx(t, f.pool, `LOCK TABLE context_blocks IN SHARE MODE`)
		raw, _ := json.Marshal(map[string]any{"source": "i453", "chunks": []map[string]any{
			{"category": "i453", "title": "i453 ingest abort", "content": "the client leaves while the upsert waits"}}})
		abortAfter(t, srv.URL+"/api/ingest", f.plain, string(raw), func() { waitLockWaiters(t, f.pool, 1) })
		waitDone(t, done)
		release()
		assertClientGone(t, logs)
		if got := writeOutcomeOf(t, logs, "ingest: upsert block error"); got != writeRolledBack {
			t.Errorf("write_outcome = %q, want %s", got, writeRolledBack)
		}
	})
}

// TestMCPWriteLineEveryWriteToolCommits — each write tool's line names its
// committed write and what it wrote: the block, the blob or the number of
// items; a staged card says staged=true, and its confirm commits the block.
func TestMCPWriteLineEveryWriteToolCommits(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newMCPLineFixture(t)
	ctx := context.Background()
	call := func(t *testing.T, ar *auth.AuthResult, tool string, args map[string]any) (string, rejectLogRecord) {
		t.Helper()
		logs := installRejectLogCapture(t)
		url, done := f.serve(t, ar, nil)
		plain := f.plain
		if ar != nil {
			plain = ""
		}
		text, isErr := callTool(t, url, plain, tool, args)
		waitDone(t, done)
		line := toolLine(t, logs)
		if line.attrs["tool"] != tool || (line.attrs["staged"] != true && isErr) {
			t.Fatalf("%s answered %q (isError %v); line %+v", tool, text, isErr, line.attrs)
		}
		return text, line
	}
	want := func(t *testing.T, line rejectLogRecord, key string, val any) {
		t.Helper()
		if line.attrs["write_outcome"] != writeCommitted || line.attrs[key] != val {
			t.Errorf("line %+v, want write_outcome=committed and %s=%v", line.attrs, key, val)
		}
	}
	jsonID := func(t *testing.T, text, key string) string {
		t.Helper()
		var env map[string]any
		if err := json.Unmarshal([]byte(text), &env); err != nil {
			t.Fatalf("answer %q: %v", text, err)
		}
		if key != "" {
			env, _ = env[key].(map[string]any)
		}
		id, _ := env["id"].(string)
		return id
	}

	t.Run("store_batch", func(t *testing.T) {
		_, line := call(t, nil, "store_batch", map[string]any{"items": []map[string]any{
			storeArgs("i453 batch one", "one"), storeArgs("i453 batch two", "two")}})
		want(t, line, "count", int64(2))
	})
	target := blockIDByTitle(t, f.pool, "i453 batch one")
	t.Run("update", func(t *testing.T) {
		_, line := call(t, nil, "update", map[string]any{"id": target, "content": "changed"})
		want(t, line, "block_id", target)
	})
	t.Run("guard_resolve", func(t *testing.T) {
		if _, err := f.pool.Exec(ctx, `UPDATE context_blocks SET guard_status = 'needs_review' WHERE id = $1::uuid`, target); err != nil {
			t.Fatal(err)
		}
		_, line := call(t, nil, "guard_resolve", map[string]any{"ids": []string{target}, "resolution": "keep"})
		want(t, line, "count", int64(1))
	})
	t.Run("archive", func(t *testing.T) {
		_, line := call(t, nil, "archive", map[string]any{"id": target})
		want(t, line, "block_id", target)
	})
	var issueID string
	t.Run("issue_create", func(t *testing.T) {
		text, line := call(t, nil, "issue_create", map[string]any{"title": "i453 issue"})
		issueID = jsonID(t, text, "issue")
		want(t, line, "block_id", issueID)
	})
	t.Run("issue_comment", func(t *testing.T) {
		text, line := call(t, nil, "issue_comment", map[string]any{"issue_id": issueID, "content": "a comment"})
		want(t, line, "block_id", jsonID(t, text, "comment"))
	})
	t.Run("issue_state", func(t *testing.T) {
		_, line := call(t, nil, "issue_state", map[string]any{"issue_id": issueID, "status": "in-progress"})
		want(t, line, "block_id", issueID)
	})
	var blobID string
	t.Run("blob_store", func(t *testing.T) {
		text, line := call(t, nil, "blob_store", map[string]any{"category": "i453", "title": "i453 blob",
			"filename": "probe.txt", "mime_type": "text/plain", "text": "blob payload"})
		blobID = jsonID(t, text, "")
		want(t, line, "blob_id", blobID)
	})
	t.Run("blob_link", func(t *testing.T) {
		_, line := call(t, nil, "blob_link", map[string]any{"id": blobID, "context_block_id": blockIDByTitle(t, f.pool, "i453 batch two")})
		want(t, line, "blob_id", blobID)
	})
	t.Run("staged store and its confirm", func(t *testing.T) {
		ar, err := auth.AuthenticateRaw(ctx, f.pool, f.plain)
		if err != nil || !ar.IsValid {
			t.Fatalf("authenticate fixture key: %v", err)
		}
		ar.ConfirmWrites = true
		text, line := call(t, ar, "store", storeArgs("i453 staged store", "staged"))
		if line.attrs["write_outcome"] != writeCommitted || line.attrs["staged"] != true {
			t.Errorf("stage line %+v, want write_outcome=committed staged=true", line.attrs)
		}
		hash := ""
		for _, l := range strings.Split(text, "\n") {
			if h, ok := strings.CutPrefix(l, "payload_hash: "); ok {
				hash = h
			}
		}
		_, line = call(t, ar, "confirm", map[string]any{"payload_hash": hash})
		want(t, line, "block_id", blockIDByTitle(t, f.pool, "i453 staged store"))
		if line.attrs["staged"] != nil {
			t.Errorf("confirm line %+v carries staged", line.attrs)
		}
	})
}

// TestMCPWriteLineReviewFixes — the line names only ids the database
// resolved (an argument that is no id never reaches it), and a call that
// changed nothing says none on every tool.
func TestMCPWriteLineReviewFixes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newMCPLineFixture(t)
	call := func(t *testing.T, tool string, args map[string]any) rejectLogRecord {
		t.Helper()
		logs := installRejectLogCapture(t)
		url, done := f.serve(t, nil, nil)
		callTool(t, url, f.plain, tool, args)
		waitDone(t, done)
		return toolLine(t, logs)
	}
	url, done := f.serve(t, nil, nil)
	callTool(t, url, f.plain, "store", storeArgs("i453 review target", "target"))
	waitDone(t, done)
	target := blockIDByTitle(t, f.pool, "i453 review target")

	t.Run("blob_link with an argument that is no id", func(t *testing.T) {
		raw := "not-a-uuid " + strings.Repeat("A", 200)
		line := call(t, "blob_link", map[string]any{"id": raw, "context_block_id": target})
		if line.attrs["blob_id"] != nil || strings.Contains(line.text(), raw) {
			t.Errorf("tool line %+v carries the client's argument", line.attrs)
		}
		if line.attrs["write_outcome"] != writeNone {
			t.Errorf("tool line %+v, want write_outcome=none", line.attrs)
		}
	})
	t.Run("blob_link names the blob as stored, not the argument's spelling", func(t *testing.T) {
		url, done := f.serve(t, nil, nil)
		text, isErr := callTool(t, url, f.plain, "blob_store", map[string]any{"category": "i453", "title": "i453 review blob",
			"filename": "probe.txt", "mime_type": "text/plain", "text": "blob payload"})
		waitDone(t, done)
		var env struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(text), &env); isErr || err != nil || env.ID == "" {
			t.Fatalf("blob_store answered %q (%v)", text, err)
		}
		line := call(t, "blob_link", map[string]any{"id": strings.ToUpper(env.ID), "context_block_id": target})
		if line.attrs["write_outcome"] != writeCommitted || line.attrs["blob_id"] != env.ID {
			t.Errorf("tool line %+v, want write_outcome=committed and blob_id=%s (the stored id)", line.attrs, env.ID)
		}
	})
	t.Run("store_batch whose items are all unchanged", func(t *testing.T) {
		line := call(t, "store_batch", map[string]any{"items": []map[string]any{storeArgs("i453 review target", "target")}})
		if line.attrs["write_outcome"] != writeNone {
			t.Errorf("tool line %+v, want write_outcome=none (nothing was written)", line.attrs)
		}
	})
	t.Run("guard_resolve whose ids are all skipped", func(t *testing.T) {
		line := call(t, "guard_resolve", map[string]any{"ids": []string{target}, "resolution": "keep"})
		if line.attrs["write_outcome"] != writeNone {
			t.Errorf("tool line %+v, want write_outcome=none (the block is not flagged, nothing was written)", line.attrs)
		}
	})
}
