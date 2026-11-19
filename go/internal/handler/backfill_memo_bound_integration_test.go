//go:build integration

// Issue #372, part 1, on the query path: backfillPending reads a block, embeds
// it over the wire without holding the row, and on a failure writes the
// block's memo. A title or content change that commits during the embed
// deletes the memo in its own transaction; the memo of the failed attempt at
// the earlier text, written after that, parked the changed block. For an
// oversize answer that park is 'infinity': the shortened text was never
// embedded until something removed the memo.
//
//	go test -tags=integration ./internal/handler/ -run TestBackfillPending_MemoOfAChangedBlock -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestBackfillPending_MemoOfAChangedBlock(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "private"
	cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
		SyncCap: 1, MaxTokens: 1_000_000, BackoffBase: time.Minute, BackoffCap: time.Hour,
	}}
	b, err := store.UpsertBlock(ctx, pool, "learnings", "memo372 block", "a long first text", nil, nil, scope, true, store.SensitivityWrite{}, "knowledge")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The first embed call commits a shorter content and then answers the
	// backend's oversize refusal; every later call answers a vector.
	var (
		mu     sync.Mutex
		inputs []string
		calls  int
	)
	vec := make([]float64, 1024)
	for i := range vec {
		vec[i] = float64((i % 2) * 2)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input any `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		in := ""
		switch v := req.Input.(type) {
		case string:
			in = v
		case []any:
			if len(v) > 0 {
				in, _ = v[0].(string)
			}
		}
		mu.Lock()
		calls++
		first := calls == 1
		inputs = append(inputs, in)
		mu.Unlock()
		if first {
			short := "short"
			if _, _, err := store.UpdateBlock(context.Background(), pool, b.ID, store.UpdateBlockData{Content: &short}, []string{scope}); err != nil {
				t.Errorf("update during the embed: %v", err)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"request exceeds the available context size","type":"exceed_context_size_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{vec}})
	}))
	t.Cleanup(srv.Close)
	st := &countingStore{}
	st.cfg.Store(snapshotTestConfig())
	h := NewQueryHandler(pool, st, embedPool(srv.URL), nil, blocktype.NewRegistry(), snapshotTestAdmitter(t))

	h.backfillPending(ctx, nil, []string{scope}, h.embedAdmission(), cfg)
	var memos int
	if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM context_embed_failures WHERE block_id = $1::uuid`, b.ID).Scan(&memos); err != nil {
		t.Fatal(err)
	}
	if memos != 0 {
		t.Errorf("after a refusal of the text before the change: %d memo rows, want 0 (the changed block is parked under the earlier attempt's verdict)", memos)
	}

	if got := h.backfillPending(ctx, nil, []string{scope}, h.embedAdmission(), cfg); got != 1 {
		t.Errorf("second pass backfilled %d, want 1: the changed block was not picked", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(inputs) != 2 || !strings.Contains(inputs[1], "short") {
		t.Errorf("embed inputs %q, want the earlier text and then the changed one", inputs)
	}
}
