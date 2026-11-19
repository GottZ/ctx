//go:build integration

// The archived-claim gate of an identity-changing UpdateBlock against a
// derivative that becomes archived while the update runs.
//
// A live derivative holds its identity in the unique index, so a rename onto it
// waits for whoever is changing that row. When the row's change is the archive
// — or the derivative was created and archived in the same transaction — the
// index entry the update waited for is gone once that transaction commits, and
// the update goes on to write. A probe evaluated in the update's own statement
// reads the snapshot the statement started with, in which the derivative was
// still live: it would let the rename through and leave an ordinary block on
// the identity of an archived derivative. The probe runs after the statement,
// in the same transaction, and sees the commit the statement waited for.
//
// The interleaving is forced (the #281 pattern): writer A's archive stays
// uncommitted until the update waits for A's transaction id.
//
//	go test -tags=integration ./internal/store/ -run 'TestUpdateIdentityArchivedClaimRace' -count=1 -v
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestUpdateIdentityArchivedClaimRace(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const category, scope, other = "learnings", "u386race", "u386race-other"
	scopes := []string{scope, other}
	strptr := func(s string) *string { return &s }

	// race opens writer A's transaction, lets prepare write into it, runs the
	// update of id with data, and commits (or, with rollback, aborts) A once
	// the update waits for A's transaction id. It returns the update's error.
	race := func(t *testing.T, prepare func(tx pgx.Tx), id string, data store.UpdateBlockData, rollback bool) error {
		t.Helper()
		txA, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = txA.Rollback(ctx) }()
		prepare(txA)
		var xidA string
		if err := txA.QueryRow(ctx, `SELECT pg_current_xact_id()::text`).Scan(&xidA); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, _, err := store.UpdateBlock(ctx, pool, id, data, scopes)
			done <- err
		}()
		ended := func() bool { return len(done) > 0 }
		if !r5Waits(t, pool, xidA, ended) {
			t.Fatalf("the update did not wait for writer A (ended=%v): the interleaving was not reached", ended())
		}
		if rollback {
			err = txA.Rollback(ctx)
		} else {
			err = txA.Commit(ctx)
		}
		if err != nil {
			t.Fatalf("writer A end: %v", err)
		}
		return <-done
	}
	archive := func(id string) func(tx pgx.Tx) {
		return func(tx pgx.Tx) {
			if _, err := tx.Exec(ctx, `UPDATE context_blocks SET is_archived = true WHERE id = $1::uuid`, id); err != nil {
				t.Fatalf("writer A archive: %v", err)
			}
		}
	}
	unchanged := func(t *testing.T, err error, id string, want u386Row) {
		t.Helper()
		if !errors.Is(err, store.ErrProvenanceProtected) {
			t.Fatalf("update = %v, want ErrProvenanceProtected", err)
		}
		if got := u386Read(t, pool, id); got != want {
			t.Errorf("refused update changed the block: %+v, want %+v", got, want)
		}
	}

	t.Run("rename while the derivative is archived", func(t *testing.T) {
		const title = "u386race archived"
		d := u386Derivative(t, pool, category, title, scope, "arm-a", false)
		c := u386Client(t, pool, category, title+" (client)", scope)
		before := u386Read(t, pool, c.ID)
		err := race(t, archive(d.ID), c.ID, store.UpdateBlockData{Title: strptr(title)}, false)
		unchanged(t, err, c.ID, before)
	})

	t.Run("scope move while the derivative is archived", func(t *testing.T) {
		const title = "u386race moved"
		d := u386Derivative(t, pool, category, title, other, "arm-a", false)
		c := u386Client(t, pool, category, title, scope)
		before := u386Read(t, pool, c.ID)
		err := race(t, archive(d.ID), c.ID, store.UpdateBlockData{Scope: strptr(other)}, false)
		unchanged(t, err, c.ID, before)
	})

	t.Run("rename while the derivative is created and archived", func(t *testing.T) {
		const title = "u386race created"
		c := u386Client(t, pool, category, title+" (client)", scope)
		before := u386Read(t, pool, c.ID)
		err := race(t, func(tx pgx.Tx) {
			d, err := store.UpsertDerivedTxForTest(ctx, tx, category, title, "derivative of arm-a",
				dsProvenanceOf("arm-a", derived.StratumDerived), scope, u386Badge, derived.TypeCatalog)
			if err != nil {
				t.Fatalf("writer A create: %v", err)
			}
			archive(d.ID)(tx)
		}, c.ID, store.UpdateBlockData{Title: strptr(title)}, false)
		unchanged(t, err, c.ID, before)
	})

	t.Run("an archive that rolls back leaves the identity collision", func(t *testing.T) {
		// Control: the derivative stays live, so the rename meets it in the
		// unique index (#227), not the archived claim.
		const title = "u386race rolled back"
		d := u386Derivative(t, pool, category, title, scope, "arm-a", false)
		c := u386Client(t, pool, category, title+" (client)", scope)
		err := race(t, archive(d.ID), c.ID, store.UpdateBlockData{Title: strptr(title)}, true)
		var taken *store.BlockIdentityTakenError
		if !errors.As(err, &taken) {
			t.Fatalf("update = %v, want *BlockIdentityTakenError", err)
		}
	})
}
