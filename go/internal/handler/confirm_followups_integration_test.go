//go:build integration

// Issue #275, points three and five.
//
// A staged update of a derivative (a block carrying the derived layer's
// provenance key) can never execute: UpdateBlock's WHERE excludes it and the
// confirm answers provenance_protected with the token consumed. The archive
// stage refuses such a target before a card exists (mcpStageArchive, D1-M2:
// a card promises a write the confirm can execute); the update stage on MCP
// and in chat issued the card. Now both refuse at stage time, as archive
// does.
//
// A confirmed write that fails for a server fault answered on MCP with the
// raw error text of the store (`%v`), SQL detail included, where REST answers
// an opaque message and logs the cause. The MCP confirm now does the same.
//
//	go test -tags=integration ./internal/handler/ -run 'TestStagedUpdateOfDerivativeRefusedAtStage|TestMCPConfirmFaultIsOpaque' -count=1 -v
package handler

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// i275Fixture is a pool with settings and a booted registry, and the context
// of a confirm_writes key whose home scope is 'private'.
func i275Fixture(t *testing.T) (*pgxpool.Pool, MCPConfig, context.Context) {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")
	t.Setenv(settings.EnvDisable, "")
	envCfg, issues := config.FromEnv()
	issues = append(issues, config.Validate(envCfg)...)
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	cfgStore := config.NewStore(envCfg)
	if err := settings.Reload(ctx, pool, cfgStore); err != nil {
		t.Fatalf("settings reload: %v", err)
	}
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	row, plain, err := store.CreateApiKey(ctx, pool, "i275-flagged", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE context_api_keys SET confirm_writes = true WHERE id = $1`, row.ID); err != nil {
		t.Fatalf("opt in key: %v", err)
	}
	return pool, MCPConfig{Pool: pool, Cfg: cfgStore, Blocktypes: reg}, mcpAuthCtx(t, ctx, pool, plain)
}

func TestStagedUpdateOfDerivativeRefusedAtStage(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, mcpCfg, keyCtx := i275Fixture(t)
	ctx := context.Background()
	derivative, err := store.UpsertBlock(ctx, pool, "learnings", "i275 derivative", "derived text", nil,
		map[string]any{"provenance": map[string]any{"v": 1, "arm": "i275-arm", "stratum": 1}}, "private", true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed derivative: %v", err)
	}
	cards := func(t *testing.T) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM context_pending_writes WHERE payload->>'id' = $1`, derivative.ID).Scan(&n); err != nil {
			t.Fatalf("count cards: %v", err)
		}
		return n
	}

	t.Run("mcp", func(t *testing.T) {
		content := "client text"
		r, _, err := mcpUpdateHandler(mcpCfg)(keyCtx, nil, updateInput{ID: derivative.ID, Content: &content})
		if err != nil {
			t.Fatalf("mcp update: %v", err)
		}
		code := ""
		if env, ok := r.StructuredContent.(mcpErrorEnvelope); ok {
			code = env.Code
		}
		if !r.IsError || code != "provenance_protected" {
			t.Errorf("stage of an update of a derivative: error=%v code=%q text=%q, want the provenance_protected refusal", r.IsError, code, resultText(t, r))
		}
		if n := cards(t); n != 0 {
			t.Errorf("%d card(s) staged for an update the confirm can never execute, want 0", n)
		}
	})

	t.Run("chat", func(t *testing.T) {
		runner := &chatStageRunner{pool: pool, cfg: mcpCfg.Cfg, blocktypes: mcpCfg.Blocktypes}
		content := "chat text"
		staged, reject, err := runner.StageUpdate(keyCtx, derivative.ID, nil, nil, &content, nil, nil)
		if err != nil {
			t.Fatalf("chat StageUpdate: %v", err)
		}
		if staged != nil || !strings.Contains(reject, "derived provenance") {
			t.Errorf("chat stage of an update of a derivative: card=%v reject=%q, want a refusal naming the provenance", staged, reject)
		}
		if n := cards(t); n != 0 {
			t.Errorf("%d card(s) staged for an update the confirm can never execute, want 0", n)
		}
	})
}

func TestMCPConfirmFaultIsOpaque(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, mcpCfg, keyCtx := i275Fixture(t)
	ctx := context.Background()
	b, err := store.UpsertBlock(ctx, pool, "learnings", "i275 fault target", "first", nil, nil, "private", true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A server fault the confirm cannot classify: a trigger that raises on
	// exactly this content, with a detail no client should read.
	for _, st := range []string{
		`CREATE FUNCTION i275_fault() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN RAISE EXCEPTION 'i275-internal-detail'; END $$`,
		`CREATE TRIGGER i275_fault BEFORE UPDATE ON context_blocks FOR EACH ROW
		 WHEN (NEW.content = 'i275 boom') EXECUTE FUNCTION i275_fault()`,
	} {
		if _, err := pool.Exec(ctx, st); err != nil {
			t.Fatalf("fault trigger: %v", err)
		}
	}
	logs := &i227LogBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	content := "i275 boom"
	r, _, err := mcpUpdateHandler(mcpCfg)(keyCtx, nil, updateInput{ID: b.ID, Content: &content})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	m := hashRe.FindStringSubmatch(resultText(t, r))
	if m == nil {
		t.Fatalf("update was not staged: %s", resultText(t, r))
	}
	r, _, err = mcpConfirmHandler(mcpCfg)(keyCtx, nil, confirmInput{PayloadHash: m[1]})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	text := resultText(t, r)
	if !r.IsError || !strings.Contains(text, "stage token is consumed") {
		t.Fatalf("confirm of a faulting update: error=%v %q, want the consumed-token failure", r.IsError, text)
	}
	if strings.Contains(text, "i275-internal-detail") || strings.Contains(text, "SQLSTATE") {
		t.Errorf("the MCP confirm answer carries the server's error text: %q", text)
	}
	if !strings.Contains(logs.take(), "i275-internal-detail") {
		t.Errorf("the cause of the fault is not in the log")
	}
}

// TestConfirmedWriteRefusalsKeepTheirClass: a confirmed write that the store
// refuses for a cause in the payload answers that cause, coded, on the MCP
// confirm as on the direct tool — not the opaque server fault. A staged store
// onto the identity of a derivative is refused by UpsertBlock's S3 guard
// (provenance_protected, as the direct store answers it); a staged blob_store
// that violates an integrity constraint is the request's (constraint, as the
// direct blob_store answers it). Both surfaced as the raw error text before
// the confirm fault became opaque, and would otherwise read "internal error".
func TestConfirmedWriteRefusalsKeepTheirClass(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, mcpCfg, keyCtx := i275Fixture(t)
	ctx := context.Background()
	confirm := func(t *testing.T, staged string) (text, code string) {
		t.Helper()
		m := hashRe.FindStringSubmatch(staged)
		if m == nil {
			t.Fatalf("write was not staged: %s", staged)
		}
		r, _, err := mcpConfirmHandler(mcpCfg)(keyCtx, nil, confirmInput{PayloadHash: m[1]})
		if err != nil {
			t.Fatalf("confirm: %v", err)
		}
		if env, ok := r.StructuredContent.(mcpErrorEnvelope); ok {
			code = env.Code
		}
		return resultText(t, r), code
	}

	t.Run("store onto a derivative", func(t *testing.T) {
		if _, err := store.UpsertBlock(ctx, pool, "learnings", "i275 derived store", "derived text", nil,
			map[string]any{"provenance": map[string]any{"v": 1, "arm": "i275-arm", "stratum": 1}}, "private", true, store.SensitivityWrite{}, ""); err != nil {
			t.Fatalf("seed derivative: %v", err)
		}
		r, _, err := mcpStoreHandler(mcpCfg)(keyCtx, nil, storeInput{Category: "learnings", Title: "i275 derived store", Content: "client text"})
		if err != nil {
			t.Fatalf("stage store: %v", err)
		}
		text, code := confirm(t, resultText(t, r))
		if code != "provenance_protected" || !strings.Contains(text, "stage token is consumed") {
			t.Errorf("confirmed store onto a derivative: code=%q %q, want provenance_protected with the consumed token", code, text)
		}
	})

	t.Run("blob_store violating a constraint", func(t *testing.T) {
		for _, st := range []string{
			`CREATE FUNCTION i275_blob_check() RETURNS trigger LANGUAGE plpgsql AS $$
			 BEGIN RAISE EXCEPTION 'i275 blob check' USING ERRCODE = 'check_violation', CONSTRAINT = 'i275_blob_check'; END $$`,
			`CREATE TRIGGER i275_blob_check BEFORE INSERT ON context_blobs FOR EACH ROW
			 WHEN (NEW.title = 'i275 blob') EXECUTE FUNCTION i275_blob_check()`,
		} {
			if _, err := pool.Exec(ctx, st); err != nil {
				t.Fatalf("blob trigger: %v", err)
			}
		}
		r, _, err := mcpBlobStoreHandler(mcpCfg)(keyCtx, nil, blobStoreInput{Category: "test", Title: "i275 blob", Filename: "a.txt", MimeType: "text/plain", Text: "payload"})
		if err != nil {
			t.Fatalf("stage blob_store: %v", err)
		}
		text, code := confirm(t, resultText(t, r))
		if code != "constraint" || !strings.Contains(text, "i275_blob_check") || !strings.Contains(text, "stage token is consumed") {
			t.Errorf("confirmed blob_store violating a constraint: code=%q %q, want constraint naming the constraint, with the consumed token", code, text)
		}
	})
}
