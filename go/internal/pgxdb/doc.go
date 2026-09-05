// Package pgxdb — the leaf that owns the SHAPE of a database transaction:
// begin, rollback on the way out, commit. Once, for every caller in the tree.
//
// The package owns the SEQUENCE, never the WORDING. Error texts stay with the
// caller and travel in Stages, because the transaction openings across the
// tree carry a wide spread of texts — some asserted on by tests, some matched
// by operational greps. A generated text would rewrite all of them silently,
// which is exactly the kind of change this package exists to make unnecessary.
//
// What one place buys at 1M-10M blocks: a statement timeout per transaction,
// a SET LOCAL application_name for lock forensics, a retry on 40001
// (serialization failure), a duration metric. Without this package each of
// those is a diff at every opening in the tree; with it, a diff in one file.
//
// THE BOUNDARY: this package imports the standard library plus
// github.com/jackc/pgx/v5 and NOTHING ELSE — no github.com/GottZ/ctx/... and
// no other third-party module. That is what lets any package take the
// transaction bracket from here without dragging a dependency along. A helper
// that needs a ctx package does not belong here; it belongs next to the thing
// it needs.
package pgxdb
