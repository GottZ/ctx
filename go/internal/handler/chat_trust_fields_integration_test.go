//go:build integration

// Issue #295, the half that reads the pool: the chat tools ctx_search, ctx_get
// and ctx_recent carry `untrusted` (the type of the block is marked
// retrieval.untrusted in the turn's registry snapshot, V-11) and
// `untrusted_origin` (the block was re-typed out of such a type, #280), named
// and omitted as on REST. The blocks are written through the MCP store tool
// and re-typed through REST manage update; the executor is the one the chat
// handler builds for a turn (turnExecutor). The probes read the serialized tool
// result, so the file compiles against the tree before the change and fails
// there at run time.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run 'TestChatToolsCarryTrustFields' -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/GottZ/ctx/internal/chat"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/llm"
)

// t295Run runs one chat tool call and decodes its result object.
func t295Run(t *testing.T, ex *chat.Executor, ctx context.Context, tool, args string) map[string]any {
	t.Helper()
	out := ex.Run(ctx, "private", []string{"private"}, "", llm.ToolCall{
		ID: "c1", Type: "function",
		Function: llm.ToolCallFunction{Name: tool, Arguments: json.RawMessage(args)},
	})
	if !out.OK {
		t.Fatalf("%s %s: outcome not OK: %s", tool, args, out.Content)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out.Content), &m); err != nil {
		t.Fatalf("%s: result is no JSON object: %v (%s)", tool, err, out.Content)
	}
	return m
}

// t295Rows maps the result rows of a list tool by id.
func t295Rows(t *testing.T, m map[string]any) map[string]map[string]any {
	t.Helper()
	rows := map[string]map[string]any{}
	list, _ := m["blocks"].([]any)
	for _, r := range list {
		row, _ := r.(map[string]any)
		if id, _ := row["id"].(string); id != "" {
			rows[id] = row
		}
	}
	return rows
}

// t295Check compares one row (or the get result) with the wanted trust
// fields: want "" / false means the key is absent.
func t295Check(t *testing.T, where string, row map[string]any, wantUntrusted bool, wantOrigin string) {
	t.Helper()
	if row == nil {
		t.Errorf("%s: row missing", where)
		return
	}
	v, ok := row["untrusted"]
	switch {
	case wantUntrusted && v != true:
		t.Errorf("%s: untrusted = %v (present %v), want true", where, v, ok)
	case !wantUntrusted && ok:
		t.Errorf("%s: untrusted = %v, want the key absent", where, v)
	}
	o, ok := row["untrusted_origin"]
	switch {
	case wantOrigin != "" && o != wantOrigin:
		t.Errorf("%s: untrusted_origin = %v (present %v), want %s", where, o, ok, wantOrigin)
	case wantOrigin == "" && ok:
		t.Errorf("%s: untrusted_origin = %v, want the key absent", where, o)
	}
}

func TestChatToolsCarryTrustFields(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI201Fixture(t)
	i203HookTenantRegistry(t)
	directAR := AuthResultFromContext(f.direct)
	mh := NewManageHandler(f.pool, nil, nil, nil, nil, nil, nil, f.cfg.Blocktypes)

	// One block of a type marked untrusted, one re-typed out of such a type
	// into a trusted one, one first-party block. One word in every title, so a
	// search for it finds the three.
	untrustedID := i203StoreTyped(t, f, f.direct, "t295zebra captured output", "tool-evidence")
	originID := i203StoreTyped(t, f, f.direct, "t295zebra re-typed note", "tool-evidence")
	if code, resp := callManage(t, mh, directAR, "update", originID, map[string]any{"type": "knowledge"}); code != http.StatusOK || resp["success"] != true {
		t.Fatalf("re-type tool-evidence → knowledge: code %d resp %v", code, resp)
	}
	plainID := i203StoreTyped(t, f, f.direct, "t295zebra first-party note", "")

	h := &ChatHandler{pool: f.pool, blocktypes: f.cfg.Blocktypes}
	ex := h.turnExecutor(f.direct, &config.Config{})

	t.Run("ctx_search", func(t *testing.T) {
		rows := t295Rows(t, t295Run(t, ex, f.direct, "ctx_search", `{"query":"t295zebra","limit":20}`))
		t295Check(t, "search untrusted type", rows[untrustedID], true, "")
		t295Check(t, "search re-typed", rows[originID], false, "tool-evidence")
		t295Check(t, "search first-party", rows[plainID], false, "")
	})

	t.Run("ctx_recent", func(t *testing.T) {
		rows := t295Rows(t, t295Run(t, ex, f.direct, "ctx_recent", `{"category":"test","limit":50}`))
		t295Check(t, "recent untrusted type", rows[untrustedID], true, "")
		t295Check(t, "recent re-typed", rows[originID], false, "tool-evidence")
		t295Check(t, "recent first-party", rows[plainID], false, "")
	})

	t.Run("ctx_get", func(t *testing.T) {
		for _, c := range []struct {
			id        string
			untrusted bool
			origin    string
		}{{untrustedID, true, ""}, {originID, false, "tool-evidence"}, {plainID, false, ""}} {
			got := t295Run(t, ex, f.direct, "ctx_get", fmt.Sprintf(`{"id":%q}`, c.id))
			t295Check(t, "get "+c.id, got, c.untrusted, c.origin)
		}
	})

	t.Run("an executor without a registry snapshot marks nothing untrusted", func(t *testing.T) {
		bare := chat.NewExecutor(f.pool, nil, 8000)
		got := t295Run(t, bare, f.direct, "ctx_get", fmt.Sprintf(`{"id":%q}`, untrustedID))
		t295Check(t, "get without a snapshot", got, false, "")
		// The record is a column, not a registry property: it stays.
		got = t295Run(t, bare, f.direct, "ctx_get", fmt.Sprintf(`{"id":%q}`, originID))
		t295Check(t, "get re-typed without a snapshot", got, false, "tool-evidence")
	})
}
