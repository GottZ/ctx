//go:build integration

// Issue #221: the query path's pre-search embed backfill step had no value
// that skips it (embed_backfill.sync_cap and sync_budget read 0 as
// unbounded), and its keys were global-only. embed_backfill.sync_disabled
// skips the step, and is tenant-overridable, so one tenant can switch its own
// queries' step off.
//
// sync_disabled_skips_the_step drives POST /api/query with the switch on in
// the request's snapshot: the question is embedded, the pending block is
// neither sent nor embedded. tenant_override_reaches_the_tenant reads a
// tenant's own settings row through the production tenant overlay: the
// tenant's generation carries the switch, the base does not.
//
//	go test -tags=integration ./internal/handler/ -run TestQueryBackfillSyncDisabled -count=1 -v
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const sdBlockTitle = "q221 pending block of a tenant that switched the step off"

func TestQueryBackfillSyncDisabled(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	run := func(t *testing.T, disabled bool, question string) (inputs []string, embedded bool) {
		t.Helper()
		if _, err := pool.Exec(ctx, `DELETE FROM context_blocks`); err != nil {
			t.Fatal(err)
		}
		b, err := store.UpsertBlock(ctx, pool, "learnings", sdBlockTitle, "body of the pending block",
			nil, nil, "private", true, store.SensitivityWrite{}, "knowledge")
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		es := &raceEmbedServer{}
		srv := es.start(t)
		cfg := bw2Config()
		cfg.EmbedBackfill.SyncCap = 4
		cfg.EmbedBackfill.SyncBudget = 30 * time.Second
		cfg.EmbedBackfill.Interval = 15 * time.Second
		cfg.EmbedBackfill.MaxTokens = 1_000_000
		cfg.EmbedBackfill.SyncDisabled = disabled
		h := NewQueryHandler(pool, config.NewStore(cfg), embedPool(srv.URL), nil, blocktype.NewRegistry(), snapshotTestAdmitter(t))
		rec := httptest.NewRecorder()
		h.HandleQuery(rec, authedQueryRequest(`{"query":"`+question+`","synthesize":false}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("query: status %d, body %s", rec.Code, rec.Body.String())
		}
		if err := pool.QueryRow(ctx, `SELECT embedding IS NOT NULL FROM context_blocks WHERE id = $1::uuid`, b.ID).Scan(&embedded); err != nil {
			t.Fatal(err)
		}
		return es.take(), embedded
	}
	sentBlock := func(inputs []string) bool {
		for _, in := range inputs {
			if strings.Contains(in, sdBlockTitle) {
				return true
			}
		}
		return false
	}

	// Premise: with the switch at its default the step embeds the block.
	t.Run("default_runs_the_step", func(t *testing.T) {
		inputs, embedded := run(t, false, "q221 the question of the default run")
		if !sentBlock(inputs) || !embedded {
			t.Fatalf("premise: the default step did not embed the pending block (inputs %q, embedded %v)", inputs, embedded)
		}
	})

	t.Run("sync_disabled_skips_the_step", func(t *testing.T) {
		// A question of its own: the embed cache would serve a repeated one.
		inputs, embedded := run(t, true, "q221 the question with the step off")
		if sentBlock(inputs) || embedded {
			t.Errorf("with embed_backfill.sync_disabled the query still embedded the pending block (inputs %q, embedded %v)", inputs, embedded)
		}
		if len(inputs) != 1 {
			t.Errorf("embed calls %q, want exactly the question's", inputs)
		}
	})

	t.Run("tenant_override_reaches_the_tenant", func(t *testing.T) {
		t.Setenv("CONTEXT_DB_PASSWORD", "test-password")
		const tenantScope = "q221tenant"
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertSetting(ctx, tx, "embed_backfill.sync_disabled", tenantScope, []byte("true"), nil); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("write the tenant's row: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		cs := config.NewStore(bw2Config())
		cs.SetOverlay(settings.TenantOverlay(pool))
		if !cs.SnapshotForTenant(ctx, tenantScope).EmbedBackfill.SyncDisabled {
			t.Error("the tenant's own row did not reach its generation: the key is not tenant-overridable")
		}
		if cs.Snapshot().EmbedBackfill.SyncDisabled || cs.SnapshotForTenant(ctx, "private").EmbedBackfill.SyncDisabled {
			t.Error("one tenant's row switched the step off for the base or another tenant")
		}
	})
}
