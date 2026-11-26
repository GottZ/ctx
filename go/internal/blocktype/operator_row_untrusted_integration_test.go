//go:build integration

package blocktype_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// TestOperatorRowOfAnUntrustedBuiltin_Integration (#289 review): an operator's
// own _global row named like a builtin type that is marked untrusted, created
// before the migration that seeds the builtin, stays as it is (196 inserts
// with ON CONFLICT DO NOTHING), the registry serves it as stored — and says so
// with a WARN on every Reload while the row drops the untrusted mark. RED
// probes: 196 overwriting the row (builtin/config change); the WARN removed or
// narrowed to rows that keep the mark.
func TestOperatorRowOfAnUntrustedBuiltin_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, 195)
	const operatorConfig = `{"v":1,"retrieval":{"policy":"full-pass"}}`
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_block_types (name, scope, builtin, is_default, config)
		 VALUES ('session-digest', '_global', false, false, $1::jsonb)`, operatorConfig); err != nil {
		t.Fatalf("operator row: %v", err)
	}
	sql, err := migrations.Section("196_session_digest_block_type.sql")
	if err != nil {
		t.Fatalf("read 196: %v", err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply 196: %v", err)
	}

	var builtin bool
	var raw []byte
	if err := pool.QueryRow(ctx,
		`SELECT builtin, config FROM context_block_types WHERE name = 'session-digest' AND scope = '_global'`,
	).Scan(&builtin, &raw); err != nil {
		t.Fatalf("read row: %v", err)
	}
	var got, want map[string]any
	_ = json.Unmarshal(raw, &got)
	_ = json.Unmarshal([]byte(operatorConfig), &want)
	if builtin || !reflect.DeepEqual(got, want) {
		t.Fatalf("196 changed the operator row: builtin=%v config=%s", builtin, raw)
	}

	warns := func(c *logCapture) int {
		c.mu.Lock()
		defer c.mu.Unlock()
		n := 0
		for _, r := range c.records {
			if r.level == slog.LevelWarn && strings.Contains(r.msg, "without its untrusted mark") && r.attrs["name"] == "session-digest" {
				n++
			}
		}
		return n
	}
	capture := installCapture(t)
	reg := blocktype.NewRegistry()
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if n := warns(capture); n != 1 {
		t.Fatalf("WARN records for the operator row after one Reload = %d, want 1: %+v", n, capture.records)
	}
	if reg.Snapshot().IsUntrusted("session-digest") {
		t.Fatal("the operator row is not served as stored (untrusted)")
	}
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatalf("second reload: %v", err)
	}
	if n := warns(capture); n != 2 {
		t.Fatalf("WARN records after two Reloads = %d, want 2 (one per Reload and name)", n)
	}

	// The row sets the mark: no WARN.
	if _, err := pool.Exec(ctx,
		`UPDATE context_block_types SET config = '{"v":1,"retrieval":{"policy":"full-pass","untrusted":true}}'::jsonb
		  WHERE name = 'session-digest' AND scope = '_global'`); err != nil {
		t.Fatalf("set the mark: %v", err)
	}
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatalf("third reload: %v", err)
	}
	if n := warns(capture); n != 2 {
		t.Fatalf("WARN records after the row set the mark = %d, want still 2", n)
	}
	if !reg.Snapshot().IsUntrusted("session-digest") {
		t.Fatal("the marked operator row is not untrusted")
	}
}
