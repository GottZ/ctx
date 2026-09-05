package pgxdb

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// pgErr builds the driver error shape the predicates unwrap.
func pgErr(code string) error { return &pgconn.PgError{Code: code, Message: "test"} }

func TestSQLStatePredicates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		// want per predicate, in the order MalformedUUID,
		// AbsentOrMalformed, UniqueViolation.
		malformed, absentOrMalformed, unique bool
	}{
		{"nil", nil, false, false, false},
		{"22P02", pgErr("22P02"), true, true, false},
		{"23505", pgErr("23505"), false, false, true},
		{"ErrNoRows", pgx.ErrNoRows, false, true, false},
		{"foreign error", errors.New("boom"), false, false, false},
		// A neighbouring code must not answer for another one: 23503 is the
		// FK violation that shares a call site with 22P02 in
		// store/block_grants.go, and 22001 is the other 22-class error the
		// tree checks elsewhere.
		{"23503", pgErr("23503"), false, false, false},
		{"22001", pgErr("22001"), false, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := MalformedUUID(tc.err); got != tc.malformed {
				t.Errorf("MalformedUUID(%v) = %v, want %v", tc.err, got, tc.malformed)
			}
			if got := AbsentOrMalformed(tc.err); got != tc.absentOrMalformed {
				t.Errorf("AbsentOrMalformed(%v) = %v, want %v", tc.err, got, tc.absentOrMalformed)
			}
			if got := UniqueViolation(tc.err); got != tc.unique {
				t.Errorf("UniqueViolation(%v) = %v, want %v", tc.err, got, tc.unique)
			}
		})
	}
}

// TestSQLStatePredicatesUnwrap is the property every call site depends on:
// the store wraps driver errors with fmt.Errorf("%w") on the way out, so the
// predicates must look THROUGH the wrapping, not at the outermost error.
func TestSQLStatePredicatesUnwrap(t *testing.T) {
	t.Parallel()

	if got := MalformedUUID(fmt.Errorf("store: get tenant: %w", pgErr("22P02"))); !got {
		t.Error("MalformedUUID through one wrap = false, want true")
	}
	if got := AbsentOrMalformed(fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", pgx.ErrNoRows))); !got {
		t.Error("AbsentOrMalformed through two wraps = false, want true")
	}
	if got := UniqueViolation(fmt.Errorf("store: create backend: %w", pgErr("23505"))); !got {
		t.Error("UniqueViolation through one wrap = false, want true")
	}
}

// TestAbsentOrMalformedIsTheUnionOfBoth pins the reason the two names are
// separate: AbsentOrMalformed must be true wherever MalformedUUID is, and
// additionally on ErrNoRows — the pair collapses the existence side channel,
// and a future edit that made them diverge would reopen it.
func TestAbsentOrMalformedIsTheUnionOfBoth(t *testing.T) {
	t.Parallel()

	for _, err := range []error{nil, pgx.ErrNoRows, pgErr("22P02"), pgErr("23505"), errors.New("boom")} {
		if MalformedUUID(err) && !AbsentOrMalformed(err) {
			t.Errorf("MalformedUUID(%v) is true but AbsentOrMalformed is not", err)
		}
	}
	if !AbsentOrMalformed(pgx.ErrNoRows) || MalformedUUID(pgx.ErrNoRows) {
		t.Error("ErrNoRows must be absent-or-malformed but NOT malformed")
	}
}
