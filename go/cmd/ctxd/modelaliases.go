// Boot of the model-alias registry (W02-03, design/02 §4.4): the /v1 model
// namespace, loaded once here and kept hot by the scheduler's NOTIFY listener
// (entity context_model_aliases, and every backlog).

package main

import (
	"context"
	"log/slog"

	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/pgxdb"
)

// bootModelAliases builds the registry and loads it. A failed load is not
// fatal: the registry stays empty — nothing offered, fail-closed — and the
// listener's first backlog reload (pgxlisten runs it on every connect, the
// first included) tries again.
func bootModelAliases(ctx context.Context, q pgxdb.Querier) *modelalias.Registry {
	reg := modelalias.NewRegistry(q)
	if err := reg.Reload(ctx); err != nil {
		slog.Error("modelalias: initial reload failed — no model alias is offered until a reload succeeds", "error", err)
	}
	return reg
}
