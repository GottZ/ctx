//go:build integration

package recall_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/recall"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestRecallRunOfADeletedTenantWritesNoRow (issue #555, review round): a
// recall run plans a stratum over a tenant's scope, the tenant is deleted
// before the run writes the stratum's row, and the row is not written — it
// holds the gate of the tenant that owned the scope when the run planned
// (StratumPlan.Tenant). Before, it stood in the deleted tenant's scope name,
// and the next owner's status listed it.
func TestRecallRunOfADeletedTenantWritesNoRow(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	tn, err := store.CreateTenant(ctx, pool, "rcrun", "rcrun")
	if err != nil {
		t.Fatal(err)
	}
	const scope = "rcrun:main"
	if _, err := store.AssignTenantScope(ctx, pool, tn.ID, scope, nil); err != nil {
		t.Fatal(err)
	}
	seedScope(t, pool, scope, 3, 900)
	cfg := recall.Config{KList: "10", QueriesPerStratum: 5, StrataBounds: "4096,65536", EfSearch: 10, LegTimeoutMS: 60000}
	deleted := false
	res, err := recall.RunOnceWithHooks(ctx, pool, cfg, blocktype.NewRegistry(), recall.Hooks{
		BeforeProbe: func(ctx context.Context) error {
			if !deleted {
				deleted = true
				if err := store.PruneTenant(ctx, pool, tn.ID, store.AuditStamp{}); err != nil {
					t.Fatalf("tenant-delete: %v", err)
				}
			}
			return errors.New("demand deferred") // every row from here on is written unmeasured
		},
	})
	if err != nil {
		t.Fatalf("recall run: %v", err)
	}
	if !deleted {
		t.Fatal("the run measured nothing — the fixture never reached a probe")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM context_recall_runs WHERE scope = $1`, scope).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d recall rows in the deleted tenant's scope %s (%v), want 0 (run rows %d)", n, scope, err, len(res.Rows))
	}
}
