//go:build integration

// Issue #139 — MCP store and POST /api/ingest still use the content-only
// HashNOOPCheck that POST /api/store used before #64/#134. A second write with
// identical content but new tags or metadata therefore answers as a noop and
// drops those fields. REST now uses store.IdenticalContentMatch, whose contract
// also covers tags, metadata, sensitivity and type.
//
// Both probes seed through the production handler under test, then repeat the
// content with new tags and metadata. They are RED before the #139 fix: MCP
// answers "No change needed" and ingest answers status "skipped", while both
// blocks retain the seed tags and metadata.
//
// Run with:
//
//	go test -tags=integration -p 1 ./internal/handler/ -run 'Test(MCPStore|Ingest)IdenticalContentWritesTagsMetadata' -count=1 -v
package handler

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

type identicalContentState struct {
	id       string
	tags     []string
	metadata map[string]any
}

func readIdenticalContentState(t *testing.T, pool *pgxpool.Pool, title string) identicalContentState {
	t.Helper()
	var got identicalContentState
	if err := pool.QueryRow(context.Background(),
		`SELECT id::text, COALESCE(tags, '{}'), COALESCE(metadata, '{}'::jsonb)
		   FROM context_blocks
		  WHERE category = 'test' AND title = $1 AND scope = 'private' AND NOT is_archived`,
		title,
	).Scan(&got.id, &got.tags, &got.metadata); err != nil {
		t.Fatalf("read block %q: %v", title, err)
	}
	return got
}

func assertIdenticalContentFields(t *testing.T, got identicalContentState, wantTag, wantRun string) {
	t.Helper()
	if !slices.Equal(got.tags, []string{wantTag}) {
		t.Errorf("stored tags = %v, want [%s]", got.tags, wantTag)
	}
	if got.metadata["run"] != wantRun || got.metadata["surface"] != wantTag {
		t.Errorf("stored metadata = %v, want run=%q and surface=%q", got.metadata, wantRun, wantTag)
	}
}

func TestMCPStoreIdenticalContentWritesTagsMetadata(t *testing.T) {
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
	mcpCfg := MCPConfig{Pool: pool, Blocktypes: reg, Cfg: staticConfigStore{cfg: &config.Config{
		Query:  config.QueryConfig{RateLimitWrite: 0},
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
	}}}
	storeTool := mcpStoreHandler(mcpCfg)

	_, plain, err := store.CreateApiKey(ctx, pool, "i139-mcp", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	keyCtx := mcpAuthCtx(t, ctx, pool, plain)
	const (
		title   = "i139 MCP identical content"
		content = "identical content with changing tags and metadata"
	)
	call := func(tag, run string) (string, bool) {
		t.Helper()
		r, _, err := storeTool(keyCtx, nil, storeInput{
			Category: "test",
			Title:    title,
			Content:  content,
			Tags:     []string{tag},
			Metadata: map[string]any{"run": run, "surface": tag},
		})
		if err != nil {
			t.Fatalf("mcp store: protocol error %v", err)
		}
		return resultText(t, r), r.IsError
	}

	if text, isErr := call("mcp-seed", "seed"); isErr {
		t.Fatalf("seed store rejected: %s", text)
	}
	seeded := readIdenticalContentState(t, pool, title)
	assertIdenticalContentFields(t, seeded, "mcp-seed", "seed")

	text, isErr := call("mcp-updated", "updated")
	if isErr {
		t.Errorf("identical-content update rejected: %s", text)
	}
	if strings.Contains(text, "No change needed") || !strings.HasPrefix(text, "Stored:") {
		t.Errorf("identical-content update response = %q, want a Stored response, not noop", text)
	}
	got := readIdenticalContentState(t, pool, title)
	if got.id != seeded.id {
		t.Errorf("block id changed: seed=%s update=%s", seeded.id, got.id)
	}
	assertIdenticalContentFields(t, got, "mcp-updated", "updated")
}

func TestIngestIdenticalContentWritesTagsMetadata(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	h := NewIngestHandler(pool, nil, nil)
	keyCtx, _ := ingestKey(t, pool, "i139-ingest")
	const (
		title   = "i139 ingest identical content"
		content = "identical content with changing tags and metadata"
	)
	body := func(tag, run string) map[string]any {
		return map[string]any{"source": "i139", "chunks": []map[string]any{{
			"category": "test",
			"title":    title,
			"content":  content,
			"tags":     []string{tag},
			"metadata": map[string]any{"run": run, "surface": tag},
		}}}
	}

	status, resp := postIngest(t, h, keyCtx, body("ingest-seed", "seed"))
	if status != http.StatusOK {
		t.Fatalf("seed status = %d, want 200 (body %v)", status, resp)
	}
	seedResult := firstResult(t, resp)
	if seedResult["status"] == "failed" {
		t.Fatalf("seed ingest failed: %v", seedResult)
	}
	seeded := readIdenticalContentState(t, pool, title)
	assertIdenticalContentFields(t, seeded, "ingest-seed", "seed")

	status, resp = postIngest(t, h, keyCtx, body("ingest-updated", "updated"))
	if status != http.StatusOK {
		t.Fatalf("update status = %d, want 200 (body %v)", status, resp)
	}
	result := firstResult(t, resp)
	if result["status"] != "updated" {
		t.Errorf("identical-content update result = %v, want status updated, not skipped/noop", result)
	}
	if result["id"] != seeded.id {
		t.Errorf("result id = %v, want seeded block %s", result["id"], seeded.id)
	}
	got := readIdenticalContentState(t, pool, title)
	if got.id != seeded.id {
		t.Errorf("block id changed: seed=%s update=%s", seeded.id, got.id)
	}
	assertIdenticalContentFields(t, got, "ingest-updated", "updated")
}

type identicalChunkState struct {
	id             string
	category       string
	title          string
	content        string
	tags           []string
	metadata       map[string]any
	scope          string
	sourceID       string
	chunkIndex     int
	lifecycleState string
	createdAt      time.Time
	updatedAt      time.Time
}

func postIdenticalChunk(t *testing.T, h *IngestHandler, keyCtx context.Context, sourceID, title, content string, tags []string, metadata map[string]any) map[string]any {
	t.Helper()
	status, resp := postIngest(t, h, keyCtx, map[string]any{"source": "i139-chunk", "chunks": []map[string]any{{
		"category":    "test",
		"title":       title,
		"content":     content,
		"tags":        tags,
		"metadata":    metadata,
		"source_id":   sourceID,
		"chunk_index": 0,
	}}})
	if status != http.StatusOK {
		t.Fatalf("ingest status = %d, want 200 (body %v)", status, resp)
	}
	return firstResult(t, resp)
}

func readIdenticalChunkState(t *testing.T, pool *pgxpool.Pool, id string) identicalChunkState {
	t.Helper()
	var got identicalChunkState
	if err := pool.QueryRow(context.Background(),
		`SELECT id::text, category, title, content, COALESCE(tags, '{}'),
		        COALESCE(metadata, '{}'::jsonb), scope, source_id::text,
		        chunk_index, lifecycle_state, created_at, updated_at
		   FROM context_blocks
		  WHERE id = $1::uuid`, id,
	).Scan(&got.id, &got.category, &got.title, &got.content, &got.tags, &got.metadata,
		&got.scope, &got.sourceID, &got.chunkIndex, &got.lifecycleState, &got.createdAt, &got.updatedAt); err != nil {
		t.Fatalf("read chunk %s: %v", id, err)
	}
	return got
}

func TestIngestIdenticalContentChunkMode(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	h := NewIngestHandler(pool, nil, nil)
	keyCtx, _ := ingestKey(t, pool, "i139-ingest-chunk")

	t.Run("identical_reingest_skips_without_rewrite", func(t *testing.T) {
		sourceID := seedSource(t, pool, "/i139/chunk-identical.md", "private")
		const (
			title   = "i139 chunk identical reingest"
			content = "i139 chunk content that must not be rewritten"
		)
		tags := []string{"chunk-seed"}
		metadata := map[string]any{"run": "seed", "surface": "chunk-seed"}
		seed := postIdenticalChunk(t, h, keyCtx, sourceID, title, content, tags, metadata)
		if seed["status"] != "inserted" {
			t.Fatalf("seed result = %v, want inserted", seed)
		}
		seededID, _ := seed["id"].(string)
		if seededID == "" {
			t.Fatalf("seed result has no id: %v", seed)
		}
		seeded := readIdenticalChunkState(t, pool, seededID)

		again := postIdenticalChunk(t, h, keyCtx, sourceID, title, content, tags, metadata)
		if again["status"] != "skipped" || again["id"] != seededID {
			t.Errorf("identical reingest = %v, want skipped id %s", again, seededID)
		}
		if got := readIdenticalChunkState(t, pool, seededID); !reflect.DeepEqual(got, seeded) {
			t.Errorf("identical reingest changed the row:\nseeded: %+v\n     got: %+v", seeded, got)
		}
	})

	t.Run("new_tags_and_metadata_are_written", func(t *testing.T) {
		sourceID := seedSource(t, pool, "/i139/chunk-updated.md", "private")
		const (
			title   = "i139 chunk changed fields"
			content = "i139 chunk content with changed tags and metadata"
		)
		seed := postIdenticalChunk(t, h, keyCtx, sourceID, title, content,
			[]string{"chunk-seed"}, map[string]any{"run": "seed", "surface": "chunk-seed", "seed_only": true})
		if seed["status"] != "inserted" {
			t.Fatalf("seed result = %v, want inserted", seed)
		}
		seededID, _ := seed["id"].(string)
		seeded := readIdenticalChunkState(t, pool, seededID)

		wantMetadata := map[string]any{"run": "updated", "surface": "chunk-updated"}
		updated := postIdenticalChunk(t, h, keyCtx, sourceID, title, content, []string{"chunk-updated"}, wantMetadata)
		if updated["status"] != "inserted" || updated["id"] != seededID {
			t.Errorf("changed-fields reingest = %v, want inserted id %s, not skipped", updated, seededID)
		}
		got := readIdenticalChunkState(t, pool, seededID)
		if !slices.Equal(got.tags, []string{"chunk-updated"}) {
			t.Errorf("stored tags = %v, want [chunk-updated]", got.tags)
		}
		if !reflect.DeepEqual(got.metadata, wantMetadata) {
			t.Errorf("stored metadata = %v, want replacement %v", got.metadata, wantMetadata)
		}
		if got.updatedAt.Equal(seeded.updatedAt) {
			t.Errorf("updated_at did not advance when tags and metadata changed")
		}
	})

	t.Run("second_source_skips_not_fails", func(t *testing.T) {
		firstSource := seedSource(t, pool, "/i139/chunk-first-source.md", "private")
		secondSource := seedSource(t, pool, "/i139/chunk-second-source.md", "private")
		const (
			title   = "i139 chunk second source"
			content = "i139 identical chunk content under another source"
		)
		tags := []string{"chunk-same"}
		metadata := map[string]any{"run": "same", "surface": "chunk-same"}
		seed := postIdenticalChunk(t, h, keyCtx, firstSource, title, content, tags, metadata)
		if seed["status"] != "inserted" {
			t.Fatalf("seed result = %v, want inserted", seed)
		}
		seededID, _ := seed["id"].(string)
		seeded := readIdenticalChunkState(t, pool, seededID)

		again := postIdenticalChunk(t, h, keyCtx, secondSource, title, content, tags, metadata)
		if again["status"] != "skipped" || again["id"] != seededID {
			t.Errorf("second-source ingest = %v, want skipped id %s, not failed", again, seededID)
		}
		if got := readIdenticalChunkState(t, pool, seededID); !reflect.DeepEqual(got, seeded) {
			t.Errorf("second-source ingest changed the first row:\nseeded: %+v\n     got: %+v", seeded, got)
		}
		var rows int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM context_blocks
			  WHERE category = 'test' AND title = $1 AND scope = 'private' AND NOT is_archived`, title,
		).Scan(&rows); err != nil {
			t.Fatalf("count second-source chunks: %v", err)
		}
		if rows != 1 {
			t.Errorf("live rows after second-source ingest = %d, want 1", rows)
		}
	})

	t.Run("second_source_different_tags_and_metadata_skips", func(t *testing.T) {
		firstSource := seedSource(t, pool, "/i139/chunk-first-source-changed.md", "private")
		secondSource := seedSource(t, pool, "/i139/chunk-second-source-changed.md", "private")
		const (
			title   = "i139 chunk second source changed fields"
			content = "i139 identical chunk content with different source fields"
		)
		seed := postIdenticalChunk(t, h, keyCtx, firstSource, title, content,
			[]string{"chunk-first"}, map[string]any{"run": "first", "surface": "chunk-first"})
		if seed["status"] != "inserted" {
			t.Fatalf("seed result = %v, want inserted", seed)
		}
		seededID, _ := seed["id"].(string)
		seeded := readIdenticalChunkState(t, pool, seededID)

		again := postIdenticalChunk(t, h, keyCtx, secondSource, title, content,
			[]string{"chunk-second"}, map[string]any{"run": "second", "surface": "chunk-second"})
		if again["status"] != "skipped" || again["id"] != seededID {
			t.Errorf("second-source changed-fields ingest = %v, want skipped id %s, not failed", again, seededID)
		}
		if got := readIdenticalChunkState(t, pool, seededID); !reflect.DeepEqual(got, seeded) {
			t.Errorf("second-source changed-fields ingest changed the first row:\nseeded: %+v\n     got: %+v", seeded, got)
		}
		var rows int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM context_blocks
			  WHERE category = 'test' AND title = $1 AND scope = 'private' AND NOT is_archived`, title,
		).Scan(&rows); err != nil {
			t.Fatalf("count second-source changed-fields chunks: %v", err)
		}
		if rows != 1 {
			t.Errorf("live rows after second-source changed-fields ingest = %d, want 1", rows)
		}
	})

	t.Run("block_mode_row_different_tags_skips", func(t *testing.T) {
		sourceID := seedSource(t, pool, "/i139/chunk-onto-block.md", "private")
		const (
			title   = "i139 chunk onto block-mode row"
			content = "i139 identical content already held by a block-mode row"
		)
		status, resp := postIngest(t, h, keyCtx, map[string]any{"source": "i139-block", "chunks": []map[string]any{{
			"category": "test",
			"title":    title,
			"content":  content,
			"tags":     []string{"block-seed"},
			"metadata": map[string]any{"run": "seed", "surface": "block-seed"},
		}}})
		if status != http.StatusOK {
			t.Fatalf("seed status = %d, want 200 (body %v)", status, resp)
		}
		seed := firstResult(t, resp)
		if seed["status"] != "updated" {
			t.Fatalf("block-mode seed result = %v, want updated", seed)
		}
		seededID, _ := seed["id"].(string)
		seeded := readIdenticalContentState(t, pool, title)

		again := postIdenticalChunk(t, h, keyCtx, sourceID, title, content,
			[]string{"chunk-different"}, map[string]any{"run": "seed", "surface": "block-seed"})
		if again["status"] != "skipped" || again["id"] != seededID {
			t.Errorf("chunk onto block-mode row = %v, want skipped id %s, not failed", again, seededID)
		}
		if got := readIdenticalContentState(t, pool, title); !reflect.DeepEqual(got, seeded) {
			t.Errorf("chunk onto block-mode row changed the block:\nseeded: %+v\n     got: %+v", seeded, got)
		}
	})
}
