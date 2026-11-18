//go:build integration

// Issue #222: POST /api/query ran its synchronous embed backfill before it
// embedded the question. An embed the sync budget cuts is dropped by ctx, but
// an engine that does not stop a started batch on disconnect keeps computing
// it, and the question's own embed, admitted next, waited behind that batch:
// the budget moved the wait from the backfill to the question's embed instead
// of bounding it. The question's vector does not depend on the backfill (only
// the search reads the backfilled rows), so the question is embedded first.
//
// The probe's embed backend has one slot and finishes a started document
// embed although the caller went away; the sync budget cuts the pending
// block's embed. RED before the fix: the question's embed reaches the backend
// after the block's, and waits for the slot the cut embed still holds.
//
//	go test -tags=integration ./internal/handler/ -run TestQueryEmbedsTheQuestionBeforeTheBackfill -count=1 -v
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

// qfEngine is an embed backend with one slot: a request holds the slot for
// as long as it computes, and a document embed computes for docHold whether
// or not its caller is still there. It records the order in which the
// requests arrive and how long each waited for the slot.
type qfEngine struct {
	slot    sync.Mutex
	docHold time.Duration

	mu    sync.Mutex
	order []string
	waits map[string]time.Duration
}

func (e *qfEngine) start(t *testing.T) *httptest.Server {
	t.Helper()
	e.waits = map[string]time.Duration{}
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
		kind := "question"
		if strings.Contains(in, qfBlockTitle) {
			kind = "block"
		}
		e.mu.Lock()
		e.order = append(e.order, kind)
		e.mu.Unlock()
		arrived := time.Now()
		e.slot.Lock()
		e.mu.Lock()
		e.waits[kind] = time.Since(arrived)
		e.mu.Unlock()
		if kind == "block" {
			time.Sleep(e.docHold) // the engine finishes the batch it started
		}
		e.slot.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{vec}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

const qfBlockTitle = "q222 pending block the sync budget cuts"

func TestQueryEmbedsTheQuestionBeforeTheBackfill(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	if _, err := store.UpsertBlock(ctx, pool, "learnings", qfBlockTitle, "body of the pending block",
		nil, nil, "private", true, store.SensitivityWrite{}, "knowledge"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	eng := &qfEngine{docHold: 3 * time.Second}
	srv := eng.start(t)
	cfg := bw2Config()
	cfg.EmbedBackfill.SyncCap = 4
	cfg.EmbedBackfill.SyncBudget = time.Second
	cfg.EmbedBackfill.Interval = 15 * time.Second
	cfg.EmbedBackfill.MaxTokens = 1_000_000
	h := NewQueryHandler(pool, config.NewStore(cfg), embedPool(srv.URL), nil, blocktype.NewRegistry(), snapshotTestAdmitter(t))

	rec := httptest.NewRecorder()
	h.HandleQuery(rec, authedQueryRequest(`{"query":"q222 the question of this request","synthesize":false}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("query: status %d, body %s", rec.Code, rec.Body.String())
	}

	eng.mu.Lock()
	order, waits := append([]string(nil), eng.order...), eng.waits
	eng.mu.Unlock()
	t.Logf("arrival order %v, slot waits %v", order, waits)
	if len(order) < 2 || order[0] != "question" || order[1] != "block" {
		t.Fatalf("embed calls arrived as %v, want the question first and then the pending block", order)
	}
	// The question did not wait for the slot the cut embed holds: it came
	// before that embed started. A generous bound, far below the hold.
	if w := waits["question"]; w > eng.docHold/2 {
		t.Errorf("the question's embed waited %v for the engine, behind the cut backfill embed", w)
	}
}
