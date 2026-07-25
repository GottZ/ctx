//go:build integration

// Rot-Gate für den Live-Vorfall 2026-07-25 (Deploy-Session, Runbook ctx
// 019f93d5): backfillPending schrieb das W04-2-Fehler-Memo über den REQUEST-
// Context — wenn der Wire-Fehler selbst der Request-Abbruch ist (60s-Deadline
// bei gesättigtem Embed-Backend, "context canceled"), stirbt der Memo-Write
// am selben toten Context. Live-Beleg: nach Stunden fehlschlagender Queries
// (33 Compaction-Parts à ~9k Token im Backfill-Stau) war
// context_embed_failures LEER — jede spätere Query pickte dieselben Blöcke
// erneut und lief in dasselbe Timeout; der Memo-Mechanismus war auf Pfad A
// genau in dem Fall wirkungslos, für den er gebaut wurde.
//
// Grün (Fix): der Memo-Write läuft auf context.WithoutCancel mit eigener
// kurzer Deadline und überlebt den Request-Abbruch; der Block ist ab der
// nächsten Query durch das Memo-Prädikat ausgeschlossen.
//
// Run: go test -tags=integration ./internal/handler/ -run TestBackfillPending_MemoSurvivesRequestCancel -count=1 -v
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestBackfillPending_MemoSurvivesRequestCancel(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope, created_at, updated_at)
		 VALUES ('learnings', 'memo-cancel-block', 'body of the memo-cancel fixture block', 'private', now(), now())`); err != nil {
		t.Fatalf("seed block: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM context_embed_failures WHERE block_id IN (SELECT id FROM context_blocks WHERE title = 'memo-cancel-block')`)
		_, _ = pool.Exec(ctx, `DELETE FROM context_blocks WHERE title = 'memo-cancel-block'`)
	})

	// The embed backend cancels the surrounding request while failing the
	// call — the shape of the live incident, where the wire error WAS the
	// request deadline. By the time backfillPending records the memo, the
	// request context is already dead.
	reqCtx, cancelReq := context.WithCancel(ctx)
	defer cancelReq()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancelReq()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	st := &countingStore{}
	st.cfg.Store(snapshotTestConfig())
	h := NewQueryHandler(pool, st, embedPool(srv.URL), nil, blocktype.NewRegistry(), snapshotTestAdmitter(t))

	cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
		SyncCap: 4, MaxTokens: 1_000_000, BackoffBase: time.Minute, BackoffCap: time.Hour,
	}}

	got := h.backfillPending(reqCtx, nil, "private", h.embedAdmission(), cfg)
	if got != 0 {
		t.Fatalf("backfilled = %d, want 0 (embed must fail in this fixture)", got)
	}
	if reqCtx.Err() == nil {
		t.Fatal("fixture premise broken: request context must be canceled after the embed call")
	}

	var memos int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_embed_failures
		 WHERE block_id IN (SELECT id FROM context_blocks WHERE title = 'memo-cancel-block')`).Scan(&memos); err != nil {
		t.Fatalf("memo count: %v", err)
	}
	if memos != 1 {
		t.Fatalf("memo rows = %d, want 1 (the failure memo must survive the canceled request context)", memos)
	}
}
