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
