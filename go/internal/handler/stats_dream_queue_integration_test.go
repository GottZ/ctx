//go:build integration

// Issue #271: `manage stats` counted its dream_queue section on every call.
// Through the real handler against PG18: within events.queue_stats_interval a
// second call of the same read-scope set answers the counted value (the
// block written in between is not in it), and another read-scope set is
// counted for itself.
//
// RED before the fix: the second call counts again and sees the new block.
//
//	go test -tags=integration ./internal/handler/ -run TestManageStatsDreamQueue -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestManageStatsDreamQueueServedPerScopeSet(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.Events.QueueStatsInterval = time.Hour
	h := NewManageHandler(pool, config.NewStore(cfg), nil, nil, nil, nil, nil, blocktype.NewRegistry())

	seed := func(title, scope string) {
		t.Helper()
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, "body "+title, nil, nil, scope, true,
			store.SensitivityWrite{}, ""); err != nil {
			t.Fatalf("seed %s: %v", title, err)
		}
	}
	awaiting := func(scopes ...string) float64 {
		t.Helper()
		ar := &auth.AuthResult{IsValid: true, HomeScope: scopes[0], ReadScopes: scopes}
		raw, _ := json.Marshal(map[string]any{"action": "stats"})
		r := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(raw))
		r = r.WithContext(context.WithValue(r.Context(), authResultKey, ar))
		rec := httptest.NewRecorder()
		h.HandleManage(rec, r)
		var body struct {
			Success    bool `json:"success"`
			DreamQueue *struct {
				AwaitingEmbed float64 `json:"awaiting_embed"`
			} `json:"dream_queue"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !body.Success || body.DreamQueue == nil {
			t.Fatalf("stats %v: %d %s", scopes, rec.Code, rec.Body.String())
		}
		return body.DreamQueue.AwaitingEmbed
	}

	seed("dq-a", "private")
	if n := awaiting("private"); n != 1 {
		t.Fatalf("first call: awaiting_embed %v, want the one block without an embedding — the fixture proves nothing otherwise", n)
	}
	seed("dq-b", "private")
	if n := awaiting("private"); n != 1 {
		t.Errorf("second call within the interval: awaiting_embed %v, want the counted 1 (the section was counted again)", n)
	}
	// Another read-scope set is its own entry and counts now.
	if n := awaiting("private", "dq-other"); n != 2 {
		t.Errorf("other scope set: awaiting_embed %v, want its own count 2", n)
	}
}
