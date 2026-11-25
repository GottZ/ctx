package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// ErrWriteConflict: a write met concurrent writes of the rows it had to lock
// and wrote nothing — its transaction ran supersedesSourceAttempts times and
// each time the block it addressed had changed scope before its lock pass
// (issue #547). It is contention, not a fault: the same write sent again
// normally goes through. The handlers answer it, and a deadlock or
// serialization failure (IsWriteConflict), with the retryable 409
// write_conflict.
var ErrWriteConflict = errors.New("store: the write met concurrent changes of the rows it needed and wrote nothing")

// IsWriteConflict reports whether err is a write that lost to concurrent
// writes and wrote nothing: ErrWriteConflict, or a deadlock (40P01) or
// serialization failure (40001) PostgreSQL answered one of its statements
// with — the transaction it ran in is rolled back whole.
func IsWriteConflict(err error) bool {
	return errors.Is(err, ErrWriteConflict) || pgxdb.SerializationConflict(err)
}

// scopesToLockError asks writeSupersedesSourceScopes to run its transaction
// again with the supersedes locks of these scopes taken up front: the
// transaction met a block in a scope whose lock it does not hold — an update
// item's block moved out of the scope the batch read for it (batchUpdateMiss),
// or a parent's ancestor chain crosses into another scope (LinkBlockParent).
// Taking a further scope lock in the middle of the transaction would break
// the sorted order every multi-scope holder takes them in. It is a
// errSupersedesSourceChanged, so it counts against the same bound.
type scopesToLockError struct{ scopes []string }

func (e *scopesToLockError) Error() string {
	return fmt.Sprintf("%v (scopes %v)", errSupersedesSourceChanged, e.scopes)
}

func (e *scopesToLockError) Is(target error) bool { return target == errSupersedesSourceChanged }

// writeSupersedesSourceScopes is writeSupersedesSource for a transaction that
// takes the supersedes locks of several scopes in sorted order (a block
// batch): fn gets the scopes earlier runs asked for (scopesToLockError), to
// lock with its own in one sorted pass. A run that misses on a scope it
// already held learns nothing new; the bound is the same either way.
//
// Why the carried scopes make a second run enough against a block that keeps
// moving (issue #547): the scope read before the locks has no lock, so a mover
// between two scopes can take the block out of the scope just read before its
// lock is granted, each run anew. Holding the locks of both scopes, the batch
// keeps every mover of the block out — a scope move takes the lock of the
// scope it moves the block from (lockSupersedesSource) — and finds the block
// in one of them.
func writeSupersedesSourceScopes(ctx context.Context, db pgxdb.Beginner, s pgxdb.Stages, fn func(tx pgx.Tx, extra []string) error) error {
	var extra []string
	for attempt := 1; ; attempt++ {
		err := pgxdb.Write(ctx, db, s, func(tx pgx.Tx) error { return fn(tx, extra) })
		if !errors.Is(err, errSupersedesSourceChanged) {
			return err
		}
		if attempt >= supersedesSourceAttempts {
			slog.Info("store: write gave up, its block kept changing scope before its lock; nothing written",
				"runs", attempt, "error", err)
			return fmt.Errorf("%w (%d runs): %w", ErrWriteConflict, attempt, err)
		}
		var more *scopesToLockError
		if errors.As(err, &more) {
			for _, sc := range more.scopes {
				if !slices.Contains(extra, sc) {
					extra = append(extra, sc)
				}
			}
		}
	}
}
