//go:build integration

// Issue #198, part B, the query path's side of the race the background arm
// closes with its row lock: backfillPending reads a block, embeds it over the
// wire for seconds without holding the row, and writes the vector. A title or
// content change that commits during the embed call must leave the block
// pending, so that the vector it gets in the end is the one of its current
// text. The write of a vector for text the block no longer holds used to land
// unconditionally: the block then kept that vector and left the queue.
//
// Both shapes: a block that was never embedded (pending as NULL) and one whose
// vector a first content change marked stale. Each test changes the content
// during the embed of the first pass and then runs a second pass, which has to
// pick the block again and embed its current content.
//
//	go test -tags=integration ./internal/handler/ -run TestBackfillPending_ContentChangeDuringEmbed -count=1 -v
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

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// raceEmbedServer answers the Ollama /api/embed shape and runs onEmbed with
// the embed input before it answers, so a test can commit a write while the
// caller waits for the vector. It records every input.
type raceEmbedServer struct {
	mu      sync.Mutex
	inputs  []string
	onEmbed func(input string)
}

func (s *raceEmbedServer) start(t *testing.T) *httptest.Server {
	t.Helper()
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
		s.mu.Lock()
		s.inputs = append(s.inputs, in)
		hook := s.onEmbed
		s.mu.Unlock()
		if hook != nil {
			hook(in)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{vec}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *raceEmbedServer) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := s.inputs
	s.inputs = nil
	return in
}

func TestBackfillPending_ContentChangeDuringEmbed(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "private"
	strptr := func(s string) *string { return &s }
	cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
		SyncCap: 1, MaxTokens: 1_000_000, BackoffBase: time.Minute, BackoffCap: time.Hour,
	}}

	for _, shape := range []string{"unembedded", "stale"} {
		t.Run(shape, func(t *testing.T) {
			if _, err := pool.Exec(ctx, `DELETE FROM context_blocks`); err != nil {
				t.Fatal(err)
			}
			title := "race198 " + shape
			b, err := store.UpsertBlock(ctx, pool, "learnings", title, "first text", nil, nil, scope, true, store.SensitivityWrite{}, "knowledge")
			if err != nil {
				t.Fatalf("seed: %v", err)
			}
			if shape == "stale" {
				vec := make([]float32, 1024)
				for i := range vec {
					vec[i] = float32(i%3) - 1
				}
				if err := store.StoreEmbedding(ctx, pool, b.ID, "test-embed", vec); err != nil {
					t.Fatal(err)
				}
				if _, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Content: strptr("second text")}, []string{scope}); err != nil {
					t.Fatal(err)
				}
			}

			es := &raceEmbedServer{}
			srv := es.start(t)
			st := &countingStore{}
			st.cfg.Store(snapshotTestConfig())
			h := NewQueryHandler(pool, st, embedPool(srv.URL), nil, blocktype.NewRegistry(), snapshotTestAdmitter(t))

			// Pass 1: the content changes while the embed call runs.
			var once sync.Once
			es.onEmbed = func(string) {
				once.Do(func() {
					if _, _, err := store.UpdateBlock(context.Background(), pool, b.ID,
						store.UpdateBlockData{Content: strptr("third text")}, []string{scope}); err != nil {
						t.Errorf("update during the embed: %v", err)
					}
				})
			}
			h.backfillPending(ctx, nil, []string{scope}, h.embedAdmission(), cfg)
			first := es.take()
			if len(first) != 1 || strings.Contains(first[0], "third text") {
				t.Fatalf("pass 1 embedded %q, want one embed of the text before the change", first)
			}

			// Pass 2: the block is still pending and gets the vector of the
			// text it holds now.
			es.onEmbed = nil
			h.backfillPending(ctx, nil, []string{scope}, h.embedAdmission(), cfg)
			second := es.take()
			if len(second) != 1 || !strings.Contains(second[0], "third text") {
				t.Fatalf("pass 2 embedded %q, want one embed of %q: the vector written for the text before the change took the block out of the queue",
					second, "third text")
			}
			var hasEmbedding bool
			if err := pool.QueryRow(ctx, `SELECT embedding IS NOT NULL FROM context_blocks WHERE id = $1::uuid`, b.ID).Scan(&hasEmbedding); err != nil {
				t.Fatal(err)
			}
			if !hasEmbedding {
				t.Errorf("after pass 2 the block has no vector")
			}
		})
	}
}

// TestBackfillPicker_OldEndTakesAStaleOverranBlock: with the background arm
// off, a block that overran the sync budget (a caller_timeout memo) is taken
// only by the old end's overran pick once its backoff has passed; the regular
// picks leave caller_timeout blocks out. A stale block can carry such a memo
// as well, and the overran pick must see it like an unembedded one, or with
// the arm off nothing would ever re-embed it.
func TestBackfillPicker_OldEndTakesAStaleOverranBlock(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "private"
	b, err := store.UpsertBlock(ctx, pool, "learnings", "overran198 stale", "first text", nil, nil, scope, true, store.SensitivityWrite{}, "knowledge")
	if err != nil {
		t.Fatal(err)
	}
	vec := make([]float32, 1024)
	for i := range vec {
		vec[i] = float32(i%3) - 1
	}
	if err := store.StoreEmbedding(ctx, pool, b.ID, "test-embed", vec); err != nil {
		t.Fatal(err)
	}
	second := "second text"
	if _, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Content: &second}, []string{scope}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordEmbedFailure(ctx, pool, b.ID, store.EmbedFailureCallerTimeout, "caller_timeout: test", time.Minute, time.Hour); err != nil {
		t.Fatal(err)
	}
	// The cut lies an hour back, beyond the one-minute backoff.
	if _, err := pool.Exec(ctx, `UPDATE context_embed_failures SET next_attempt_at = now() - interval '1 hour' WHERE block_id = $1::uuid`, b.ID); err != nil {
		t.Fatal(err)
	}

	bp := backends.NewPool(nil, nil)
	bp.SeedSnapshotForTest([]backends.Backend{
		{ID: "g", Name: "shared", Scope: backends.GlobalScope, Roles: []string{backends.RoleEmbed}, Enabled: true, Trust: backends.TrustFull},
	})
	p := newBackfillPicker(pool, false, bp.TrustCeilings(backends.RoleEmbed), nil, []string{scope})
	p.backoffBase, p.backoffCap = time.Minute, time.Hour
	row, err := p.next(ctx, 0)
	if err != nil {
		t.Fatalf("old end: %v, want the stale block that overran the budget", err)
	}
	if row.id != b.ID || row.content != second {
		t.Errorf("old end picked %s (%q), want %s with its current text", row.id, row.content, b.ID)
	}
}
