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
	reg := bootModelAliases(ctx, pool)
	if _, ok := modelalias.Lookup(reg.Snapshot(), "chat", ""); reg.Generation() != 1 || !ok {
		t.Errorf("boot load: generation %d, chat offered %v — want 1 and true", reg.Generation(), ok)
	}
	dead := bootModelAliases(ctx, refusingQuerier{})
	if dead == nil || dead.Generation() != 0 || len(modelalias.Offers(dead.Snapshot(), "")) != 0 {
		t.Errorf("refused load: %v — want an empty registry at generation 0", dead)
	}
}
