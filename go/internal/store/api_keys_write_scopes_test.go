package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// validateWriteScopes is the mint/update-time half of the 078 write_scopes invariant
// (write_scopes ⊆ allowed_scopes ∪ {home_scope}, plus the '_'-reserved namespace).
// These pure tests pin its contract with no DB.
func TestValidateWriteScopes(t *testing.T) {
	cases := []struct {
		name    string
		home    string
		allowed []string
		write   []string
		wantErr error  // nil, ErrWriteScopeNotAllowed, or reserved-substring via wantSub
		wantSub string // substring for the reserved case
	}{
		{"nil is a no-op", "private", []string{"shared"}, nil, nil, ""},
		{"empty is a no-op", "private", []string{"shared"}, []string{}, nil, ""},
		{"home is always writable", "private", nil, []string{"private"}, nil, ""},
		{"allowed scope is writable", "private", []string{"shared", "work"}, []string{"work"}, nil, ""},
		{"multi valid", "private", []string{"shared", "work"}, []string{"work", "shared"}, nil, ""},
		{"blind writer rejected", "private", []string{"shared"}, []string{"work"}, ErrWriteScopeNotAllowed, ""},
		{"one bad among good rejected", "private", []string{"shared", "work"}, []string{"work", "nope"}, ErrWriteScopeNotAllowed, ""},
		{"reserved rejected", "private", []string{"shared"}, []string{"_global"}, nil, "reserved"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateWriteScopes(c.home, c.allowed, c.write)
			switch {
			case c.wantSub != "":
				if err == nil || !strings.Contains(err.Error(), c.wantSub) {
					t.Errorf("err = %v, want substring %q", err, c.wantSub)
				}
			case c.wantErr != nil:
				if !errors.Is(err, c.wantErr) {
					t.Errorf("err = %v, want %v", err, c.wantErr)
				}
			default:
				if err != nil {
					t.Errorf("err = %v, want nil", err)
				}
			}
		})
	}
}

// insertApiKeyTx must run the write_scopes gate BEFORE it touches the querier — the
// same early-return contract as the reserved-scope gates (TestInsertApiKeyTx_
// ValidationOwnedByPrimitive). A nil querier proves the guard fires before any DB
// access: GREEN returns ErrWriteScopeNotAllowed; without the wired call, execution
// would fall through to q.QueryRow(nil) and panic.
func TestInsertApiKeyTx_RejectsBlindWriterBeforeDB(t *testing.T) {
	ctx := context.Background()
	_, _, err := insertApiKeyTx(ctx, nil, "lbl", "private", []string{"shared"}, []string{"work"}, "", "member")
	if !errors.Is(err, ErrWriteScopeNotAllowed) {
		t.Fatalf("insertApiKeyTx blind-writer err = %v, want ErrWriteScopeNotAllowed", err)
	}
}
