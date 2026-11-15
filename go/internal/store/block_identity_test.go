package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestBlockIdentityViolation pins which driver errors become the issue #227
// refusal: the 23505 of the identity index and nothing else — not another
// unique index, not another SQLSTATE on the same name — and hands back the
// error's DETAIL, the refused key the holder read is checked against.
func TestBlockIdentityViolation(t *testing.T) {
	const detail = "Key (category, title, scope)=(c, t, private) already exists."
	cases := []struct {
		name    string
		err     error
		want    bool
		wantKey string
	}{
		{"identity index, wrapped", fmt.Errorf("outer: %w", &pgconn.PgError{Code: "23505", ConstraintName: blockIdentityIndex, Detail: detail}), true, detail},
		{"identity index, bare", &pgconn.PgError{Code: "23505", ConstraintName: blockIdentityIndex, Detail: detail}, true, detail},
		{"identity index without a detail", &pgconn.PgError{Code: "23505", ConstraintName: blockIdentityIndex}, true, ""},
		{"other unique index", &pgconn.PgError{Code: "23505", ConstraintName: "uq_source_chunk", Detail: detail}, false, ""},
		{"unique violation without a name", &pgconn.PgError{Code: "23505"}, false, ""},
		{"other SQLSTATE on the identity name", &pgconn.PgError{Code: "23514", ConstraintName: blockIdentityIndex}, false, ""},
		{"not a driver error", errors.New("duplicate key value violates unique constraint \"" + blockIdentityIndex + "\""), false, ""},
		{"nil", nil, false, ""},
	}
	for _, c := range cases {
		key, got := blockIdentityViolation(c.err)
		if got != c.want || key != c.wantKey {
			t.Errorf("%s: blockIdentityViolation = (%q, %v), want (%q, %v)", c.name, key, got, c.wantKey, c.want)
		}
	}
}
