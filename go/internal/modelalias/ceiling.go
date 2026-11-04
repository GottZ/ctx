package modelalias

import "fmt"

// Ceilings are the setting-bound caps of alias policies (design/02 §3.2
// column "Decke", §3.3) on top of the static ones ParsePolicy holds. The
// package reads no settings (it is a leaf); cmd/ctxd builds a getter over the
// gateway.* keys and hands it to the registry (SetCeilings) and to the admin
// action, so both read the same values.
type Ceilings struct {
	// MaxOutputTokens (gateway.max_output_tokens_ceiling) caps
	// max_output_tokens and default_max_tokens of every alias.
	MaxOutputTokens int
	// MaxRequestBytes (gateway.max_request_bytes) caps vision_max_bytes: one
	// image never outgrows the request that carries it.
	MaxRequestBytes int64
	// EmbeddingMaxFloats (gateway.embedding_max_floats) caps
	// embedding.max_inputs × embedding.native_dims when native_dims is set
	// (an unset max_inputs counts at its default).
	EmbeddingMaxFloats int64
	// Tenant (gateway.tenant_alias_ceiling.*) caps every alias outside
	// _global — whoever writes it, a server-admin included.
	Tenant TenantCeiling
}

// TenantCeiling is the server admin's ceiling on tenant-alias policies
// (design/02 §3.3 "tenant_alias_ceiling", Finding 8b): refused above it on
// write, clamped to it with a WARN on reload (a psql edit, or a ceiling
// lowered after the write).
type TenantCeiling struct {
	MaxOutputTokens    int // also caps default_max_tokens
	MaxN               int
	TimeoutS           int
	EmbeddingMaxInputs int
	RerankMaxDocuments int
	ToolsMaxCount      int // 0 is legal: tenant aliases then offer no tools
}

// tenantBound is one dimension of the tenant ceiling: the policy key it caps,
// the ceiling's value and the entry it caps.
type tenantBound struct {
	key   string
	limit int
	opt   *Opt[int]
}

// bounds lists the tenant-ceiling dimensions of p in key-table order.
func (t TenantCeiling) bounds(p *Policy) []tenantBound {
	return []tenantBound{
		{"max_output_tokens", t.MaxOutputTokens, &p.MaxOutputTokens},
		{"default_max_tokens", t.MaxOutputTokens, &p.DefaultMaxTokens},
		{"max_n", t.MaxN, &p.MaxN},
		{"timeout_s", t.TimeoutS, &p.TimeoutS},
		{"tools.max_count", t.ToolsMaxCount, &p.ToolsMaxCount},
		{"embedding.max_inputs", t.EmbeddingMaxInputs, &p.EmbeddingMaxInputs},
		{"rerank.max_documents", t.RerankMaxDocuments, &p.RerankMaxDocuments},
	}
}

// CheckCeilings reports every entry of the parsed policy p above a
// setting-bound ceiling — the write-time half (the admin action answers each
// with a 422 at that key). tenant marks an alias outside _global, which the
// tenant ceiling caps as well. nil when p fits. An unset entry is never
// over a ceiling here: the reading endpoint resolves its default (and bounds
// it by the same ceilings, design/02 §4.5.2).
func CheckCeilings(p Policy, c Ceilings, tenant bool) []*PolicyError {
	var out []*PolicyError
	over := func(key string, v, limit int64, setting string) {
		if v > limit {
			out = append(out, &PolicyError{Key: key, Reason: fmt.Sprintf("%d is over %s = %d", v, setting, limit)})
		}
	}
	if p.MaxOutputTokens.Set {
		over("max_output_tokens", int64(p.MaxOutputTokens.V), int64(c.MaxOutputTokens), "gateway.max_output_tokens_ceiling")
	}
	if p.DefaultMaxTokens.Set {
		over("default_max_tokens", int64(p.DefaultMaxTokens.V), int64(c.MaxOutputTokens), "gateway.max_output_tokens_ceiling")
	}
	if p.VisionMaxBytes.Set {
		over("vision_max_bytes", int64(p.VisionMaxBytes.V), c.MaxRequestBytes, "gateway.max_request_bytes")
	}
	if p.EmbeddingNativeDims.Set {
		// The refusal names a key the policy states: max_inputs when set,
		// else native_dims (max_inputs then counts at its default).
		inputs, key := DefaultEmbeddingMaxInputs, "embedding.native_dims"
		if p.EmbeddingMaxInputs.Set {
			inputs, key = p.EmbeddingMaxInputs.V, "embedding.max_inputs"
		}
		// Both factors are ≤ policyIntMax, so the product stays inside int64.
		if f := int64(inputs) * int64(p.EmbeddingNativeDims.V); f > c.EmbeddingMaxFloats {
			out = append(out, &PolicyError{Key: key, Reason: fmt.Sprintf(
				"embedding.max_inputs × embedding.native_dims = %d × %d = %d is over gateway.embedding_max_floats = %d",
				inputs, p.EmbeddingNativeDims.V, f, c.EmbeddingMaxFloats)})
		}
	}
	if tenant {
		for _, b := range c.Tenant.bounds(&p) {
			if b.opt.Set {
				over(b.key, int64(b.opt.V), int64(b.limit), "gateway.tenant_alias_ceiling (tenant alias)")
			}
		}
	}
	return out
}

// ClampTenant lowers every set entry of p above the tenant ceiling t to the
// ceiling — the reload-time half for tenant aliases (design/02 §3.3 "Klemme +
// WARN"). It returns the clamped policy and one "key: from → to" note per
// clamped entry (nil when nothing moved). A clamped entry may become 0
// (tools.max_count under a ceiling of 0 = no tools). Lowering max_inputs or
// max_documents only shrinks the sub-block count, so ParsePolicy's
// invariants still hold.
func ClampTenant(p Policy, t TenantCeiling) (Policy, []string) {
	var notes []string
	for _, b := range t.bounds(&p) {
		if b.opt.Set && b.opt.V > b.limit {
			notes = append(notes, fmt.Sprintf("%s: %d → %d", b.key, b.opt.V, b.limit))
			b.opt.V = b.limit
		}
	}
	return p, notes
}
