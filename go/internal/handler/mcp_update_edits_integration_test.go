//go:build integration

// Issue #406, MCP surface: the `update` tool takes `edits` and
// `metadata_merge` and reaches the store function manage update reaches. A
// confirm_writes key stages them: the edits resolve at stage time against the
// block the card pins (base_updated_at), and the card binds the content they
// produce — the confirm writes exactly that, or refuses when the block moved.
// The merge stays a merge: it binds the keys it writes, and the confirm merges
// them into the metadata as it is then.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestMCPUpdateEdits -count=1 -v
package handler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPUpdateEdits(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI406Fixture(t)
	updateTool := mcpUpdateHandler(f.mcpCfg)
	confirmTool := mcpConfirmHandler(f.mcpCfg)
	edit := func(old, repl string) updateEditInput { return updateEditInput{Old: old, New: repl} }
	code := func(r *mcp.CallToolResult) string {
		if env, ok := r.StructuredContent.(mcpErrorEnvelope); ok {
			return env.Code
		}
		return ""
	}
	stage := func(t *testing.T, in updateInput) string {
		t.Helper()
		r, _, err := updateTool(f.flaggedCtx, nil, in)
		if err != nil {
			t.Fatalf("stage: %v", err)
		}
		m := hashRe.FindStringSubmatch(resultText(t, r))
		if !r.IsError || m == nil || code(r) != "" {
			t.Fatalf("update of a confirm_writes key was not staged: %s", resultText(t, r))
		}
		return m[1]
	}
	confirm := func(t *testing.T, hash string) *mcp.CallToolResult {
		t.Helper()
		r, _, err := confirmTool(f.flaggedCtx, nil, confirmInput{PayloadHash: hash})
		if err != nil {
			t.Fatalf("confirm: %v", err)
		}
		return r
	}
	openStages := func(t *testing.T) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM context_pending_writes WHERE api_key_id = $1 AND consumed_at IS NULL`, f.flaggedKey).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("direct: edits and merge reach the store", func(t *testing.T) {
		id := f.block(t, "mcp-direct", "alpha beta", map[string]any{"keep": "k", "drop": "d"})
		r, _, err := updateTool(f.directCtx, nil, updateInput{
			ID:            id,
			Edits:         []updateEditInput{edit("beta", "BETA"), {Old: "a", New: "4", ReplaceAll: true}},
			MetadataMerge: map[string]any{"drop": nil, "add": "x"},
		})
		if err != nil || r.IsError {
			t.Fatalf("update: %v %s", err, resultText(t, r))
		}
		row := f.row(t, id)
		if row.content != "4lph4 BETA" {
			t.Fatalf("content = %q", row.content)
		}
		if _, ok := row.metadata["drop"]; ok || row.metadata["keep"] != "k" || row.metadata["add"] != "x" {
			t.Fatalf("metadata = %v", row.metadata)
		}
	})

	t.Run("direct: a refused edit names it, codes edit_mismatch and writes nothing", func(t *testing.T) {
		id := f.block(t, "mcp-direct-refused", "same same", nil)
		before := f.row(t, id)
		r, _, err := updateTool(f.directCtx, nil, updateInput{ID: id, Edits: []updateEditInput{edit("same", "x")}})
		if err != nil {
			t.Fatal(err)
		}
		if !r.IsError || code(r) != "edit_mismatch" || !strings.Contains(resultText(t, r), "edits[0]: old matches 2 times") {
			t.Fatalf("answer %q code %q, want edit_mismatch naming edits[0] and 2 matches", resultText(t, r), code(r))
		}
		if after := f.row(t, id); after.content != before.content || !after.updated.Equal(before.updated) {
			t.Fatal("a refused edit wrote")
		}
	})

	t.Run("direct: the patch rules", func(t *testing.T) {
		id := f.block(t, "mcp-direct-rules", "seed", nil)
		c := "whole"
		for _, in := range []updateInput{
			{ID: id, Content: &c, Edits: []updateEditInput{edit("seed", "x")}},
			{ID: id, Metadata: map[string]any{}, MetadataMerge: map[string]any{"k": 1}},
			{ID: id, Edits: []updateEditInput{edit("", "x")}},
		} {
			r, _, err := updateTool(f.directCtx, nil, in)
			if err != nil || !r.IsError {
				t.Fatalf("update %+v was not refused: %v", in, err)
			}
		}
		r, _, _ := updateTool(f.directCtx, nil, updateInput{ID: id, MetadataMerge: map[string]any{"provenance": nil}})
		if !r.IsError || code(r) != "reserved_metadata" {
			t.Fatalf("a merge naming provenance: %q code %q, want reserved_metadata", resultText(t, r), code(r))
		}
		if f.row(t, id).content != "seed" {
			t.Fatal("a refused patch wrote")
		}
	})

	t.Run("staged: the card binds the resolved content and the confirm writes exactly it", func(t *testing.T) {
		id := f.block(t, "mcp-staged", "one two three", nil)
		hash := stage(t, updateInput{ID: id, Edits: []updateEditInput{edit("two", "2")}})
		if got := f.row(t, id).content; got != "one two three" {
			t.Fatalf("staging wrote: %q", got)
		}
		var payload map[string]any
		var raw []byte
		if err := f.pool.QueryRow(f.ctx, `SELECT payload::text FROM context_pending_writes WHERE payload_hash = $1`, hash).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["content"] != "one 2 three" {
			t.Fatalf("staged content = %v, want the resolved text", payload["content"])
		}
		if _, ok := payload["edits"]; ok {
			t.Fatalf("the stage holds the edits instead of their result: %s", raw)
		}
		if fields, _ := payload["update_fields"].([]any); len(fields) != 1 || fields[0] != "content" {
			t.Fatalf("update_fields = %v, want [content]", payload["update_fields"])
		}
		r := confirm(t, hash)
		if r.IsError {
			t.Fatalf("confirm: %s", resultText(t, r))
		}
		if got := f.row(t, id).content; got != "one 2 three" {
			t.Fatalf("confirmed content = %q", got)
		}
	})

	t.Run("staged: a change after the stage refuses the confirm and keeps the token", func(t *testing.T) {
		id := f.block(t, "mcp-staged-drift", "one two three", nil)
		hash := stage(t, updateInput{ID: id, Edits: []updateEditInput{edit("two", "2")}})
		r, _, err := updateTool(f.directCtx, nil, updateInput{ID: id, Edits: []updateEditInput{edit("one", "1")}})
		if err != nil || r.IsError {
			t.Fatalf("concurrent edit: %v %s", err, resultText(t, r))
		}
		open := openStages(t)
		r = confirm(t, hash)
		if !r.IsError || !strings.Contains(resultText(t, r), "changed") {
			t.Fatalf("confirm after a change answered %q, want the drift refusal", resultText(t, r))
		}
		if got := f.row(t, id).content; got != "1 two three" {
			t.Fatalf("content = %q, want the concurrent edit kept and the staged one not applied", got)
		}
		if openStages(t) != open {
			t.Fatal("the drift refusal consumed the stage token")
		}
	})

	// The drift check reads without a lock before the token is consumed. A
	// writer whose transaction is open at that moment commits while the
	// confirm's statement waits for its row lock; the statement must then
	// refuse instead of writing the stage-time text over that change.
	t.Run("staged: a change committed during the confirm is not overwritten", func(t *testing.T) {
		id := f.block(t, "mcp-staged-race", "one two three", nil)
		hash := stage(t, updateInput{ID: id, Edits: []updateEditInput{edit("two", "2")}})
		holder, err := f.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = holder.Rollback(f.ctx) }()
		if _, err := holder.Exec(f.ctx,
			`UPDATE context_blocks SET content = 'held one two three', updated_at = now() WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		done := make(chan *mcp.CallToolResult, 1)
		go func() {
			r, _, _ := confirmTool(f.flaggedCtx, nil, confirmInput{PayloadHash: hash})
			done <- r
		}()
		deadline := time.Now().Add(10 * time.Second)
		for {
			var waiting int
			if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM pg_stat_activity
				WHERE datname = current_database() AND cardinality(pg_blocking_pids(pid)) > 0`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the confirm never waited for the held row")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := holder.Commit(f.ctx); err != nil {
			t.Fatal(err)
		}
		r := <-done
		if r == nil || !r.IsError || !strings.Contains(resultText(t, r), "changed while this update was confirmed") ||
			!strings.Contains(resultText(t, r), "the stage token is consumed") {
			t.Fatalf("confirm answered %v, want the drift refusal after the consume", r)
		}
		if got := f.row(t, id).content; got != "held one two three" {
			t.Fatalf("content = %q, want the concurrent writer's text kept", got)
		}
	})

	t.Run("staged: an edit past the size cap gets no card", func(t *testing.T) {
		id := f.block(t, "mcp-staged-cap", "seed", nil)
		open := openStages(t)
		r, _, _ := updateTool(f.flaggedCtx, nil, updateInput{ID: id, Edits: []updateEditInput{edit("seed", strings.Repeat("x", store.MaxBlockContentBytes+1))}})
		if code(r) != "size_cap" || openStages(t) != open {
			t.Fatalf("answer %q code %q, want size_cap and no card", resultText(t, r), code(r))
		}
	})

	t.Run("staged: a refused edit gets no card", func(t *testing.T) {
		id := f.block(t, "mcp-staged-refused", "one", nil)
		open := openStages(t)
		r, _, err := updateTool(f.flaggedCtx, nil, updateInput{ID: id, Edits: []updateEditInput{edit("absent", "x")}})
		if err != nil {
			t.Fatal(err)
		}
		if code(r) != "edit_mismatch" || hashRe.MatchString(resultText(t, r)) {
			t.Fatalf("answer %q code %q, want edit_mismatch and no card", resultText(t, r), code(r))
		}
		if openStages(t) != open {
			t.Fatal("a refused edit was staged")
		}
	})

	// A guard pass writes metadata without moving updated_at, so the card's
	// pin does not see it. The staged merge writes its own keys at confirm and
	// leaves that key alone; a merge resolved into a full replacement at stage
	// time would write the stage-time object back and drop it.
	t.Run("staged: the merge stays a merge", func(t *testing.T) {
		id := f.block(t, "mcp-staged-merge", "body", map[string]any{"keep": "k", "drop": "d"})
		hash := stage(t, updateInput{ID: id, MetadataMerge: map[string]any{"add": "a", "drop": nil}})
		if _, err := f.pool.Exec(f.ctx,
			`UPDATE context_blocks SET metadata = metadata || '{"server_mark":"m"}' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		r := confirm(t, hash)
		if r.IsError {
			t.Fatalf("confirm: %s", resultText(t, r))
		}
		m := f.row(t, id).metadata
		if m["keep"] != "k" || m["add"] != "a" || m["server_mark"] != "m" {
			t.Fatalf("metadata = %v, want keep, add and the server's key", m)
		}
		if _, ok := m["drop"]; ok {
			t.Fatalf("the staged null did not remove the key: %v", m)
		}
	})

	t.Run("staged: a merge naming provenance gets no card", func(t *testing.T) {
		id := f.block(t, "mcp-staged-prov", "body", nil)
		r, _, _ := updateTool(f.flaggedCtx, nil, updateInput{ID: id, MetadataMerge: map[string]any{"provenance": map[string]any{"v": 1}}})
		if code(r) != "reserved_metadata" {
			t.Fatalf("answer %q code %q, want reserved_metadata", resultText(t, r), code(r))
		}
	})

	t.Run("the confirm re-checks a staged merge's keys", func(t *testing.T) {
		// A card staged before a key became reserved must not execute under
		// the list of the confirm's moment: the claim re-check reads the merge
		// as it reads a replacement. Here the stage row is rewritten to carry
		// the key, the shape such a card would have.
		id := f.block(t, "mcp-confirm-recheck", "body", nil)
		cw := store.CanonicalWrite{
			Op: "update", ID: id, Scope: "private",
			MetadataMerge: map[string]any{"provenance": map[string]any{"v": 1}},
			UpdateFields:  []string{"metadata_merge"},
		}
		if rej := confirmClaimReject(f.ctx, f.mcpCfg.Blocktypes, cw); rej == nil || rej.Code != "reserved_metadata" {
			t.Fatalf("confirmClaimReject = %+v, want reserved_metadata", rej)
		}
	})
}
