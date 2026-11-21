package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestTemporalBlockGone pins which refusal of a dimension row counts as "the
// block is gone": the foreign key of context_temporal.block_id, by code and by
// name. context_temporal carries no other foreign key and no other constraint
// of that name, so the other combinations cannot come from the table today;
// they are built here so that the predicate keeps both halves.
func TestTemporalBlockGone(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"block fk", &pgconn.PgError{Code: "23503", ConstraintName: temporalBlockFK}, true},
		{"block fk wrapped", fmt.Errorf("store: temporal: batch query 1: %w", &pgconn.PgError{Code: "23503", ConstraintName: temporalBlockFK}), true},
		{"other fk", &pgconn.PgError{Code: "23503", ConstraintName: "context_temporal_other_fkey"}, false},
		{"other code, same name", &pgconn.PgError{Code: "23505", ConstraintName: temporalBlockFK}, false},
		{"no constraint", &pgconn.PgError{Code: "23503"}, false},
		{"plain error", errors.New("connection reset"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := temporalBlockGone(c.err); got != c.want {
			t.Errorf("%s: temporalBlockGone = %v, want %v", c.name, got, c.want)
		}
	}
}
