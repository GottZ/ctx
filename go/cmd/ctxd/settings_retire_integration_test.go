//go:build integration

// Gate of Entflechtung-Stufe-2 wave β11 (A05-W-D2, design/05 §7): migration
// 133 deletes the context_settings rows of the 29 retired backend tuple keys
// across EVERY scope, the audit trigger records each delete as an attributable
// unset, living keys are untouched, and the operator hears about it.
//
//	go test -tags=integration ./internal/store/ -run TestMigration133 -count=1 -v
//
// The migration is never run against the live database from here — the whole
// gate lives on a throwaway testcontainer database capped at migration 132.
//
// It lives in cmd/ctxd rather than internal/store (where design/05 §7 sketched
// it) for the same reason retiredsources_integration_test.go does: only the
// cmd/** layer may import internal/config (F1 layering, depguard), and the
// expectation this gate holds the migration against has to BE
// config.RetiredKeyNames() — a fixture that merely resembles it would drift
// with the thing it is supposed to catch drifting. The second half of the
// wave's protection, the set equality between the SQL array and that same map,
// is a unit test next to the map itself (internal/config/retiredmigration_test.go)
// and needs no database at all.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const (
	retireMigrationVersion = 133
	retireMigrationFile    = "133_retire_backend_tuple_rows.sql"
	retireRequestID        = "migration-133-retire-backend-tuples"
	retireHopHint          = "migrate to last 4.x"

	// settingEntity is the entity_type the settings audit trigger stamps on
	// its rows. context_settings_audit is a SHARED table — the block-type
	// trigger writes into it too — so every count this gate makes a claim
	// about is narrowed to this entity; see the append-only subtest.
	settingEntity = "setting"

	// contrastKey is a LIVING key that survived the registry cut and shares
	// the `rerank.` prefix with three retired ones — the closest neighbour the
	// array literal could plausibly over-reach into. It is seeded at both a
	// global and a tenant scope (it is tenant-overridable) so an over-broad
	// DELETE has two ways to be caught, not one.
	contrastKey = "rerank.enabled"
)

// tenantScopes are the two non-global scopes the seed uses. Two rather than
// one: a scope filter that someone "fixes" into the migration would have to
// enumerate both to stay green, which is a harder mistake to make by accident
// than special-casing a single fixture name.
var tenantScopes = []string{"acme", "globex"}

// TestMigration133RetiresBackendTupleRows is the wave's main gate.
func TestMigration133RetiresBackendTupleRows(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()

	// Capped at 132: the file under test is the next one in the chain and has
	// NOT run yet, so rows can be seeded into the pre-migration world the way
	// a foreign installation carries them.
	pool, dsn := testdb.SetupTestDBUpToWithDSN(t, retireMigrationVersion-1)

	if applied := migrationApplied(t, pool, retireMigrationVersion); applied {
		t.Fatalf("migration %d is already recorded on a database capped at %d — the seed below would test nothing",
			retireMigrationVersion, retireMigrationVersion-1)
	}

	retired := config.RetiredKeyNames()
	if len(retired) != 29 {
		t.Fatalf("config.RetiredKeyNames() = %d keys, want 29 (the six backend role tuples)", len(retired))
	}

	// Seed 1 — one _global row per retired key, programmatically from the map
	// rather than a hand-picked sample: a key missing from the migration's
	// array is exactly the drift this gate has to see, and three spot checks
	// would see it only by luck.
	for _, key := range retired {
		insertSettingRow(t, pool, key, store.GlobalScope, seedValueFor(key))
	}

	// Seed 2 — tenant-scoped rows on the api_key keys. These are the class a
	// `WHERE scope = '_global'` filter would leave behind: after the registry
	// cut they are invisible to the settings list, to referencedBy and even to
	// the boot WARN (the full reload only ever loads '_global').
	tenantSeeded := 0
	for _, scope := range tenantScopes {
		for _, key := range retired {
			if !strings.HasSuffix(key, ".api_key") {
				continue
			}
			insertSettingRow(t, pool, key, scope, `"legacy-secret-name"`)
			tenantSeeded++
		}
	}
	if tenantSeeded != 12 {
		t.Fatalf("seeded %d tenant rows, want 12 (six *.api_key keys × two scopes)", tenantSeeded)
	}

	// Seed 3 — the contrast rows on a living key, global and tenant.
	insertSettingRow(t, pool, contrastKey, store.GlobalScope, `true`)
	insertSettingRow(t, pool, contrastKey, tenantScopes[0], `false`)

	wantDeleted := len(retired) + tenantSeeded // 29 + 12 = 41
	auditBefore := countAudit(t, pool, settingEntity, "")
	if auditBefore == 0 {
		t.Fatalf("no audit rows after seeding %d settings rows — the 051 audit trigger is not firing, and every assertion below would be vacuous", wantDeleted+2)
	}
	// Foreign entities present before the chain runs (the block-type seeds of
	// 113_baseline.sql go through the same audit table). Captured so the
	// append-only subtest can name what the chain added on top of them.
	foreignBefore, foreignBeforeBreakdown := countForeignAudit(t, pool)
	t.Logf("audit rows after seeding: %d for entity_type=%q, %d for other entities (%s)",
		auditBefore, settingEntity, foreignBefore, foreignBeforeBreakdown)

	// Run the rest of the chain over a PRODUCTION-shaped pool: store.NewPool
	// is what cmd/ctxd builds before store.RunMigrations, and it is the thing
	// that carries a RAISE NOTICE into the process log.
	logs := captureSlog(t)
	prodPool, err := store.NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open production-shaped pool: %v", err)
	}
	defer prodPool.Close()

	if err := store.RunMigrations(ctx, prodPool); err != nil {
		t.Fatalf("run migrations through %d: %v", retireMigrationVersion, err)
	}

	t.Run("every retired row is gone, in every scope", func(t *testing.T) {
		var left int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_settings WHERE key = ANY($1)`, retired,
		).Scan(&left); err != nil {
			t.Fatalf("count surviving retired rows: %v", err)
		}
		if left != 0 {
			rows, _ := pool.Query(ctx,
				`SELECT key, scope FROM context_settings WHERE key = ANY($1) ORDER BY key, scope`, retired)
			defer rows.Close()
			var leftovers []string
			for rows.Next() {
				var k, s string
				_ = rows.Scan(&k, &s)
				leftovers = append(leftovers, k+"@"+s)
			}
			t.Errorf("%d retired row(s) survived migration %d: %v", left, retireMigrationVersion, leftovers)
		}
	})

	t.Run("the living key keeps both of its rows", func(t *testing.T) {
		var alive int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_settings WHERE key = $1`, contrastKey,
		).Scan(&alive); err != nil {
			t.Fatalf("count contrast rows: %v", err)
		}
		if alive != 2 {
			t.Errorf("%s has %d row(s) after the migration, want 2 — the array literal reaches into a LIVING key", contrastKey, alive)
		}
	})

	t.Run("each delete left an attributable unset in the audit", func(t *testing.T) {
		marked := countAudit(t, pool, settingEntity, retireRequestID)
		if marked != wantDeleted {
			t.Errorf("audit rows carrying request_id %q = %d, want %d (one per deleted row)", retireRequestID, marked, wantDeleted)
		}

		var bad int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM context_settings_audit
			 WHERE metadata->>'request_id' = $1
			   AND NOT (entity_type = 'setting'
			            AND action = 'unset'
			            AND metadata->>'via' = 'sql'
			            AND api_key_id IS NULL
			            AND old_value IS NOT NULL)`, retireRequestID,
		).Scan(&bad); err != nil {
			t.Fatalf("inspect marked audit rows: %v", err)
		}
		if bad != 0 {
			t.Errorf("%d marked audit row(s) are not a plain SQL-side unset with the deleted value attached — the rollback query in the runbook would return junk", bad)
		}

		// Scope fidelity: the audit has to name the scope each row lived in,
		// otherwise a tenant value cannot be restored to the right place.
		for _, scope := range append([]string{store.GlobalScope}, tenantScopes...) {
			var n int
			if err := pool.QueryRow(ctx, `
				SELECT count(*) FROM context_settings_audit
				 WHERE metadata->>'request_id' = $1 AND scope = $2`, retireRequestID, scope,
			).Scan(&n); err != nil {
				t.Fatalf("count marked audit rows for scope %s: %v", scope, err)
			}
			want := len(retired)
			if scope != store.GlobalScope {
				want = 6
			}
			if n != want {
				t.Errorf("scope %q: %d marked audit row(s), want %d", scope, n, want)
			}
		}
	})

	t.Run("the pre-existing settings audit history is untouched", func(t *testing.T) {
		// The claim is append-only for the SETTINGS history, not for every
		// row of context_settings_audit. That table is shared: the block-type
		// trigger trg_block_types_audit (audit_block_type_write,
		// 113_baseline.sql:7384) writes into it as well, with
		// entity_type = 'block_type'. RunMigrations above applies the WHOLE
		// remaining chain, not just 133, and two of those later files touch
		// context_block_types — 136_tool_evidence_block_types.sql seeds the
		// two tool-* rows, 138_tool_types_untrusted.sql updates the same two —
		// so four foreign audit rows legitimately appear here that say nothing
		// about whether migration 133 rewrote or dropped settings history.
		// Counting the whole table would make this gate fail on every future
		// registry migration; counting entity_type = 'setting' keeps it
		// pinning exactly the invariant it is named after.
		total := countAudit(t, pool, settingEntity, "")
		if total != auditBefore+wantDeleted {
			t.Errorf("audit rows for entity_type=%q = %d, want %d (%d before + %d unsets) — append-only was violated",
				settingEntity, total, auditBefore+wantDeleted, auditBefore, wantDeleted)
		}

		// The foreign rows are still held to two things, so that this subtest
		// keeps seeing what it is here to see: they must be plain SQL-side
		// migration writes, and none of them may be attributed to migration
		// 133 (no timestamp comparison needed — the marker IS the attribution).
		foreignAfter, foreignAfterBreakdown := countForeignAudit(t, pool)
		t.Logf("audit rows of other entities: %d before the chain (%s), %d after (%s)",
			foreignBefore, foreignBeforeBreakdown, foreignAfter, foreignAfterBreakdown)

		var notSQL int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM context_settings_audit
			 WHERE entity_type <> $1
			   AND metadata->>'via' IS DISTINCT FROM 'sql'`, settingEntity,
		).Scan(&notSQL); err != nil {
			t.Fatalf("inspect foreign audit rows: %v", err)
		}
		if notSQL != 0 {
			t.Errorf("%d audit row(s) of other entities are not plain SQL-side migration writes — something other than the migration chain wrote here", notSQL)
		}

		var misattributed int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM context_settings_audit
			 WHERE entity_type <> $1 AND metadata->>'request_id' = $2`, settingEntity, retireRequestID,
		).Scan(&misattributed); err != nil {
			t.Fatalf("inspect foreign audit rows for the retire marker: %v", err)
		}
		if misattributed != 0 {
			t.Errorf("%d audit row(s) of other entities carry request_id %q — migration %d reached beyond context_settings",
				misattributed, retireRequestID, retireMigrationVersion)
		}
	})

	t.Run("the operator is told, and told where to go", func(t *testing.T) {
		out := logs.String()
		if !strings.Contains(out, retireHopHint) {
			t.Errorf("the boot log carries no %q hint. Log:\n%s", retireHopHint, out)
		}
		if !strings.Contains(out, fmt.Sprintf("deleted %d context_settings row(s)", wantDeleted)) {
			t.Errorf("the boot log does not name the number of deleted rows (%d). Log:\n%s", wantDeleted, out)
		}
		if !strings.Contains(out, retireRequestID) {
			t.Errorf("the boot log does not name the audit marker %q, so the operator cannot find the deleted values. Log:\n%s", retireRequestID, out)
		}
	})

	t.Run("a second application changes and says nothing", func(t *testing.T) {
		// Unfiltered on purpose: nothing but the re-applied file runs here, so
		// a re-run must not append a row of ANY entity, not merely no setting.
		auditBeforeRerun := countAudit(t, pool, "", "")
		logs.reset()

		// RunMigrations cannot serve here: version 133 is recorded now and
		// would be skipped, which tests the runner's bookkeeping instead of
		// the statement's idempotency (pattern: applyM121Again).
		applyRetireMigrationAgain(t, prodPool)

		if got := countAudit(t, pool, "", ""); got != auditBeforeRerun {
			t.Errorf("audit grew from %d to %d on the second application — the DELETE is not a no-op on an already-clean database", auditBeforeRerun, got)
		}
		if out := logs.String(); strings.Contains(out, retireHopHint) {
			t.Errorf("the second application still tells the operator to migrate — the notice is not gated on rows actually deleted. Log:\n%s", out)
		}
		var alive int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_settings WHERE key = $1`, contrastKey).Scan(&alive); err != nil {
			t.Fatalf("re-count contrast rows: %v", err)
		}
		if alive != 2 {
			t.Errorf("%s lost rows on the second application (%d left, want 2)", contrastKey, alive)
		}
	})
}

// seedValueFor gives each retired key a plausible pre-cut value. The api_key
// keys get a bare JSON string, which IS the secret_ref shape for the fp-class
// keys (the row holds the NAME of a context_secrets entry, never plaintext —
// the settings API rejects plaintext on them with 422). Precedent:
// cmd/ctxd/retiredsources_integration_test.go seeds the same shape.
func seedValueFor(key string) string {
	switch {
	case strings.HasSuffix(key, ".api_key"):
		return `"legacy-secret-name"`
	case strings.HasSuffix(key, ".num_ctx"), strings.HasSuffix(key, ".timeout"):
		return `4096`
	case strings.HasSuffix(key, ".think"):
		return `true`
	default:
		return `"http://legacy.example.com:11434"`
	}
}

// insertSettingRow writes a row the psql way — deliberately bypassing the
// settings API, which after the registry cut answers 404 on these keys and
// could not create the fixture at all.
func insertSettingRow(t *testing.T, pool *pgxpool.Pool, key, scope, jsonValue string) {
	t.Helper()
	if !json.Valid([]byte(jsonValue)) {
		t.Fatalf("fixture value for %s@%s is not JSON: %s", key, scope, jsonValue)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_settings (key, scope, value) VALUES ($1, $2, $3::jsonb)`,
		key, scope, jsonValue); err != nil {
		t.Fatalf("seed setting %s@%s: %v", key, scope, err)
	}
}

// countAudit counts audit rows. A non-empty entityType narrows to one entity —
// context_settings_audit is shared with the block-type trigger, so a claim
// about settings history has to say so — and a non-empty requestID narrows
// further to the rows the migration marked. Empty means "do not narrow".
func countAudit(t *testing.T, pool *pgxpool.Pool, entityType, requestID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM context_settings_audit
		 WHERE ($1 = '' OR entity_type = $1)
		   AND ($2 = '' OR metadata->>'request_id' = $2)`, entityType, requestID,
	).Scan(&n); err != nil {
		t.Fatalf("count audit rows (entity_type=%q, request_id=%q): %v", entityType, requestID, err)
	}
	return n
}

// countForeignAudit counts the audit rows that belong to some OTHER entity
// than a setting and describes them by entity_type and action, so the test log
// names who else writes into the shared table rather than leaving a bare number.
func countForeignAudit(t *testing.T, pool *pgxpool.Pool) (int, string) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT entity_type, action, count(*)
		  FROM context_settings_audit
		 WHERE entity_type <> $1
		 GROUP BY entity_type, action
		 ORDER BY entity_type, action`, settingEntity)
	if err != nil {
		t.Fatalf("break down foreign audit rows: %v", err)
	}
	defer rows.Close()
	total := 0
	var parts []string
	for rows.Next() {
		var entity, action string
		var n int
		if err := rows.Scan(&entity, &action, &n); err != nil {
			t.Fatalf("scan foreign audit breakdown: %v", err)
		}
		total += n
		parts = append(parts, fmt.Sprintf("%s/%s=%d", entity, action, n))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read foreign audit breakdown: %v", err)
	}
	if len(parts) == 0 {
		return 0, "none"
	}
	return total, strings.Join(parts, " ")
}

func migrationApplied(t *testing.T, pool *pgxpool.Pool, version int) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM _migrations WHERE version = $1)`, version).Scan(&exists); err != nil {
		t.Fatalf("look up migration %d: %v", version, err)
	}
	return exists
}

// applyRetireMigrationAgain re-executes the embedded file in its own
// transaction, mirroring what the runner does — the SET LOCAL statements the
// file opens with are only meaningful inside one.
func applyRetireMigrationAgain(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	sql, err := migrations.FS.ReadFile(retireMigrationFile)
	if err != nil {
		t.Fatalf("read %s: %v", retireMigrationFile, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin re-apply tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("re-apply %s: %v", retireMigrationFile, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit re-apply: %v", err)
	}
}

// logSink collects the process log for the span of one test. The notice
// handler runs on pgx's connection reader, so the buffer is mutex-guarded even
// though integration tests never run in parallel.
type logSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *logSink) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Reset()
}

func captureSlog(t *testing.T) *logSink {
	t.Helper()
	sink := &logSink{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return sink
}
