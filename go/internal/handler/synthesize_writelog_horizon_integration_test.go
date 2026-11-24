//go:build integration

// Issue #277: with scheduler.writelog_retention_days set, the janitor deletes
// the write-log rows past the horizon, and with them the guard and dream
// decisions of a day past it. A report backfill (?date=) of such a day would
// replace the day's report through its upsert key with one whose decision
// part is short or empty; the handler refuses it with a 400 before any
// database or LLM work. A day inside the horizon, or any day without a
// horizon, goes on to the synthesis as before (here: a window without
// activity, so no report is written either way).
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestHandleDailyBackfillRefusesADayPastTheWriteLogHorizon(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "scope-horizon"

	day := time.Now().UTC().AddDate(0, 0, -10).Format("2006-01-02")
	// The day's report as the synthesis writes it (dream.dailyReportTitleFor,
	// the legacy language), and a block of today.
	report, err := store.UpsertBlock(ctx, pool, "learnings", "Tagesbericht "+day, "the report as it stands, with its decisions",
		nil, nil, scope, true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed report: %v", err)
	}
	if _, err := store.UpsertBlock(ctx, pool, "decisions", "seed", "seed content", nil, nil, scope, true, store.SensitivityWrite{}, ""); err != nil {
		t.Fatalf("seed block: %v", err)
	}

	const origin = "http://gpu:8089"
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{{
		ID: "gpu", Name: "gpu", Host: origin, Protocol: backends.ProtocolOpenAI, Model: "m",
		Trust: backends.TrustFull, Enabled: true, Roles: []string{backends.RoleDigest},
	}})
	cfg := &swapStore{}

	post := func(days int) (int, map[string]any) {
		t.Helper()
		c := &config.Config{}
		c.Scheduler.WriteLogRetentionDays = days
		cfg.p.Store(c)
		h := NewSynthesizeHandler(pool, bpool, blocktype.NewRegistry(), rejectHintAdmitter{origin: origin, hint: time.Second}, cfg)
		req := httptest.NewRequest(http.MethodPost, "/api/synthesize/daily?date="+day, nil)
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, &auth.AuthResult{
			IsValid: true, ApiKeyID: "key-1", TenantID: "tenant-1", HomeScope: scope,
		}))
		rr := httptest.NewRecorder()
		h.HandleDaily(rr, req)
		var body map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
		return rr.Code, body
	}

	// Ten days back, a horizon of seven: the window starts past it.
	code, body := post(7)
	if code != http.StatusBadRequest {
		t.Fatalf("retention 7, a day 10 days back: status %d (%v), want 400", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "write-log horizon") || !strings.Contains(msg, "scheduler.writelog_retention_days = 7") {
		t.Errorf("refusal text %q does not name the horizon and its setting", msg)
	}

	// Inside the horizon, and without one: on to the synthesis of the
	// window, which holds no activity (the blocks above were written now), so
	// it answers no_activity and writes nothing — not the horizon's 400.
	for _, days := range []int{30, 0} {
		code, body := post(days)
		if code != http.StatusOK || body["reason"] != "no_activity" {
			t.Errorf("retention %d, a day 10 days back: status %d (%v), want 200 no_activity (the backfill goes on)", days, code, body)
		}
	}

	var content string
	if err := pool.QueryRow(ctx, `SELECT content FROM context_blocks WHERE id = $1`, report.ID).Scan(&content); err != nil {
		t.Fatalf("read report: %v", err)
	}
	if content != "the report as it stands, with its decisions" {
		t.Errorf("the day's report changed: %q", content)
	}
}

// horizonEchoBackend is a digest backend whose answer is the user prompt it
// was sent: the report it writes shows what the synthesis read.
func horizonEchoBackend(t *testing.T) *backends.Pool {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		user := ""
		for _, m := range req.Messages {
			if m.Role == "user" {
				user = m.Content
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "x", "object": "chat.completion", "model": "m",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "REPORT<<" + user + ">>"}}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	t.Cleanup(srv.Close)
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{{
		ID: "gpu", Name: "gpu", Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "m",
		Trust: backends.TrustFull, Enabled: true, Roles: []string{backends.RoleDigest},
	}})
	return bpool
}

// horizonPost sends the backfill of day ("": the report of the last 24
// hours, no date) under a config whose retention is days, as the caller of
// scope, and answers the status and the body.
func horizonPost(t *testing.T, h *SynthesizeHandler, cfg *swapStore, scope, day string, days int) (int, map[string]any) {
	t.Helper()
	c := &config.Config{}
	c.Scheduler.WriteLogRetentionDays = days
	cfg.p.Store(c)
	target := "/api/synthesize/daily"
	if day != "" {
		target += "?date=" + day
	}
	req := httptest.NewRequest(http.MethodPost, target, nil)
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, &auth.AuthResult{
		IsValid: true, ApiKeyID: "key-1", TenantID: "tenant-1", HomeScope: scope,
	}))
	rr := httptest.NewRecorder()
	h.HandleDaily(rr, req)
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	return rr.Code, body
}

// TestHandleDailyBackfillAfterTheRetentionWasRaised: the retention at two
// days deletes the guard decisions of a day; the operator raises it to 30,
// or back to 0. A backfill of that day is still refused — the eviction
// boundary the janitor recorded before it deleted, not the setting of the
// moment, decides — and the day's report keeps the decisions it was written
// with. Before, the backfill passed and replaced the report (same block id,
// no version history) with one without them.
//
// The day's block and its decisions are written with a created_at in the
// window: every production writer stamps now(), so no write path produces
// them; the report is written by the backfill itself, the eviction by the
// janitor's store.EvictWriteLog.
func TestHandleDailyBackfillAfterTheRetentionWasRaised(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "scope-raised"
	dayT := time.Now().UTC().AddDate(0, 0, -5)
	day := dayT.Format("2006-01-02")
	inWindow := time.Date(dayT.Year(), dayT.Month(), dayT.Day(), 3, 0, 0, 0, time.UTC).Add(-6 * time.Hour)

	b, err := store.UpsertBlock(ctx, pool, "decisions", "window block", "content of the window block", nil, nil, scope, true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET created_at = $2 WHERE id = $1::uuid`, b.ID, inWindow); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_write_log (decision, similarity, scope, block_title, block_category, metadata, created_at)
		SELECT 'duplicate', 0.97, $1, 'wl ' || g, 'decisions', '{}'::jsonb, $2::timestamptz + make_interval(secs => g)
		  FROM generate_series(1, 7) g`, scope, inWindow); err != nil {
		t.Fatal(err)
	}

	cfg := &swapStore{}
	h := NewSynthesizeHandler(pool, horizonEchoBackend(t), blocktype.NewRegistry(), newHandlerRecordingAdmitter(t), cfg)
	report := func() (string, string) {
		t.Helper()
		var id, content string
		if err := pool.QueryRow(ctx, `SELECT id::text, content FROM context_blocks
			 WHERE category = 'learnings' AND scope = $1 AND title = 'Tagesbericht ' || $2`, scope, day).Scan(&id, &content); err != nil {
			t.Fatalf("read the day's report: %v", err)
		}
		return id, content
	}

	if code, body := horizonPost(t, h, cfg, scope, day, 0); code != http.StatusOK {
		t.Fatalf("the day's report: status %d (%v)", code, body)
	}
	id, written := report()
	if !strings.Contains(written, "duplicate") {
		t.Fatalf("the day's report does not carry its decisions: %q", written)
	}
	if n, err := store.EvictWriteLog(ctx, pool, 2); err != nil || n != 7 {
		t.Fatalf("retention 2 deleted %d rows (%v), want the day's 7", n, err)
	}
	for _, days := range []int{30, 0} {
		code, body := horizonPost(t, h, cfg, scope, day, days)
		if msg, _ := body["error"].(string); code != http.StatusBadRequest || !strings.Contains(msg, "eviction boundary") {
			t.Errorf("retention raised to %d, a day the retention at 2 emptied: status %d (%v), want 400 naming the eviction boundary", days, code, body)
		}
		if gotID, content := report(); gotID != id || content != written {
			t.Errorf("retention raised to %d: the day's report was replaced (id %s -> %s): %q", days, id, gotID, content)
		}
	}
}

// TestHandleDailyBackfillGateStopsAtTheEvictionBoundary: the gate refuses a
// window that starts before the recorded boundary, also when it ends after
// it, and passes the next day's window, which starts after it, and the
// report without a date (the window of the 03:00 run).
func TestHandleDailyBackfillGateStopsAtTheEvictionBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "scope-boundary"
	if _, err := store.EvictWriteLog(ctx, pool, 2); err != nil {
		t.Fatalf("janitor run: %v", err)
	}
	var boundary time.Time
	if err := pool.QueryRow(ctx, `SELECT write_log_evicted_before FROM context_guard_state WHERE id = true`).Scan(&boundary); err != nil {
		t.Fatalf("read the boundary the janitor recorded: %v", err)
	}
	boundary = boundary.UTC()
	// The window [D-1 03:00, D 03:00) that holds the boundary, and the next.
	start := time.Date(boundary.Year(), boundary.Month(), boundary.Day(), 3, 0, 0, 0, time.UTC)
	if start.After(boundary) {
		start = start.AddDate(0, 0, -1)
	}
	straddling := start.AddDate(0, 0, 1).Format("2006-01-02")
	after := start.AddDate(0, 0, 2).Format("2006-01-02")

	cfg := &swapStore{}
	h := NewSynthesizeHandler(pool, horizonEchoBackend(t), blocktype.NewRegistry(), newHandlerRecordingAdmitter(t), cfg)
	code, body := horizonPost(t, h, cfg, scope, straddling, 0)
	if msg, _ := body["error"].(string); code != http.StatusBadRequest || !strings.Contains(msg, "eviction boundary") {
		t.Errorf("the window [%s 03:00, %s 03:00) holds the boundary %s: status %d (%v), want 400 naming the eviction boundary",
			start.Format("2006-01-02"), straddling, boundary.Format(time.RFC3339), code, body)
	}
	code, body = horizonPost(t, h, cfg, scope, after, 0)
	if code != http.StatusOK || body["reason"] != "no_activity" {
		t.Errorf("the window after the boundary %s (date %s): status %d (%v), want 200 no_activity (the backfill goes on)",
			boundary.Format(time.RFC3339), after, code, body)
	}
	code, body = horizonPost(t, h, cfg, scope, "", 2)
	if code != http.StatusOK || body["reason"] != "no_activity" {
		t.Errorf("the report of the last 24 hours beside the boundary %s: status %d (%v), want 200 no_activity",
			boundary.Format(time.RFC3339), code, body)
	}
	// A boundary the handler cannot read (the way back of migration 194
	// dropped it) refuses every backfill.
	if _, err := pool.Exec(ctx, `ALTER TABLE context_guard_state DROP COLUMN write_log_evicted_before`); err != nil {
		t.Fatal(err)
	}
	if code, body = horizonPost(t, h, cfg, scope, after, 0); code != http.StatusInternalServerError {
		t.Errorf("a backfill without a readable boundary: status %d (%v), want 500 (fail-closed)", code, body)
	}
}
