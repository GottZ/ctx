package events

import (
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
)

// This file is the owner-side mapping between the config/backends layer and
// the leaf package internal/dispatch (Vorhaben E, wave MW2 carrying the W1
// construction leftover — design/01 §3.1 + §7 W1: "the owner side maps
// config.DispatchConfig onto dispatch.Settings and calls UpdateSettings on
// reload; dispatch never imports config"). It lives in events because the
// NOTIFY reload owner (SettingsWriteHandler) is here; cmd/ctxd/main.go uses
// the same mappers for the boot-time construction.

// DispatchSettings maps the dispatch.* config surface onto the dispatcher's
// knobs (design/01 §3.1). Fairness stays the FIFO baseline — its config key
// is the fairness activation wave's (MW23); until then the structural field
// is pinned here so the mapping stays total. RankOrder is the named order of
// dispatch.external_order (A-E2 no. 1, A03-W3: o1 or o2, validated by V42);
// an empty or unknown name — only a literal Config bypassing Validate can
// carry one — maps to the default order o1.
func DispatchSettings(c config.DispatchConfig) dispatch.Settings {
	order, ok := dispatch.RankOrderFor(c.ExternalOrder)
	if !ok {
		order = dispatch.DefaultRankOrder()
	}
	return dispatch.Settings{
		Enabled:                      c.Enabled,
		BackgroundQueueMax:           c.BackgroundQueueMax,
		InteractiveQueuePerPrincipal: c.InteractiveQueuePerPrincipal,
		InteractiveQueuePerTenant:    c.InteractiveQueuePerTenant,
		InteractiveQueueMax:          c.InteractiveQueueMax,
		LeaseReapGrace:               c.LeaseReapGrace,
		LeaseMaxAge:                  c.LeaseMaxAge,
		PreemptReleaseTimeout:        c.PreemptReleaseTimeout,
		BackgroundAgingAfter:         c.BackgroundAgingAfter,
		Fairness:                     dispatch.FairnessFIFO,
		RankOrder:                    order,
		ExternalQueuePerConsumer:     c.ExternalQueuePerConsumer,
		ExternalQueuePerTenant:       c.ExternalQueuePerTenant,
		ExternalQueueMax:             c.ExternalQueueMax,
		ExternalLeaseMax:             c.ExternalLeaseMax,
		ExternalMaxWait:              c.ExternalMaxWait,
		// UsageWindow stays zero (= package default): its config key belongs
		// to the fairness activation wave (MW23), the Fairness pattern.
	}
}

// DispatchBackendRows projects the backend-pool snapshot onto the
// dispatcher's leaf-safe row type (design/01 N9): exactly the dimensions
// DerivePolicy needs — never the credential-bearing tuple itself, and not
// api_key_ref either: authentication is no condition of the external offer
// (DECISIONS A-E5). ALL rows
// are projected, including disabled ones: a disabled row's declared slots
// still joins the K2 MIN merge on a shared origin (fail-closed toward the
// protected good — a row flip must never silently widen a cap).
func DispatchBackendRows(bs []backends.Backend) []dispatch.BackendRow {
	rows := make([]dispatch.BackendRow, 0, len(bs))
	for _, b := range bs {
		rows = append(rows, dispatch.BackendRow{
			Name:          b.Name,
			Scope:         b.Scope,
			BaseURL:       b.Host,
			ProviderClass: b.ProviderClass,
			Limits:        b.Limits,
			Roles:         b.Roles,
			External:      b.Locality == backends.LocalityExternal,
			// Self-admitting (design/03 §3.2 kind P, A03-W3): a peer ctx
			// instance or an external locality admits on its own — the
			// external opt-in there is limits.external_offer.
			SelfAdmitting: b.ProviderClass == backends.ProviderCtx || b.Locality == backends.LocalityExternal,
		})
	}
	return rows
}
