// Boot of the instance identity (F-W2, design/04 §4.2): the id and the hop
// token key of this instance, read once from context_instance (migration
// 163). Without them there is no loop protection, so the boot refuses to
// start rather than serve without one.

package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
)

// instanceIdentityAbort is the boot-abort message (design/04 §4.2). The usual
// cause is a database the migrations have not reached; a row deleted by hand
// is re-created with INSERT INTO context_instance DEFAULT VALUES — a new
// identity (docs/operations.md, Migration 163).
const instanceIdentityAbort = "federation: instance identity missing — run migrations"

// bootInstanceIdentity loads the identity or ends the process: a database
// error, no row, or a secret that is not 32 bytes long exits 1 before the
// scheduler and the HTTP server start. The log names the instance id, never
// the key.
func bootInstanceIdentity(ctx context.Context, q pgxdb.Rower) fedhop.Identity {
	id, secret, err := store.LoadInstanceIdentity(ctx, q)
	if err != nil {
		slog.Error(instanceIdentityAbort, "error", err)
		os.Exit(1)
	}
	ident := fedhop.NewIdentity(id, secret)
	slog.Info("federation: instance identity loaded", "instance", ident)
	return ident
}
