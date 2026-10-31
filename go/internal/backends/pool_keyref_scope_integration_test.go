//go:build integration

// GHSA 2026-10-31 §1: Reload hands the resolver the ROW's scope, never a fixed
// _global — the resolver (settings.BackendSecretResolver) decides from it which
// secret scopes the row may read. §B adds the row's host: the opt-in fallback
// to _global is bound to the origin of an operator row, so the resolver needs
// the base_url of the row it resolves for. This pins the pool half of that
// contract with a capturing resolver; the resolver's own scope and host rule
// has its probe in internal/settings.
//
//	go test -tags=integration ./internal/backends/ -run TestReload_ResolverGetsRowScope -count=1 -v
package backends_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestReload_ResolverGetsRowScope(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	for _, row := range []struct{ name, scope string }{
		{"glob-row", backends.GlobalScope}, {"ta-row", "tenanta"}, {"tb-row", "tenantb"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_backends (name, base_url, scope, api_key_ref) VALUES ($1, $2, $3, 'shared-name')`,
			row.name, "https://"+row.name+".example.com/v1", row.scope); err != nil {
			t.Fatalf("insert %s: %v", row.name, err)
		}
	}

	got := map[string]string{} // api_key_ref@scope → host the resolver was asked for
	resolver := func(_ context.Context, name, scope, host string) (string, error) {
		got[name+"@"+scope] = host
		return "key-for-" + scope, nil
	}
	p := backends.NewPool(pool, resolver)
	if err := p.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	for want, host := range map[string]string{
		"shared-name@" + backends.GlobalScope: "https://glob-row.example.com/v1",
		"shared-name@tenanta":                 "https://ta-row.example.com/v1",
		"shared-name@tenantb":                 "https://tb-row.example.com/v1",
	} {
		gotHost, ok := got[want]
		if !ok {
			t.Errorf("resolver never asked for %s — the row scope did not reach it (calls: %v)", want, got)
			continue
		}
		if gotHost != host {
			t.Errorf("resolver for %s got host %q, want the row's base_url %q", want, gotHost, host)
		}
	}
	// And the snapshot carries each row's OWN answer, so the three same-named
	// refs resolve independently per scope.
	byName := map[string]string{}
	for _, b := range p.Snapshot() {
		byName[b.Name] = b.APIKey
	}
	for name, scope := range map[string]string{"glob-row": backends.GlobalScope, "ta-row": "tenanta", "tb-row": "tenantb"} {
		if byName[name] != "key-for-"+scope {
			t.Errorf("%s APIKey = %q, want the %s resolution", name, byName[name], scope)
		}
	}
}
