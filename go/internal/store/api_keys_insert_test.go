package store

import (
	"context"
	"strings"
	"testing"
)

// insertApiKeyTx is the shared INSERT primitive behind CreateApiKey (and the
// later owner / quota-gated mint paths). It is unexported, so the DB-backed
// role-write is proved THROUGH CreateApiKey in store_test (the testcontainers
// harness imports store and cannot be reached from an in-package test without
// an import cycle). These pure tests pin what the primitive owns with NO DB:
// the input validation now lives HERE (not in the thin CreateApiKey wrapper),
// and the role argument is opaque to the store — it is a bound INSERT parameter,
// never gated by value (ValidRole/the 23514 CHECK are the handler/DB's job).
// All cases use a nil querier: every assertion early-returns BEFORE q.QueryRow,
// exactly like the existing CreateApiKey nil-pool unit tests.

func TestInsertApiKeyTx_ValidationOwnedByPrimitive(t *testing.T) {
	ctx := context.Background()
	// The role argument must not change which inputs are rejected — run the
	// whole validation table under each of the three canonical roles plus a
	// bogus one, proving role-opacity at the store layer.
	for _, role := range []string{"member", "admin", "owner", "bogus"} {
		cases := []struct {
			name      string
			label     string
			homeScope string
			allowed   []string
			wantErr   string
		}{
			{"empty label", "", "private", nil, "label is required"},
			{"empty home_scope", "lbl", "", nil, "home_scope is required"},
			{"reserved home_scope", "lbl", "_global", nil, "reserved"},
			{"reserved allowed_scope", "lbl", "tenant-a", []string{"shared", "_x"}, "reserved"},
		}
		for _, c := range cases {
			_, _, err := insertApiKeyTx(ctx, nil, c.label, c.homeScope, c.allowed, "", role)
			if err == nil {
				t.Errorf("role=%s %s: expected error, got nil", role, c.name)
				continue
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("role=%s %s: error = %q, want substring %q", role, c.name, err.Error(), c.wantErr)
			}
		}
	}
}
