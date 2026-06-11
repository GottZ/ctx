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
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/testdb"
)

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

func TestGenerateDailyReport_HappyPath(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedDecisionRows(t, pool, reportScope)
	seedSynthesisBlock(t, pool, reportScope)

	mockChatJSONExternal(t, func(_ context.Context, _, _, _ string, _ *bool, _, _ string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		return &llm.ChatResponse{
			Message:      llm.Message{Role: "assistant", Content: "Tagesbericht-content. Test fix: dream-link 52, clean 5, neue blocks 5."},
			EvalCount:    100,
			PromptTokens: 200,
		}, nil
	})

	blockID, err := dream.GenerateDailyReport(ctx, pool, backends.Backend{Host: "h", APIKey: "k", Model: "m"}, llm.Options{}, reportScope)
	if err != nil {
		t.Fatalf("generate report: %v", err)
	}
	if blockID == "" {
		t.Fatal("want block_id, got empty string")
	}

	var (
		title     string
		blockType string
		blockRole string
	)
	if err := pool.QueryRow(ctx,
		`SELECT title, block_type, block_role FROM context_blocks WHERE id = $1::uuid`,
		blockID,
	).Scan(&title, &blockType, &blockRole); err != nil {
		t.Fatalf("read created block: %v", err)
	}

	today := time.Now().UTC().Format("2006-01-02")
	wantTitle := "Tagesbericht " + today
	if title != wantTitle {
		t.Errorf("title mismatch: got %q, want %q", title, wantTitle)
	}
	if blockType != "synthesis" {
		t.Errorf("block_type mismatch: got %q, want %q", blockType, "synthesis")
	}
	if blockRole != "audit-trail" {
		t.Errorf("block_role mismatch: got %q, want %q", blockRole, "audit-trail")
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

	blockID, err := dream.GenerateDailyReport(ctx, pool, backends.Backend{Host: "h", APIKey: "k", Model: "m"}, llm.Options{}, reportScope)
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

func TestGenerateDailyReport_LLMError(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedDecisionRows(t, pool, reportScope)

	mockChatJSONExternal(t, func(_ context.Context, _, _, _ string, _ *bool, _, _ string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		return nil, errors.New("ollama exploded")
	})

	_, err := dream.GenerateDailyReport(ctx, pool, backends.Backend{Host: "h", APIKey: "k", Model: "m"}, llm.Options{}, reportScope)
	if err == nil {
		t.Fatal("want wrapped error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "synthesize report") || !strings.Contains(msg, "ollama exploded") {
		t.Errorf("error not wrapped as expected: %v", err)
	}
}
