package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestBlockIdentityViolation pins which driver errors become the issue #227
// refusal: the 23505 of the identity index and nothing else — not another
// unique index, not another SQLSTATE on the same name.
func TestBlockIdentityViolation(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"identity index, wrapped", fmt.Errorf("outer: %w", &pgconn.PgError{Code: "23505", ConstraintName: blockIdentityIndex}), true},
		{"identity index, bare", &pgconn.PgError{Code: "23505", ConstraintName: blockIdentityIndex}, true},
		{"other unique index", &pgconn.PgError{Code: "23505", ConstraintName: "uq_source_chunk"}, false},
		{"unique violation without a name", &pgconn.PgError{Code: "23505"}, false},
		{"other SQLSTATE on the identity name", &pgconn.PgError{Code: "23514", ConstraintName: blockIdentityIndex}, false},
		{"not a driver error", errors.New("duplicate key value violates unique constraint \"" + blockIdentityIndex + "\""), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := blockIdentityViolation(c.err); got != c.want {
			t.Errorf("%s: blockIdentityViolation = %v, want %v", c.name, got, c.want)
		}
	}
}
