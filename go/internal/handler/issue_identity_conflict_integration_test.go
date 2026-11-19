//go:build integration

// Issue #275, first two points: the issue verbs run into the same identity
// index as UpdateBlock (uq_context_category_title_scope, issue #227). An issue
// create writes "#L<seq> <title>" in category issue, a comment create
// "<issue prefix>.cL<seq> <author>" in category comment, and an issue update
// rewrites the title behind the kept prefix — each of them collides when
// another active block of the scope already holds that category and title
// (a block stored under category issue or comment, say). Before the fix every
// issue transport answered the collision as a server fault: 500 "Internal
// server error" on REST and manage, "issue write failed" on MCP, each with an
// ERROR line. It is the caller's input, resolved by another title, so the
// issue verbs now answer the 409 of the constraint class that the block
// update surfaces answer (identityTakenReject).
//
// The last subtest pins the comment half of the first point: the prefix
// pattern of a title edit did not match a comment title ("#L1.cL2 alice"), so
// an edit of a comment's title dropped its prefix.
//
//	go test -tags=integration ./internal/handler/ -run TestIssueVerbsIdentityCollision -count=1 -v
package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestIssueVerbsIdentityCollision(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	pid, scope := w6SeedProject(t, pool, "i275")
	ar := w7Writer(scope)
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, reg)
	mcpCfg := MCPConfig{Pool: pool, Blocktypes: reg}

	// occupy writes an ordinary block that holds (category, title) in the scope.
	occupy := func(t *testing.T, category, title string) {
		t.Helper()
		if _, err := store.UpsertBlock(ctx, pool, category, title, "occupant", nil, nil, scope, true, store.SensitivityWrite{}, ""); err != nil {
			t.Fatalf("occupy %s/%s: %v", category, title, err)
		}
	}
	titleOf := func(t *testing.T, id string) string {
		t.Helper()
		var title string
		if err := pool.QueryRow(ctx, `SELECT title FROM context_blocks WHERE id = $1::uuid`, id).Scan(&title); err != nil {
			t.Fatalf("read title: %v", err)
		}
		return title
	}
	idOf := func(t *testing.T, resp map[string]any, key string) string {
		t.Helper()
		b, _ := resp[key].(map[string]any)
		id, _ := b["id"].(string)
		if id == "" {
			t.Fatalf("no %s id in %v", key, resp)
		}
		return id
	}
	// wantConflict checks a REST or manage answer: 409, the constraint code
	// and the one wording of the collision.
	wantConflict := func(t *testing.T, code int, resp map[string]any) {
		t.Helper()
		msg, _ := resp["error"].(string)
		if code != http.StatusConflict || resp["code"] != "constraint" || !strings.Contains(msg, blockIdentityTakenMsg) {
			t.Fatalf("answer = %d %v, want 409 constraint %q", code, resp, blockIdentityTakenMsg)
		}
	}
	code, resp := callManage(t, h, ar, "issue-create", "", map[string]any{"title": "first"})
	if code != http.StatusOK {
		t.Fatalf("issue-create: %d %v", code, resp)
	}
	issueID := idOf(t, resp, "issue")
	if got := titleOf(t, issueID); got != "#L1 first" {
		t.Fatalf("issue title = %q, want #L1 first", got)
	}

	t.Run("manage issue-update onto a taken title", func(t *testing.T) {
		occupy(t, store.IssueTypeName, "#L1 taken")
		code, resp := callManage(t, h, ar, "issue-update", issueID, map[string]any{"title": "taken"})
		wantConflict(t, code, resp)
		if got := titleOf(t, issueID); got != "#L1 first" {
			t.Errorf("title after the refusal = %q, want it unchanged", got)
		}
	})

	t.Run("rest patch onto a taken title", func(t *testing.T) {
		occupy(t, store.IssueTypeName, "#L1 taken-rest")
		rec := w7Do(t, pool, reg, nil, ar, http.MethodPatch, "/api/project/"+pid+"/issues/"+issueID, `{"title":"taken-rest"}`)
		wantConflict(t, rec.Code, w6DecodeBody(t, rec))
	})

	t.Run("manage issue-create onto a taken title", func(t *testing.T) {
		occupy(t, store.IssueTypeName, "#L2 second")
		code, resp := callManage(t, h, ar, "issue-create", "", map[string]any{"title": "second"})
		wantConflict(t, code, resp)
	})

	t.Run("rest create onto a taken title", func(t *testing.T) {
		// The refused create above rolled back, so the next number is still 2.
		occupy(t, store.IssueTypeName, "#L2 second-rest")
		rec := w7Do(t, pool, reg, nil, ar, http.MethodPost, "/api/project/"+pid+"/issues", `{"title":"second-rest"}`)
		wantConflict(t, rec.Code, w6DecodeBody(t, rec))
	})

	t.Run("mcp issue_create onto a taken title", func(t *testing.T) {
		occupy(t, store.IssueTypeName, "#L2 second-mcp")
		res, _, err := mcpIssueCreateHandler(mcpCfg)(w12Ctx(ar), nil, issueCreateInput{Title: "second-mcp"})
		if err != nil {
			t.Fatalf("transport error: %v", err)
		}
		code := ""
		if env, ok := res.StructuredContent.(mcpErrorEnvelope); ok {
			code = env.Code
		}
		if !res.IsError || code != "constraint" || !strings.Contains(mcpText(res), blockIdentityTakenMsg) {
			t.Fatalf("mcp issue_create: error=%v code=%q %q, want the constraint refusal", res.IsError, code, mcpText(res))
		}
	})

	t.Run("comment create onto a taken title", func(t *testing.T) {
		occupy(t, store.CommentTypeName, "#L1.cL1 alice")
		code, resp := callManage(t, h, ar, "issue-comment-create", "", map[string]any{"parent_id": issueID, "author": "alice", "content": "c"})
		wantConflict(t, code, resp)

		rec := w7Do(t, pool, reg, nil, ar, http.MethodPost, "/api/project/"+pid+"/issues/"+issueID+"/comments", `{"author":"alice","content":"c"}`)
		wantConflict(t, rec.Code, w6DecodeBody(t, rec))

		res, _, err := mcpIssueCommentHandler(mcpCfg)(w12Ctx(ar), nil, issueCommentInput{IssueID: issueID, Author: "alice", Content: "c"})
		if err != nil {
			t.Fatalf("transport error: %v", err)
		}
		mcode := ""
		if env, ok := res.StructuredContent.(mcpErrorEnvelope); ok {
			mcode = env.Code
		}
		if !res.IsError || mcode != "constraint" || !strings.Contains(mcpText(res), blockIdentityTakenMsg) {
			t.Fatalf("mcp issue_comment: error=%v code=%q %q, want the constraint refusal", res.IsError, mcode, mcpText(res))
		}
	})

	t.Run("a title edit of a comment keeps its prefix", func(t *testing.T) {
		code, resp := callManage(t, h, ar, "issue-comment-create", "", map[string]any{"parent_id": issueID, "author": "bob", "content": "c"})
		if code != http.StatusOK {
			t.Fatalf("comment create: %d %v", code, resp)
		}
		commentID := idOf(t, resp, "comment")
		before := titleOf(t, commentID)
		if before != "#L1.cL1 bob" {
			t.Fatalf("comment title = %q, want #L1.cL1 bob", before)
		}
		code, resp = callManage(t, h, ar, "issue-update", commentID, map[string]any{"title": "carol"})
		if code != http.StatusOK {
			t.Fatalf("comment title edit: %d %v", code, resp)
		}
		if got := titleOf(t, commentID); got != "#L1.cL1 carol" {
			t.Errorf("comment title after the edit = %q, want #L1.cL1 carol (the prefix kept)", got)
		}
	})
}

// TestIngestChunkIdentityCollision is the ingest half of point two: InsertChunk's
// arbiter is the (source_id, chunk_index) slot, so a chunk whose (category,
// title, scope) another active block holds runs into the identity index on the
// insert. The per-chunk answer named a server fault ("insert chunk failed",
// ERROR line); it now names the collision, as the block writers do.
func TestIngestChunkIdentityCollision(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	h := NewIngestHandler(pool, nil, nil)
	keyCtx, _ := ingestKey(t, pool, "i275-ingest")
	if _, err := store.UpsertBlock(ctx, pool, "test", "i275 chunk title", "occupant", nil, nil, "private", true, store.SensitivityWrite{}, ""); err != nil {
		t.Fatalf("occupy: %v", err)
	}
	sourceID := seedSource(t, pool, "/i275/chunk.md", "private")
	got := postIdenticalChunk(t, h, keyCtx, sourceID, "i275 chunk title", "chunk text", nil, nil)
	if got["status"] != "failed" || !strings.Contains(fmt.Sprint(got["error"]), blockIdentityTakenMsg) {
		t.Errorf("chunk onto a taken identity: %v, want a failed chunk naming the collision %q", got, blockIdentityTakenMsg)
	}
}
