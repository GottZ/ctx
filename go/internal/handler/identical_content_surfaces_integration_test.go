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
