//go:build integration

package dream_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/testdb"
)

// reportRouter is testRouterFor plus a DB-booted block-type registry (WF T4:
// GenerateDailyReport classifies via Router.TypeSet — rules are M072 data).
func reportRouter(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *dream.Router {
	t.Helper()
	r := testRouterFor("h", "m", "")
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	r.Blocktypes = reg
	return r
}

// testRouterFor routes the integration tests through a seeded
// single-backend pool (G28): one enabled full-trust row carrying the dream +
// digest roles. With the chatJSON seam installed, host/model never reach the
// wire; without it (prompt regression), the row's protocol selects the path.
func testRouterFor(host, model string, protocol backends.Protocol) *dream.Router {
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{{
		ID: "it-backend", Name: "it-backend",
		Host: host, APIKey: "k", Protocol: protocol,
		Trust: backends.TrustFull, Locality: "lan",
		Roles:    []string{backends.RoleDream, backends.RoleDigest},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: model}},
		Priority: 100, Enabled: true,
	}})
	return &dream.Router{Pool: p, Admit: reportAdmission()}
}

// mockChatJSONExternal replaces dream.ChatJSON for the duration of one test.
// It mirrors the in-package mockChatJSON helper used in evaluate_test.go but
// goes through the exported SetChatJSON test seam (declared in this same
// _test.go file via build-tag `integration`).
func mockChatJSONExternal(t *testing.T, fn func(ctx context.Context, host, apiKey, model string, think *bool, systemPrompt, userPrompt string, opts llm.Options, timeout time.Duration) (*llm.ChatResponse, error)) {
	t.Helper()
	saved := dream.SetChatJSONForTest(fn)
	t.Cleanup(func() { dream.SetChatJSONForTest(saved) })
}

const reportScope = "private"

// seedDecisionRows inserts a small audit-log volume so the daily aggregation
// returns non-empty stats. Rows are timestamped within the last 24h.
func seedDecisionRows(t *testing.T, pool *pgxpool.Pool, scope string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := pool.Exec(ctx,
		`INSERT INTO context_write_log (decision, scope, created_at)
		 SELECT decision, $1, now() - (offset_min || ' minutes')::interval
		 FROM (VALUES
			('dream_link', 5),
			('dream_link', 30),
			('dream_link', 60),
			('clean', 90),
			('clean', 120)
		 ) AS v(decision, offset_min)`,
		scope,
	)
	if err != nil {
		t.Fatalf("seed write log: %v", err)
	}
}

// seedSynthesisBlock inserts one fresh block within the 24h window so the
// "new blocks" axis of the report has content.
func seedSynthesisBlock(t *testing.T, pool *pgxpool.Pool, scope string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope, created_at, updated_at)
		 VALUES ('decisions', 'fresh block for synthesis', 'content', $1, now(), now())`,
		scope,
	)
	if err != nil {
		t.Fatalf("seed fresh block: %v", err)
	}
}

// seedLinkAnchorBlock inserts one block OUTSIDE the 24h window (2 days old)
// so it can anchor structural links without polluting the "Neue Blocks 24h"
// prompt section.
func seedLinkAnchorBlock(t *testing.T, pool *pgxpool.Pool, scope, title string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_blocks (category, title, content, scope, created_at, updated_at)
		 VALUES ('reference', $2, 'link anchor', $1, now() - interval '2 days', now() - interval '2 days')
		 RETURNING id::text`,
		scope, title,
	).Scan(&id); err != nil {
		t.Fatalf("seed link anchor block: %v", err)
	}
	return id
}

// seedStructuralLink raw-inserts one structural edge with a chosen age. Raw
// SQL on purpose: the aggregation under test must count whatever the table
// holds, independent of the write-path guards.
func seedStructuralLink(t *testing.T, pool *pgxpool.Pool, scope, src, dst, class, origin, age string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx,
		`INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin, created_at)
		 VALUES ($1::uuid, $2::uuid, $3, $4, $5, now() - $6::interval)`,
		src, dst, class, scope, origin, age,
	); err != nil {
		t.Fatalf("seed structural link %s→%s: %v", src, dst, err)
	}
}

func TestGenerateDailyReport_HappyPath(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedDecisionRows(t, pool, reportScope)
	seedSynthesisBlock(t, pool, reportScope)

	mockChatJSONExternal(t, func(_ context.Context, _, _, _ string, _ *bool, _, _ string, opts llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		// Report length is EOS-terminated: a NumPredict cap on the synthesis
		// call truncated every report mid-sentence (66× completion_tokens=400).
		if opts.NumPredict != 0 {
			t.Errorf("daily synthesis must not cap NumPredict, got %d", opts.NumPredict)
		}
		return &llm.ChatResponse{
			Message:      llm.Message{Role: "assistant", Content: "Tagesbericht-content. Test fix: dream-link 52, clean 5, neue blocks 5."},
			EvalCount:    100,
			PromptTokens: 200,
		}, nil
	})

	blockID, err := dream.GenerateDailyReport(ctx, pool, reportRouter(t, ctx, pool), reportScope)
	if err != nil {
		t.Fatalf("generate report: %v", err)
	}
	if blockID == "" {
		t.Fatal("want block_id, got empty string")
	}

	var (
		title     string
		blockType string
		typeName  string
	)
	if err := pool.QueryRow(ctx,
		`SELECT title, lifecycle_state, type_name FROM context_blocks WHERE id = $1::uuid`,
		blockID,
	).Scan(&title, &blockType, &typeName); err != nil {
		t.Fatalf("read created block: %v", err)
	}

	today := time.Now().UTC().Format("2006-01-02")
	wantTitle := "Tagesbericht " + today
	if title != wantTitle {
		t.Errorf("title mismatch: got %q, want %q", title, wantTitle)
	}
	if blockType != "synthesis" {
		t.Errorf("lifecycle_state mismatch: got %q, want %q", blockType, "synthesis")
	}
	if typeName != "audit-trail" {
		t.Errorf("type_name mismatch: got %q, want %q", typeName, "audit-trail")
	}

	// M103: the report persists its enumerated sources as deterministic
	// references edges (report → seeded fresh block, exactly one here).
	var links int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*)::int FROM context_structural_links
		 WHERE source_block_id = $1::uuid AND link_class = 'references' AND origin = 'system'`,
		blockID,
	).Scan(&links); err != nil {
		t.Fatalf("count structural links: %v", err)
	}
	if links != 1 {
		t.Errorf("report source links: got %d, want 1", links)
	}
}

// TestGenerateDailyReport_StructuralSectionInPrompt is the GD2 end-to-end
// probe the unit golden test cannot give: real DB aggregation
// (fetchDailyStructuralLinks) feeding the wire-bound userPrompt. The seeded
// counts double as leak probes — an out-of-window edge or a foreign-scope
// edge would bump "references (system)" past 2 and fail the exact match.
func TestGenerateDailyReport_StructuralSectionInPrompt(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedDecisionRows(t, pool, reportScope) // activity so the skip gate reports

	a := seedLinkAnchorBlock(t, pool, reportScope, "anchor a")
	b := seedLinkAnchorBlock(t, pool, reportScope, "anchor b")
	c := seedLinkAnchorBlock(t, pool, reportScope, "anchor c")
	d := seedLinkAnchorBlock(t, pool, reportScope, "anchor d")
	w1 := seedLinkAnchorBlock(t, pool, "work", "anchor w1")
	w2 := seedLinkAnchorBlock(t, pool, "work", "anchor w2")

	// In-window, home scope: 2× references(system), 1× duplicate-of(forge-sync).
	seedStructuralLink(t, pool, reportScope, a, b, "references", "system", "1 hour")
	seedStructuralLink(t, pool, reportScope, a, c, "references", "system", "2 hours")
	seedStructuralLink(t, pool, reportScope, a, b, "duplicate-of", "forge-sync", "3 hours")
	// Window leak probe: same class/origin but 25h old — must not be counted.
	seedStructuralLink(t, pool, reportScope, a, d, "references", "system", "25 hours")
	// Scope leak probe: same class/origin, in-window, foreign scope.
	seedStructuralLink(t, pool, "work", w1, w2, "references", "system", "1 hour")

	var gotPrompt string
	mockChatJSONExternal(t, func(_ context.Context, _, _, _ string, _ *bool, _, userPrompt string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		gotPrompt = userPrompt
		return &llm.ChatResponse{
			Message: llm.Message{Role: "assistant", Content: "Tagesbericht mit Structural-Statistik."},
		}, nil
	})

	blockID, err := dream.GenerateDailyReport(ctx, pool, reportRouter(t, ctx, pool), reportScope)
	if err != nil {
		t.Fatalf("generate report: %v", err)
	}
	if blockID == "" {
		t.Fatal("want block_id, got empty string")
	}

	// Contiguous match pins section header, per-line format, and the
	// COUNT DESC order (2 before 1) in the exact GD2 shape.
	want := "\nStructural-Links 24h:\n- references (system): 2\n- duplicate-of (forge-sync): 1\n"
	if !strings.Contains(gotPrompt, want) {
		t.Errorf("wire prompt is missing the GD2 structural section\nwant substring:\n%s\ngot prompt:\n%s", want, gotPrompt)
	}
}

func TestGenerateDailyReportFor_Backfill(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Activity INSIDE the historical window of 2026-01-15
	// ([2026-01-14 03:00, 2026-01-15 03:00) UTC) — and nothing anywhere else.
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_write_log (decision, scope, created_at)
		 VALUES ('dream_link', $1, '2026-01-14T12:00:00Z')`,
		reportScope,
	); err != nil {
		t.Fatalf("seed historical write log: %v", err)
	}

	mockChatJSONExternal(t, func(_ context.Context, _, _, _ string, _ *bool, _, userPrompt string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		if !strings.Contains(userPrompt, "Datum: 2026-01-15") {
			t.Errorf("prompt must carry the backfill date, got: %q", userPrompt)
		}
		return &llm.ChatResponse{
			Message: llm.Message{Role: "assistant", Content: "Historischer Tagesbericht."},
		}, nil
	})

	day := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	blockID, err := dream.GenerateDailyReportFor(ctx, pool, reportRouter(t, ctx, pool), reportScope, day)
	if err != nil {
		t.Fatalf("backfill report: %v", err)
	}
	if blockID == "" {
		t.Fatal("want block_id for window with activity, got empty")
	}

	var title string
	if err := pool.QueryRow(ctx,
		`SELECT title FROM context_blocks WHERE id = $1::uuid`, blockID,
	).Scan(&title); err != nil {
		t.Fatalf("read backfilled block: %v", err)
	}
	if title != "Tagesbericht 2026-01-15" {
		t.Errorf("title mismatch: got %q, want %q", title, "Tagesbericht 2026-01-15")
	}

	// A day whose window holds NO activity skips without an LLM call.
	empty := time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)
	blockID, err = dream.GenerateDailyReportFor(ctx, pool, reportRouter(t, ctx, pool), reportScope, empty)
	if err != nil {
		t.Fatalf("empty-window backfill: %v", err)
	}
	if blockID != "" {
		t.Errorf("empty window must yield no block, got %q", blockID)
	}
}

func TestGenerateDailyReport_NoActivity(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	called := false
	mockChatJSONExternal(t, func(_ context.Context, _, _, _ string, _ *bool, _, _ string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		called = true
		return nil, nil
	})

	blockID, err := dream.GenerateDailyReport(ctx, pool, reportRouter(t, ctx, pool), reportScope)
	if err != nil {
		t.Fatalf("expected nil error on empty activity, got %v", err)
	}
	if blockID != "" {
		t.Errorf("expected empty block_id on empty activity, got %q", blockID)
	}
	if called {
		t.Error("LLM must not be invoked when there is no 24h activity")
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*)::int FROM context_blocks WHERE category = 'learnings' AND title LIKE 'Tagesbericht %'`,
	).Scan(&n); err != nil {
		t.Fatalf("count synthesis blocks: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 synthesis blocks, got %d", n)
	}
}

// TestGenerateDailyReport_InactiveScopeSkipsDespiteForeignDreams is the GD3-F3
// probe: before E14 the dream aggregation was global, so ANY tenant's dream
// links kept EVERY scope reporting. Seed dream activity only under 'work',
// then ask for the 'private' report — it must skip without an LLM call. The
// 'work' counter-run on the same data proves the seed constitutes real
// activity (guards against a green-by-broken-fixture skip).
func TestGenerateDailyReport_InactiveScopeSkipsDespiteForeignDreams(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	w1 := seedLinkAnchorBlock(t, pool, "work", "dream anchor w1")
	w2 := seedLinkAnchorBlock(t, pool, "work", "dream anchor w2")
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, scope, created_at)
		 VALUES ($1::uuid, $2::uuid, 'topical', 'work', now() - interval '1 hour')`,
		w1, w2,
	); err != nil {
		t.Fatalf("seed foreign dream link: %v", err)
	}

	called := false
	mockChatJSONExternal(t, func(_ context.Context, _, _, _ string, _ *bool, _, _ string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		called = true
		return &llm.ChatResponse{
			Message: llm.Message{Role: "assistant", Content: "Tagesbericht work."},
		}, nil
	})

	blockID, err := dream.GenerateDailyReport(ctx, pool, reportRouter(t, ctx, pool), reportScope)
	if err != nil {
		t.Fatalf("inactive scope: want nil error, got %v", err)
	}
	if blockID != "" {
		t.Errorf("inactive scope must skip despite foreign dream links, got block %q", blockID)
	}
	if called {
		t.Error("LLM must not be invoked for a scope whose own 24h window is empty")
	}

	// Counter-run: the scope that owns the seeded dream link DOES report.
	blockID, err = dream.GenerateDailyReport(ctx, pool, reportRouter(t, ctx, pool), "work")
	if err != nil {
		t.Fatalf("active scope: %v", err)
	}
	if blockID == "" {
		t.Fatal("active scope with in-window dream links must report, got empty block_id")
	}
	if !called {
		t.Error("counter-run must reach the LLM — otherwise the seed never constituted activity and the skip assertion above was vacuous")
	}
}

func TestGenerateDailyReport_LLMError(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedDecisionRows(t, pool, reportScope)

	mockChatJSONExternal(t, func(_ context.Context, _, _, _ string, _ *bool, _, _ string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		return nil, errors.New("ollama exploded")
	})

	_, err := dream.GenerateDailyReport(ctx, pool, reportRouter(t, ctx, pool), reportScope)
	if err == nil {
		t.Fatal("want wrapped error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "synthesize report") || !strings.Contains(msg, "ollama exploded") {
		t.Errorf("error not wrapped as expected: %v", err)
	}
}

// reportTestDispatcher is one shared pass-through dispatcher for this file's
// integration routers (MW3: Router.Admit is mandatory).
var reportTestDispatcher = dispatch.New(nil, dispatch.DefaultSettings())

func reportAdmission() llm.Admission {
	return llm.Admission{Admitter: reportTestDispatcher, Class: dispatch.ClassBackground}
}
