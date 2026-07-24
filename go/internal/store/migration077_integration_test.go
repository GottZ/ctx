//go:build integration

package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// TestMigration077_WorkflowStatus pins Achse-02 Welle I-B migration 077
// (design/02 §3.3): after the full chain context_blocks carries a nullable
// workflow_status VARCHAR(50) column (col_count 39→40 vs M075) and the partial
// keyset board index idx_blocks_workflow_board. The runner's gap handling
// (076→077→078: live already has 78 recorded, 77 free) and idempotency of the
// real embedded file are proven directly.
func TestMigration077_WorkflowStatus(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// (1) Column exists, nullable, VARCHAR(50).
	var dataType, nullable string
	var maxLen int
	if err := pool.QueryRow(ctx,
		`SELECT data_type, is_nullable, coalesce(character_maximum_length,0)
		   FROM information_schema.columns
		  WHERE table_name='context_blocks' AND column_name='workflow_status'`,
	).Scan(&dataType, &nullable, &maxLen); err != nil {
		t.Fatalf("workflow_status column probe: %v", err)
	}
	if dataType != "character varying" || nullable != "YES" || maxLen != 50 {
		t.Errorf("workflow_status = %s nullable=%s len=%d, want character varying/YES/50", dataType, nullable, maxLen)
	}

	// (2) T07 oracle live-verification: context_blocks column count = 41
	// since M114 (Achse 04 W04-3) added the dual-column cutover pair
	// embedding_next/embed_model_next (was 39: 40 pre-109 minus
	// embed_status, M109 W04-1 provenance repair removes embed_status; +2
	// for M114's pair). test.sh T07 pins the LIVE DB count separately —
	// that flip belongs to the live-migration runbook, not to the
	// fresh-chain truth asserted here.
	var colCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns WHERE table_name='context_blocks'`,
	).Scan(&colCount); err != nil {
		t.Fatalf("col count: %v", err)
	}
	if colCount != 41 {
		t.Errorf("context_blocks column count = %d, want 41 (39 post-109 + M114 embedding_next/embed_model_next)", colCount)
	}

	// (3) Partial keyset board index exists with the design/02 §3.3 shape.
	var indexDef string
	if err := pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes
		  WHERE tablename='context_blocks' AND indexname='idx_blocks_workflow_board'`,
	).Scan(&indexDef); err != nil {
		t.Fatalf("board index probe: %v", err)
	}
	for _, want := range []string{
		"scope", "type_name", "workflow_status", "updated_at DESC", "id",
		"workflow_status IS NOT NULL", "NOT is_archived",
	} {
		if !strings.Contains(indexDef, want) {
			t.Errorf("idx_blocks_workflow_board def missing %q\n  def: %s", want, indexDef)
		}
	}

	// (4) The full chain recorded 76, 77 AND 78 (77 is not skipped by the gap).
	for _, v := range []int{76, 77, 78} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM _migrations WHERE version=$1)`, v).Scan(&ok); err != nil {
			t.Fatalf("migration %d exists probe: %v", v, err)
		}
		if !ok {
			t.Errorf("_migrations missing version %d", v)
		}
	}

	// (5) GAP HANDLING: delete the 077 record while 078 stays, re-run the runner.
	// The runner must re-apply the missing lower version regardless of the higher
	// applied one (live truth: version 78 present, 77 was the reserved gap). The
	// migration body is idempotent (ADD COLUMN IF NOT EXISTS / CREATE INDEX IF
	// NOT EXISTS), so the re-apply is a clean no-op that re-records 77.
	if _, err := pool.Exec(ctx, `DELETE FROM _migrations WHERE version=77`); err != nil {
		t.Fatalf("delete 077 record: %v", err)
	}
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("re-run migrations after gap: %v", err)
	}
	var reRecorded, still78 bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM _migrations WHERE version=77)`).Scan(&reRecorded); err != nil {
		t.Fatalf("re-record probe: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM _migrations WHERE version=78)`).Scan(&still78); err != nil {
		t.Fatalf("78 probe: %v", err)
	}
	if !reRecorded {
		t.Error("runner did not re-apply the missing version 77 (gap handling broken)")
	}
	if !still78 {
		t.Error("version 78 vanished after re-run")
	}

	// (6) Direct idempotency of the REAL embedded file (no test-local SQL copy —
	// fixture-collusion guard, the M075/M072 line): re-apply twice more.
	sqlBytes, err := migrations.FS.ReadFile("077_workflow_status.sql")
	if err != nil {
		t.Fatalf("read embedded 077: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("re-apply 077 (pass %d): %v", i, err)
		}
	}

	// Column and index survive the re-applies unchanged.
	var idxCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE tablename='context_blocks' AND indexname='idx_blocks_workflow_board'`,
	).Scan(&idxCount); err != nil {
		t.Fatalf("post-idempotency index probe: %v", err)
	}
	if idxCount != 1 {
		t.Errorf("board index count = %d after re-applies, want 1", idxCount)
	}
}
