package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// The generic commit-less exit of this package is pgxdb.ErrRollback, not a
// sentinel of its own (DECISIONS.md K37): a body returns it, pgxdb.Write lets
// its deferred rollback run and hands the sentinel back UNCHANGED, and one
// errors.Is at the call site turns it into whatever that caller's "nothing to
// do" is — the (false, nil) a miss returned before the closure form existed.
//
// errTxCommitted below is the OTHER half, and it stays package-private on
// purpose: it says something pgxdb cannot say, namely that the branch already
// sent its own COMMIT.

// errTxCommitted ends the body AFTER the branch committed itself. Those
// branches carry a commit wording of their own — the idempotent create exits
// and the two token-revoke exits — and one pgxdb.Stages pair cannot hold two
// commit texts, so the branch sends its own COMMIT with its own error text and
// raises this to stop the helper from committing a second time. The helper's
// deferred rollback then runs on a committed transaction: the same no-op the
// deferred rollback in these functions always was.
//
// Every raise site is guarded by TestTxSentinelsAreTranslated (tx_test.go):
// it must have a matching errors.Is translation in the same function, so
// deleting a translation is a red test rather than a sentinel leaking to an
// API caller. The guard covers pgxdb.ErrRollback the same way.
var errTxCommitted = errors.New("store: transaction body stopped, already committed")

// commitThenStop is the bare-commit exit: the branches that ended with
// `return …, tx.Commit(ctx)`, so a commit failure travels back UNWRAPPED while
// a successful commit is not an error at all. Used only by the sites that had
// exactly that line.
func commitThenStop(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return errTxCommitted
}
