//go:build integration

// Issue #191, second lifecycle writer: the daily report upserts on
// (category, title, scope) and then stamps the returned row
// lifecycle_state='synthesis'. When an ingested chunk already holds that
// identity, the upsert lands on the chunk row — and the stamp moved the chunk
// out of InsertChunk's arbiter (lifecycle_state = 'chunk'), so the next
// changed-content ingest of that slot failed on
// uq_context_category_title_scope. The chunk is written by store.InsertChunk,
// the production writer behind POST /api/ingest's chunk mode; the source row
// is seeded directly because ctxd has no endpoint that creates one.
//
//	go test -tags=integration -p 1 ./internal/dream/ -run TestGenerateDailyReport_ChunkHeldTitleKeepsChunkLifecycle -count=1 -v
package dream_test

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestGenerateDailyReport_ChunkHeldTitleKeepsChunkLifecycle(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var sourceID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_sources (file_path, file_hash, scope) VALUES ('/i191/reports.md', 'i191', $1) RETURNING id::text`,
		reportScope).Scan(&sourceID); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	title := "Tagesbericht " + time.Now().UTC().Format("2006-01-02")
	chunk, err := store.InsertChunk(ctx, pool, sourceID, 0, "learnings", title,
		"i191 an ingested note that carries the report's title", nil, nil, reportScope)
	if err != nil {
		t.Fatalf("ingest chunk: %v", err)
	}

	mockChatJSONExternal(t, func(_ context.Context, _, _, _ string, _ *bool, _, _ string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		return &llm.ChatResponse{Message: llm.Message{Role: "assistant", Content: "i191 report body."}}, nil
	})
	reportID, err := dream.GenerateDailyReport(ctx, pool, reportRouter(t, ctx, pool), reportScope)
	if err != nil {
		t.Fatalf("generate report: %v", err)
	}
	if reportID != chunk.ID {
		t.Fatalf("report id = %q, want the chunk row %s that holds (learnings, %q, %s)", reportID, chunk.ID, title, reportScope)
	}

	var lifecycle string
	if err := pool.QueryRow(ctx, `SELECT lifecycle_state FROM context_blocks WHERE id = $1::uuid`, chunk.ID).Scan(&lifecycle); err != nil {
		t.Fatalf("read lifecycle: %v", err)
	}
	if lifecycle != "chunk" {
		t.Errorf("chunk row after the daily report: lifecycle_state = %q, want chunk", lifecycle)
	}

	again, err := store.InsertChunk(ctx, pool, sourceID, 0, "learnings", title,
		"i191 the note, edited at the source", nil, nil, reportScope)
	if err != nil {
		t.Fatalf("changed-content ingest of the slot: %v", err)
	}
	if again.ID != chunk.ID {
		t.Errorf("changed-content ingest wrote row %s, want the slot row %s", again.ID, chunk.ID)
	}
}
