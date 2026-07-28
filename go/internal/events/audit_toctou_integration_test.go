//go:build integration

// Wave H10 probe (a), run-level half (design 04 §2.4-C, §7 H10-a): a content
// swap between pick and write must reach the SAME already-modelled outcome the
// manual race reaches — verdict discarded, Discarded++, block untouched and
// back in the pick set.
//
// The store-level probes (internal/store/sensitivity_toctou_integration_test.go)
// pin the SQL predicate. This one pins the drain loop's bookkeeping around it,
// and it does so with a real race window rather than a simulated one: the
// classify seam IS the window (it is where the run spends its time between
// PickAuditBlocks and ApplyAuditVerdict), so the scripted classifier rewrites
// the content from inside the call. No sleeps, no goroutine ordering.
//
// RED against Ist (pre-H10 predicate without `AND md5(content) = $3`): the
// verdict lands on the rewritten row — Discarded stays 0, ToInternal becomes 1,
// and a block whose current text was never classified carries an llm-audit
// stamp that takes it out of the pick set for good.
//
// Run with:
//
//	GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/events/ -run TestAuditContentSwap_Integration -count=1 -v
package events

import (
	"context"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAuditContentSwap_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const tenantScope = "h10-race"

	cfgStore := config.NewStore(&config.Config{
		Pool:      config.PoolConfig{LLMAuditMinSensitivity: backends.SensInternal},
		Scheduler: config.SchedulerConfig{HomeScope: "private"},
	})
	s := NewScheduler(pool, cfgStore, backends.NewPool(nil, nil), StartupConfig{})

	// Seeded locally rather than reusing the H9 helper: this file must stay
	// applicable as its own change set.
	var blockID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_blocks (category, title, content, scope)
		 VALUES ('learnings', 'h10-race-victim', 'v1: ordinary prose', $1)
		 RETURNING id`, tenantScope).Scan(&blockID); err != nil {
		t.Fatalf("seed block: %v", err)
	}

	// The race, injected where it really lives: while the model is "thinking",
	// the writer replaces the content. once{} keeps it to a single rewrite so a
	// re-pick in the same drain would see a stable row.
	var once sync.Once
	s.classify = func(_ context.Context, _ *pgxpool.Pool, _ *backends.Pool,
		_, _, _, _ string, _ llm.Admission) (bool, error) {
		once.Do(func() {
			if _, err := pool.Exec(ctx,
				`UPDATE context_blocks SET content = $2 WHERE id = $1`,
				blockID, "v2 written while the classifier was answering"); err != nil {
				t.Errorf("mid-flight rewrite: %v", err)
			}
		})
		return false, nil // "nein" twice ⇒ the internal verdict, unclamped
	}

	// limit=1: a discarded verdict leaves the block IN the pick set by design
	// (that is the point — it gets re-judged), so an unbounded drain would
	// simply re-audit the now-stable v2 and prove nothing about the discard.
	bt := backgroundTenant{scope: tenantScope, owned: []string{tenantScope}}
	if abort := s.auditTenantScope(ctx, bt, false, 1); abort {
		t.Fatalf("audit aborted: %+v", s.SensitivityAuditStatus())
	}

	st := s.SensitivityAuditStatus()
	if st.Discarded != 1 {
		t.Fatalf("Discarded = %d, want 1 — a verdict formed over v1 must not land on v2 (status: %+v)", st.Discarded, st)
	}
	if st.ToInternal != 0 {
		t.Fatalf("ToInternal = %d, want 0 — the downgrade must not be counted as applied", st.ToInternal)
	}

	var sens, source string
	if err := pool.QueryRow(ctx,
		`SELECT sensitivity, sensitivity_source FROM context_blocks WHERE id = $1`, blockID).
		Scan(&sens, &source); err != nil {
		t.Fatalf("read block state: %v", err)
	}
	if sens != string(backends.SensCredentials) || source != "default" {
		t.Fatalf("raced row = sens %q source %q, want credentials/default (untouched, re-pickable)", sens, source)
	}
}
