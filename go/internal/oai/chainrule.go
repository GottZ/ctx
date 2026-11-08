package oai

import (
	"context"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
)

// gateChain is design/06 §4.1 step 6 after the chain build of every /v1
// route (wave W06-14): first the tenant quota gate over the consumer's tenant
// (masterplan K17, asymmetric — ctx's own use of the tenant stops /v1, the
// consumer's use never counts there), then the egress and cost rule
// (backends.WithoutBillable, design/06 §4.5) under the consumer's policy for
// op. The route answers a chain this empties with 503 before the meter: an
// empty chain books no quota. Without an egress source the policy keeps only
// the free rows (fail-closed, E-06-14).
func (d Deps) gateChain(ctx context.Context, ca *auth.ConsumerAuth, op string, chain []backends.Backend) ([]backends.Backend, error) {
	gated, err := d.Quota.GateTenant(ca.TenantID, chain)
	if err != nil {
		return nil, err
	}
	return backends.WithoutBillable(gated, d.egressPolicy(ctx, ca, op)), nil
}

// egressPolicy is the consumer's egress policy for op — the zero policy
// (free rows only, E-06-14) without a source. The chain dry run (W05-04)
// reads it through here too, between the two halves of gateChain.
func (d Deps) egressPolicy(ctx context.Context, ca *auth.ConsumerAuth, op string) backends.EgressPolicy {
	var p backends.EgressPolicy
	if d.Egress != nil {
		p = d.Egress.Egress(ctx, ca, op)
	}
	return p
}

// tenantQuotaRetryAfter is the Retry-After of a tenant quota refusal
// (design/06 §4.4 row GateTenant, W06-14): the earliest relief of the figure
// that tripped minus now, at least one second — the moment the count sinks;
// over by k, k + 1 rows have to fall out, so a retry then can meet the
// refusal again. An unknown relief (zero) keeps the former contract, the
// next full hour (design/02 §4.10.2).
func tenantQuotaRetryAfter(qe *backends.ErrQuotaExceeded, now time.Time) time.Duration {
	if qe.ReliefAt.IsZero() {
		return untilNextHour(now)
	}
	return max(qe.ReliefAt.Sub(now), time.Second)
}

// CostAdder is the cost half of a reservation (W06-14): the route adds the
// provider-reported cost of its wire calls before it ends the reservation,
// and the meter books it into the consumer's and its tenant envelope's
// monthly cost budget — the figure behind EgressPolicy.CostExhausted. A
// reservation without it (test fakes) books no cost.
type CostAdder interface {
	AddCost(usd float64)
}

// addCost adds the reported cost to l when it books cost (nil = none).
func addCost(l LimitReservation, usd *float64) {
	if ca, ok := l.(CostAdder); ok && usd != nil {
		ca.AddCost(*usd)
	}
}
