//go:build integration

// Issue #372, part 3: the rule which configuration a tenant's scope resolves
// under in the background exists twice: in Go (Scheduler.backgroundTenantFor,
// the default tenant to _global, any other tenant to its first owned scope)
// and in SQL (store.ScopeOwnerConfigScopes, which the query path's backfill
// and its candidate annotation use to apply an owner's sensitivity floor).
// A store test pins the SQL side alone; this test holds the two against each
// other, for every scope of every tenant, so a change to one that the other
// does not follow fails here.
//
//	go test -tags=integration ./internal/events/ -run TestBackgroundTenantRuleIsTheScopeOwnerRule -count=1 -v
package events

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestBackgroundTenantRuleIsTheScopeOwnerRule(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	s := &Scheduler{pool: pool}

	// Scopes assigned out of order, with separators whose order depends on
	// the collation, so the first owned scope is neither the first assigned
	// nor obvious; the second tenant owns a single scope, the third none.
	for _, tc := range []struct {
		slug   string
		scopes []string
	}{
		{"own-rule-a", []string{"own-rule-a:zeta", "own-rule-a_beta", "own-rule-a:alpha", "own-rule-a-gamma"}},
		{"own-rule-b", []string{"own-rule-b:only"}},
		{"own-rule-c", nil},
	} {
		tn, err := store.CreateTenant(ctx, pool, tc.slug, tc.slug)
		if err != nil {
			t.Fatalf("create tenant %s: %v", tc.slug, err)
		}
		for _, sc := range tc.scopes {
			if _, err := store.AssignTenantScope(ctx, pool, tn.ID, sc, nil); err != nil {
				t.Fatalf("assign %s: %v", sc, err)
			}
		}
	}

	tenants, err := store.ListTenants(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	compared := 0
	for _, tn := range tenants {
		owned, err := store.TenantScopes(ctx, pool, tn.ID)
		if err != nil {
			t.Fatalf("tenant scopes of %s: %v", tn.ID, err)
		}
		bt, ok := s.backgroundTenantFor(ctx, tn)
		sql, err := store.ScopeOwnerConfigScopes(ctx, pool, owned)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			// No background resolution: a non-default tenant without scopes,
			// which has no scope for the SQL rule to map either.
			if len(owned) != 0 || len(sql) != 0 {
				t.Errorf("tenant %s: no background resolution, but owns %v (SQL rule %v)", tn.ID, owned, sql)
			}
			continue
		}
		for _, sc := range owned {
			compared++
			if sql[sc] != bt.scope {
				t.Errorf("tenant %s, scope %s: background resolves under %q, ScopeOwnerConfigScopes under %q", tn.ID, sc, bt.scope, sql[sc])
			}
		}
	}
	if compared < 6 {
		t.Fatalf("compared %d scopes, want the fixture's five plus the default tenant's: the test compares nothing", compared)
	}
}
