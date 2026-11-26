//go:build integration

package blocktype_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// TestTenantRowOfAnUntrustedBuiltin565_Integration (issue #565, part 2): a
// tenant row that serves a builtin untrusted type's name without the mark is
// served to its tenant as stored — and now says so with the same WARN the
// '_global' row gets, once per build of that tenant's set (after a Reload or
// the tenant's invalidation, on its next read), never per request. The
// comparison is against the COMPILED definition, not the '_global' row.
//
// The fixture is the realistic path to such a row: a tenant created its own
// type under the name while the server did not ship it yet (a database at 195,
// the store write PUT /api/types runs), then migration 196 seeded the builtin.
//
// RED on 184c0012: no WARN for the tenant row at all. Mutations: the call in
// buildTenantSet removed (red); the comparison taken against the base set
// instead of the compiled definitions (red at the last step).
func TestTenantRowOfAnUntrustedBuiltin565_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, 195)
	for _, row := range []struct {
		scope, name, config string
		isDefault           bool
	}{
		{"tenant-a", "session-digest", `{"v":1,"retrieval":{"policy":"full-pass"}}`, false},
		{"tenant-b", "session-digest", `{"v":1,"retrieval":{"policy":"damped","damping_factor":0.5,"untrusted":true}}`, false},
		// The tenant's knowledge row replaces the base default, so it carries
		// the default flag itself — without it the set has no default, the
		// build fails and the "no WARN for a trusted name" check proves nothing.
		{"tenant-c", "knowledge", `{"v":1,"retrieval":{"policy":"full-pass"}}`, true},
	} {
		if _, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{
			Name: row.name, Scope: row.scope, IsDefault: row.isDefault, Config: json.RawMessage(row.config),
		}, nil, ""); err != nil {
			t.Fatalf("tenant row %s/%s: %v", row.scope, row.name, err)
		}
	}
	sql, err := migrations.Section("196_session_digest_block_type.sql")
	if err != nil {
		t.Fatalf("read 196: %v", err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply 196: %v", err)
	}

	capture := installCapture(t)
	warns := func(scope string) int {
		capture.mu.Lock()
		defer capture.mu.Unlock()
		n := 0
		for _, r := range capture.records {
			if r.level == slog.LevelWarn && strings.Contains(r.msg, "without its untrusted mark") && r.attrs["scope"] == scope {
				n++
			}
		}
		return n
	}
	reg := blocktype.NewRegistry()
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if n := warns("_global"); n != 0 {
		t.Fatalf("WARN for the marked _global row: %d", n)
	}

	for i := 0; i < 3; i++ {
		if reg.SnapshotForTenant(ctx, "tenant-a").IsUntrusted("session-digest") {
			t.Fatal("tenant-a's row is not served as stored (it has no mark)")
		}
	}
	if n := warns("tenant-a"); n != 1 {
		t.Fatalf("WARN records for tenant-a's row after one build and three reads = %d, want 1", n)
	}
	if !reg.SnapshotForTenant(ctx, "tenant-b").IsUntrusted("session-digest") {
		t.Fatal("tenant-b's marked row is not untrusted")
	}
	if reg.SnapshotForTenant(ctx, "tenant-c") == reg.Snapshot() {
		t.Fatal("fixture premise: tenant-c's set did not build (the base is served)")
	}
	if n := warns("tenant-b") + warns("tenant-c"); n != 0 {
		t.Fatalf("WARN for a marked tenant row or a trusted builtin name: %d", n)
	}

	// A Reload rebuilds every tenant's set on its next read: one more WARN.
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatalf("second reload: %v", err)
	}
	_ = reg.SnapshotForTenant(ctx, "tenant-a")
	_ = reg.SnapshotForTenant(ctx, "tenant-a")
	if n := warns("tenant-a"); n != 2 {
		t.Fatalf("WARN records for tenant-a after a Reload = %d, want 2", n)
	}
	// The tenant's invalidation (its NOTIFY): one more.
	reg.InvalidateTenant("tenant-a")
	_ = reg.SnapshotForTenant(ctx, "tenant-a")
	if n := warns("tenant-a"); n != 3 {
		t.Fatalf("WARN records for tenant-a after its invalidation = %d, want 3", n)
	}

	// Measured against the compiled definition: a '_global' row that lost the
	// mark itself does not silence the tenant row.
	if _, err := pool.Exec(ctx, `UPDATE context_block_types
		  SET config = jsonb_set(config, '{retrieval,untrusted}', 'false'::jsonb)
		WHERE name = 'session-digest' AND scope = '_global'`); err != nil {
		t.Fatalf("drop the _global mark: %v", err)
	}
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatalf("third reload: %v", err)
	}
	_ = reg.SnapshotForTenant(ctx, "tenant-a")
	if n := warns("tenant-a"); n != 4 {
		t.Fatalf("WARN records for tenant-a with an unmarked _global row = %d, want 4", n)
	}
	if n := warns("_global"); n != 1 {
		t.Fatalf("WARN records for the unmarked _global row = %d, want 1", n)
	}

	// A tenant set whose build fails (a row that does not decode) is not
	// served — the base is, and the build is retried on every read. Its rows
	// must not be reported as served: no untrusted WARN, however often it is
	// read. (The _global row gets its mark back first, so the base answers
	// untrusted for the name.)
	if _, err := pool.Exec(ctx, `UPDATE context_block_types
		  SET config = jsonb_set(config, '{retrieval,untrusted}', 'true'::jsonb)
		WHERE name = 'session-digest' AND scope = '_global'`); err != nil {
		t.Fatalf("restore the _global mark: %v", err)
	}
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatalf("fourth reload: %v", err)
	}
	for _, row := range []struct{ name, config string }{
		{"session-digest", `{"v":1,"retrieval":{"policy":"full-pass"}}`},
		{"zz-broken", `{"v":2}`},
	} {
		if _, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{
			Name: row.name, Scope: "tenant-d", Config: json.RawMessage(row.config),
		}, nil, ""); err != nil {
			t.Fatalf("tenant-d row %s: %v", row.name, err)
		}
	}
	for i := 0; i < 3; i++ {
		if !reg.SnapshotForTenant(ctx, "tenant-d").IsUntrusted("session-digest") {
			t.Fatal("tenant-d: a failed build must serve the base set")
		}
	}
	if n := warns("tenant-d"); n != 0 {
		t.Fatalf("untrusted WARN records for tenant-d, whose set is never served = %d, want 0", n)
	}

	// The same for a build whose rows all decode but whose set is refused: a
	// knowledge row without the default flag leaves the merged set without a
	// default (NewSet fails), and the base is served.
	for _, row := range []struct{ name, config string }{
		{"session-digest", `{"v":1,"retrieval":{"policy":"full-pass"}}`},
		{"knowledge", `{"v":1,"retrieval":{"policy":"full-pass"}}`},
	} {
		if _, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{
			Name: row.name, Scope: "tenant-e", Config: json.RawMessage(row.config),
		}, nil, ""); err != nil {
			t.Fatalf("tenant-e row %s: %v", row.name, err)
		}
	}
	for i := 0; i < 3; i++ {
		if reg.SnapshotForTenant(ctx, "tenant-e") != reg.Snapshot() {
			t.Fatal("fixture premise: tenant-e's set built although it has no default")
		}
	}
	if n := warns("tenant-e"); n != 0 {
		t.Fatalf("untrusted WARN records for tenant-e, whose set is refused = %d, want 0", n)
	}
}
