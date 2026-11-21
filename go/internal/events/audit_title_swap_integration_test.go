//go:build integration

// Issue #452 point 5, the sensitivity audit verdict: the classifier is asked
// about the block's title AND content (auditOneBlock passes both), so the
// verdict is about that version of both. Since H10 it is bound to the content
// it was formed over (md5(content)); the title was not bound, so a title
// written while the model answered received a verdict about a title nobody
// classified — and the llm-audit stamp takes the block out of the pick set
// for good. A title-only write between pick and verdict must reach the
// outcome a content write reaches: verdict discarded, Discarded++, block
// untouched and back in the pick set.
//
// The window is the classify seam, as in TestAuditContentSwap_Integration;
// the write is the production title update (store.UpdateBlock).
//
//	go test -tags=integration ./internal/events/ -run TestAuditTitleSwap -count=1 -v
package events

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestAuditTitleSwap_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const tenantScope = "i452-audit"

	cfgStore := config.NewStore(&config.Config{
		Pool:      config.PoolConfig{LLMAuditMinSensitivity: backends.SensInternal},
		Scheduler: config.SchedulerConfig{HomeScope: "private"},
	})
	s := NewScheduler(pool, cfgStore, backends.NewPool(nil, nil), StartupConfig{})

	b, err := store.UpsertBlock(ctx, pool, "learnings", "harmless title", "ordinary prose", nil, nil, tenantScope, true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	var once sync.Once
	var askedTitle string
	s.classify = func(_ context.Context, _ *pgxpool.Pool, _ *backends.Pool,
		_, title, _, _ string, _ llm.Admission) (bool, error) {
		once.Do(func() {
			askedTitle = title
			nt := "title written while the classifier was answering"
			if _, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Title: &nt}, []string{tenantScope}); err != nil {
				t.Errorf("mid-flight title write: %v", err)
			}
		})
		return false, nil // "no" twice ⇒ the internal verdict
	}

	// limit=1: a discarded verdict leaves the block in the pick set, and an
	// unbounded drain would judge the new title at once.
	bt := backgroundTenant{scope: tenantScope, owned: []string{tenantScope}}
	if abort := s.auditTenantScope(ctx, bt, false, 1); abort {
		t.Fatalf("audit aborted: %+v", s.SensitivityAuditStatus())
	}
	if askedTitle != "harmless title" {
		t.Fatalf("the classifier was asked about title %q, want the picked one", askedTitle)
	}

	st := s.SensitivityAuditStatus()
	if st.Discarded != 1 || st.ToInternal != 0 {
		t.Errorf("Discarded=%d ToInternal=%d, want 1/0 — a verdict about the old title must not land on the new one (status %+v)", st.Discarded, st.ToInternal, st)
	}
	var sens, source string
	if err := pool.QueryRow(ctx,
		`SELECT sensitivity, sensitivity_source FROM context_blocks WHERE id = $1`, b.ID).Scan(&sens, &source); err != nil {
		t.Fatalf("read block: %v", err)
	}
	if source != "default" {
		t.Errorf("the block is at %s/%s after the title swap, want source default (unclassified, re-pickable)", sens, source)
	}
	picks, err := store.PickAuditBlocks(ctx, pool, tenantScope, 0, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(picks) != 1 || picks[0].Title != "title written while the classifier was answering" {
		t.Errorf("pick set afterwards = %d row(s), want the block with its new title back in it", len(picks))
	}
}
