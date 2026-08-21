//go:build integration

// Integration gates for wave T3 (design/01-type-registry.md §7-T3) against a
// real PG18 testcontainer. Every probe is a NEGATIVE probe: the comment names
// the implementation drift that turns it red.
//
// Run with:
//
//	GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/blocktype/ -count=1 -v
package blocktype_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// ── slog capture ─────────────────────────────────────────────────────────────

type capturedRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

type logCapture struct {
	mu      sync.Mutex
	records []capturedRecord
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *logCapture) WithGroup(string) slog.Handler            { return c }
func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	rec := capturedRecord{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	c.mu.Lock()
	c.records = append(c.records, rec)
	c.mu.Unlock()
	return nil
}

// has reports whether a record at level with msgPart in the message (and, if
// non-empty, attrVal in any attr value) was captured.
func (c *logCapture) has(level slog.Level, msgPart, attrVal string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if r.level != level || !strings.Contains(r.msg, msgPart) {
			continue
		}
		if attrVal == "" {
			return true
		}
		for _, v := range r.attrs {
			if strings.Contains(v, attrVal) {
				return true
			}
		}
	}
	return false
}

func installCapture(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	old := slog.Default()
	slog.SetDefault(slog.New(c))
	t.Cleanup(func() { slog.SetDefault(old) })
	return c
}

func dampingOf(t *testing.T, s *blocktype.Set) float64 {
	t.Helper()
	// A query no intent pattern matches — audit-trail stays damped.
	names, factors := s.DampedTypesFor("zzz probe query zzz")
	if len(names) != 1 || names[0] != "audit-trail" {
		t.Fatalf("damped types = %v, want [audit-trail]", names)
	}
	return factors[0]
}

// TestRegistryGolden_Integration is the fixed-source drift gate (§4.1 R1):
// the migration 072 seeds come from the embedded migrations.FS (testdb applies
// the real chain), the expectation is the compiled-in builtin set — NO
// test-local JSON copy. RED when either side drifts (probed: builtin damping
// 0.3→0.35 flips golden_rows_match_builtin red).
func TestRegistryGolden_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	t.Run("golden_rows_match_builtin", func(t *testing.T) {
		rows, err := pool.Query(ctx,
			`SELECT name, scope, builtin, is_default, config FROM context_block_types ORDER BY name`)
		if err != nil {
			t.Fatalf("select seeds: %v", err)
		}
		defer rows.Close()
		got := map[string]blocktype.Policy{}
		for rows.Next() {
			var (
				name, scope        string
				builtin, isDefault bool
				raw                []byte
			)
			if err := rows.Scan(&name, &scope, &builtin, &isDefault, &raw); err != nil {
				t.Fatalf("scan: %v", err)
			}
			p, err := blocktype.DecodePolicy(name, scope, builtin, isDefault, raw)
			if err != nil {
				t.Fatalf("seed row %q does not decode: %v", name, err)
			}
			got[name] = p
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}

		want := map[string]blocktype.Policy{}
		for _, b := range blocktype.BuiltinPoliciesForTest() {
			want[b.Name] = b
		}
		if len(got) != len(want) {
			t.Fatalf("seed rows = %d, want %d (%v)", len(got), len(want), got)
		}
		for name, w := range want {
			g, ok := got[name]
			if !ok {
				t.Errorf("builtin %q missing from seeds", name)
				continue
			}
			if !reflect.DeepEqual(g, w) {
				t.Errorf("seed/builtin drift for %q:\n  seed:    %+v\n  builtin: %+v", name, g, w)
			}
		}
	})

	t.Run("seeds_idempotent_on_rerun", func(t *testing.T) {
		// Re-exec the RAW migration file (the runner skips by version — the
		// idempotency claim is about the file body: CREATE IF NOT EXISTS,
		// DROP TRIGGER IF EXISTS, ON CONFLICT DO NOTHING).
		sqlBytes, err := migrations.Section("072_block_type_registry.sql")
		if err != nil {
			t.Fatalf("read embedded migration: %v", err)
		}
		if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("second run of 072 failed (not idempotent): %v", err)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_block_types`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		// 7 since M107: 072 seeds 4, 084 seeds issue+comment, 107 seeds
		// checkpoint; re-running 072 (ON CONFLICT DO NOTHING) adds none.
		if n != 7 {
			t.Errorf("rows after double-run = %d, want 7", n)
		}
	})

	t.Run("audit_rows_via_sql", func(t *testing.T) {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_settings_audit
			  WHERE entity_type = 'block_type' AND action = 'insert'
			    AND metadata->>'via' = 'sql'`).Scan(&n); err != nil {
			t.Fatalf("audit count: %v", err)
		}
		// 7 since M107: 4 seed inserts from 072 + issue/comment from 084 +
		// checkpoint from 107, all via=sql (migration path, no api_key_id).
		if n != 7 {
			t.Errorf("block_type insert audit rows = %d, want 7 (seed inserts, via=sql)", n)
		}
	})

	t.Run("notify_trigger_fires_settings_channel", func(t *testing.T) {
		lc, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire LISTEN conn: %v", err)
		}
		defer lc.Release()
		if _, err := lc.Exec(ctx, "LISTEN ctx_settings_write"); err != nil {
			t.Fatalf("LISTEN: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE context_block_types SET updated_at = now() WHERE name = 'knowledge'`); err != nil {
			t.Fatalf("update: %v", err)
		}
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		n, err := lc.Conn().WaitForNotification(wctx)
		if err != nil {
			t.Fatalf("WaitForNotification (072 trigger missing?): %v", err)
		}
		var p struct{ Entity, Key, Scope, Op string }
		if err := json.Unmarshal([]byte(n.Payload), &p); err != nil {
			t.Fatalf("payload %q: %v", n.Payload, err)
		}
		if p.Entity != "context_block_types" || p.Key != "knowledge" || p.Scope != "_global" {
			t.Errorf("payload = %+v, want entity=context_block_types key=knowledge scope=_global", p)
		}
	})

	t.Run("boot_healthy_and_reload_reflects_db", func(t *testing.T) {
		capture := installCapture(t)
		reg := blocktype.NewRegistry()
		bctx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)
		reg.Boot(bctx, pool)
		if got := reg.Health(); got != blocktype.HealthOK {
			t.Fatalf("healthy boot: Health() = %q, want ok", got)
		}
		if capture.has(slog.LevelError, "", "") {
			t.Errorf("healthy boot emitted ERROR records: %+v", capture.records)
		}
		if got := dampingOf(t, reg.Snapshot()); got != 0.3 {
			t.Errorf("damping after healthy boot = %v, want 0.3", got)
		}
	})
}

// TestRegistryDegradation_Integration walks the §4.3/§5.6 degradation matrix
// in one container: corrupt row (class b, loud + retry heal), merge-reload
// (builtin row deleted), orphan sweep, table missing (class a, quiet builtin).
func TestRegistryDegradation_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Corrupt-row probe (class b): damped without factor fails DecodePolicy.
	// RED if Boot treats every error like class (a) — a blanket WARN/quiet
	// builtin path leaves Health()=="ok" and no ERROR record.
	t.Run("corrupt_row_boots_loud_and_degraded", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`UPDATE context_block_types SET config = '{"v":1,"retrieval":{"policy":"damped"}}'
			  WHERE name = 'audit-trail'`); err != nil {
			t.Fatalf("corrupt row: %v", err)
		}
		capture := installCapture(t)
		reg := blocktype.NewRegistry()
		reg.RetryInterval = 100 * time.Millisecond
		bctx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)
		reg.Boot(bctx, pool)

		if got := reg.Health(); got != blocktype.HealthBuiltinFallback {
			t.Fatalf("corrupt-row boot: Health() = %q, want builtin-fallback (silent WARN path?)", got)
		}
		if !capture.has(slog.LevelError, "boot reload failed", "") {
			t.Errorf("no ERROR record for the corrupt-row boot — degradation is silent: %+v", capture.records)
		}
		if got := dampingOf(t, reg.Snapshot()); got != 0.3 {
			t.Errorf("builtin fallback damping = %v, want 0.3", got)
		}

		// Retry heal WITHOUT restart (§4.3 b): fix the row, the 100ms retry
		// loop must clear the degraded state and load the DB value 0.4.
		if _, err := pool.Exec(ctx,
			`UPDATE context_block_types
			    SET config = '{"v":1,"retrieval":{"policy":"damped","damping_factor":0.4}}'::jsonb
			  WHERE name = 'audit-trail'`); err != nil {
			t.Fatalf("heal row: %v", err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for reg.Health() != blocktype.HealthOK && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if got := reg.Health(); got != blocktype.HealthOK {
			t.Fatalf("degraded state not cleared by retry loop within 10s: Health() = %q", got)
		}
		if got := dampingOf(t, reg.Snapshot()); got != 0.4 {
			t.Errorf("damping after heal = %v, want DB value 0.4 (retry did not load DB rows)", got)
		}
		// Restore the seed value for the following subtests.
		if _, err := pool.Exec(ctx,
			`UPDATE context_block_types
			    SET config = jsonb_set(config, '{retrieval,damping_factor}', '0.3')
			  WHERE name = 'audit-trail'`); err != nil {
			t.Fatalf("restore row: %v", err)
		}
	})

	// Merge-reload probe (§4.1 R1): deleting the knowledge row past the Go
	// delete guard must NOT blank the default type. RED under replace
	// semantics: knowledge would vanish from the set → corpus blackout under
	// allowlist semantics (and NewSet would even reject the default-less set).
	t.Run("merge_survives_builtin_row_delete", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`DELETE FROM context_block_types WHERE name = 'knowledge'`); err != nil {
			t.Fatalf("delete knowledge row: %v", err)
		}
		capture := installCapture(t)
		reg := blocktype.NewRegistry()
		if err := reg.Reload(ctx, pool); err != nil {
			t.Fatalf("reload after builtin delete errored (replace semantics?): %v", err)
		}
		p, ok := reg.Snapshot().Resolve("knowledge")
		if !ok {
			t.Fatal("knowledge unresolvable after row delete — replace semantics, corpus blackout")
		}
		if p.Retrieval.Kind != blocktype.RetrievalFullPass || !p.IsDefault {
			t.Errorf("knowledge fallback policy = %+v, want builtin full-pass default", p)
		}
		if !hasString(reg.Snapshot().VisibleTypes(), "knowledge") {
			t.Error("knowledge missing from VisibleTypes after row delete")
		}
		if !capture.has(slog.LevelError, "builtin row missing", "knowledge") {
			t.Errorf("no ERROR 'builtin row missing' for knowledge — silent merge: %+v", capture.records)
		}
		// Restore the seed row (the 072 file is idempotent — re-exec re-seeds).
		sqlBytes, err := migrations.Section("072_block_type_registry.sql")
		if err != nil {
			t.Fatalf("read embedded migration: %v", err)
		}
		if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("re-seed: %v", err)
		}
	})

	// Orphan-sweep probe (§5.1 a): a block with an unregistered type_name
	// must WARN with the type name after a reload. The M035 CHECK constraint
	// is still live pre-073 — dropping it here SIMULATES the post-073 state
	// in which orphans become writable (documented test scaffold, not a
	// production path). RED without the sweep call in Reload.
	t.Run("orphan_sweep_warns_with_type_name", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`ALTER TABLE context_blocks DROP CONSTRAINT IF EXISTS context_blocks_block_role_check`); err != nil {
			t.Fatalf("drop check (post-073 simulation): %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope, type_name)
			 VALUES ('learnings', 't3-orphan-probe', 'c', 'private', 'wf-orphan-probe')`); err != nil {
			t.Fatalf("insert orphan block: %v", err)
		}
		capture := installCapture(t)
		reg := blocktype.NewRegistry()
		if err := reg.Reload(ctx, pool); err != nil {
			t.Fatalf("reload: %v", err)
		}
		if !capture.has(slog.LevelWarn, "orphan type", "wf-orphan-probe") {
			t.Errorf("no WARN naming the orphan type wf-orphan-probe: %+v", capture.records)
		}
	})

	// Class (a) probe: table missing (pre-072 boot / plain test DB) is the
	// QUIET builtin path — WARN, no ERROR, Health()=="ok". RED if the
	// error-class split is missing and 42P01 lands in the degraded path.
	t.Run("missing_table_boots_quiet_builtin", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `DROP TABLE context_block_types CASCADE`); err != nil {
			t.Fatalf("drop table: %v", err)
		}
		capture := installCapture(t)
		reg := blocktype.NewRegistry()
		bctx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)
		reg.Boot(bctx, pool)
		if got := reg.Health(); got != blocktype.HealthOK {
			t.Fatalf("pre-072 boot: Health() = %q, want ok (42P01 is class a, not b)", got)
		}
		if !capture.has(slog.LevelWarn, "registry table missing", "") {
			t.Errorf("no WARN for the missing table: %+v", capture.records)
		}
		if capture.has(slog.LevelError, "", "") {
			t.Errorf("pre-072 boot emitted ERROR records (class split broken): %+v", capture.records)
		}
		if got := dampingOf(t, reg.Snapshot()); got != 0.3 {
			t.Errorf("builtin damping = %v, want 0.3", got)
		}
	})
}

func hasString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
