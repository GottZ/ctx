package dream

import (
	"log/slog"
	"sync"

	"github.com/GottZ/ctx/internal/backends"
)

// BackendNotice is what an operator hears about Dream's chat backend going
// missing (issue #495): a WARN when a change of state makes the dream loop
// skip, saying what is missing and how to fix it; DEBUG for every cycle it
// skips after that; an INFO when a backend serves again. Before, every
// skipped cycle logged a WARN (Chain's empty chain) and two INFO lines — at
// the default dream.idle_wait three of each a minute and worker, about 4,300
// WARN lines a day on a new installation that was not seeded yet.
//
// One value serves every dream worker of a scheduler (the zero value is
// ready): a line comes once per change of state, not once per worker or turn
// of the round robin. The state is the set of tenants whose cycles skip, each
// with the reason it skips, and the lines follow the reason:
//
//   - A tenant with no backend of role dream of its own skips because the
//     shared view ('_global') serves none: its dream chain is the shared one
//     (backends.TrustCeilings.For). Every such tenant skips for the same
//     reason, so they form one group: one WARN when the group goes from empty
//     to not empty — the seed hint while the shared view has no row of the
//     role, the disabled rows otherwise — DEBUG for every tenant that joins
//     it, one INFO when it is empty again. N tenants without a backend meet
//     one line, not N.
//   - A tenant with dream rows of its own, none of them serving, skips for a
//     reason that is its own (the shared view is empty too, or its chain would
//     not be). Its WARN names the tenant and `ctx backends`, never the seed —
//     a deployment whose shared view carries no dream row on purpose is not
//     missing one — and its INFO comes when that tenant serves again. Its
//     next outage is a WARN again, whatever other tenants do meanwhile.
//
// Tenants that left the background tenant list are forgotten at the start of
// each round of the loop (Retain), so the set holds at most one entry per
// tenant of the current list plus those removed during the current round.
type BackendNotice struct {
	mu      sync.Mutex
	skipped map[string]skipReason // tenant scope → why its dream cycles skip
	shared  int                   // entries with reasonShared
}

// skipReason is why a tenant's dream cycles skip (see BackendNotice).
type skipReason uint8

const (
	reasonShared skipReason = iota + 1 // no dream row of its own: the shared view serves none
	reasonOwn                          // dream rows of its own, none of them serving
)

// Skip reports whether the dream cycle of r's tenant must be skipped because
// no backend serves the dream role for it, and logs the change of state (see
// BackendNotice). It reads the pool through the probes that write no log line
// (Router.available, Pool.Snapshot, Pool.RoleConfiguredFor).
func (n *BackendNotice) Skip(r *Router) bool {
	if r.available(backends.RoleDream) {
		n.served(r.Tenant)
		return false
	}
	n.skip(r)
	return true
}

// served takes a tenant whose dream chain serves out of the set.
func (n *BackendNotice) served(tenant string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	reason, ok := n.skipped[tenant]
	if !ok {
		return
	}
	delete(n.skipped, tenant)
	if reason == reasonOwn {
		slog.Info("dream: a backend of role dream of this tenant serves again — its dream cycles resume", "tenant_scope", tenant)
		return
	}
	n.shared--
	if n.shared == 0 {
		slog.Info("dream: a backend of role dream serves again — the dream cycles resume")
	}
}

// skip puts a tenant whose dream chain is empty into the set and logs a
// change of state.
func (n *BackendNotice) skip(r *Router) {
	own := ownDreamRow(r.Pool.Snapshot(), r.Tenant)
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.skipped[r.Tenant]; ok {
		slog.Debug("dream: no eligible dream backend — cycle skipped before pick", "tenant_scope", r.Tenant)
		return
	}
	if n.skipped == nil {
		n.skipped = make(map[string]skipReason)
	}
	if own {
		n.skipped[r.Tenant] = reasonOwn
		slog.Warn("dream: no backend of role dream of this tenant serves — they are disabled, held by a disable profile "+
			"or below the trust any block needs; Dream skips this tenant's cycles until one serves again, `ctx backends` "+
			"lists the rows. This line repeats only after one served in between",
			"tenant_scope", r.Tenant)
		return
	}
	n.skipped[r.Tenant] = reasonShared
	n.shared++
	if n.shared > 1 {
		slog.Debug("dream: no eligible dream backend — cycle skipped before pick", "tenant_scope", r.Tenant)
		return
	}
	if r.Pool.RoleConfiguredFor(backends.RoleDream, backends.GlobalScope) {
		slog.Warn("dream: every backend of role dream is disabled or held by a disable profile — Dream skips its cycles " +
			"until one serves again; `ctx backends` lists the rows. This line repeats only after a backend served in between")
		return
	}
	slog.Warn("dream: no backend of role dream — Dream skips its cycles until one serves; `ctx backends seed` seeds the pool " +
		"(a chat and an embed backend), `ctx backends create` adds a backend with role dream. This line repeats only after " +
		"a backend served in between")
}

// Retain forgets the tenants that are not in scopes — the background tenant
// list of the round that starts (events.Scheduler, issue #495) — so a deleted
// tenant neither stays in the set nor keeps the group of tenants without a
// backend from ever being empty. Forgetting logs nothing: no backend served.
// A list that degraded to the default tenant alone (a failed tenant read)
// forgets the others too; a tenant that still skips afterwards is a change of
// state again and gets its line again.
func (n *BackendNotice) Retain(scopes []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.skipped) == 0 {
		return
	}
	keep := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		keep[s] = true
	}
	for tenant, reason := range n.skipped {
		if keep[tenant] {
			continue
		}
		delete(n.skipped, tenant)
		if reason == reasonShared {
			n.shared--
		}
	}
}

// ownDreamRow reports whether tenant has a backend of role dream of its own:
// a row in its private scope, whatever its state. The shared rows ('_global'
// and the unscoped test row) are nobody's own, also not the default
// tenant's, whose view they are.
func ownDreamRow(snap []backends.Backend, tenant string) bool {
	for i := range snap {
		b := &snap[i]
		if b.Scope == tenant && !backends.VisibleTo(b.Scope, "") && b.HasRole(backends.RoleDream) {
			return true
		}
	}
	return false
}
