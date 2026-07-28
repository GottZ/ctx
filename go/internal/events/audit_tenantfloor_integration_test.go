//go:build integration

// Wave H9 probe (f), wiring half (design 04 §4.5-c, §7 H9-f): the audit verdict
// floor pool.llm_audit_min_sensitivity is tenant-overridable, so the value that
// bites must come from the ITERATED TENANT's config generation — the one
// auditTenantScope already takes at its batch head — not from the process-wide
// base snapshot.
//
// Why this needs a container and cannot live in audit_clamp_test.go: the unit
// probe (TestAuditOneBlockUsesPassedConfigNotProcessWide) pins that
// auditOneBlock honours its cfg ARGUMENT. It says nothing about which
// generation the drain loop hands down, and that is exactly where a wrong
// read point would sit. Proving it needs a real pick set (PickAuditBlocks) and
// a real verdict write (ApplyAuditVerdict).
//
// RED arm: auditTenantScope reading s.cfg.Snapshot() instead of
// SnapshotForTenant(ctx, bt.scope) — the block lands on the _global 'internal'
// instead of the tenant's stricter 'personal', i.e. a tenant that believes it
// raised its own floor is silently served the global one and its corpus stays
// eligible for no-credentials backends.
//
// Run with:
//
//	GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/events/ -run TestAuditTenantFloor_Integration -count=1 -v
package events

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedDefaultBlock inserts one unclassified block (sensitivity_source stays at
// the 'default' the pick predicate keys on) and returns its id.
func seedDefaultBlock(t *testing.T, pool *pgxpool.Pool, scope, title string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope)
		 VALUES ('learnings', $1, 'ordinary prose, no structural signal', $2)
		 RETURNING id`, title, scope).Scan(&id); err != nil {
		t.Fatalf("seed block %s: %v", title, err)
	}
	return id
}

func TestAuditTenantFloor_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const tenantScope = "h9-tenant"

	// Base generation = the _global policy: the registry default floor.
	base := &config.Config{
		Pool:      config.PoolConfig{LLMAuditMinSensitivity: backends.SensInternal},
		Scheduler: config.SchedulerConfig{HomeScope: "private"},
	}
	// This tenant raised its own floor — the whole point of tenant:overridable.
	tenantCfg := &config.Config{
		Pool:      config.PoolConfig{LLMAuditMinSensitivity: backends.SensPersonal},
		Scheduler: config.SchedulerConfig{HomeScope: "private"},
	}

	cfgStore := config.NewStore(base)
	cfgStore.SetOverlay(func(_ context.Context, _ *config.Config, scope string) (*config.Config, error) {
		if scope == tenantScope {
			return tenantCfg, nil
		}
		return nil, nil
	})

	s := NewScheduler(pool, cfgStore, backends.NewPool(nil, nil), StartupConfig{})
	// The model says "nein" twice — unclamped that is the 'internal' verdict the
	// live corpus carries 475 times. Everything this probe asserts is about what
	// the POLICY does with that answer.
	s.classify = func(_ context.Context, _ *pgxpool.Pool, _ *backends.Pool,
		_, _, _, _ string, _ llm.Admission) (bool, error) {
		return false, nil
	}

	// HomeScope 'private' is not owned by this tenant, so effectiveHomeScope
	// falls back to the identity scope — the audit drains exactly this scope.
	bt := backgroundTenant{scope: tenantScope, owned: []string{tenantScope}}
	blockID := seedDefaultBlock(t, pool, tenantScope, "h9-tenant-floor")

	if abort := s.auditTenantScope(ctx, bt, false, 5); abort {
		st := s.SensitivityAuditStatus()
		t.Fatalf("audit aborted: %+v", st)
	}

	var sens, source string
	if err := pool.QueryRow(ctx,
		`SELECT sensitivity, sensitivity_source FROM context_blocks WHERE id = $1`, blockID).
		Scan(&sens, &source); err != nil {
		t.Fatalf("read block state: %v", err)
	}
	if source != "llm-audit" {
		t.Fatalf("sensitivity_source = %q, want llm-audit — the verdict never reached the row", source)
	}
	if sens != string(backends.SensPersonal) {
		t.Fatalf("sensitivity = %q, want personal — the tenant's own floor must win over the _global value %q",
			sens, base.Pool.LLMAuditMinSensitivity)
	}
}
