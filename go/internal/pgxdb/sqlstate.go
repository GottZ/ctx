package pgxdb

import (
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// hasCode reports whether err carries a *pgconn.PgError with exactly this
// SQLSTATE. It is the one place that knows how a driver error is unwrapped,
// so a call site spells the MEANING of a code and never its plumbing.
//
// It answers a single code on purpose. A site that discriminates BETWEEN
// several codes — a switch over pgErr.Code, or a condition that reads
// pgErr.ConstraintName after the match — needs the error value itself, not a
// bool; forcing it through a predicate would cost a second unwrap and say
// less. Those sites keep their own errors.As and are not duplicates of what
// lives here.
func hasCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// MalformedUUID reports the 22P02 (invalid_text_representation) that a
// `$1::uuid` cast raises when the parameter is not a UUID.
//
// It is a NOT-FOUND signal, not a server fault: the tree binds ids as text
// and casts them in the statement, so a caller-supplied garbage id reaches
// the database and comes back as 22P02. Answering it with a 500 would be
// wrong twice — the request was malformed, not the server — and it would turn
// the error class into an oracle (see AbsentOrMalformed).
func MalformedUUID(err error) bool {
	return hasCode(err, "22P02")
}

// AbsentOrMalformed reports BOTH ways an id can fail to name a row: the
// well-formed-but-absent one (pgx.ErrNoRows) and the malformed one (22P02).
//
// It is deliberately a SEPARATE name from MalformedUUID rather than a flag on
// it, because the difference between the two is a (weak) existence side
// channel: a caller that maps only ErrNoRows to 404 and lets 22P02 become a
// 500 tells an unauthorised prober which of its guesses were at least
// well-formed ids. Every lookup that answers "no such row" must collapse both
// into ONE answer. The store keeps that contract on its side of the boundary
// too — store/tenant.go documents it at GetTenant and store/blobs.go at
// UpdateBlobBlockRef, where the same reading produces (nil, nil).
func AbsentOrMalformed(err error) bool {
	return errors.Is(err, pgx.ErrNoRows) || MalformedUUID(err)
}

// UniqueViolation reports the 23505 a unique index or primary key raises on a
// duplicate. Callers translate it into their own sentinel — a name that
// already exists, an active migration that already runs — which is why this
// stays a bool and never maps to an error of its own: the meaning of a
// duplicate belongs to the table, not to the driver.
func UniqueViolation(err error) bool {
	return hasCode(err, "23505")
}
