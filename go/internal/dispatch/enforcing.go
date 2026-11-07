package dispatch

import (
	"log/slog"
	"sort"
	"strings"
)

// backgroundRoles are the routing roles background arms actually pull chains
// for (design/05 §4.2 coverage term): dream chat generation (dream), the
// background-embed chain incl. its embed fallback when dream-embed is not
// configured (dream/router.go) plus the backfill embeds, the daily synthesis
// report (digest, synthesize_report.go) and the audit classify calls
// (llm/classify.go). chat/synthesis/translate/rerank are interactive-only —
// a target carrying none of these roles cannot receive background wire calls,
// so it never gates Enforcing.
var backgroundRoles = map[string]struct{}{
	"dream":       {},
	"dream-embed": {},
	"embed":       {},
	"digest":      {},
	"classify":    {},
}

// Enforcing reports whether the dispatcher carries the yield removal
// (design/05 §4.2, the fail-open hinge of the feature gating): enabled AND
// policy non-empty AND — coverage term — EVERY background-reachable LOCAL
// target has a slots cap. false ⇒ Acquire is a pass-through (design/01 B2)
// or the coverage is holey, and the legacy yield fallback loops apply
// (waves MW16/MW17). Hot: two atomic loads; the coverage bool is computed
// ONCE per policy derivation (O(1) read).
func (d *Dispatcher) Enforcing() bool {
	if !d.currentSettings().Enabled {
		return false
	}
	p := d.policy.Load()
	return len(p.Targets) > 0 && p.covered
}

// providerClassCtx mirrors backends.ProviderCtx without the import (leaf
// rule); the owner-side Option-1 matrix (events TestCtxRowsOption1) drives it
// through the projection, so a drifting literal turns that gate red.
const providerClassCtx = "ctx"

// deriveCoverage computes the Enforcing coverage term over one derivation
// (design/05 §4.2, closes B-R2 structurally): every origin reachable by a
// background-capable role on a LOCAL row must carry a slots cap in the
// derived policy. External-only origins are exempt — a deliberately uncapped
// external target ("external skaliert selbst", design/01 §3.2) must not
// force the fallback forever. ctx targets count as local: B's own order must
// hold up to A's door (design/04 §4.14) — the owner-side projection never
// marks a provider_class ctx row External, whatever its locality (F-W7) —
// as long as the row's limits count for its origin (coverageRows, F-W5).
// Each uncovered origin logs ONE WARN per derivation (reload), naming origin
// + the background roles that reach it — the operator sees WHY Enforcing is
// off.
func deriveCoverage(byOrigin map[string][]BackendRow, targets map[string]TargetPolicy, logger *slog.Logger) bool {
	origins := make([]string, 0, len(byOrigin))
	for o := range byOrigin {
		origins = append(origins, o)
	}
	sort.Strings(origins) // deterministic WARN order
	covered := true
	for _, origin := range origins {
		roles := localBackgroundRoles(coverageRows(byOrigin[origin]))
		if len(roles) == 0 || targets[origin].Slots > 0 {
			continue
		}
		covered = false
		logger.Warn("dispatch: background-reachable local target without slots cap, Enforcing degraded to fallback",
			"origin", origin, "roles", strings.Join(roles, ","))
	}
	return covered
}

// coverageRows returns one origin's rows as the coverage term sees them
// (F-W5 Option 1, F-W7 Review-1 finding 1): a ctx row whose limits do not
// count for the origin under the K2 authority rule — a tenant row on an
// origin a '_global' row carries, or one of several tenants' rows on an
// origin without one — counts as External. Its slots never reach the policy
// (DerivePolicy ignores them with the K2 WARN), so holding it to the slots
// duty would let one tenant's row switch the instance-wide mode
// (operations.md: tenant rows never switch other tenants' modes) and leave a
// remedy only a server-admin has. Its background calls pass through this
// side's admission, like a cloud target's. An authoritative ctx row — a
// '_global' row, or the rows of the one scope that carries the origin alone —
// stays coverage-bound (design/04 §4.14). K2 authority is a property of the
// scope within the origin (splitAuthority partitions by scope), so the
// foreign scopes identify the foreign rows. Without a foreign ctx row the
// group is returned as is.
func coverageRows(group []BackendRow) []BackendRow {
	_, foreign := splitAuthority(group)
	foreignScopes := make(map[string]bool)
	for _, r := range foreign {
		if r.ProviderClass == providerClassCtx && !r.External {
			foreignScopes[r.Scope] = true
		}
	}
	if len(foreignScopes) == 0 {
		return group
	}
	out := make([]BackendRow, len(group))
	copy(out, group)
	for i := range out {
		if out[i].ProviderClass == providerClassCtx && foreignScopes[out[i].Scope] {
			out[i].External = true
		}
	}
	return out
}

// localBackgroundRoles returns the sorted background-capable roles of one
// origin's rows, or nil when the origin is external-only (coverage exemption)
// or carries no background-capable role. An origin counts as local as soon as
// ONE row is non-external (fail-closed toward the coverage requirement).
func localBackgroundRoles(group []BackendRow) []string {
	externalOnly := true
	set := make(map[string]struct{})
	for _, r := range group {
		if !r.External {
			externalOnly = false
		}
		for _, role := range r.Roles {
			if _, ok := backgroundRoles[role]; ok {
				set[role] = struct{}{}
			}
		}
	}
	if externalOnly || len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for role := range set {
		out = append(out, role)
	}
	sort.Strings(out)
	return out
}
