//go:build integration

// The registry against a real database at migration 162: the load query
// reads every column in the order Reload scans it, a psql-edited broken
// policy is loaded but never offered, a psql UPDATE becomes visible with the
// next Reload (generation + 1), and the tenant / _global rules hold on rows
// the CHECKs admitted.
//
//	go test -tags=integration -p 1 -count=1 ./internal/modelalias/
package modelalias_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestRegistryOverMigration162(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO context_model_aliases (scope, name, kind, role, sensitivity, policy, description) VALUES
	        ('_global',   'chat',  'chat',      'chat',  'internal',    '{"max_n": 4, "reasoning": "off"}', 'operator chat'),
	        ('_global',   'embed', 'embedding', 'embed', 'credentials', '{"embedding.max_inputs": 64}',     ''),
	        ('acme:main', 'chat',  'chat',      'chat',  'credentials', '{}',                               ''),
	        ('acme:main', 'bad',   'chat',      'chat',  'credentials', '{"max_tokens": 1}',                '')`)

	reg := modelalias.NewRegistry(pool)
	if err := reg.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	s := reg.Snapshot()
	if s.Generation() != 1 || len(s.Aliases()) != 4 {
		t.Fatalf("after the first reload: generation %d, %d alias(es), want 1 and 4", s.Generation(), len(s.Aliases()))
	}
	chat, ok := modelalias.Lookup(s, "chat", "")
	if !ok || chat.Kind != modelalias.KindChat || chat.Role != "chat" || chat.Sensitivity != backends.SensInternal ||
		chat.Policy.MaxN != (modelalias.Opt[int]{V: 4, Set: true}) || chat.Policy.Reasoning.V != modelalias.ReasoningOff ||
		chat.Description != "operator chat" || !chat.Enabled || chat.CreatedAt.IsZero() || chat.ID == "" {
		t.Fatalf("Lookup(chat, \"\") = %+v %v", chat, ok)
	}
	if a, ok := modelalias.Lookup(s, "chat", "acme:main"); !ok || a.Scope != "acme:main" {
		t.Errorf("tenant acme: chat resolves to %+v %v, want the shadowing tenant alias", a, ok)
	}
	if _, ok := modelalias.Lookup(s, "bad", "acme:main"); ok {
		t.Errorf("a broken policy is offered")
	}
	var got []string
	for _, a := range modelalias.Offers(s, "acme:main") {
		got = append(got, a.Scope+"/"+a.Name)
	}
	if strings.Join(got, ",") != "acme:main/chat,_global/embed" {
		t.Errorf("Offers(acme) = %v, want acme:main/chat,_global/embed", got)
	}

	exec(`UPDATE context_model_aliases SET enabled = false WHERE scope = '_global' AND name = 'embed'`)
	if _, ok := modelalias.Lookup(reg.Snapshot(), "embed", ""); !ok {
		t.Errorf("the snapshot changed before the reload")
	}
	if err := reg.Reload(ctx); err != nil {
		t.Fatalf("second reload: %v", err)
	}
	if reg.Generation() != 2 {
		t.Errorf("generation %d after the second reload, want 2", reg.Generation())
	}
	if _, ok := modelalias.Lookup(reg.Snapshot(), "embed", ""); ok {
		t.Errorf("a disabled alias is offered after the reload")
	}
}
