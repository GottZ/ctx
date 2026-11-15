//go:build integration

// Issue #280 integration probes (real PG18 testcontainer).
//
//   - TestRetypeUntrustedOrigin: a re-type out of a type marked untrusted is
//     accepted on every write surface — REST manage update, the MCP update tool
//     (direct, and staged + confirmed), the store upsert (REST /api/store, MCP
//     store direct and staged) and the auto-classifier — and records the type it
//     left as untrusted_origin. The record is the FIRST such type, survives later
//     re-types and edits, cannot be written by a statement, and reaches every
//     read surface: manage get, MCP get, MCP search, MCP recent (text), REST
//     search and the /api/query sources. A block that never left such a type
//     answers without the key.
//   - TestRetypeEmptyType: manage update `{"type": ""}` is refused and the row
//     keeps its type (point 2).
//   - TestRetypeBoundBlocks: an issue or a comment keeps its type on every
//     surface (a parent alone pins nothing), and `issue` cannot be claimed by a
//     generic write (point 3).
//
// Every probe reads the SERIALIZED answer (JSON maps, tool text) or the row, so
// the file compiles against the tree before the change and fails there at run
// time.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run 'TestRetype' -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
)

// roNote is the text-surface wording of a record, pinned verbatim.
const roNote = "untrusted_origin:tool-evidence (agent-produced text: re-typed out of a type marked untrusted)"

// roOrigin reads the stored record ("" for NULL).
func roOrigin(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var origin string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(untrusted_origin, '') FROM context_blocks WHERE id = $1`, id).Scan(&origin); err != nil {
		t.Fatalf("read untrusted_origin of %s: %v", id, err)
	}
	return origin
}

// roStoreHandler is the REST /api/store handler over the fixture's registry.
func roStoreHandler(f *i201Fixture) *StoreHandler {
	return NewStoreHandler(f.pool, staticConfigStore{cfg: &config.Config{
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
	}}, f.cfg.Blocktypes)
}

// roRestStore posts one /api/store body as ar and returns status and the map.
func roRestStore(t *testing.T, h *StoreHandler, ar *auth.AuthResult, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/store", bytes.NewReader(raw))
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.HandleStore(rec, req)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

// roBlockField reads one key of resp["block"]; present=false when absent.
func roBlockField(resp map[string]any, key string) (any, bool) {
	blk, _ := resp["block"].(map[string]any)
	v, ok := blk[key]
	return v, ok
}

func TestRetypeUntrustedOrigin(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI201Fixture(t)
	ctx := context.Background()
	i203HookTenantRegistry(t)
	i203CreateType(t, f.pool, "ro-foreign-untrusted", "i201-b", i203UntrustedConfig)

	directAR := AuthResultFromContext(f.direct)
	mh := NewManageHandler(f.pool, nil, nil, nil, nil, nil, nil, f.cfg.Blocktypes)
	sh := roStoreHandler(f)

	t.Run("REST manage update records the type it left, the first one, for good", func(t *testing.T) {
		id := i203StoreTyped(t, f, f.direct, "ro-rest-evidence", "tool-evidence")
		code, resp := callManage(t, mh, directAR, "update", id, map[string]any{"type": "knowledge"})
		if code != http.StatusOK || resp["success"] != true {
			t.Fatalf("re-type tool-evidence → knowledge: code %d resp %v, want accepted", code, resp)
		}
		if v, _ := roBlockField(resp, "untrusted_origin"); v != "tool-evidence" {
			t.Errorf("update answer untrusted_origin = %v, want tool-evidence", v)
		}
		if v, _ := roBlockField(resp, "type"); v != "knowledge" {
			t.Errorf("update answer type = %v, want knowledge", v)
		}
		// Through another untrusted type and out again: the first record stays.
		for _, next := range []string{"checkpoint", "reference"} {
			if code, resp := callManage(t, mh, directAR, "update", id, map[string]any{"type": next}); code != http.StatusOK || resp["success"] != true {
				t.Fatalf("re-type → %s: code %d resp %v", next, code, resp)
			}
		}
		// Edits of every other field leave it, metadata included.
		if code, resp := callManage(t, mh, directAR, "update", id, map[string]any{
			"title": "ro-rest-evidence renamed", "content": "rewritten by a person", "tags": []string{"x"},
			"metadata": map[string]any{"untrusted_origin": nil, "note": "edited"},
		}); code != http.StatusOK || resp["success"] != true {
			t.Fatalf("edit: code %d resp %v", code, resp)
		}
		if got := roOrigin(t, f.pool, id); got != "tool-evidence" {
			t.Errorf("record after re-types and edits = %q, want tool-evidence", got)
		}
		// No statement writes the column, not even a direct one.
		if _, err := f.pool.Exec(ctx, `UPDATE context_blocks SET untrusted_origin = NULL WHERE id = $1`, id); err != nil {
			t.Fatalf("direct clear: %v", err)
		}
		if _, err := f.pool.Exec(ctx, `UPDATE context_blocks SET untrusted_origin = 'knowledge' WHERE id = $1`, id); err != nil {
			t.Fatalf("direct overwrite: %v", err)
		}
		if got := roOrigin(t, f.pool, id); got != "tool-evidence" {
			t.Errorf("record after direct writes = %q, want tool-evidence", got)
		}
		code, resp = callManage(t, mh, directAR, "get", id, nil)
		if code != http.StatusOK {
			t.Fatalf("manage get: code %d resp %v", code, resp)
		}
		if v, _ := roBlockField(resp, "untrusted_origin"); v != "tool-evidence" {
			t.Errorf("manage get untrusted_origin = %v, want tool-evidence", v)
		}
	})

	t.Run("a block that never left such a type answers without the key", func(t *testing.T) {
		id := i203StoreTyped(t, f, f.direct, "ro-rest-plain", "")
		code, resp := callManage(t, mh, directAR, "update", id, map[string]any{"type": "reference"})
		if code != http.StatusOK || resp["success"] != true {
			t.Fatalf("re-type knowledge → reference: code %d resp %v", code, resp)
		}
		if v, ok := roBlockField(resp, "untrusted_origin"); ok {
			t.Errorf("update answer carries untrusted_origin = %v for a trusted origin, want the key absent", v)
		}
		// Moving INTO an untrusted type records nothing either.
		if code, resp := callManage(t, mh, directAR, "update", id, map[string]any{"type": "tool-evidence"}); code != http.StatusOK || resp["success"] != true {
			t.Fatalf("re-type → tool-evidence: code %d resp %v", code, resp)
		}
		if got := roOrigin(t, f.pool, id); got != "" {
			t.Errorf("record = %q, want none", got)
		}
	})

	t.Run("the MCP update tool accepts it, direct and staged", func(t *testing.T) {
		for keyName, keyCtx := range map[string]context.Context{"direct": f.direct, "flagged": f.flagged} {
			id := i203StoreTyped(t, f, f.direct, "ro-mcp-"+keyName, "checkpoint")
			a := f.call(t, keyCtx, "update", map[string]any{"id": id, "type": "knowledge"})
			if a.RPCError != "" {
				t.Fatalf("%s: JSON-RPC error %s", keyName, a.RPCError)
			}
			if keyName == "flagged" {
				m := hashRe.FindStringSubmatch(a.Text)
				if !a.IsError || a.Code != "" || m == nil {
					t.Fatalf("flagged: %+v, want an uncoded STAGED card", a)
				}
				a = f.call(t, f.flagged, "confirm", map[string]any{"payload_hash": m[1]})
			}
			if a.IsError {
				t.Fatalf("%s: checkpoint → knowledge: %+v, want accepted", keyName, a)
			}
			if name, source := i203TypeOf(t, f.pool, id); name != "knowledge" || source != "manual" {
				t.Errorf("%s: type = (%q, %q), want (knowledge, manual)", keyName, name, source)
			}
			if got := roOrigin(t, f.pool, id); got != "checkpoint" {
				t.Errorf("%s: record = %q, want checkpoint", keyName, got)
			}
		}
	})

	t.Run("the store upsert records it on REST and MCP, direct and staged", func(t *testing.T) {
		// REST /api/store onto the existing (category, title, scope).
		restID := i203StoreTyped(t, f, f.direct, "ro-upsert-rest", "tool-overview")
		code, resp := roRestStore(t, sh, directAR, map[string]any{
			"category": "test", "title": "ro-upsert-rest", "content": "accepted as knowledge", "type": "knowledge",
		})
		if code != http.StatusOK {
			t.Fatalf("REST upsert re-type: code %d resp %v", code, resp)
		}
		if got := roOrigin(t, f.pool, restID); got != "tool-overview" {
			t.Errorf("REST upsert record = %q, want tool-overview", got)
		}
		// MCP store, direct and through the confirm of a staged card.
		for keyName, keyCtx := range map[string]context.Context{"direct": f.direct, "flagged": f.flagged} {
			id := i203StoreTyped(t, f, f.direct, "ro-upsert-"+keyName, "checkpoint")
			a := f.call(t, keyCtx, "store", map[string]any{
				"category": "test", "title": "ro-upsert-" + keyName, "content": "taken up", "type": "knowledge", "sensitivity": "internal",
			})
			if keyName == "flagged" {
				m := hashRe.FindStringSubmatch(a.Text)
				if m == nil {
					t.Fatalf("flagged store: %+v, want a STAGED card", a)
				}
				a = f.call(t, f.flagged, "confirm", map[string]any{"payload_hash": m[1]})
			}
			if a.RPCError != "" || a.IsError {
				t.Fatalf("%s MCP upsert re-type: %+v", keyName, a)
			}
			if got := roOrigin(t, f.pool, id); got != "checkpoint" {
				t.Errorf("%s MCP upsert record = %q, want checkpoint", keyName, got)
			}
		}
	})

	t.Run("the auto-classifier's re-type records it as well", func(t *testing.T) {
		id := i203StoreTyped(t, f, f.direct, "Compaction source ro-classifier", "")
		if name, source := i203TypeOf(t, f.pool, id); name != "checkpoint" || source != "auto" {
			t.Fatalf("fixture type = (%q, %q), want (checkpoint, auto)", name, source)
		}
		// is_meta classifies to system-meta (priority 10, ahead of checkpoint's 30).
		if code, resp := callManage(t, mh, directAR, "update", id, map[string]any{"metadata": map[string]any{"is_meta": true}}); code != http.StatusOK || resp["success"] != true {
			t.Fatalf("metadata update: code %d resp %v", code, resp)
		}
		if name, source := i203TypeOf(t, f.pool, id); name != "system-meta" || source != "auto" {
			t.Fatalf("after re-classify type = (%q, %q), want (system-meta, auto)", name, source)
		}
		if got := roOrigin(t, f.pool, id); got != "checkpoint" {
			t.Errorf("classifier re-type record = %q, want checkpoint", got)
		}
	})

	t.Run("another tenant's registry rows decide nothing, an orphan records nothing", func(t *testing.T) {
		// No production write leaves a block of this key on another tenant's
		// type name or on an unregistered one; the names are planted by SQL.
		// Were the other tenant's row read, the record would tell this key
		// that some tenant holds an untrusted type of that name.
		foreign := i203StoreTyped(t, f, f.direct, "ro-foreign", "")
		orphan := i203StoreTyped(t, f, f.direct, "ro-orphan", "")
		for id, name := range map[string]string{foreign: "ro-foreign-untrusted", orphan: "ro-orphan-type"} {
			if _, err := f.pool.Exec(ctx, `UPDATE context_blocks SET type_name = $2 WHERE id = $1`, id, name); err != nil {
				t.Fatalf("plant %s: %v", name, err)
			}
			if a := f.call(t, f.direct, "update", map[string]any{"id": id, "type": "knowledge"}); a.RPCError != "" || a.IsError {
				t.Fatalf("%s → knowledge: %+v, want accepted", name, a)
			}
		}
		if got := roOrigin(t, f.pool, foreign); got != "" {
			t.Errorf("record of the block on another tenant's type name = %q, want none — that row is not this block's registry", got)
		}
		if got := roOrigin(t, f.pool, orphan); got != "" {
			t.Errorf("record of the orphan block = %q, want none — no registry row marks that name", got)
		}

		// The tenant's own type, registered under the namespace "private",
		// counts on a block in another scope of the same tenant ("work").
		i203CreateType(t, f.pool, "ro-own-untrusted", "private", i203UntrustedConfig)
		b, err := store.UpsertBlock(ctx, f.pool, "test", "ro-own-work", "a tool capture", nil, nil, "work", true,
			store.SensitivityWrite{Value: backends.SensPublic}, "")
		if err != nil {
			t.Fatalf("seed work block: %v", err)
		}
		if _, err := f.pool.Exec(ctx, `UPDATE context_blocks SET type_name = 'ro-own-untrusted' WHERE id = $1`, b.ID); err != nil {
			t.Fatalf("plant own type: %v", err)
		}
		typ := "knowledge"
		if _, _, err := store.UpdateBlock(ctx, f.pool, b.ID, store.UpdateBlockData{Type: &typ}, []string{"work"}); err != nil {
			t.Fatalf("re-type in work: %v", err)
		}
		if got := roOrigin(t, f.pool, b.ID); got != "ro-own-untrusted" {
			t.Errorf("record of the tenant's own type in another of its scopes = %q, want ro-own-untrusted", got)
		}
	})

	t.Run("the read surfaces carry it", func(t *testing.T) {
		withID := i203StoreTyped(t, f, f.direct, "ro-read-with", "tool-evidence")
		withoutID := i203StoreTyped(t, f, f.direct, "ro-read-without", "")
		if code, resp := callManage(t, mh, directAR, "update", withID, map[string]any{"type": "knowledge", "category": "ro-read"}); code != http.StatusOK || resp["success"] != true {
			t.Fatalf("re-type: code %d resp %v", code, resp)
		}
		if code, resp := callManage(t, mh, directAR, "update", withoutID, map[string]any{"category": "ro-read"}); code != http.StatusOK || resp["success"] != true {
			t.Fatalf("move: code %d resp %v", code, resp)
		}

		// MCP get: the Block JSON.
		a := f.call(t, f.direct, "get", map[string]any{"id": withID})
		var blk map[string]any
		if err := json.Unmarshal([]byte(a.Text), &blk); err != nil || blk["untrusted_origin"] != "tool-evidence" {
			t.Errorf("MCP get untrusted_origin = %v (err %v) in %s, want tool-evidence", blk["untrusted_origin"], err, a.Text)
		}

		// MCP search: the JSON rows, compact.
		a = f.call(t, f.direct, "search", map[string]any{"category": "ro-read", "limit": 10})
		var rows []map[string]any
		if err := json.Unmarshal([]byte(a.Text), &rows); err != nil {
			t.Fatalf("MCP search: %v in %s", err, a.Text)
		}
		seen := map[string]any{}
		for _, r := range rows {
			if v, ok := r["untrusted_origin"]; ok {
				seen[r["id"].(string)] = v
			}
		}
		if seen[withID] != "tool-evidence" {
			t.Errorf("MCP search row untrusted_origin = %v, want tool-evidence (%s)", seen[withID], a.Text)
		}
		if _, ok := seen[withoutID]; ok {
			t.Errorf("MCP search row of the trusted block carries untrusted_origin")
		}

		// MCP recent: the text, the note on a line of its own under the row.
		a = f.call(t, f.direct, "recent", map[string]any{"category": "ro-read", "limit": 10})
		if !strings.Contains(a.Text, "id:"+withID+"\n    "+roNote+"\n    ") {
			t.Errorf("MCP recent lacks the note line under %s:\n%s", withID, a.Text)
		}
		if strings.Count(a.Text, "untrusted_origin:") != 1 {
			t.Errorf("MCP recent carries %d notes, want 1:\n%s", strings.Count(a.Text, "untrusted_origin:"), a.Text)
		}

		// REST search: the JSON rows.
		srh := NewSearchHandler(f.pool, staticConfigStore{cfg: &config.Config{}}, f.cfg.Blocktypes)
		body, _ := json.Marshal(map[string]any{"category": "ro-read", "limit": 10})
		req := httptest.NewRequest(http.MethodPost, "/api/search", bytes.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, directAR))
		rec := httptest.NewRecorder()
		srh.HandleSearch(rec, req)
		var sresp struct {
			Results []map[string]any `json:"results"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &sresp); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("REST search: code %d err %v body %s", rec.Code, err, rec.Body.String())
		}
		var restGot any
		for _, r := range sresp.Results {
			if r["id"] == withID {
				restGot = r["untrusted_origin"]
			}
		}
		if restGot != "tool-evidence" {
			t.Errorf("REST search row untrusted_origin = %v, want tool-evidence", restGot)
		}
	})

	t.Run("the query sources carry it", func(t *testing.T) {
		// The sd fixture's embed stub answers only the question; the block
		// carries the same vector, so the semantic arm returns it.
		id := i203StoreTyped(t, f, f.direct, "ro-query", "tool-evidence")
		if code, resp := callManage(t, mh, directAR, "update", id, map[string]any{"type": "knowledge"}); code != http.StatusOK || resp["success"] != true {
			t.Fatalf("re-type: code %d resp %v", code, resp)
		}
		if err := store.StoreEmbedding(ctx, f.pool, id, "test-embed", sdQueryVector()); err != nil {
			t.Fatalf("embed: %v", err)
		}
		srv := sdQueryEmbedServer(t)
		qh := NewQueryHandler(f.pool, config.NewStore(w023Config(false)), embedPool(srv.URL), nil, f.cfg.Blocktypes, snapshotTestAdmitter(t))
		body, _ := json.Marshal(map[string]any{"query": "what is the " + sdQuestionMarker + " policy", "synthesize": false, "limit": 10})
		req := httptest.NewRequest(http.MethodPost, "/api/query", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, directAR))
		rec := httptest.NewRecorder()
		qh.HandleQuery(rec, req)
		var qresp struct {
			Sources []map[string]any `json:"sources"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &qresp); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("query: code %d err %v body %s", rec.Code, err, rec.Body.String())
		}
		var found map[string]any
		for _, s := range qresp.Sources {
			if s["id"] == id {
				found = s
			}
			// Every source answers exactly its row's record, absent for none.
			v, ok := s["untrusted_origin"]
			if want := roOrigin(t, f.pool, s["id"].(string)); (want == "" && ok) || (want != "" && v != want) {
				t.Errorf("source %v untrusted_origin = %v (present %v), want %q", s["id"], v, ok, want)
			}
		}
		if found == nil {
			t.Fatalf("fixture broken: the re-typed block is not among the sources: %s", rec.Body.String())
		}
		if found["untrusted_origin"] != "tool-evidence" {
			t.Errorf("query source untrusted_origin = %v, want tool-evidence", found["untrusted_origin"])
		}
	})
}

func TestRetypeEmptyType(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI201Fixture(t)
	directAR := AuthResultFromContext(f.direct)
	mh := NewManageHandler(f.pool, nil, nil, nil, nil, nil, nil, f.cfg.Blocktypes)

	id := i203StoreTyped(t, f, f.direct, "ro-empty-type", "")
	status, code, msg, ok := i203RestUpdate(t, mh, directAR, id, map[string]any{"type": ""})
	if ok || status != http.StatusUnprocessableEntity || code != "unknown_type" {
		t.Errorf(`manage update {"type": ""} = (status %d, code %q, success %v, %q), want (422, unknown_type, false)`, status, code, ok, msg)
	}
	if name, source := i203TypeOf(t, f.pool, id); name != "knowledge" || source != "auto" {
		t.Errorf("type = (%q, %q), want unchanged (knowledge, auto)", name, source)
	}
}

func TestRetypeBoundBlocks(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI201Fixture(t)
	ctx := context.Background()
	directAR := AuthResultFromContext(f.direct)
	mh := NewManageHandler(f.pool, nil, nil, nil, nil, nil, nil, f.cfg.Blocktypes)
	sh := roStoreHandler(f)

	code, resp := callManage(t, mh, directAR, "issue-create", "", map[string]any{"title": "ro bound issue", "content": "an issue body"})
	if code != http.StatusOK {
		t.Fatalf("issue-create: code %d resp %v", code, resp)
	}
	issueID := resp["issue"].(map[string]any)["id"].(string)
	code, resp = callManage(t, mh, directAR, "issue-comment-create", "", map[string]any{"parent_id": issueID, "content": "a comment"})
	if code != http.StatusOK {
		t.Fatalf("issue-comment-create: code %d resp %v", code, resp)
	}
	commentID := resp["comment"].(map[string]any)["id"].(string)

	type row struct{ typeName, status, parent, category, title string }
	read := func(id string) row {
		t.Helper()
		var r row
		if err := f.pool.QueryRow(ctx,
			`SELECT type_name, COALESCE(workflow_status, ''), COALESCE(parent_id::text, ''), category, title
			   FROM context_blocks WHERE id = $1`, id).Scan(&r.typeName, &r.status, &r.parent, &r.category, &r.title); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return r
	}
	issueBefore, commentBefore := read(issueID), read(commentID)
	if issueBefore.typeName != "issue" || issueBefore.status == "" || commentBefore.typeName != "comment" || commentBefore.parent != issueID {
		t.Fatalf("fixture rows: issue %+v comment %+v", issueBefore, commentBefore)
	}

	t.Run("an issue and a comment keep their type on every update surface", func(t *testing.T) {
		for _, id := range []string{issueID, commentID} {
			status, code, msg, ok := i203RestUpdate(t, mh, directAR, id, map[string]any{"type": "knowledge"})
			if ok || status != http.StatusConflict || code != "constraint" {
				t.Errorf("REST re-type of %s = (status %d, code %q, success %v, %q), want (409, constraint, false)", id, status, code, ok, msg)
			}
			a := f.call(t, f.direct, "update", map[string]any{"id": id, "type": "knowledge"})
			if a.RPCError != "" || !a.IsError || a.Code != "constraint" || a.Text != msg {
				t.Errorf("MCP re-type of %s = %+v, want constraint with REST's text %q", id, a, msg)
			}
			// The flagged key gets the refusal from the statement at confirm.
			a = f.call(t, f.flagged, "update", map[string]any{"id": id, "type": "knowledge"})
			m := hashRe.FindStringSubmatch(a.Text)
			if m == nil {
				t.Fatalf("flagged stage of %s: %+v", id, a)
			}
			a = f.call(t, f.flagged, "confirm", map[string]any{"payload_hash": m[1]})
			if !a.IsError || a.Code != "constraint" || !strings.HasSuffix(a.Text, msg) {
				t.Errorf("confirmed re-type of %s = %+v, want constraint ending in %q", id, a, msg)
			}
		}
		// The store upsert onto the comment's identity with another type.
		code, resp := roRestStore(t, sh, directAR, map[string]any{
			"category": commentBefore.category, "title": commentBefore.title, "content": "hijacked", "type": "knowledge",
		})
		if c, _ := resp["code"].(string); code != http.StatusConflict || c != "constraint" {
			t.Errorf("REST upsert re-type of the comment = code %d resp %v, want 409 constraint", code, resp)
		}
		a := f.call(t, f.direct, "store", map[string]any{
			"category": commentBefore.category, "title": commentBefore.title, "content": "hijacked", "type": "knowledge", "sensitivity": "internal",
		})
		if a.RPCError != "" || !a.IsError || a.Code != "constraint" {
			t.Errorf("MCP upsert re-type of the comment = %+v, want constraint", a)
		}
		if got := read(issueID); got != issueBefore {
			t.Errorf("issue row changed: %+v, want %+v", got, issueBefore)
		}
		if got := read(commentID); got.typeName != "comment" || got.parent != issueID {
			t.Errorf("comment row changed: %+v", got)
		}
		// An issue without a workflow status (a forge state that maps to none)
		// is an issue all the same: its type keeps it.
		code, resp = callManage(t, mh, directAR, "issue-create", "", map[string]any{"title": "ro statusless issue"})
		if code != http.StatusOK {
			t.Fatalf("issue-create: code %d resp %v", code, resp)
		}
		statusless := resp["issue"].(map[string]any)["id"].(string)
		if _, err := f.pool.Exec(ctx, `UPDATE context_blocks SET workflow_status = NULL WHERE id = $1`, statusless); err != nil {
			t.Fatalf("clear workflow status: %v", err)
		}
		if status, code, _, ok := i203RestUpdate(t, mh, directAR, statusless, map[string]any{"type": "knowledge"}); ok || status != http.StatusConflict || code != "constraint" {
			t.Errorf("REST re-type of a statusless issue = (status %d, code %q, success %v), want (409, constraint, false)", status, code, ok)
		}
		if name, _ := i203TypeOf(t, f.pool, statusless); name != "issue" {
			t.Errorf("statusless issue type = %q, want issue", name)
		}
		// The rule is the statement's: a direct one is refused too.
		_, err := f.pool.Exec(ctx, `UPDATE context_blocks SET type_name = 'knowledge' WHERE id = $1`, commentID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "ck_context_blocks_retype_bound" {
			t.Errorf("direct re-type of the comment: err = %v, want the retype-bound check violation", err)
		}
	})

	t.Run("a parent alone pins nothing: classified and re-typed as before", func(t *testing.T) {
		parent := i203StoreTyped(t, f, f.direct, "ro-plain-parent", "")
		// /api/store with a parent and no type: the classifier's re-type after
		// the link must still land (any type may carry a parent since NZ-3).
		code, resp := roRestStore(t, sh, directAR, map[string]any{
			"category": "test", "title": "Compaction source ro-parented", "content": "a transcript part", "parent_id": parent,
		})
		if code != http.StatusOK {
			t.Fatalf("store with parent: code %d resp %v", code, resp)
		}
		id, _ := roBlockField(resp, "id")
		child := id.(string)
		var parentID string
		if err := f.pool.QueryRow(ctx, `SELECT COALESCE(parent_id::text, '') FROM context_blocks WHERE id = $1`, child).Scan(&parentID); err != nil || parentID != parent {
			t.Fatalf("fixture: parent_id = %q (err %v), want %s", parentID, err, parent)
		}
		if name, source := i203TypeOf(t, f.pool, child); name != "checkpoint" || source != "auto" {
			t.Errorf("classified type of a parented block = (%q, %q), want (checkpoint, auto)", name, source)
		}
		if status, code, msg, ok := i203RestUpdate(t, mh, directAR, child, map[string]any{"type": "reference"}); !ok || status != http.StatusOK {
			t.Errorf("re-type of a parented non-comment block = (status %d, code %q, %q), want accepted", status, code, msg)
		}
		if got := roOrigin(t, f.pool, child); got != "checkpoint" {
			t.Errorf("record = %q, want checkpoint", got)
		}
	})

	t.Run("a domain type committed while a re-type waits decides the re-type", func(t *testing.T) {
		// Interleaving (Präambel 9a b): a second connection turns the block
		// into a comment and holds its transaction; the re-type waits on the
		// row lock and, after the commit, the trigger reads the committed row
		// — not the version the waiting statement started from.
		child := i203StoreTyped(t, f, f.direct, "ro-race-child", "")
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `UPDATE context_blocks SET type_name = 'comment', parent_id = $1 WHERE id = $2`, issueID, child); err != nil {
			t.Fatalf("turn into a comment in the open tx: %v", err)
		}
		typ := "reference"
		done := make(chan error, 1)
		go func() {
			_, _, err := store.UpdateBlock(ctx, f.pool, child, store.UpdateBlockData{Type: &typ}, []string{"private"})
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("the re-type did not wait for the row lock: err = %v", err)
		case <-time.After(300 * time.Millisecond):
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if err := <-done; !errors.Is(err, store.ErrRetypeBound) {
			t.Errorf("re-type after the comment committed: err = %v, want store.ErrRetypeBound", err)
		}
		if name, _ := i203TypeOf(t, f.pool, child); name != "comment" {
			t.Errorf("type = %q, want comment", name)
		}
	})

	t.Run("issue is not claimable by a generic write", func(t *testing.T) {
		id := i203StoreTyped(t, f, f.direct, "ro-into-issue", "")
		status, code, msg, ok := i203RestUpdate(t, mh, directAR, id, map[string]any{"type": "issue"})
		if ok || status != http.StatusUnprocessableEntity || code != "reserved_type" {
			t.Errorf("REST re-type into issue = (status %d, code %q, success %v, %q), want (422, reserved_type, false)", status, code, ok, msg)
		}
		if a := f.call(t, f.direct, "update", map[string]any{"id": id, "type": "issue"}); !a.IsError || a.Code != "reserved_type" || a.Text != msg {
			t.Errorf("MCP re-type into issue = %+v, want reserved_type with REST's text", a)
		}
		code2, resp := roRestStore(t, sh, directAR, map[string]any{"category": "test", "title": "ro-new-issue", "content": "x", "type": "issue"})
		if c, _ := resp["code"].(string); code2 != http.StatusUnprocessableEntity || c != "reserved_type" {
			t.Errorf("REST store of a new issue block = code %d resp %v, want 422 reserved_type", code2, resp)
		}
		if name, _ := i203TypeOf(t, f.pool, id); name != "knowledge" {
			t.Errorf("type = %q, want unchanged knowledge", name)
		}
	})
}
