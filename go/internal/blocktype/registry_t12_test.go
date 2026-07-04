package blocktype_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
)

// TestT12_FailSafeFastPaths pins the config.Store-twin fail-safe branches of
// snapshotForScope WITHOUT a DB (short, always-run): a registry that never
// booted (pool nil) — or a reserved / empty scope — resolves to the base
// generation BYTE-FOR-BYTE (same *Set pointer), so the multi-tenant machinery
// is inert until a real tenant scope is asked for. This is the default-tenant
// equivalence floor: the pre-T12 single-tenant server keeps the exact base set.
func TestT12_FailSafeFastPaths(t *testing.T) {
	reg := blocktype.NewRegistry() // no Boot ⇒ pool nil ⇒ overlay inert
	ctx := context.Background()
	base := reg.Snapshot()

	// Pre-Boot: any scope resolves to the base pointer (pool nil branch).
	for _, scope := range []string{"tenant-a", "private", "work"} {
		if got := reg.SnapshotForTenant(ctx, scope); got != base {
			t.Errorf("pre-Boot SnapshotForTenant(%q) != base pointer — overlay not inert without a pool", scope)
		}
	}

	// Reserved (_-prefixed) and empty scopes never earn a tenant generation,
	// even after a pool is present — the reserved-prefix / empty guards.
	for _, scope := range []string{"_global", "_anything", ""} {
		if got := reg.SnapshotForTenant(ctx, scope); got != base {
			t.Errorf("SnapshotForTenant(%q) != base pointer — reserved/empty guard missing", scope)
		}
	}

	// SnapshotForRequest with no hook installed ⇒ empty scope ⇒ base pointer.
	if got := reg.SnapshotForRequest(ctx); got != base {
		t.Errorf("SnapshotForRequest without a scope hook != base pointer")
	}

	// The request-scope hook seam resolves; a hook returning "" stays on base.
	blocktype.SetRequestScopeHook(func(context.Context) string { return "" })
	t.Cleanup(func() { blocktype.SetRequestScopeHook(nil) })
	if got := reg.SnapshotForRequest(ctx); got != base {
		t.Errorf("SnapshotForRequest with empty-scope hook != base pointer")
	}
}
