package handler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestClassifyLLMError(t *testing.T) {
	cases := []struct{ in, want string }{
		{"llm: unexpected status 403: forbidden", "http_403"},
		{"llm: unexpected status 500: boom", "http_500"},
		{"context deadline exceeded", "timeout"},
		{"Client.Timeout exceeded while awaiting headers", "timeout"},
		{"dial tcp 10.0.0.1:8089: connect: connection refused", "unreachable"},
		{"lookup herbert: no such host", "unreachable"},
		{"no_eligible_backend for role synthesis", "no_backend"},
		{"unexpected EOF", "eof"},
		{"something weird happened", "error"},
	}
	for _, c := range cases {
		if got := classifyLLMError(c.in); got != c.want {
			t.Errorf("classifyLLMError(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestNormalizeLLMError pins the privacy cap: a prompt marker placed BEYOND the
// 256-rune cap is dropped (the raw error can embed up to 1 KiB of provider body
// with prompt fragments — design 04 §3.2/R3). nil/blank yield nil.
func TestNormalizeLLMError(t *testing.T) {
	if normalizeLLMError(nil) != nil {
		t.Error("nil raw must yield nil")
	}
	blank := "   "
	if normalizeLLMError(&blank) != nil {
		t.Error("blank raw must yield nil")
	}

	marker := "SECRET-PROMPT-IN-ERROR"
	raw := "llm: unexpected status 403: " + strings.Repeat("x", 300) + marker
	got := normalizeLLMError(&raw)
	if got == nil {
		t.Fatal("non-empty raw must yield a value")
	}
	if got.Class != "http_403" {
		t.Errorf("class = %q, want http_403", got.Class)
	}
	if n := len([]rune(got.Detail)); n != errDetailCap {
		t.Errorf("detail must be capped to %d runes, got %d", errDetailCap, n)
	}
	if strings.Contains(got.Detail, marker) {
		t.Error("marker beyond the cap must be dropped, detail still contains it")
	}
}

func TestTruncateRunes(t *testing.T) {
	if truncateRunes("abc", 10) != "abc" {
		t.Error("short string must pass through unchanged")
	}
	if got := truncateRunes("abcdef", 3); got != "abc" {
		t.Errorf("truncateRunes = %q, want abc", got)
	}
	// rune-safe: never split a multibyte character at the boundary.
	if got := truncateRunes("äöü€", 2); got != "äö" {
		t.Errorf("rune-safe truncate = %q, want äö", got)
	}
}

// TestLLMLogGoldenKeys pins the /api/llmlog wire field names (anchor for the TS
// types). Critically the entry set must NEVER grow a body column.
func TestLLMLogGoldenKeys(t *testing.T) {
	dur := 10
	entry := llmlogEntry{
		ID: "x", Pipeline: "p", Model: "m", Backend: "b",
		DurationMs: &dur, Error: &llmlogError{Class: "http_403", Detail: "d"},
	}
	b, err := json.Marshal(map[string]any{"success": true, "entries": []llmlogEntry{entry}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertKeys(t, "llmlog", b, []string{"success", "entries"})

	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var es []json.RawMessage
	if err := json.Unmarshal(top["entries"], &es); err != nil {
		t.Fatalf("unmarshal entries: %v", err)
	}
	assertKeys(t, "entry", es[0], []string{
		"id", "created_at", "pipeline", "model", "backend",
		"duration_ms", "error", "prompt_tokens", "completion_tokens", "cost_usd",
		// MW12/091 dispatch telemetry — pure Lease measurands, never body columns.
		"queue_wait_ms", "dispatch_class", "dispatch_abort",
	})

	var entryMap map[string]json.RawMessage
	if err := json.Unmarshal(es[0], &entryMap); err != nil {
		t.Fatalf("unmarshal entry: %v", err)
	}
	assertKeys(t, "error", entryMap["error"], []string{"class", "detail"})
}

// TestLLMLogDetailGoldenKeys pins the GET /api/llmlog/{id} wire field names
// (anchor for the TS LLMLogDetail type). This is the ONLY llmlog shape that MAY
// carry body columns — and only per-id, gated.
func TestLLMLogDetailGoldenKeys(t *testing.T) {
	body := "sys"
	d := llmlogDetail{
		ID: "x", CreatedAt: time.Unix(0, 0).UTC(), Pipeline: "p", Model: "m",
		Backend: "b", RequiredSensitivity: "internal", BodyState: bodyPresent,
		RequestSystem: &body,
	}
	b, err := json.Marshal(map[string]any{"success": true, "detail": d})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertKeys(t, "detail-envelope", b, []string{"success", "detail"})

	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	assertKeys(t, "detail", top["detail"], []string{
		"id", "created_at", "pipeline", "model", "backend",
		"required_sensitivity", "body_state",
		"request_system", "request_user", "response_content",
	})
}

// TestClassifyBodies pins the body_state decision: credentials ⇒ sealed (bodies
// dropped), any body ⇒ present (bodies returned), all-empty ⇒ evicted (bodies
// dropped). A regression that leaked a sealed/evicted body would turn this red.
func TestClassifyBodies(t *testing.T) {
	s := "sys"
	u := "user"
	empty := ""

	// credentials-class: SEALED, every body nil regardless of stored content.
	if state, os, ou, or := classifyBodies("credentials", &s, &u, &s); state != bodySealed || os != nil || ou != nil || or != nil {
		t.Fatalf("credentials: got state=%q os=%v ou=%v or=%v; want sealed + all nil", state, os, ou, or)
	}
	// non-credentials with content: PRESENT, bodies passed through.
	if state, os, _, _ := classifyBodies("internal", &s, &u, nil); state != bodyPresent || os != &s {
		t.Fatalf("present: got state=%q os=%v; want present + passthrough", state, os)
	}
	// non-credentials, all bodies nil/empty: EVICTED, bodies nil.
	if state, os, ou, or := classifyBodies("internal", nil, &empty, nil); state != bodyEvicted || os != nil || ou != nil || or != nil {
		t.Fatalf("evicted: got state=%q os=%v ou=%v or=%v; want evicted + all nil", state, os, ou, or)
	}
}
