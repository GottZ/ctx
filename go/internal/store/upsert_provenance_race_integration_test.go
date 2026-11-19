//go:build integration

// The provenance gate of UpsertBlock (S3 / I7, design D-01 §4.3.1) against a
// derivative whose first create commits while another write of the same
// (category, title, scope) is under way.
//
// The gate's contract: a write without the derived layer's badge never
// replaces a block that carries a provenance, and a badged write replaces only
// a derivative of its own (arm, stratum). The decision used to come from the
// upsert's pre-read. A row that read cannot see — another transaction's first
// create, not yet committed — takes no lock and answers "no row"; the upsert's
// INSERT then waits for that transaction on the unique index and, once it
// commits, takes the conflict branch on a row the gate never looked at. The
// write replaced the derivative's content and, with the wholesale metadata
// replacement of a content change, its provenance — from then on the identity
// was an ordinary client-writable block. Issue #281 closed the same class for
// the vector invalidation; this file pins it for the gate.
//
// The interleaving is forced, not hoped for (the #281 pattern): writer A's
// first create of the derivative stays uncommitted while writer B runs; the
// test waits until B waits for A's transaction id, then commits A.
//
//	go test -tags=integration ./internal/store/ -run 'TestUpsertProvenanceGateFirstCreateRace|TestUpsertOntoNullMetadata' -count=1 -v
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestUpsertProvenanceGateFirstCreateRace(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)

	const category, scope = "learnings", "s3race"
	const derivedText = "derivative text, written by arm-a"
	badge := store.SensitivityWrite{Value: backends.SensInternal, Derived: true}
	armA := dsProvenanceOf("arm-a", derived.StratumDerived)

	// race holds writer A's first create of an arm-a derivative open, runs
	// writer B, commits A once B waits for it, and returns the state of A's
	// row afterwards — its content, the arm its provenance names ("" without
	// one), how many active rows hold the identity — and B's error.
	race := func(t *testing.T, title string, writerB func() error) (content, arm string, active int, errB error) {
		t.Helper()
		txA, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = txA.Rollback(ctx) }()
		a, err := store.UpsertDerivedTxForTest(ctx, txA, category, title, derivedText, armA, scope, badge, derived.TypeCatalog)
		if err != nil {
			t.Fatalf("writer A first create: %v", err)
		}
		var xidA string
		if err := txA.QueryRow(ctx, `SELECT pg_current_xact_id()::text`).Scan(&xidA); err != nil {
			t.Fatal(err)
		}

		done := make(chan error, 1)
		go func() { done <- writerB() }()
		ended := func() bool { return len(done) > 0 }
		if !r5Waits(t, pool, xidA, ended) {
			t.Fatalf("writer B did not wait for writer A's uncommitted first create (ended=%v): the interleaving was not reached", ended())
		}
		if err := txA.Commit(ctx); err != nil {
			t.Fatalf("writer A commit: %v", err)
		}
		errB = <-done

		if err := pool.QueryRow(ctx,
			`SELECT content, COALESCE(metadata->'`+derived.MetadataKey+`'->>'arm', '') FROM context_blocks WHERE id = $1::uuid`, a.ID,
		).Scan(&content, &arm); err != nil {
			t.Fatalf("read A's row: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_blocks WHERE category = $1 AND title = $2 AND scope = $3 AND NOT is_archived`,
			category, title, scope,
		).Scan(&active); err != nil {
			t.Fatalf("count rows: %v", err)
		}
		return content, arm, active, errB
	}

	// refused is the outcome of every write the gate turns away: the sentinel,
	// and A's derivative as A wrote it.
	refused := func(t *testing.T, errB error, content, arm string, active int) {
		t.Helper()
		if !errors.Is(errB, store.ErrProvenanceProtected) {
			t.Errorf("writer B = %v, want ErrProvenanceProtected", errB)
		}
		if content != derivedText || arm != "arm-a" {
			t.Errorf("the derivative after writer B: content=%q provenance arm=%q, want %q and arm-a", content, arm, derivedText)
		}
		if active != 1 {
			t.Errorf("%d active rows hold the identity, want 1", active)
		}
	}

	t.Run("a write without the badge is refused", func(t *testing.T) {
		const title = "s3race-unbadged"
		content, arm, active, errB := race(t, title, func() error {
			_, err := store.UpsertBlock(ctx, pool, category, title, "client text", nil, map[string]any{"note": "client"},
				scope, true, store.SensitivityWrite{}, "")
			return err
		})
		refused(t, errB, content, arm, active)
	})

	t.Run("a badged write without a provenance of its own is refused", func(t *testing.T) {
		const title = "s3race-badge-no-provenance"
		content, arm, active, errB := race(t, title, func() error {
			_, err := store.UpsertBlock(ctx, pool, category, title, "aborted arm run", nil, nil,
				scope, true, badge, derived.TypeCatalog)
			return err
		})
		refused(t, errB, content, arm, active)
	})

	t.Run("a badged write of another arm is refused", func(t *testing.T) {
		const title = "s3race-badge-foreign"
		content, arm, active, errB := race(t, title, func() error {
			_, err := store.UpsertBlock(ctx, pool, category, title, "arm-b text", nil,
				dsProvenanceOf("arm-b", derived.StratumDerived), scope, true, badge, derived.TypeInsight)
			return err
		})
		refused(t, errB, content, arm, active)
	})

	t.Run("a write declaring a supersedes is refused", func(t *testing.T) {
		const title = "s3race-supersedes"
		tgt, err := store.UpsertBlock(ctx, pool, category, "s3race-supersedes-target", "target text", nil, nil,
			scope, true, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("seed target: %v", err)
		}
		content, arm, active, errB := race(t, title, func() error {
			_, err := store.UpsertBlockSupersedes(ctx, pool, category, title, "client text", nil, nil,
				scope, true, store.SensitivityWrite{}, "",
				store.SupersedesDeclaration{TargetID: tgt.ID, APIKeyID: "s3race", Types: reg.Snapshot()})
			return err
		})
		refused(t, errB, content, arm, active)
	})

	t.Run("the owner's badged write lands", func(t *testing.T) {
		// The other role: a regeneration by the arm that owns the derivative is
		// admitted on the row it finds, exactly as on the sequential path.
		const title = "s3race-badge-owner"
		const regenerated = "regenerated by arm-a"
		content, arm, active, errB := race(t, title, func() error {
			_, err := store.UpsertBlock(ctx, pool, category, title, regenerated, nil, armA,
				scope, true, badge, derived.TypeCatalog)
			return err
		})
		if errB != nil {
			t.Fatalf("owner's write = %v, want it admitted", errB)
		}
		if content != regenerated || arm != "arm-a" || active != 1 {
			t.Errorf("after the owner's write: content=%q arm=%q active=%d, want %q, arm-a, 1", content, arm, active, regenerated)
		}
	})
}

// TestUpsertOntoNullMetadata: metadata is nullable (DEFAULT '{}'), and a NULL
// holds no provenance key — the reading notDerivedSQL gives the by-id writers.
// An upsert onto such a row is an ordinary write of an ordinary block.
func TestUpsertOntoNullMetadata(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const category, title, scope = "learnings", "null-metadata", "s3null"
	b, err := store.UpsertBlock(ctx, pool, category, title, "first text", nil, nil, scope, true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// No production writer stores NULL; the column admits it, so the state is
	// set directly.
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET metadata = NULL WHERE id = $1::uuid`, b.ID); err != nil {
		t.Fatalf("null the metadata: %v", err)
	}
	if _, err := store.UpsertBlock(ctx, pool, category, title, "second text", nil, nil, scope, true, store.SensitivityWrite{}, ""); err != nil {
		t.Fatalf("upsert onto a row with NULL metadata = %v, want it admitted", err)
	}
	var content string
	if err := pool.QueryRow(ctx, `SELECT content FROM context_blocks WHERE id = $1::uuid`, b.ID).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if content != "second text" {
		t.Fatalf("content = %q, want the second write's", content)
	}
}
