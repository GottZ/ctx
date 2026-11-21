package pgxdb

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Issue #417: a caller that states what a failed write left behind needs to
// tell a failed COMMIT from a failure inside fn. FailedAtCommit recognises
// the commit failure of a labeled bracket and nothing else; the error text
// and the errors.Is chain stay the ones wrap produced.
func TestFailedAtCommitRecognisesOnlyALabeledCommit(t *testing.T) {
	t.Run("labeled commit failure", func(t *testing.T) {
		db := newFake()
		db.tx.commitErr = context.Canceled
		err := Write(context.Background(), db, At("store: upsert block"), func(pgx.Tx) error { return nil })
		if !FailedAtCommit(err) {
			t.Fatalf("FailedAtCommit(%v) = false, want true", err)
		}
		if got, want := err.Error(), "store: upsert block: commit: context canceled"; got != want {
			t.Errorf("text = %q, want %q", got, want)
		}
		if !errors.Is(err, context.Canceled) {
			t.Error("errors.Is no longer reaches the driver error")
		}
	})
	t.Run("failure inside fn", func(t *testing.T) {
		db := newFake()
		err := Write(context.Background(), db, At("store: upsert block"), func(pgx.Tx) error { return context.Canceled })
		if FailedAtCommit(err) {
			t.Errorf("FailedAtCommit(%v) = true for a failure inside fn", err)
		}
	})
	t.Run("begin failure", func(t *testing.T) {
		db := newFake()
		db.beginErr = context.Canceled
		err := Write(context.Background(), db, At("store: upsert block"), func(pgx.Tx) error { return nil })
		if FailedAtCommit(err) {
			t.Errorf("FailedAtCommit(%v) = true for a failed BeginTx", err)
		}
	})
	t.Run("unlabeled commit keeps the driver error's identity", func(t *testing.T) {
		db := newFake()
		db.tx.commitErr = errBoom
		err := Write(context.Background(), db, Stages{}, func(pgx.Tx) error { return nil })
		if err != errBoom { //nolint:errorlint // identity is the promise of an empty label
			t.Fatalf("err = %v (%T), want exactly errBoom", err, err)
		}
		if FailedAtCommit(err) {
			t.Error("an unlabeled commit failure cannot be recognised and must not claim to be")
		}
	})
}
