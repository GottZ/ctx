//go:build integration

// End to end for the Hermes plugin ctx_checkpoint (GottZ/ctx#290): the MCP
// endpoint as ctxd mounts it (Auth middleware, then the Streamable HTTP
// handler) on a fresh database, and the plugin's checkpoint chain written
// against it through Hermes' own MCP client
// (integrations/hermes/plugin/ctx_checkpoint/tests/e2e_real_ctx.py). The driver
// also runs both plugins' checkpoint head lookups (ctx_checkpoint and
// ctx_handover, #342): with `types` against this server, and against this server
// answering as one before #342 (the refusal of the shared answer fixture) and as
// one that leaves never-ranked types out of a search without `types`.
//
// The Go command below starts the MCP handler of its own checkout, so it checks
// the current server. CTX_E2E_SERVER=before-342 changes what the driver
// expects, not the server: the endpoint itself must refuse the type. To check
// the plugins against a real older server, start that server on its own (a
// checkout or image of the older release, with a key of its database) and run
// the driver from this checkout against it:
//
//	CTX_E2E_URL=https://<older server>/mcp CTX_E2E_KEY=<key> HERMES_REPO=/path/to/hermes-agent \
//	  CTX_E2E_SERVER=before-342 /path/to/venv/bin/python -I \
//	  integrations/hermes/plugin/ctx_checkpoint/tests/e2e_real_ctx.py
//
// The driver writes a chain under its own two root ids into that database. A
// checkout from #290 on also has this Go test: with this driver and both plugin
// directories copied into it, the Go command there with CTX_E2E_SERVER set
// starts the older handler on a fresh test database instead.
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
	"slices"
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
	var heads, manifests, parts, texts int
	var stored, listed []string
	if err := pool.QueryRow(ctx, `
		WITH chain AS (
		  SELECT id, title, content, metadata FROM context_blocks
		   WHERE category = 'compaction-checkpoints' AND type_name = 'checkpoint' AND NOT is_archived)
		SELECT (SELECT count(*) FROM chain WHERE title LIKE 'Compaction checkpoint head %'),
		       (SELECT count(*) FROM chain WHERE title LIKE '% manifest'),
		       (SELECT count(*) FROM chain WHERE title LIKE 'Compaction source part %'),
		       (SELECT count(DISTINCT content) FROM chain WHERE title LIKE 'Compaction source part %'),
		       (SELECT COALESCE(array_agg(id::text ORDER BY id::text), '{}') FROM chain
		         WHERE title LIKE 'Compaction source part %'),
		       (SELECT COALESCE(array_agg(DISTINCT e.pid ORDER BY e.pid), '{}')
		          FROM chain, jsonb_array_elements_text(metadata->'source_block_ids') e(pid))`,
	).Scan(&heads, &manifests, &parts, &texts, &stored, &listed); err != nil {
		t.Fatalf("count the chain blocks: %v", err)
	}
	if heads != 2 || manifests != 2 {
		t.Errorf("the database holds %d heads and %d manifests after two roots, want 2 and 2", heads, manifests)
	}
	if parts < 2 || texts != parts {
		t.Errorf("parts: %d blocks, %d distinct texts — want one block per text", parts, texts)
	}
	// The ids both manifests list are exactly the stored parts: none dangling, none unlisted.
	if !slices.Equal(stored, listed) {
		t.Errorf("stored part ids %v, ids the manifests list %v — want the same set", stored, listed)
	}
}
