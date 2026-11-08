package handler

// FOLGE-Z Z3 — no hidden defaults at consumer creation: model-consumer-create
// takes optional starting limits per op (written as the consumer's row of
// the CALLER's tier in the create's transaction, validated exactly like
// gateway-limits-set), and create and get answer with effective_limits per
// op — the values the meter applies and the level each one comes from.

import (
	"net/http"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/gatewaylimits"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/store"
)

// ConsumerEffectiveLimits is one op of effective_limits: the wire form of
// gateway-limits-get's effective plus its sources.
type ConsumerEffectiveLimits struct {
	GatewayLimitsEffective
	// BulkMaxConcurrency is the concurrency a request of this consumer gets
	// in the bulk lane: the meter's share of max_concurrency under
	// gateway.xu_reserve_ratio (keyquota.Limits.BulkConcurrency) — present
	// whatever the consumer's lane (FOLGE-Z §3 amendment).
	BulkMaxConcurrency int64                           `json:"bulk_max_concurrency"`
	Sources            map[string]gatewaylimits.Source `json:"sources"`
}

// ops of the limits object in wire order, with their request values.
func (l *consumerCreateLimits) ops() []struct {
	op keyquota.Op
	v  *gatewaylimits.Values
} {
	return []struct {
		op keyquota.Op
		v  *gatewaylimits.Values
	}{{keyquota.OpChat, l.Chat}, {keyquota.OpEmbed, l.Embed}, {keyquota.OpRerank, l.Rerank}}
}

// valuesSet reports whether v names at least one dimension (an empty or
// all-null object says nothing and writes no row).
func valuesSet(v *gatewaylimits.Values) bool {
	return v != nil && (v.RPM != nil || v.TPM != nil || v.MaxConcurrency != nil || v.DailyTokens != nil ||
		v.MonthlyTokens != nil || v.DailyRequests != nil || v.MonthlyCostUSD != nil ||
		v.AllowExternal != nil || v.AllowUnmeteredExternal != nil)
}

// consumerStartLimits validates the create's limits for a new consumer of
// tenant written at the caller's tier — shape (ValidateValues) and, for a
// tenant-admin, narrowing under the ceiling (CheckNarrowing) — and returns
// the rows to write. A refusal is written (400 with param
// limits.<op>.<field>, the gateway-limits-set wording; 503 when the limits
// registry is not wired) and ok is false: nothing has been written yet.
func (h *ManageHandler) consumerStartLimits(w http.ResponseWriter, ar *auth.AuthResult, tenant string, l *consumerCreateLimits) ([]store.ConsumerStartLimit, bool) {
	if l == nil {
		return nil, true
	}
	var out []store.ConsumerStartLimit
	tier := gatewaylimits.TierTenant
	if ar.IsServerAdmin() {
		tier = gatewaylimits.TierServer
	}
	for _, o := range l.ops() {
		if !valuesSet(o.v) {
			continue
		}
		if !h.glReady(w) {
			return nil, false
		}
		d := h.gatewayLimitDefaults(o.op)
		fe := gatewaylimits.ValidateValues(gatewaylimits.SubjectConsumer, tier, *o.v, d)
		if fe == nil && tier == gatewaylimits.TierTenant {
			ceiling, err := h.gatewayLimits.Snapshot().Ceiling(gatewaylimits.SubjectConsumer, "", tenant, o.op, d)
			if err != nil {
				writeAdminRequired(w)
				return nil, false
			}
			fe = gatewaylimits.CheckNarrowing(*o.v, ceiling)
		}
		if fe != nil {
			writeCreateLimitError(w, o.op, fe)
			return nil, false
		}
		out = append(out, store.ConsumerStartLimit{Tier: tier, Op: o.op, Values: *o.v})
	}
	return out, true
}

// writeCreateLimitError is writeGLFieldError with the field's path in the
// create payload as param.
func writeCreateLimitError(w http.ResponseWriter, op keyquota.Op, fe *gatewaylimits.FieldError) {
	param := "limits." + string(op) + "." + fe.Field
	if fe.Code == gatewaylimits.CodeWidens {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "limit widens: " + param + " — " + fe.Msg, "param": param})
		return
	}
	writeGLInvalid(w, param, param+": "+fe.Msg)
}

// consumerEffectiveLimits are the effective limits of consumer (tenant) per
// op as the meter resolves them, over the registry's snapshot plus written
// — the rows this request just committed, so the answer never lags a
// reload. nil when the limits registry is not wired (the field is then
// null).
func (h *ManageHandler) consumerEffectiveLimits(consumer, tenant string, written []gatewaylimits.Row, serverAdmin bool) map[string]ConsumerEffectiveLimits {
	if h.gatewayLimits == nil || h.gatewayLimitDefaults == nil {
		return nil
	}
	snap := h.gatewayLimits.Snapshot()
	if len(written) > 0 {
		snap = gatewaylimits.NewSnapshot(append(snap.Rows(), written...))
	}
	r := h.xuReserveRatio()
	out := make(map[string]ConsumerEffectiveLimits, len(gatewayLimitsOps))
	for _, op := range gatewayLimitsOps {
		eff := snap.Resolve(consumer, tenant, op, h.gatewayLimitDefaults(op))
		out[string(op)] = ConsumerEffectiveLimits{GatewayLimitsEffective: *glEffective(eff, serverAdmin, tenant != ""),
			BulkMaxConcurrency: keyquota.Limits{Concurrency: eff.Concurrency, XUReserve: r}.BulkConcurrency(),
			Sources:            eff.Sources}
	}
	return out
}

// xuReserveRatio is gateway.xu_reserve_ratio of the _global snapshot (the
// meter's r; global-only), the registry default without a store.
func (h *ManageHandler) xuReserveRatio() float64 {
	if h.cfg != nil {
		if c := h.cfg.Snapshot(); c != nil { //nolint:forbidigo // MT 06 BLIND: gateway.xu_reserve_ratio is global-only — one r for every tenant's meter.
			return c.Gateway.XUReserveRatio
		}
	}
	return consumerDefaults().Gateway.XUReserveRatio
}

// startLimitRows are the written starting limits as registry rows of
// consumer.
func startLimitRows(consumer string, ls []store.ConsumerStartLimit) []gatewaylimits.Row {
	rows := make([]gatewaylimits.Row, 0, len(ls))
	for _, l := range ls {
		rows = append(rows, gatewaylimits.Row{Kind: gatewaylimits.SubjectConsumer, ConsumerID: consumer, Tier: l.Tier, Op: l.Op, Values: l.Values})
	}
	return rows
}
