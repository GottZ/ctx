//go:build integration

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/testdb"
)

type refusingQuerier struct{}

func (refusingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("connection refused")
}

// TestBootModelAliases: the boot loads the table once (generation 1, the row
// offered); a database that refuses the load leaves an empty registry
// (generation 0, nothing offered) instead of failing the boot.
func TestBootModelAliases(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	if _, err := pool.Exec(ctx, `INSERT INTO context_model_aliases (name, kind, role) VALUES ('chat', 'chat', 'chat')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	reg := bootModelAliases(ctx, pool, nil)
	if _, ok := modelalias.Lookup(reg.Snapshot(), "chat", ""); reg.Generation() != 1 || !ok {
		t.Errorf("boot load: generation %d, chat offered %v — want 1 and true", reg.Generation(), ok)
	}
	dead := bootModelAliases(ctx, refusingQuerier{}, nil)
	if dead == nil || dead.Generation() != 0 || len(modelalias.Offers(dead.Snapshot(), "")) != 0 {
		t.Errorf("refused load: %v — want an empty registry at generation 0", dead)
	}
}

// TestBootModelAliasesClampsTenantAliases (W02-04): the boot installs the
// ceiling getter BEFORE the first load, so a tenant alias stored above
// gateway.tenant_alias_ceiling (a psql edit) is served clamped from the very
// first snapshot; a _global alias is not capped by the tenant ceiling.
func TestBootModelAliasesClampsTenantAliases(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	if _, err := pool.Exec(ctx, `INSERT INTO context_model_aliases (scope, name, kind, role, policy) VALUES
	        ('_global', 'chat', 'chat', 'chat', '{"max_n": 4}'),
	        ('acme',    'chat', 'chat', 'chat', '{"max_n": 4, "timeout_s": 3600}')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ceil := func() modelalias.Ceilings {
		return modelalias.Ceilings{Tenant: modelalias.TenantCeiling{MaxN: 1, TimeoutS: 600}}
	}
	reg := bootModelAliases(ctx, pool, ceil)
	ten, ok := modelalias.Lookup(reg.Snapshot(), "chat", "acme")
	if !ok || ten.Policy.MaxN.V != 1 || ten.Policy.TimeoutS.V != 600 {
		t.Errorf("tenant alias at boot = %+v %v, want max_n 1 and timeout_s 600 (clamped)", ten.Policy, ok)
	}
	glob, ok := modelalias.Lookup(reg.Snapshot(), "chat", "")
	if !ok || glob.Policy.MaxN.V != 4 {
		t.Errorf("_global alias at boot = %+v %v, want max_n 4 (no tenant ceiling)", glob.Policy, ok)
	}
}
