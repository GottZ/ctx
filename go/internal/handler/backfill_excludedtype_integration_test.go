//go:build integration

// Rot-Gate (Pfad A) für die Welle "Embed-Backfill respektiert die
// Block-Typ-Retrieval-Politik": backfillPending pickt HEUTE jeden Block mit
// `embedding IS NULL AND NOT is_archived` — auch solche, deren Typ in der
// Registry retrieval-Policy 'excluded' trägt (live: checkpoint, system-meta).
// Deren Vektor hat null Konsumenten: jeder gerankte Pfad filtert über die
// Visible-Type-Allowlist (rrf p_types_visible / blocktype.Set.VisibleTypes).
//
// Vorfall 2026-07-25: 33 Hermes-"Compaction source"-Parts (type=checkpoint,
// je ~36,5 kB) fluteten den CPU-Embed-Server über Stunden; der Digest-Rebuild
// schreibt topic-map-private (type=system-meta, 73,7 kB ≈ real ~32k Token)
// nach JEDEM Boot neu → ClearEmbedding → ~60 min CPU-Embed für einen toten
// Vektor, wiederkehrend.
//
// Der Test läuft durch die ECHTE Produktionsfunktion (h.backfillPending) mit
// SyncCap=0 (unbegrenzt, drainiert die ganze Queue in einem Aufruf), damit die
// Aussage "excluded-Typ wird nie gepickt" nicht von der Cap-Arithmetik lebt.
// Drei Proben in einem Lauf:
//
//	(1) Exclusion:  checkpoint + system-meta bleiben unembedded …
//	(2) Reihenfolge: … obwohl sie die ÄLTESTEN pending Blöcke sind
//	(3) Fallback:   ein Typ OHNE Registry-Zeile wird weiter gepickt
//	    (+ Positiv-Kontrolle knowledge und audit-trail/damped)
//
// Run: go test -tags=integration ./internal/handler/ -run TestBackfillPending_RetrievalExcludedType_Integration -count=1 -v
package handler

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestBackfillPending_RetrievalExcludedType_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Ältester zuerst: die beiden excluded-Typen liegen VOR den embedbaren
	// Blöcken, damit ein Pick, der sie überspringt, nicht mit "war halt
	// hinten" erklärbar ist.
	fixtures := []struct {
		title    string
		typeName string
		ageMin   int
		wantEmb  bool
	}{
		{"exclx-checkpoint-oldest", "checkpoint", 50, false},
		{"exclx-systemmeta-older", "system-meta", 40, false},
		{"exclx-knowledge-normal", "knowledge", 30, true},
		{"exclx-audittrail-damped", "audit-trail", 20, true},
		{"exclx-unregistered-type", "exclx-no-registry-row", 10, true},
	}

	for _, f := range fixtures {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope, type_name, type_source, created_at, updated_at)
			 VALUES ('learnings', $1, 'body of the excluded-type fixture block', 'private', $2, 'manual',
			         now() - make_interval(mins => $3), now())`,
			f.title, f.typeName, f.ageMin); err != nil {
			t.Fatalf("seed %s: %v", f.title, err)
		}
	}

	// Vorbedingung: checkpoint und system-meta tragen in der frischen
	// Migrationskette (M107 bzw. M072) wirklich retrieval=excluded — sonst
	// prüft der Test nichts.
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

	srv, hits := fakeEmbedServer(t)
	st := &countingStore{}
	st.cfg.Store(snapshotTestConfig())
	h := NewQueryHandler(pool, st, embedPool(srv.URL), nil, blocktype.NewRegistry(), snapshotTestAdmitter(t))

	cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
		SyncCap: 0, MaxTokens: 1_000_000, BackoffBase: time.Minute, BackoffCap: time.Hour,
	}}

	got := h.backfillPending(ctx, nil, "private", h.embedAdmission(), cfg)

	embedded := map[string]bool{}
	rows, err := pool.Query(ctx,
		`SELECT title, embedding IS NOT NULL FROM context_blocks WHERE title LIKE 'exclx-%'`)
	if err != nil {
		t.Fatalf("read fixture state: %v", err)
	}
	for rows.Next() {
		var title string
		var has bool
		if err := rows.Scan(&title, &has); err != nil {
			t.Fatalf("scan fixture state: %v", err)
		}
		embedded[title] = has
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate fixture state: %v", err)
	}

	var wantCount int
	for _, f := range fixtures {
		if f.wantEmb {
			wantCount++
		}
		if embedded[f.title] != f.wantEmb {
			verb := "was embedded"
			if f.wantEmb {
				verb = "was NOT embedded"
			}
			t.Errorf("block %q (type %q) %s — retrieval policy of its type must decide the pick",
				f.title, f.typeName, verb)
		}
	}

	if got != wantCount {
		t.Errorf("backfillPending = %d, want %d (only types whose retrieval policy is not 'excluded' may be picked)", got, wantCount)
	}
	if int(hits.Load()) != wantCount {
		t.Errorf("embed wire hits = %d, want %d (an excluded-type block must not cost a wire call)", hits.Load(), wantCount)
	}

	still := []string{}
	for title, has := range embedded {
		if !has {
			still = append(still, title)
		}
	}
	sort.Strings(still)
	t.Logf("backfilled=%d wire_hits=%d still_pending=%v", got, hits.Load(), still)

	// Tenant-Overlay-Arm (design/01 §5.4 / D6 "Overlay gewinnt", gepinnt von
	// rrf T12): hebt EIN Tenant-Scope checkpoint auf full-pass, hat der Vektor
	// dieses Typs wieder einen Leser — der Pick muss ihn ab da wieder sehen.
	// Diskriminierend, weil system-meta unangetastet bleibt und weiter
	// ausgeschlossen ist (der Filter liest die Policy, nicht eine Typ-Liste).
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_block_types (name, scope, builtin, is_default, config)
		 VALUES ('checkpoint', 'exclx-tenant', false, false,
		         '{"v":1,"retrieval":{"policy":"full-pass"}}'::jsonb)`); err != nil {
		t.Fatalf("insert tenant override: %v", err)
	}

	before := hits.Load()
	got2 := h.backfillPending(ctx, nil, "private", h.embedAdmission(), cfg)
	if got2 != 1 {
		t.Errorf("after tenant override: backfillPending = %d, want 1 (checkpoint is lifted, system-meta is not)", got2)
	}
	if delta := hits.Load() - before; delta != 1 {
		t.Errorf("after tenant override: wire hits delta = %d, want 1", delta)
	}
	var cpEmbedded, smEmbedded bool
	if err := pool.QueryRow(ctx,
		`SELECT embedding IS NOT NULL FROM context_blocks WHERE title = 'exclx-checkpoint-oldest'`).Scan(&cpEmbedded); err != nil {
		t.Fatalf("read checkpoint fixture: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT embedding IS NOT NULL FROM context_blocks WHERE title = 'exclx-systemmeta-older'`).Scan(&smEmbedded); err != nil {
		t.Fatalf("read system-meta fixture: %v", err)
	}
	if !cpEmbedded {
		t.Errorf("tenant override did not re-open the checkpoint block for the backfill — the overlay arm is dead")
	}
	if smEmbedded {
		t.Errorf("system-meta got embedded although no scope lifted it — the predicate is not policy-driven")
	}
}
