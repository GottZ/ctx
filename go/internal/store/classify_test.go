//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// dbRegistry boots a registry off the migrated test DB (M072 seeds) — the
// classify rules come from DATA, exactly like production wiring (T4).
func dbRegistry(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *blocktype.Registry {
	t.Helper()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	return reg
}

// TestClassifyBlockAfterUpsert_Decisions verifies the decision semantics of
// the T4 registry-driven hook against the pre-T4 tree behaviour (the full
// 42-triple golden corpus lives in blocktype/classify_golden_test.go; this
// covers the DB-write layer: UPDATE applied vs. no-op).
func TestClassifyBlockAfterUpsert_Decisions(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	set := dbRegistry(t, ctx, pool).Snapshot()

	cases := []struct {
		name     string
		title    string
		metadata map[string]any
		wantRole string
	}{
		{
			name:     "metadata-is-meta-flag", // metadata KEY (classify input) — the column fell with M075/T9
			title:    "Some Block",
			metadata: map[string]any{"is_meta": true},
			wantRole: "system-meta",
		},
		{
			name:     "metadata-dream-source",
			title:    "Tagesbericht 2026-05-07",
			metadata: map[string]any{"source": "dream-synthesis"},
			wantRole: "audit-trail",
		},
		{
			name:     "title-pattern-session",
			title:    "Session 7 Abschlussbilanz",
			metadata: map[string]any{},
			wantRole: "audit-trail",
		},
		{
			name:     "title-pattern-welle",
			title:    "Welle 42 Bench-Verdict",
			metadata: nil,
			wantRole: "audit-trail",
		},
		{
			name:     "no-pattern-no-meta-noop",
			title:    "ddstatus mariadb donnerstags",
			metadata: map[string]any{"source": "claude-code-2026-05-07"},
			wantRole: "", // no UPDATE — defaults stay
		},
		{
			name:     "bare-dream-prefix-noop", // old-tree parity: len(src) > 6
			title:    "Irgendwas",
			metadata: map[string]any{"source": "dream-"},
			wantRole: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := insertClassifyTestBlock(t, pool, tc.title)
			role, err := store.ClassifyBlockAfterUpsert(ctx, pool, set, id, tc.title, tc.metadata)
			if err != nil {
				t.Fatalf("ClassifyBlockAfterUpsert: %v", err)
			}
			if role != tc.wantRole {
				t.Errorf("role = %q, want %q", role, tc.wantRole)
			}

			// Verify DB state matches return values (or default state for noop).
			var dbRole string
			err = pool.QueryRow(ctx,
				`SELECT type_name FROM context_blocks WHERE id = $1::uuid`, id,
			).Scan(&dbRole)
			if err != nil {
				t.Fatalf("verify DB: %v", err)
			}
			expectRole := tc.wantRole
			if expectRole == "" {
				expectRole = "knowledge"
			}
			if dbRole != expectRole {
				t.Errorf("DB type_name = %q, want %q", dbRole, expectRole)
			}
		})
	}

	// nil set: loud error, no silent builtin fallback.
	t.Run("nil-set-errors", func(t *testing.T) {
		id := insertClassifyTestBlock(t, pool, "Session nil-set probe")
		if _, err := store.ClassifyBlockAfterUpsert(ctx, pool, nil, id, "Session nil-set probe", nil); err == nil {
			t.Error("nil set accepted — classify must fail loudly, not fall back")
		}
	})
}

// TestClassifyPolicyEffect_Integration is the T4 policy-effect gate
// (design/01 §7-T4): extending audit-trail's classify.title_patterns by
// "retro" via SQL + Reload makes a NEW block "Sprint Retro" audit-trail
// WITHOUT a process restart.
//
// RED state proven against the pre-T4 code (hardcoded tree, commit 0352d9d):
// oldClassify("Sprint Retro", {}) == "" — no DB edit could reach the
// compiled-in list (captured in the T4 build log before the tree was
// replaced). GREEN here because the rules are registry data now.
func TestClassifyPolicyEffect_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := dbRegistry(t, ctx, pool)

	// Baseline: "Sprint Retro" matches no seed pattern → no classification.
	idBefore := insertClassifyTestBlock(t, pool, "Sprint Retro")
	role, err := store.ClassifyBlockAfterUpsert(ctx, pool, reg.Snapshot(), idBefore, "Sprint Retro", nil)
	if err != nil {
		t.Fatalf("baseline classify: %v", err)
	}
	if role != "" {
		t.Fatalf("baseline: role = %q, want no classification (seed patterns)", role)
	}

	// SQL-UPDATE the registry row (psql-equivalent edit) + Reload (the NOTIFY
	// listener path calls exactly this — no restart).
	if _, err := pool.Exec(ctx,
		`UPDATE context_block_types
		    SET config = jsonb_set(config, '{classify,title_patterns}',
		        (config->'classify'->'title_patterns') || '["retro"]'::jsonb)
		  WHERE name = 'audit-trail' AND scope = '_global'`); err != nil {
		t.Fatalf("pattern update: %v", err)
	}
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// New block with the same title now classifies audit-trail.
	idAfter := insertClassifyTestBlock(t, pool, "Sprint Retro 2")
	role, err = store.ClassifyBlockAfterUpsert(ctx, pool, reg.Snapshot(), idAfter, "Sprint Retro 2", nil)
	if err != nil {
		t.Fatalf("post-edit classify: %v", err)
	}
	if role != "audit-trail" {
		t.Fatalf("post-edit: role = %q, want audit-trail (DB pattern edit had no effect)", role)
	}
	var dbRole string
	if err := pool.QueryRow(ctx,
		`SELECT type_name FROM context_blocks WHERE id = $1::uuid`, idAfter).Scan(&dbRole); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if dbRole != "audit-trail" {
		t.Errorf("DB type_name = %q, want audit-trail", dbRole)
	}
}

// TestClassifyManualOverride_Integration is the T4 manual-override gate:
// a type_source='manual' block keeps its type even when title/metadata match
// a classify rule (sensitivity_source semantics — manual wins permanently).
func TestClassifyManualOverride_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	set := dbRegistry(t, ctx, pool).Snapshot()

	// Manual reference block whose title matches the audit patterns.
	id := insertClassifyTestBlock(t, pool, "Session 9 Handover Manual")
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET type_name = 'reference', type_source = 'manual' WHERE id = $1::uuid`,
		id); err != nil {
		t.Fatalf("set manual: %v", err)
	}

	role, err := store.ClassifyBlockAfterUpsert(ctx, pool, set, id, "Session 9 Handover Manual", nil)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if role != "" {
		t.Errorf("applied role = %q, want none (manual override must block the write)", role)
	}
	var dbRole, dbSource string
	if err := pool.QueryRow(ctx,
		`SELECT type_name, type_source FROM context_blocks WHERE id = $1::uuid`, id).Scan(&dbRole, &dbSource); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if dbRole != "reference" || dbSource != "manual" {
		t.Errorf("DB (type_name, type_source) = (%q, %q), want (reference, manual)", dbRole, dbSource)
	}

	// Counter-probe: the SAME title on an auto block IS re-classified —
	// proves the guard is type_source, not a general skip.
	idAuto := insertClassifyTestBlock(t, pool, "Session 9 Handover Auto")
	role, err = store.ClassifyBlockAfterUpsert(ctx, pool, set, idAuto, "Session 9 Handover Auto", nil)
	if err != nil {
		t.Fatalf("auto classify: %v", err)
	}
	if role != "audit-trail" {
		t.Errorf("auto block role = %q, want audit-trail", role)
	}
}

// insertClassifyTestBlock inserts a minimal context_blocks row and returns
// its id. UUID is auto-generated by uuidv7() default.
func insertClassifyTestBlock(t *testing.T, pool *pgxpool.Pool, title string) string {
	t.Helper()
	now := time.Now().UTC()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, created_at, updated_at)
		 VALUES ($1, $2, $3, 'private', $4, $4)
		 RETURNING id::text`,
		"test_classify", title, "classify-test-content", now,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert classify-test block: %v", err)
	}
	return id
}
