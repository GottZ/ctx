package pgxdb

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The three handles below plus Beginner (tx.go) are the FOUR shapes anything
// in this tree ever needs from a database handle. *pgxpool.Pool, *pgx.Conn,
// pgx.Tx and a pgxmock double satisfy them implicitly — no adapter, no
// registration, nothing to keep in sync.
//
// Each handle carries EXACTLY ONE method on purpose. A function that only
// reads rows says so in its own signature, and a caller that needs two of
// them writes the combination down where it is used:
//
//	type handle interface {
//		pgxdb.Querier
//		pgxdb.Rower
//	}
//
// Combinations do NOT belong here. There are fifteen of them over four atoms,
// each interesting to exactly one caller, and a package that collects them
// grows a name per caller instead of a shape per method. The atoms are
// bounded; their combinations are not.
//
// WHY ONE PLACE AT ALL: before this file the tree carried twelve local
// interfaces that re-declared these same three method signatures — every one
// of them a place to edit when the pgx surface moves under us, and every one
// a chance for two of them to drift into "almost the same". The method text
// now exists once.

// Execer runs a statement and reports what it touched. It is the write half
// of a handle: INSERT, UPDATE, DELETE, DDL.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Querier runs a statement that returns rows. The caller owns the returned
// pgx.Rows and must Close it.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Rower runs a statement that returns at most one row. A failure travels
// inside the returned pgx.Row and surfaces on Scan — pgx.ErrNoRows among it,
// which is why this method has no error of its own.
type Rower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
