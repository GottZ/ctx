//go:build integration

// Wave W01-Seed gate 4 (design D-01 §4.3 + §7 W01-2 gate 4) against a real PG18
// testcontainer: the derived writer's PRIMARY path is an explicit typeName on
// UpsertBlock, which stamps type_source='manual' and takes the block out of the
// auto-classifier's reach for good (classify.go:56 carries `AND type_source =
// 'auto'`). The classify title patterns seeded by migration 143 are the NET for
// a writer that forgets the type — not the primary path — so the two are probed
// separately, and each is negatively probed.
//
// This file deliberately does NOT test the write LOCK (I7/S1-S3). That is wave
// W01-2a; between the registry row landing and that lock the type is
// client-claimable, which is why the migration stays undeployed until the lock
// lands with it.
//
//	go test -tags=integration ./internal/store/ -run TestDerivedManual -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// derivedTypeState reads the two columns the gate is about, straight out of the
// table — a Block-struct getter could paper over a write that never landed.
func derivedTypeState(t *testing.T, pool *pgxpool.Pool, id string) (typeName, typeSource string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT type_name, type_source FROM context_blocks WHERE id = $1`, id).Scan(&typeName, &typeSource); err != nil {
		t.Fatalf("read type state of %s: %v", id, err)
	}
	return typeName, typeSource
}

// derivedRegistrySet boots the registry off the migrated test DB — the same
// snapshot ClassifyBlockAfterUpsert gets in production.
func derivedRegistrySet(t *testing.T, pool *pgxpool.Pool) *blocktype.Set {
	t.Helper()
	reg := blocktype.NewRegistry()
	reg.Boot(context.Background(), pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	return reg.Snapshot()
}

func TestDerivedManualType_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	set := derivedRegistrySet(t, pool)

	t.Run("explicit_type_stamps_manual_and_classify_leaves_it", func(t *testing.T) {
		// A title that matches NO classify pattern of any type — so the only
		// thing that can put 'insight' on this row is the explicit argument.
		const title = "w01 seed explicit type probe"
		b, err := store.UpsertBlock(ctx, pool, "learnings", title, "body", nil, nil, "private", false,
			store.SensitivityWrite{}, derived.TypeInsight)
		if err != nil {
			t.Fatalf("upsert with explicit type: %v", err)
		}
		if name, source := derivedTypeState(t, pool, b.ID); name != derived.TypeInsight || source != "manual" {
			t.Fatalf("explicit typeName wrote type_name=%q type_source=%q, want insight/manual", name, source)
		}
		// Premise of the gate, asserted rather than assumed: the title really is
		// unclaimed, so the pattern net is not what is being measured here.
		if got, matched := set.Classify(title, nil); matched {
			t.Fatalf("the probe title classifies to %q — it must match no pattern, otherwise this "+
				"probe cannot tell the explicit path from the net", got)
		}
		// Second premise, and what makes this subtest red before the wave rather
		// than vacuously green: the title handed to the follow-up classify must
		// be one the registry DOES claim, so that declining to apply it is a
		// decision about type_source and not about an unknown pattern.
		const followUp = "Katalog #0123456789abcdef0123456789abcdef"
		if got, matched := set.Classify(followUp, nil); !matched || got != derived.TypeCatalog {
			t.Fatalf("the follow-up title classifies to (%q, %v), want (catalog, true) — without a "+
				"claimable title the classify below declines for the wrong reason", got, matched)
		}
		// The gate: a follow-up classify pass does not touch a manual row.
		applied, err := store.ClassifyBlockAfterUpsert(ctx, pool, set, b.ID, followUp, nil)
		if err != nil {
			t.Fatalf("follow-up classify: %v", err)
		}
		if applied != "" {
			t.Errorf("classify applied %q to a manual row — classify.go:56's `AND type_source = "+
				"'auto'` is the whole of I7's write-side protection today", applied)
		}
		if name, source := derivedTypeState(t, pool, b.ID); name != derived.TypeInsight || source != "manual" {
			t.Errorf("manual row moved to type_name=%q type_source=%q", name, source)
		}
	})

	t.Run("negative_probe_auto_row_is_reclassified", func(t *testing.T) {
		// Without the manual stamp the very same call DOES move the row. The
		// guarantee lives in the predicate, not in the classifier being shy.
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_blocks (category, title, content, scope)
			 VALUES ('learnings', 'w01 seed auto probe', 'body', 'private') RETURNING id`).Scan(&id); err != nil {
			t.Fatalf("seed auto row: %v", err)
		}
		if _, source := derivedTypeState(t, pool, id); source != "auto" {
			t.Fatalf("seeded row is not type_source='auto' — the negative probe has no subject")
		}
		applied, err := store.ClassifyBlockAfterUpsert(ctx, pool, set, id,
			"Katalog #0123456789abcdef0123456789abcdef", nil)
		if err != nil {
			t.Fatalf("classify auto row: %v", err)
		}
		if applied != derived.TypeCatalog {
			t.Fatalf("classify applied %q to an auto row, want catalog — the probe cannot show that "+
				"the manual predicate is what protects the row above", applied)
		}
		if name, source := derivedTypeState(t, pool, id); name != derived.TypeCatalog || source != "auto" {
			t.Errorf("auto row ended at type_name=%q type_source=%q, want catalog/auto", name, source)
		}
	})

	t.Run("net_catches_a_writer_that_forgot_the_type", func(t *testing.T) {
		// The net, end to end through the real registry: an anchor-titled block
		// written WITHOUT a type is classified onto the derived type — and away
		// from audit-trail, which is where it landed before this wave and which
		// would have sent it into guard AND dream.
		for title, want := range map[string]string{
			"Session insights 019d25d8b8aa7f028ad0e0bba7b7cfcf ab #1000": derived.TypeInsight,
			"Katalog #fedcba9876543210fedcba9876543210":                  derived.TypeCatalog,
		} {
			b, err := store.UpsertBlock(ctx, pool, "learnings", title, "body", nil, nil, "private", false,
				store.SensitivityWrite{}, "")
			if err != nil {
				t.Fatalf("upsert %q: %v", title, err)
			}
			if _, source := derivedTypeState(t, pool, b.ID); source != "auto" {
				t.Fatalf("a write without a type must stay type_source='auto'")
			}
			applied, err := store.ClassifyBlockAfterUpsert(ctx, pool, set, b.ID, title, nil)
			if err != nil {
				t.Fatalf("classify %q: %v", title, err)
			}
			if applied != want {
				t.Errorf("classify(%q) applied %q, want %q", title, applied, want)
			}
			if name, _ := derivedTypeState(t, pool, b.ID); name != want {
				t.Errorf("%q ended on type_name=%q, want %q", title, name, want)
			}
		}
	})
}
