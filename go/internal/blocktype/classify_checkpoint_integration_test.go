//go:build integration

// Migration 120 gates (H-W13): the second checkpoint title pattern reaches the
// registry row, and it does so WITHOUT overwriting operator tuning (M107
// doctrine — a seed never clobbers a hand-edited config).
//
//	GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/blocktype/ -run TestClassify_ -count=1 -v
package blocktype_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// checkpointPatterns reads the live title_patterns of the _global checkpoint
// row as a JSON text — the exact value migration 120 predicates on.
func checkpointPatterns(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var raw string
	if err := pool.QueryRow(context.Background(),
		`SELECT config->'classify'->'title_patterns'
		   FROM context_block_types WHERE name = 'checkpoint' AND scope = '_global'`).Scan(&raw); err != nil {
		t.Fatalf("read checkpoint patterns: %v", err)
	}
	return raw
}

// TestClassify_OperatorTuningPreserved runs migration 120 against two pre-120
// states in one container: the untouched M107 seed (must gain the head
// pattern) and an operator-tuned row (must stay byte-identical). The second
// case is the negative probe — dropping the value predicate from the UPDATE
// turns it red.
func TestClassify_OperatorTuningPreserved(t *testing.T) {
	ctx := context.Background()

	t.Run("untuned_row_gains_head_pattern", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 119)
		if got := checkpointPatterns(t, pool); got != `["compaction source"]` {
			t.Fatalf("pre-120 patterns = %s, want the bare M107 seed", got)
		}
		if err := store.RunMigrationsUpTo(ctx, pool, 120); err != nil {
			t.Fatalf("apply migration 120: %v", err)
		}
		if got := checkpointPatterns(t, pool); got != `["compaction source", "compaction checkpoint"]` {
			t.Errorf("post-120 patterns = %s, want both patterns", got)
		}
	})

	t.Run("tuned_row_untouched", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 119)
		const tuned = `["compaction source", "operator tuned"]`
		if _, err := pool.Exec(ctx,
			`UPDATE context_block_types
			    SET config = jsonb_set(config, '{classify,title_patterns}', $1::jsonb)
			  WHERE name = 'checkpoint' AND scope = '_global'`, tuned); err != nil {
			t.Fatalf("operator tuning: %v", err)
		}
		if err := store.RunMigrationsUpTo(ctx, pool, 120); err != nil {
			t.Fatalf("apply migration 120: %v", err)
		}
		if got := checkpointPatterns(t, pool); got != tuned {
			t.Errorf("post-120 patterns = %s, want the operator value %s untouched", got, tuned)
		}
	})
}

// TestClassify_ManualUntouched pins the write layer on the new pattern: a head
// block classifies checkpoint on the auto path, while the same title on a
// type_source='manual' row is never re-classified (classify.go's
// `AND type_source = 'auto'` predicate).
func TestClassify_ManualUntouched(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	set := reg.Snapshot()

	const title = "Compaction checkpoint head 20260728_xyz"
	insert := func(t *testing.T, typeName, typeSource string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_blocks (category, title, content, type_name, type_source)
			 VALUES ('compaction-checkpoints', $1, 'head manifest', $2, $3) RETURNING id`,
			title+" "+typeSource, typeName, typeSource).Scan(&id); err != nil {
			t.Fatalf("insert %s block: %v", typeSource, err)
		}
		return id
	}
	typeOf := func(t *testing.T, id string) (string, string) {
		t.Helper()
		var name, source string
		if err := pool.QueryRow(ctx,
			`SELECT type_name, type_source FROM context_blocks WHERE id = $1::uuid`, id).Scan(&name, &source); err != nil {
			t.Fatalf("read type: %v", err)
		}
		return name, source
	}

	idAuto := insert(t, "knowledge", "auto")
	role, err := store.ClassifyBlockAfterUpsert(ctx, pool, set, idAuto, title+" auto", nil)
	if err != nil {
		t.Fatalf("classify auto block: %v", err)
	}
	if role != "checkpoint" {
		t.Errorf("auto role = %q, want checkpoint", role)
	}
	if name, source := typeOf(t, idAuto); name != "checkpoint" || source != "auto" {
		t.Errorf("auto block = (%q, %q), want (checkpoint, auto)", name, source)
	}

	idManual := insert(t, "reference", "manual")
	role, err = store.ClassifyBlockAfterUpsert(ctx, pool, set, idManual, title+" manual", nil)
	if err != nil {
		t.Fatalf("classify manual block: %v", err)
	}
	if role != "" {
		t.Errorf("manual role = %q, want none — a manual type assertion is permanent", role)
	}
	if name, source := typeOf(t, idManual); name != "reference" || source != "manual" {
		t.Errorf("manual block = (%q, %q), want (reference, manual)", name, source)
	}
}
