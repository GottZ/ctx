package handler

// Issue #295, the ctx_query half without a database: the source's trust
// fields of POST /api/query (untrusted, V-11; untrusted_origin, #280) reach the
// chat tool result the model reads. The search/get/recent half reads the pool
// and lives in chat_trust_fields_integration_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/chat"
	"github.com/GottZ/ctx/internal/llm"
)

// TestChatQueryToolCarriesTrustFields runs the real executor over the real
// query delegation (chatQueryRunner) against a query handler that answers
// three sources: one of a type marked untrusted, one re-typed out of such a
// type, one first-party. The tool result names the first two as the REST
// answer does and leaves both keys out of the third.
func TestChatQueryToolCarriesTrustFields(t *testing.T) {
	fake := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"success":    true,
			"confidence": "confident",
			"sources": []map[string]any{
				{"id": "src-untrusted", "title": "captured output", "category": "test", "score": 0.9, "content": "c1", "untrusted": true},
				{"id": "src-origin", "title": "re-typed", "category": "test", "score": 0.8, "content": "c2", "untrusted_origin": "tool-evidence"},
				{"id": "src-plain", "title": "first-party", "category": "test", "score": 0.7, "content": "c3"},
			},
		})
	})
	// The sensitivity annotation reads the pool; a pool that cannot connect
	// makes it fail closed (credentials) without touching the result rows.
	pool, err := pgxpool.New(context.Background(), "postgres://t295:t295@127.0.0.1:1/t295?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("dead pool: %v", err)
	}
	t.Cleanup(pool.Close)

	ex := chat.NewExecutor(pool, &chatQueryRunner{handler: fake}, 8000)
	out := ex.Run(context.Background(), "private", []string{"private"}, "", llm.ToolCall{
		ID: "c1", Type: "function",
		Function: llm.ToolCallFunction{Name: "ctx_query", Arguments: json.RawMessage(`{"query":"trust"}`)},
	})
	if !out.OK {
		t.Fatalf("ctx_query outcome not OK: %s", out.Content)
	}
	var got struct {
		Blocks []map[string]any `json:"blocks"`
	}
	if err := json.Unmarshal([]byte(out.Content), &got); err != nil {
		t.Fatalf("tool result is no JSON object: %v (%s)", err, out.Content)
	}
	if len(got.Blocks) != 3 {
		t.Fatalf("tool result rows = %d, want 3: %s", len(got.Blocks), out.Content)
	}
	if v := got.Blocks[0]["untrusted"]; v != true {
		t.Errorf("row of a type marked untrusted: untrusted = %v, want true (%s)", v, out.Content)
	}
	if _, ok := got.Blocks[0]["untrusted_origin"]; ok {
		t.Errorf("row without a record carries untrusted_origin: %s", out.Content)
	}
	if v := got.Blocks[1]["untrusted_origin"]; v != "tool-evidence" {
		t.Errorf("re-typed row: untrusted_origin = %v, want tool-evidence (%s)", v, out.Content)
	}
	if _, ok := got.Blocks[1]["untrusted"]; ok {
		t.Errorf("re-typed row of a trusted type carries untrusted: %s", out.Content)
	}
	for _, key := range []string{"untrusted", "untrusted_origin"} {
		if _, ok := got.Blocks[2][key]; ok {
			t.Errorf("first-party row carries %s, want the key absent: %s", key, out.Content)
		}
	}
}
