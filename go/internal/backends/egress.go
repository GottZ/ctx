package backends

// The egress and cost rule of the model API's chains (wave W06-14,
// design/06 §4.3 "Egress", §4.5 "Egress-/Kostenregel", B-06-13, B-06-20,
// B-06-29). A /v1 consumer reaches what costs nobody anything always, a
// billable row only with a permission: the ctx peers stay — a peer row on a
// public FQDN is "external" by locality, and the locality-based
// withoutExternal would close the pass-through B→A by default and open it
// only together with OpenRouter (§4.5). The internal paths keep the
// locality-based external_off degrade of the tenant quota unchanged
// (withoutExternal, quota.go): this rule is the /v1 chain's alone.

// Billing is the egress class of one chain row.
type Billing uint8

const (
	// BillingFree is a row that bills nobody: locality local or lan, or a
	// ctx peer (ProviderCtx, design/04 M-F2) whatever its locality.
	BillingFree Billing = iota
	// BillingMetered is a billable row that reports its cost: an openrouter
	// row (usage.cost, the only cost source ctx books — package comment of
	// quota.go). It moves the cost budget.
	BillingMetered
	// BillingUnmetered is every other row — external and not reporting a
	// cost (the anthropic Messages API, a generic /v1 server) or of a
	// locality ctx does not know: it books NULL and never moves a cost
	// budget, so only a permission of its own opens it (B-06-29).
	BillingUnmetered
)

// String names the class (logs and test messages).
func (b Billing) String() string {
	switch b {
	case BillingFree:
		return "free"
	case BillingMetered:
		return "metered"
	}
	return "unmetered"
}

// BillingOf classifies b (design/06 §4.5 table): free when its locality is
// local or lan or it is a ctx peer; metered when it is an openrouter row;
// otherwise unmetered — an unknown locality counts as billable, never as
// free (fail-closed).
func BillingOf(b Backend) Billing {
	switch {
	case b.Locality == LocalityLocal || b.Locality == LocalityLAN || b.ProviderClass == ProviderCtx:
		return BillingFree
	case b.ProviderClass == ProviderOpenRouter:
		return BillingMetered
	}
	return BillingUnmetered
}

// EgressPolicy is what one /v1 consumer may reach of an op beyond the free
// rows (design/06 §4.3): until the limits rows of W06-15 AllowExternal is
// gateway.consumer_allow_external_default, AllowUnmetered has no source and
// stays false, and CostExhausted reports that the consumer's monthly cost
// budget or its tenant envelope's is used up (the meter, from Settle and the
// boot seed).
type EgressPolicy struct {
	AllowExternal  bool
	AllowUnmetered bool
	CostExhausted  bool
	// CostUntracked: the op's route books no provider cost (embeddings and
	// rerank add none to the meter), so a cost-reporting row could never
	// move the budget there — it counts as unmetered for the op (Fremd-
	// Review 1 L-6).
	CostUntracked bool
}

// keeps reports whether p keeps a row of class c: free always; metered with
// the permission while the cost budget lasts (after it the row drops — a
// degrade like external_off, never a 429); unmetered with the permission
// and the unmetered permission, whatever the cost budget (it never moves).
func (p EgressPolicy) keeps(c Billing) bool {
	switch c {
	case BillingFree:
		return true
	case BillingMetered:
		if !p.CostUntracked {
			return p.AllowExternal && !p.CostExhausted
		}
	}
	return p.AllowExternal && p.AllowUnmetered
}

// WithoutBillable returns chain without the rows p does not keep, in the
// chain's order. It allocates a fresh slice — the snapshot chain is shared
// and must not be mutated in place (like withoutExternal). It replaces
// withoutExternal for /v1 chains (design/06 §4.5) and runs after the tenant
// gate: a chain it empties is a 503 before the meter, so it books no quota
// (design/06 §4.1 step 6).
func WithoutBillable(chain []Backend, p EgressPolicy) []Backend {
	out := make([]Backend, 0, len(chain))
	for _, b := range chain {
		if p.keeps(BillingOf(b)) {
			out = append(out, b)
		}
	}
	return out
}
