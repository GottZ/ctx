//go:build integration

// End to end for the Hermes plugin ctx_checkpoint (GottZ/ctx#290): the MCP
// endpoint as ctxd mounts it (Auth middleware, then the Streamable HTTP
// handler) on a fresh database, and the plugin's checkpoint chain written
// against it through Hermes' own MCP client
// (integrations/hermes/plugin/ctx_checkpoint/tests/e2e_real_ctx.py).
//
// It needs a Python with hermes-agent's dependencies and the hermes-agent
// checkout: CTX_CHECKPOINT_E2E_PYTHON names the interpreter, HERMES_REPO the
// checkout. Without CTX_CHECKPOINT_E2E_PYTHON the test skips — the ci.yml
// integration shards have no Hermes; the workflow hermes-ctx-checkpoint.yml
// (job e2e) sets both and runs it.
//
// Run with:
//
//	CTX_CHECKPOINT_E2E_PYTHON=/path/to/venv/bin/python HERMES_REPO=/path/to/hermes-agent \
//	  go test -tags=integration -p 1 ./internal/handler/ -run 'TestCheckpointPluginChainAgainstMCPEndpoint' -count=1 -v
package handler

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// checkpointE2EDriver is the plugin-side driver, relative to this package
// directory (go/internal/handler).
const checkpointE2EDriver = "../../../integrations/hermes/plugin/ctx_checkpoint/tests/e2e_real_ctx.py"

func TestCheckpointPluginChainAgainstMCPEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	python := os.Getenv("CTX_CHECKPOINT_E2E_PYTHON")
	if python == "" {
		t.Skip("CTX_CHECKPOINT_E2E_PYTHON is not set (the ctx_checkpoint workflow runs this test)")
	}
	hermes := os.Getenv("HERMES_REPO")
	if hermes == "" {
		t.Fatal("CTX_CHECKPOINT_E2E_PYTHON is set but HERMES_REPO is not")
	}
	driver, err := filepath.Abs(filepath.FromSlash(checkpointE2EDriver))
	if err != nil {
		t.Fatalf("driver path: %v", err)
	}

	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	mcpH := NewMCPHandler(MCPConfig{Pool: pool, Blocktypes: reg, Cfg: staticConfigStore{cfg: &config.Config{
		Query:  config.QueryConfig{RateLimitWrite: 0},
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
	}}})
	srv := httptest.NewServer(Auth(pool)(mcpH))
	t.Cleanup(srv.Close)

	_, plain, err := store.CreateApiKey(ctx, pool, "i290-checkpoint-e2e", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(runCtx, python, "-I", driver)
	// The key goes by environment, never on the command line.
	cmd.Env = append(os.Environ(),
		"CTX_E2E_URL="+srv.URL+"/",
		"CTX_E2E_KEY="+plain,
		"HERMES_REPO="+hermes,
		"HERMES_HOME="+t.TempDir(),
		"PYTHONDONTWRITEBYTECODE=1",
	)
	out, err := cmd.CombinedOutput()
	t.Logf("driver output:\n%s", out)
	if err != nil {
		t.Fatalf("the plugin chain against the MCP endpoint failed: %v", err)
	}

	// Two roots wrote the same transcript (GottZ/ctx#344): each has its own head and manifest,
	// and each part's text is in the database once — both manifests list the same blocks.
	var heads, manifests, parts, texts, listed int
	if err := pool.QueryRow(ctx, `
		WITH chain AS (
		  SELECT title, content, metadata FROM context_blocks
		   WHERE category = 'compaction-checkpoints' AND type_name = 'checkpoint' AND NOT is_archived)
		SELECT (SELECT count(*) FROM chain WHERE title LIKE 'Compaction checkpoint head %'),
		       (SELECT count(*) FROM chain WHERE title LIKE '% manifest'),
		       (SELECT count(*) FROM chain WHERE title LIKE 'Compaction source part %'),
		       (SELECT count(DISTINCT content) FROM chain WHERE title LIKE 'Compaction source part %'),
		       (SELECT count(DISTINCT e.pid) FROM chain, jsonb_array_elements_text(metadata->'source_block_ids') e(pid))`,
	).Scan(&heads, &manifests, &parts, &texts, &listed); err != nil {
		t.Fatalf("count the chain blocks: %v", err)
	}
	if heads != 2 || manifests != 2 {
		t.Errorf("the database holds %d heads and %d manifests after two roots, want 2 and 2", heads, manifests)
	}
	if parts < 2 || texts != parts || listed != parts {
		t.Errorf("parts: %d blocks, %d distinct texts, %d listed ids — want one block per text, every one listed", parts, texts, listed)
	}
}
