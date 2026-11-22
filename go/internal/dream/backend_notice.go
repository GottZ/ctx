package dream

import (
	"log/slog"
	"sync/atomic"

	"github.com/GottZ/ctx/internal/backends"
)

// BackendNotice is what an operator hears about Dream's chat backend going
// missing (issue #495): one WARN when the dream loop first finds no backend
// of role dream, saying what is missing and how to fix it; DEBUG for every
// cycle it skips after that; one INFO when a backend serves again. Before,
// every skipped cycle logged a WARN (Chain's empty chain) and two INFO lines —
// at the default dream.idle_wait three of each a minute and worker, about
// 4,300 WARN lines a day on a new installation that was not seeded yet.
//
// One value serves every dream worker of a scheduler (the zero value is
// ready): the line comes once per change of state, not once per worker.
//
// The state is that of the shared pool, not of the iterated tenant: a tenant's
// dream chain is empty only while the shared rows ('_global') serve no dream
// role — a routable tenant-private row of its own makes it non-empty
// (backends.TrustCeilings.For) — so "missing" always means the shared view is
// empty, and "serves again" is read from the shared view as well. A tenant
// that dreams on a private row while other tenants skip therefore neither
// clears the notice nor re-arms it on each turn of the round robin, and N
// tenants without a backend of their own meet one line, not N.
type BackendNotice struct {
	missing atomic.Bool
}

// Skip reports whether the dream cycle of r's tenant must be skipped because
// no backend serves the dream role for it, and logs the change of state (see
// BackendNotice). It reads the pool through the probes that write no log line
// (Router.available, Pool.TrustCeilings, Pool.RoleConfiguredFor).
func (n *BackendNotice) Skip(r *Router) bool {
	if r.available(backends.RoleDream) {
		if n.missing.Load() && r.Pool.TrustCeilings(backends.RoleDream).Shared >= backends.SensPublic.Rank() &&
			n.missing.CompareAndSwap(true, false) {
			slog.Info("dream: a backend of role dream serves again — the dream cycles resume")
		}
		return false
	}
	if !n.missing.CompareAndSwap(false, true) {
		slog.Debug("dream: no eligible dream backend — cycle skipped before pick", "tenant_scope", r.Tenant)
		return true
	}
	if r.Pool.RoleConfiguredFor(backends.RoleDream, r.Tenant) {
		slog.Warn("dream: every backend of role dream is disabled or held by a disable profile — Dream skips its cycles "+
			"until one serves again; `ctx backends` lists the rows. This line repeats only after a backend served in between",
			"tenant_scope", r.Tenant)
		return true
	}
	slog.Warn("dream: no backend of role dream — Dream skips its cycles until one serves; `ctx backends seed` seeds the pool "+
		"(a chat and an embed backend), `ctx backends create` adds a backend with role dream. This line repeats only after "+
		"a backend served in between",
		"tenant_scope", r.Tenant)
	return true
}
