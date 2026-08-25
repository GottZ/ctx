package dream

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
)

// TestGenerateKeywords_FallbackAfterRetries pins the deterministic fallback:
// when the LLM degenerates on every retry (budget cap / reasoning burn /
// content collapse), GenerateKeywords must return tokenizer keywords instead
// of failing, so the block still gets searched this cycle and is not parked.
func TestGenerateKeywords_FallbackAfterRetries(t *testing.T) {
	r := newTestRouter()
	var calls int
	mockChatJSON(t, func(ctx context.Context, _, _, _ string, _ *bool, _, _ string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		calls++
		return constResp(`{"`)(ctx, "", "", "", nil, "", "", llm.Options{}, 0)
	})

	blk := srcBlock("00000000-0000-4000-8000-00000000000a")
	blk.Sensitivity = backends.SensInternal
	blk.Title = "Transfer-Dashboard Tabelle Server-Passwort"
	blk.Content = "TRANSFER-DASHBOARD UPDATE: Dashboard-JSON fertig (Streaming-PC, in brades-infra/grafana/dashboard-transfer.json, UID brades-transfer, 6 Panels: Uebertragung-GB-Kurve beidseitig, Speed-Kurve, Cache/Archiv-Platz, aktuelle Datei, Fehler-Zaehler, Phase). ABER: Tabelle rclone_transfer_stats kann NICHT vom Streaming-PC angelegt werden (PostgreSQL laeuft nur auf dem BRADES-SERVER). FIX: Tabelle muss auf dem Server angelegt werden."

	kws, err := GenerateKeywords(context.Background(), nil, r, &blk)
	if err != nil {
		t.Fatalf("GenerateKeywords should fall back, got error: %v", err)
	}
	if calls != KeywordsMaxRetries {
		t.Fatalf("LLM calls = %d, want %d (all retries before fallback)", calls, KeywordsMaxRetries)
	}
	if len(kws) < MinKeywords {
		t.Fatalf("fallback keywords = %v, want >= %d", kws, MinKeywords)
	}
}
