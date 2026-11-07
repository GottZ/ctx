package handler

import "errors"

// W06-17 (design/06 §4.8 row "GET /api/llmlog?consumer_id=", design/05
// W05-03 + V-06-a (b)): the consumer branch of the llmlog list.
//
// Contract (RC-D S1 — stubs, the handler is not wired to it yet):
//
//   - A server-admin keeps the unfiltered list; ?consumer_id=<uuid> narrows
//     it to that consumer's rows (consumer_id = $n, idx_llm_log_consumer).
//   - A tenant-admin reads the UNION ALL of two sub-queries, each with its own
//     lower bound created_at > now() - interval '30 days' and its own
//     ORDER BY created_at DESC LIMIT: the tenant's knowledge keys
//     (api_key_id = ANY($keys), idx_llm_log_apikey — store.TenantAPIKeyIDs,
//     unchanged) and the tenant's model consumers (consumer_id = ANY($consumers),
//     idx_llm_log_consumer — store.TenantConsumerIDs); the outer query sorts
//     and limits again. ?consumer_id= INTERSECTS with the tenant's consumers
//     (never OR): a foreign or unknown id yields zero rows, no oracle.
//   - The SQL text is chosen in Go by role; no "$x IS NULL OR …" predicate
//     (the generic plan of that form cannot use either attribution index).
//   - Every entry carries consumer_id (null for knowledge-key and background
//     rows). The detail GET /api/llmlog/{id} of a row with consumer_id set
//     answers a non-server-admin with the uniform 404 (no body link).
//
// llmlogConsumerFloor is the lower bound of every tenant-admin sub-query
// (design/06 §4.8, hardcoded-legitim like the 92-day usage span: it is the
// index-bounding of the list, not a policy).
const llmlogConsumerFloor = "30 days"

// errLLMLogNotImplemented is the S1 stub answer of the list-query builder.
var errLLMLogNotImplemented = errors.New("llmlog consumer branch: not implemented (RC-D S1 stub)")

// llmlogListFilter is the role-resolved input of the list query.
type llmlogListFilter struct {
	// ServerAdmin selects the unfiltered SQL text; KeyIDs/ConsumerIDs are
	// ignored then.
	ServerAdmin bool
	// KeyIDs are the tenant's knowledge keys (store.TenantAPIKeyIDs), non-nil
	// for a tenant-admin (empty ⇒ the key branch yields nothing).
	KeyIDs []string
	// ConsumerIDs are the tenant's model consumers (store.TenantConsumerIDs),
	// non-nil for a tenant-admin.
	ConsumerIDs []string
	// Consumer is the ?consumer_id= parameter ("" = none). For a tenant-admin
	// it is intersected with ConsumerIDs before the query is built.
	Consumer   string
	Pipeline   string
	ErrorsOnly bool
	Limit      int
}

// llmlogListQuery returns the SQL text and its arguments for f — the one
// place that chooses the text by role (W06-17). S1 stub: no text.
func llmlogListQuery(f llmlogListFilter) (string, []any, error) {
	_ = f
	return "", nil, errLLMLogNotImplemented
}
