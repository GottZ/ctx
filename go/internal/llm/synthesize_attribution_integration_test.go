//go:build integration

// Integration test for MT wave T35a (Achse 04-W3, synthesize subset): the
// query-synthesis llm_log row carries the caller's api_key_id. The
// query-synthesis path is the expensive one (can run external synthesis) and
// was the only major write-path leaving api_key_id NULL (design/04 §4.3); the
// per-tenant cost accounting (04-W4) is structurally blind without it. An empty
// key (the background/dream invariant) stays NULL.
//
// External test package (llm_test): internal/testdb → internal/store →
// internal/rrf → internal/llm (rrf/rerank.go uses llm.ChainCall), so an
// internal-package test importing testdb would cycle. A bare httptest server
// stands in for the synthesis backend (the same minimal OpenAI body the
// internal newWireRecorder serves) so the synthesis succeeds and Synthesize logs
// the full Entry. llmlog.Record inserts asynchronously (go insert), so the
// assertions poll until the rows land.
//
// Run with:
//
//	go test -tags=integration ./internal/llm/ -run TestSynthesize_AttributesAPIKeyID -count=1 -v
package llm_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/testdb"
)

// waitForSynthRows blocks until at least want 'query-synthesize' llm_log rows
// have landed (llmlog.Record inserts in a goroutine).
func waitForSynthRows(t *testing.T, pool *pgxpool.Pool, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM context_llm_log WHERE pipeline = 'query-synthesize'`).Scan(&n); err != nil {
			t.Fatalf("count rows: %v", err)
		}
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d query-synthesize rows landed (async Record)", n, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSynthesize_AttributesAPIKeyID(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"An answer."}}],"usage":{"completion_tokens":1,"prompt_tokens":1}}`)
	}))
	defer srv.Close()

	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{{
		ID: "wire", Name: "wire", Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "m",
		Trust: backends.TrustFull, Enabled: true, Roles: []string{backends.RoleSynthesis},
	}})

	settings := llm.SynthesisSettings{ScoreThreshold: 0.001, ConfidentThreshold: 0.008, PromptVersion: llm.PromptVersionV52}
	sources := []llm.Source{{ID: "00000000-0000-0000-0000-000000000001", Title: "t", Category: "c", Content: "body", Score: 0.5, AgeDays: 1}}

	// api_key_id has no FK (054:11-13, "key delete must not anonymize history"),
	// so an arbitrary well-formed UUID stands in for a caller key.
	const key = "00000000-0000-0000-0000-0000000000aa"

	// Foreground synthesis attributes the row to the caller's key.
	if _, err := llm.Synthesize(context.Background(), pool, bpool, backends.GamingState{},
		settings, backends.SensPersonal, "q", sources, nil, key, ""); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	waitForSynthRows(t, pool, 1)
	var apiKeyID *string
	if err := pool.QueryRow(context.Background(),
		`SELECT api_key_id::text FROM context_llm_log
		  WHERE pipeline = 'query-synthesize' ORDER BY created_at DESC LIMIT 1`).Scan(&apiKeyID); err != nil {
		t.Fatalf("read llm_log: %v", err)
	}
	if apiKeyID == nil || *apiKeyID != key {
		t.Fatalf("api_key_id = %v, want %s (caller attribution — query-synthesize was NULL before T35a)", apiKeyID, key)
	}

	// An empty key (the background/dream invariant) stays NULL — nullUUID drops it.
	if _, err := llm.Synthesize(context.Background(), pool, bpool, backends.GamingState{},
		settings, backends.SensPersonal, "q2", sources, nil, "", ""); err != nil {
		t.Fatalf("Synthesize (empty key): %v", err)
	}
	waitForSynthRows(t, pool, 2)
	var nullCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_llm_log WHERE pipeline = 'query-synthesize' AND api_key_id IS NULL`).Scan(&nullCount); err != nil {
		t.Fatalf("count null: %v", err)
	}
	if nullCount != 1 {
		t.Fatalf("NULL api_key_id rows = %d, want 1 (empty key → NULL, the background invariant)", nullCount)
	}
}
