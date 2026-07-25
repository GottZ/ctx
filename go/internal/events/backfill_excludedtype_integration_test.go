//go:build integration

// Rot-Gate (Pfad B) für die Welle "Embed-Backfill respektiert die
// Block-Typ-Retrieval-Politik" — das Zwillingsstück zu
// handler/backfill_excludedtype_integration_test.go (Pfad A). Der
// Scheduler-Arm backfillOneEmbedding pickt HEUTE oldest-first jeden Block mit
// `embedding IS NULL AND NOT is_archived`, also auch Typen mit
// retrieval-Policy 'excluded' (live: checkpoint, system-meta) — deren Vektor
// wird geschrieben und nie gelesen (jeder gerankte Pfad filtert über die
// Visible-Type-Allowlist).
//
// Der Arm embeddet EINEN Block pro Zyklus, deshalb ist der Test eine
// Zyklen-Kette: die beiden excluded-Blöcke sind die ÄLTESTEN, ein Pick, der
// sie überspringt, kann also nicht "Reihenfolge" heißen.
//
//	Zyklus 1 → knowledge-Block (excluded-Typen übersprungen)
//	Zyklus 2 → Block mit Typ OHNE Registry-Zeile (Fallback bleibt embedbar)
//	Zyklus 3 → false (nichts mehr pickbar), beide excluded-Blöcke NULL
//
// Run: go test -tags=integration ./internal/events/ -run TestBackfillExcludedType_Integration -count=1 -v
package events

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestBackfillExcludedType_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	s := NewScheduler(pool, config.NewStore(&config.Config{}), backends.NewPool(nil, nil), StartupConfig{})

	fixtures := []struct {
		title    string
		typeName string
		ageMin   int
		wantEmb  bool
	}{
		{"exclb-checkpoint-oldest", "checkpoint", 50, false},
		{"exclb-systemmeta-older", "system-meta", 40, false},
		{"exclb-knowledge-normal", "knowledge", 30, true},
		{"exclb-unregistered-type", "exclb-no-registry-row", 20, true},
	}
	for _, f := range fixtures {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope, type_name, type_source, created_at, updated_at)
			 VALUES ('learnings', $1, 'body of the excluded-type fixture block', 'shared', $2, 'manual',
			         now() - make_interval(mins => $3), now())`,
			f.title, f.typeName, f.ageMin); err != nil {
			t.Fatalf("seed %s: %v", f.title, err)
		}
	}
	defer clearBlocks(t, pool)

	for _, name := range []string{"checkpoint", "system-meta"} {
		var policy string
		if err := pool.QueryRow(ctx,
			`SELECT config->'retrieval'->>'policy' FROM context_block_types
			  WHERE name = $1 AND scope = '_global'`, name).Scan(&policy); err != nil {
			t.Fatalf("precondition: read %s policy: %v", name, err)
		}
		if policy != "excluded" {
			t.Fatalf("precondition broken: _global type %q has retrieval policy %q, want %q", name, policy, "excluded")
		}
	}

	srv := headOfLineEmbedServer(t)
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{embedPoolRow("embed-a", srv.URL, 100)})
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	router := backfillRouter(bpool, d)
	cfg := headOfLineCfg()

	embeddedTitle := func(cycle int) string {
		t.Helper()
		var title string
		err := pool.QueryRow(ctx,
			`SELECT title FROM context_blocks
			  WHERE embedding IS NOT NULL AND title LIKE 'exclb-%'
			  ORDER BY updated_at DESC, id DESC LIMIT 1`).Scan(&title)
		if err != nil {
			t.Fatalf("cycle %d: read newest embedded fixture: %v", cycle, err)
		}
		return title
	}

	// Zyklus 1: der älteste PICKBARE Block ist der knowledge-Block — die
	// beiden älteren excluded-Typen dürfen den Arm nicht beschäftigen.
	ok, err := s.backfillOneEmbedding(ctx, router, cfg)
	if err != nil {
		t.Fatalf("cycle 1: unexpected error %v", err)
	}
	if !ok {
		t.Fatalf("cycle 1: backfilled=false, want true (the knowledge fixture must be picked)")
	}
	if got := embeddedTitle(1); got != "exclb-knowledge-normal" {
		t.Errorf("cycle 1 embedded %q, want %q (excluded-type blocks must be skipped even though they are older)",
			got, "exclb-knowledge-normal")
	}

	// Zyklus 2: der Typ ohne Registry-Zeile bleibt embedbar (Fallback).
	ok, err = s.backfillOneEmbedding(ctx, router, cfg)
	if err != nil {
		t.Fatalf("cycle 2: unexpected error %v", err)
	}
	if !ok {
		t.Fatalf("cycle 2: backfilled=false, want true (a type without a registry row stays embeddable)")
	}
	if got := embeddedTitle(2); got != "exclb-unregistered-type" {
		t.Errorf("cycle 2 embedded %q, want %q", got, "exclb-unregistered-type")
	}

	// Zyklus 3: nichts mehr pickbar — die beiden excluded-Blöcke sind für
	// den Arm unsichtbar, nicht bloß hinten angestellt.
	ok, err = s.backfillOneEmbedding(ctx, router, cfg)
	if err != nil {
		t.Fatalf("cycle 3: unexpected error %v", err)
	}
	if ok {
		t.Errorf("cycle 3: backfilled=true, want false (only excluded-type blocks are left — they must be invisible to the pick)")
	}

	for _, f := range fixtures {
		var has bool
		if err := pool.QueryRow(ctx,
			`SELECT embedding IS NOT NULL FROM context_blocks WHERE title = $1`, f.title).Scan(&has); err != nil {
			t.Fatalf("read %s: %v", f.title, err)
		}
		if has != f.wantEmb {
			t.Errorf("block %q (type %q): embedded=%v, want %v", f.title, f.typeName, has, f.wantEmb)
		}
	}
	if got := pendingCount(t, pool); got != 2 {
		t.Errorf("pending = %d, want 2 (both excluded-type blocks stay NULL — parked, never dropped)", got)
	}
}
