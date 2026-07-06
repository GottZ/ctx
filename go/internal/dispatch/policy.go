package dispatch

import (
	"log/slog"
	"sort"
	"time"
)

// FairnessMode selects the interactive pick order per target (design/04
// §4.2). fifo is the tag-1 default; principal_rr is the MW23 data
// activation. The structure (classQueue) carries both from MW1 on, so the
// flip is policy, never a structural wave.
type FairnessMode string

// The two pick orders of the interactive class.
const (
	FairnessFIFO        FairnessMode = "fifo"
	FairnessPrincipalRR FairnessMode = "principal_rr"
)

// HeraldScope is the per-target herald_scope limits value (K5 enum): global
// (default — the process-wide herald term gates background admission),
// target (only target-local interactive pressure counts — hook lands in
// MD1), exempt (no herald term for this target; class order stays fully in
// force). MW1 parses and VALIDATES the key (exempt needs slots ≥ 2, C5);
// the predicate hook itself is the MD1 wave — until then every declared
// value is visible in the snapshot but the global term applies.
type HeraldScope string

// The three herald_scope values, ordered by permissiveness.
const (
	HeraldGlobal HeraldScope = "global"
	HeraldTarget HeraldScope = "target"
	HeraldExempt HeraldScope = "exempt"
)

// heraldRank orders the enum by permissiveness for the conservative merge.
func heraldRank(h HeraldScope) int {
	switch h {
	case HeraldTarget:
		return 1
	case HeraldExempt:
		return 2
	default:
		return 0
	}
}

// TargetPolicy is the derived per-origin policy. The zero value means
// pass-through (no admission limit — exactly today's behavior), which keeps
// every code wave behavior-neutral until the deliberate data activation.
type TargetPolicy struct {
	// Slots caps concurrent slot-holding leases on the origin; 0 = pass-through.
	Slots int
	// PreemptBackground allows the dispatcher to cancel background leases in
	// favor of waiting interactive acquires (consumer: preempt path, MW18;
	// activation only after the measurement gate).
	PreemptBackground bool
	// HeraldScope "" is read as HeraldGlobal (zero value = default).
	HeraldScope HeraldScope
}

// declared reports whether any effective dispatch key survived the merge.
func (t TargetPolicy) declared() bool {
	return t.Slots > 0 || t.PreemptBackground || (t.HeraldScope != "" && t.HeraldScope != HeraldGlobal)
}

// Policy is the derived admission policy over all origins, hot-swapped as
// one immutable value on the backends NOTIFY reload path (N9). Empty =
// pass-through everywhere.
type Policy struct {
	Targets map[string]TargetPolicy
}

// BackendRow is the leaf-safe projection of one context_backends row —
// exactly the three dimensions the derivation needs (the owner side maps
// backends.Backend → BackendRow; dispatch stays stdlib-only).
type BackendRow struct {
	Name string
	// Scope is the row's tenant dimension: '_global' = server-admin-owned
	// shared backend, '<tenant>' = tenant-private (migration 062).
	Scope   string
	BaseURL string
	Limits  map[string]any
}

// The limits keys the dispatcher derives policy from. background_reserved_slots
// is VALIDATED here (R ≤ S−1) but deliberately has no predicate consumer:
// option A stays a named, un-built follow-up (K7) — the validation exists so
// a data step typo surfaces as WARN before any consumer lands.
const (
	limitSlots         = "slots"
	limitPreempt       = "preempt_background"
	limitHeraldScope   = "herald_scope"
	limitReservedSlots = "background_reserved_slots"
)

var dispatchLimitKeys = []string{limitSlots, limitPreempt, limitHeraldScope, limitReservedSlots}

func hasDispatchKeys(limits map[string]any) bool {
	for _, k := range dispatchLimitKeys {
		if _, ok := limits[k]; ok {
			return true
		}
	}
	return false
}

// DerivePolicy computes the per-origin policy from context_backends.limits
// under the K2 authority rule (fail-closed against an admission DoS by
// tenant rows): the slots MIN merge and ALL dispatch keys on a shared origin
// (carried by at least one '_global' row) count ONLY from '_global' rows;
// tenant rows act only on tenant-EXCLUSIVE origins (one single scope carries
// the origin). A foreign row's dispatch keys are ignored + WARN. An origin
// shared by several tenants WITHOUT a '_global' row has no authority at all:
// dispatch keys ignored + WARN, pass-through (no peer may cap the other).
func DerivePolicy(rows []BackendRow, logger *slog.Logger) Policy {
	if logger == nil {
		logger = slog.Default()
	}
	byOrigin := make(map[string][]BackendRow)
	for _, r := range rows {
		origin, err := NormalizeOrigin(r.BaseURL)
		if err != nil {
			if hasDispatchKeys(r.Limits) {
				logger.Warn("dispatch: limits on unparseable base_url ignored",
					"backend", r.Name, "scope", r.Scope, "base_url", r.BaseURL)
			}
			continue
		}
		byOrigin[origin] = append(byOrigin[origin], r)
	}
	targets := make(map[string]TargetPolicy)
	for origin, group := range byOrigin {
		auth, foreign := splitAuthority(group)
		for _, r := range foreign {
			if hasDispatchKeys(r.Limits) {
				logger.Warn("dispatch: limits keys on non-authoritative row ignored (K2 origin authority)",
					"backend", r.Name, "scope", r.Scope, "origin", origin)
			}
		}
		pol := mergeRows(origin, auth, logger)
		if pol.declared() {
			targets[origin] = pol
		}
	}
	return Policy{Targets: targets}
}

// splitAuthority partitions one origin's rows into authoritative and foreign
// per the K2 rule (see DerivePolicy doc).
func splitAuthority(group []BackendRow) (auth, foreign []BackendRow) {
	for _, r := range group {
		if r.Scope == GlobalScope {
			auth = append(auth, r)
		} else {
			foreign = append(foreign, r)
		}
	}
	if len(auth) > 0 {
		return auth, foreign
	}
	// No _global row: tenant rows are authoritative only when ONE scope
	// exclusively carries the origin.
	scopes := make(map[string]bool)
	for _, r := range group {
		scopes[r.Scope] = true
	}
	if len(scopes) == 1 {
		return group, nil
	}
	return nil, group
}

// mergeRows folds the authoritative rows of one origin into a TargetPolicy:
// slots = MIN over declared positive values (fail-closed toward the
// protected good), preempt_background = OR over declared bools (all rows are
// operator-owned once authoritative), herald_scope = most CONSERVATIVE
// declared value. Invalid values never widen the policy — they degrade to
// the design/01 default semantics + WARN.
func mergeRows(origin string, auth []BackendRow, logger *slog.Logger) TargetPolicy {
	var pol TargetPolicy
	reserved := -1 // -1 = undeclared
	for _, r := range auth {
		if v, ok := r.Limits[limitSlots]; ok {
			if n, numOK := intFromAny(v); numOK && n > 0 {
				if pol.Slots == 0 || n < pol.Slots {
					pol.Slots = n
				}
			} else {
				logger.Warn("dispatch: invalid limits.slots ignored", "backend", r.Name, "origin", origin, "value", v)
			}
		}
		if v, ok := r.Limits[limitPreempt]; ok {
			if b, boolOK := v.(bool); boolOK {
				pol.PreemptBackground = pol.PreemptBackground || b
			} else {
				logger.Warn("dispatch: invalid limits.preempt_background ignored", "backend", r.Name, "origin", origin, "value", v)
			}
		}
		if v, ok := r.Limits[limitHeraldScope]; ok {
			pol.HeraldScope = mergeHerald(pol.HeraldScope, v, r.Name, origin, logger)
		}
		if v, ok := r.Limits[limitReservedSlots]; ok {
			if n, numOK := intFromAny(v); numOK {
				if reserved < 0 || n < reserved {
					reserved = n
				}
			} else {
				logger.Warn("dispatch: invalid limits.background_reserved_slots ignored", "backend", r.Name, "origin", origin, "value", v)
			}
		}
	}
	// C5 gate: exempt only on multi-slot targets — the single named damage
	// case is exactly the 1-slot GPU target. slots 0 (pass-through) has no
	// herald consumer either, so exempt is reset there too.
	if pol.HeraldScope == HeraldExempt && pol.Slots < 2 {
		logger.Warn("dispatch: herald_scope=exempt needs slots >= 2, reset to global (C5)",
			"origin", origin, "slots", pol.Slots)
		pol.HeraldScope = HeraldGlobal
	}
	// C4 validation R ≤ S−1: a reservation at or above the slot count would
	// invert the protected good by typo. The key has NO predicate consumer
	// yet (option A un-built, K7) — validation is early-WARN visibility only.
	if reserved > 0 && (pol.Slots <= 0 || reserved > pol.Slots-1) {
		logger.Warn("dispatch: limits.background_reserved_slots exceeds slots-1, ignored (C4)",
			"origin", origin, "reserved", reserved, "slots", pol.Slots)
	}
	return pol
}

// mergeHerald folds one declared herald_scope value into the accumulator:
// unknown values are ignored + WARN (fail-closed to global), conflicts
// resolve to the most conservative declared rank.
func mergeHerald(acc HeraldScope, v any, backend, origin string, logger *slog.Logger) HeraldScope {
	s, ok := v.(string)
	if !ok || (HeraldScope(s) != HeraldGlobal && HeraldScope(s) != HeraldTarget && HeraldScope(s) != HeraldExempt) {
		logger.Warn("dispatch: invalid limits.herald_scope ignored", "backend", backend, "origin", origin, "value", v)
		return acc
	}
	declared := HeraldScope(s)
	if acc == "" {
		return declared
	}
	if heraldRank(declared) < heraldRank(acc) {
		return declared
	}
	return acc
}

// intFromAny coerces the JSONB number forms (float64 from json.Unmarshal,
// int/int64 from test fixtures — the backendMaxTokens idiom).
func intFromAny(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

// Settings are the config-derived dispatcher knobs (the 7 dispatch.* keys,
// all hot + global-only). The owner side maps config.DispatchConfig onto
// this struct and calls UpdateSettings on reload; dispatch never imports
// config (layering rule).
type Settings struct {
	// Enabled false degrades every Acquire to a pass-through — the emergency
	// stop (E-U3: default true; the waves are behavior-neutral through
	// policy emptiness, a second activation truth would drift, B7).
	Enabled bool
	// BackgroundQueueMax caps waiting background acquires per target;
	// above it Acquire fails fast with ErrQueueFull. 0 = uncapped.
	BackgroundQueueMax int
	// InteractiveQueuePerPrincipal / PerTenant / Max are the Deckel-Staffel
	// (defaults 8/16/64, D3-U4 + E-U6; 0 = that cap off). Check order in
	// Acquire: principal → tenant → aggregate — the most specific offender
	// first, so a flooder is rejected before the aggregate cap hits
	// bystanders.
	InteractiveQueuePerPrincipal int
	InteractiveQueuePerTenant    int
	InteractiveQueueMax          int
	// LeaseReapGrace is the slack AFTER the reap reference before the reaper
	// force-releases a lease that was never Release()d.
	LeaseReapGrace time.Duration
	// LeaseMaxAge is the reap fallback for leases WITHOUT a deadline hint
	// and WITHOUT a ctx deadline (the embed path) — without it a leaked
	// embed lease would NEVER be reaped.
	LeaseMaxAge time.Duration
	// Fairness is the interactive pick order (structure built in MW1, E-F2;
	// the config key registration is the fairness wave's). Unknown or empty
	// reads as fifo.
	Fairness FairnessMode
}

// DefaultSettings mirrors the config registry defaults of the dispatch.* keys.
func DefaultSettings() Settings {
	return Settings{
		Enabled:                      true,
		BackgroundQueueMax:           32,
		InteractiveQueuePerPrincipal: 8,
		InteractiveQueuePerTenant:    16,
		InteractiveQueueMax:          64,
		LeaseReapGrace:               30 * time.Second,
		LeaseMaxAge:                  900 * time.Second,
		Fairness:                     FairnessFIFO,
	}
}

// sortedOrigins returns the policy origins in stable order (snapshot).
func sortedOrigins(m map[string]*targetState) []string {
	out := make([]string, 0, len(m))
	for o := range m {
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}
