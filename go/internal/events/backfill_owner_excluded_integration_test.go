//go:build integration

// Issue #213, the background arm's side: a block of a type that only tenant
// A's registry knows, excluded there, was embedded by the arm bound to A and
// by the unbound pass. Since migration 176 it is marked and the arm leaves
// it: bound to A it embeds A's knowledge block and then finds nothing; bound
// to B it embeds B's block; unbound it embeds the two knowledge blocks.
//
//	go test -tags=integration ./internal/events/ -run TestBackfillOwnerExcludedType_Integration -count=1 -v
package events

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestBackfillOwnerExcludedType_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	for _, s := range []string{"oxs-a", "oxs-b"} {
		tn, err := store.CreateTenant(ctx, pool, s, s)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.AssignTenantScope(ctx, pool, tn.ID, s, nil); err != nil {
			t.Fatal(err)
		}
	}
	cfgJSON := json.RawMessage(`{"retrieval":{"policy":"excluded"}}`)
	if _, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{Name: "oxs-x", Scope: "oxs-a", DisplayName: "oxs-x", Config: cfgJSON}, nil, "oxs-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileEmbedExcluded(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, b := range []struct{ scope, title, typ string }{
		{"oxs-a", "oxs a x 1", "oxs-x"},
		{"oxs-a", "oxs a x 2", "oxs-x"},
		{"oxs-a", "oxs a knowledge", "knowledge"},
		{"oxs-b", "oxs b knowledge", "knowledge"},
	} {
		if _, err := store.UpsertBlock(ctx, pool, "oxs-cat", b.title, "body of "+b.title, nil, nil, b.scope, true, store.SensitivityWrite{}, b.typ); err != nil {
			t.Fatalf("upsert %s: %v", b.title, err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	srv := headOfLineEmbedServer(t)
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{embedPoolRow("embed-a", srv.URL, 100)})
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	s := NewScheduler(pool, config.NewStore(&config.Config{}), bpool, StartupConfig{})
	router := backfillRouter(bpool, d)
	cfg := headOfLineCfg()

	embedded := func() string {
		rows, err := pool.Query(ctx, `SELECT title FROM context_blocks WHERE category = 'oxs-cat' AND embedding IS NOT NULL ORDER BY title`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var title string
			if err := rows.Scan(&title); err != nil {
				t.Fatal(err)
			}
			out = append(out, title)
		}
		return strings.Join(out, "|")
	}
	for _, pass := range []struct {
		name   string
		owned  []string
		cycles int
		want   string
	}{
		{"bound to A", []string{"oxs-a"}, 1, "oxs a knowledge"},
		{"bound to B", []string{"oxs-b"}, 1, "oxs b knowledge"},
		{"unbound", nil, 2, "oxs a knowledge|oxs b knowledge"},
	} {
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET embedding = NULL, embed_model = NULL WHERE category = 'oxs-cat'`); err != nil {
			t.Fatal(err)
		}
		cycles := 0
		for ; cycles < 10; cycles++ {
			ok, err := s.backfillOneEmbedding(ctx, router, cfg, pass.owned)
			if err != nil {
				t.Fatalf("%s, cycle %d: %v", pass.name, cycles, err)
			}
			if !ok {
				break
			}
		}
		if got := embedded(); cycles != pass.cycles || got != pass.want {
			t.Errorf("%s: %d embed cycles, embedded %q; want %d, %q (a block of A's excluded own type takes no cycle)",
				pass.name, cycles, got, pass.cycles, pass.want)
		}
	}
}
