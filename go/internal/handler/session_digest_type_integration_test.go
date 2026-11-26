//go:build integration

// Issue #289 point 4: the builtin type session-digest (migration 196) through
// the production write and read paths (real PG18 testcontainer, the real
// migration chain, the registry booted from its rows).
//
//   - A client claims the type on REST /api/store and on the MCP store tool
//     (type_source manual); a write that names no type is classified as before
//     the type existed (the type has no classify rule).
//   - Every read path a recall takes frames a digest and a fact and leaves a
//     knowledge block on the same topic unframed: the MCP search rows, the MCP
//     query sources without synthesis (the plugin's recall), the MCP get block,
//     the REST search rows and the /api/query retrieval-only sources.
//   - A re-type out of the type records untrusted_origin = session-digest —
//     trg_block_retype (174) decides by the registry row, so this half needs
//     the migration and not only builtin.go.
//   - manage type-list lists the row as a builtin.
//
// The file compiles against the tree before the change and fails there at run
// time (unknown_type on the first claim).
//
//	go test -tags=integration -p 1 ./internal/handler/ -run 'TestSessionDigestType' -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
)

const sdtCategoryDigest, sdtCategoryFact = "agent-briefing", "learnings"

// sdtStoreMCP stores one block over the MCP store tool and returns its id.
func sdtStoreMCP(t *testing.T, f *i201Fixture, args map[string]any) string {
	t.Helper()
	a := f.call(t, f.direct, "store", args)
	if a.RPCError != "" || a.IsError {
		t.Fatalf("MCP store %v: %+v, want accepted", args["title"], a)
	}
	m := i201StoredID.FindStringSubmatch(a.Text)
	if m == nil {
		t.Fatalf("MCP store %v: no id in %q", args["title"], a.Text)
	}
	return m[1]
}

// sdtFlags reads the untrusted field of each row, keyed by id; a row without
// the field maps to nil.
func sdtFlags(rows []map[string]any) map[string]any {
	out := map[string]any{}
	for _, r := range rows {
		id, _ := r["id"].(string)
		out[id] = r["untrusted"]
	}
	return out
}

func TestSessionDigestType(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI201Fixture(t)
	ctx := context.Background()
	directAR := AuthResultFromContext(f.direct)
	sh := roStoreHandler(f)
	mh := NewManageHandler(f.pool, nil, nil, nil, nil, nil, nil, f.cfg.Blocktypes)

	// The plugin's two writes with the type named: a digest over REST, a fact
	// over MCP. A knowledge block on the same topic is the control.
	code, resp := roRestStore(t, sh, directAR, map[string]any{
		"category": sdtCategoryDigest, "title": "Session digest 0sdtrest seg 001 2026-11-26",
		"content": "Intent: sdq rotation of the staging host", "type": "session-digest",
	})
	if code != http.StatusOK || resp["success"] != true {
		t.Fatalf("REST store with type session-digest: code %d resp %v, want accepted", code, resp)
	}
	restID, _ := roBlockField(resp, "id")
	digestID, _ := restID.(string)
	if name, source := i203TypeOf(t, f.pool, digestID); name != "session-digest" || source != "manual" {
		t.Errorf("REST digest type = (%q, %q), want (session-digest, manual)", name, source)
	}
	factID := sdtStoreMCP(t, f, map[string]any{
		"category": sdtCategoryFact, "title": "fact: The sdq rotation deploy keeps the previous image",
		"content": "The sdq rotation deploy keeps the previous image as rollback.", "type": "session-digest",
		"sensitivity": "internal",
	})
	if name, source := i203TypeOf(t, f.pool, factID); name != "session-digest" || source != "manual" {
		t.Errorf("MCP fact type = (%q, %q), want (session-digest, manual)", name, source)
	}
	knowledgeID := sdtStoreMCP(t, f, map[string]any{
		"category": "infrastructure", "title": "Staging host sdq rotation runbook",
		"content": "The sdq rotation of the staging host runs every Monday.", "sensitivity": "internal",
	})
	if name, _ := i203TypeOf(t, f.pool, knowledgeID); name != "knowledge" {
		t.Fatalf("control block type = %q, want knowledge", name)
	}
	framed := map[string]bool{digestID: true, factID: true, knowledgeID: false}
	checkFrame := func(t *testing.T, surface string, seen map[string]any) {
		t.Helper()
		for id, want := range framed {
			got, present := seen[id]
			if !present {
				t.Errorf("%s: block %s missing from the answer (%v)", surface, id, seen)
				continue
			}
			if want && got != true {
				t.Errorf("%s: block %s untrusted = %v, want true", surface, id, got)
			}
			if !want && got != nil {
				t.Errorf("%s: knowledge block %s untrusted = %v, want the field absent", surface, id, got)
			}
		}
	}

	t.Run("a write that names no type is classified as before the type existed", func(t *testing.T) {
		// The type has no classify rule: only a writer that names it gets it.
		// A digest of a plugin release that names no type stays audit-trail
		// (its title holds "session"), a fact knowledge — and so does a title
		// a person writes about session digests.
		for title, want := range map[string]string{
			"Session digest 0sdtauto seg 002 2026-11-26":    "audit-trail",
			"fact: The sdq rotation needs the cache warmed": "knowledge",
			"How the session digest works":                  "audit-trail",
			"memory: Session digest settings of the plugin": "audit-trail",
			"Artifact: the sdq rotation bundle":             "knowledge",
		} {
			id := i203StoreTyped(t, f, f.direct, title, "")
			if name, source := i203TypeOf(t, f.pool, id); name != want || source != "auto" {
				t.Errorf("%q classified to (%q, %q), want (%q, auto)", title, name, source, want)
			}
		}
	})

	// The query paths rank by vector as well: the embed stub answers only the
	// question, and the three blocks carry its vector.
	for _, id := range []string{digestID, factID, knowledgeID} {
		if err := store.StoreEmbedding(ctx, f.pool, id, "test-embed", sdQueryVector()); err != nil {
			t.Fatalf("embed %s: %v", id, err)
		}
	}
	srv := sdQueryEmbedServer(t)
	qh := NewQueryHandler(f.pool, config.NewStore(w023Config(false)), embedPool(srv.URL), nil, f.cfg.Blocktypes, snapshotTestAdmitter(t))
	question := "what was the " + sdQuestionMarker + " outcome"

	t.Run("the recall paths frame the digest and the fact, not the knowledge block", func(t *testing.T) {
		// MCP search rows (the plugin's fallback recall and its head lookup).
		a := f.call(t, f.direct, "search", map[string]any{"query": "sdq rotation", "limit": 20})
		var rows []map[string]any
		if err := json.Unmarshal([]byte(a.Text), &rows); err != nil {
			t.Fatalf("MCP search: %v in %s", err, a.Text)
		}
		checkFrame(t, "MCP search", sdtFlags(rows))

		// MCP query without synthesis: the plugin's default recall.
		mcpQuery := &i201Fixture{mcp: NewMCPHandler(MCPConfig{
			Pool: f.pool, Cfg: f.cfg.Cfg, Blocktypes: f.cfg.Blocktypes, QueryHandler: http.HandlerFunc(qh.HandleQuery),
		})}
		a = mcpQuery.call(t, f.direct, "query", map[string]any{"question": question, "synthesize": false, "limit": 10})
		if a.RPCError != "" || a.IsError {
			t.Fatalf("MCP query synthesize:false: %+v", a)
		}
		var qdoc struct {
			Sources []map[string]any `json:"sources"`
		}
		if err := json.Unmarshal([]byte(a.Text), &qdoc); err != nil {
			t.Fatalf("MCP query: %v in %s", err, a.Text)
		}
		checkFrame(t, "MCP query (synthesize false)", sdtFlags(qdoc.Sources))

		// MCP get, one block at a time.
		got := map[string]any{}
		for id := range framed {
			a := f.call(t, f.direct, "get", map[string]any{"id": id})
			var block map[string]any
			if err := json.Unmarshal([]byte(a.Text), &block); err != nil || a.IsError {
				t.Fatalf("MCP get %s: %v %+v", id, err, a)
			}
			got[id] = block["untrusted"]
		}
		checkFrame(t, "MCP get", got)

		// REST search rows.
		srh := NewSearchHandler(f.pool, staticConfigStore{cfg: &config.Config{}}, f.cfg.Blocktypes)
		body, _ := json.Marshal(map[string]any{"query": "sdq rotation", "limit": 20})
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
		checkFrame(t, "REST search", sdtFlags(sresp.Results))

		// REST /api/query retrieval-only sources.
		body, _ = json.Marshal(map[string]any{"query": question, "synthesize": false, "limit": 10})
		req = httptest.NewRequest(http.MethodPost, "/api/query", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, directAR))
		rec = httptest.NewRecorder()
		qh.HandleQuery(rec, req)
		var qresp struct {
			Sources []map[string]any `json:"sources"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &qresp); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("REST query: code %d err %v body %s", rec.Code, err, rec.Body.String())
		}
		checkFrame(t, "REST query (synthesize false)", sdtFlags(qresp.Sources))
	})

	t.Run("a re-type out of the type records it", func(t *testing.T) {
		code, resp := callManage(t, mh, directAR, "update", factID, map[string]any{"type": "knowledge"})
		if code != http.StatusOK || resp["success"] != true {
			t.Fatalf("re-type session-digest → knowledge: code %d resp %v", code, resp)
		}
		if got := roOrigin(t, f.pool, factID); got != "session-digest" {
			t.Errorf("untrusted_origin after the re-type = %q, want session-digest (trg_block_retype reads the registry row)", got)
		}
		a := f.call(t, f.direct, "get", map[string]any{"id": factID})
		var block map[string]any
		if err := json.Unmarshal([]byte(a.Text), &block); err != nil || a.IsError {
			t.Fatalf("MCP get %s: %v %+v", factID, err, a)
		}
		if block["untrusted_origin"] != "session-digest" {
			t.Errorf("MCP get after the re-type: untrusted_origin = %v, want session-digest", block["untrusted_origin"])
		}
	})

	t.Run("manage type-list lists the builtin row", func(t *testing.T) {
		code, resp := callManage(t, mh, directAR, "type-list", "", nil)
		if code != http.StatusOK || resp["success"] != true {
			t.Fatalf("type-list: code %d resp %v", code, resp)
		}
		types, _ := resp["types"].([]any)
		for _, x := range types {
			if row, _ := x.(map[string]any); row["name"] == "session-digest" {
				if row["builtin"] != true || row["scope"] != "_global" {
					t.Errorf("type-list row = %v, want a _global builtin", row)
				}
				return
			}
		}
		t.Errorf("type-list lacks session-digest: %v", types)
	})
}
