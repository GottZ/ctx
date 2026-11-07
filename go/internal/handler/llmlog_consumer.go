package handler

import (
	"errors"
	"slices"
	"strconv"
	"strings"
)

// W06-17 (design/06 §4.8 row "GET /api/llmlog?consumer_id=", design/05
// W05-03 + V-06-a (b)): the consumer branch of the llmlog list.
//
// Contract:
//
//   - A server-admin keeps the unfiltered list; ?consumer_id=<uuid> narrows
//     it to that consumer's rows (consumer_id = $n, idx_llm_log_consumer).
//   - A tenant-admin reads the UNION ALL of two sub-queries, each with its own
//     lower bound created_at > now() - interval '30 days' and its own
//     ORDER BY created_at DESC LIMIT: the tenant's knowledge keys
//     (api_key_id, idx_llm_log_apikey — store.TenantAPIKeyIDs, unchanged) and
//     the tenant's model consumers (consumer_id, idx_llm_log_consumer —
//     store.TenantConsumerIDs); the outer query sorts and limits again.
//     ?consumer_id= INTERSECTS with the tenant's consumers (never OR): a
//     foreign or unknown id yields zero rows, no oracle, and never reaches
//     the statement's arguments. With ?consumer_id= set the key branch is
//     empty — the parameter names one consumer, a key row is none of its.
//   - The SQL text is chosen in Go by role; no "$x IS NULL OR …" predicate
//     (the generic plan of that form cannot use either attribution index).
//     The optional pipeline / errors_only filters are chosen in Go as well
//     (a predicate only when set), so no "$x = '' OR …" form reaches a plan.
//   - Measured in S1 (TestLLMLogConsumerGenericPlan, 20k/40k/200k rows): a
//     branch written as "= ANY($n) … ORDER BY created_at DESC LIMIT" gets a
//     generic plan over context_llm_log_created_at_idx, not the attribution
//     index. Each branch is therefore a per-id top-N probe —
//     unnest($ids) × LATERAL (… WHERE consumer_id = id AND created_at > floor
//     ORDER BY created_at DESC LIMIT $n) — the llmusage windowSQL pattern;
//     UNION ALL of the two branches, outer ORDER BY + LIMIT. The LIMIT inside
//     the probe already keeps the planner from flattening it into a join (no
//     OFFSET 0 fence needed); the DISTINCT keeps the = ANY semantics for a
//     list that names an id twice. The two branches never return one row
//     twice: migration 158's CHECK allows at most one of api_key_id and
//     consumer_id per row.
//   - Every entry carries consumer_id (null for knowledge-key and background
//     rows). The detail GET /api/llmlog/{id} of a row with consumer_id set
//     answers a non-server-admin with the uniform 404 (no body link) — see
//     HandleLLMLogDetail.
//   - The body columns (request_system/request_user/response_content) are in
//     no projection of either text (llmlogListColumns).
//
// llmlogConsumerFloor is the lower bound of every tenant-admin sub-query
// (design/06 §4.8, hardcoded-legitim like the 92-day usage span: it is the
// index-bounding of the list, not a policy).
const llmlogConsumerFloor = "30 days"

// errLLMLogLimit rejects a list query without a positive row limit.
var errLLMLogLimit = errors.New("llmlog list: limit must be positive")

// llmlogListColumns is the projection of every list text — explicitly body-
// free (TestLLMLogNoPrompts). The order is the scan order of HandleLLMLog.
const llmlogListColumns = `id::text AS id, created_at, pipeline, model,
	       COALESCE(backend_name, host) AS backend,
	       duration_ms, error, prompt_tokens, completion_tokens, cost_usd,
	       queue_wait_ms, dispatch_class, dispatch_abort,
	       consumer_id::text AS consumer_id`

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
	// Consumer is the ?consumer_id= parameter ("" = none), lower-case. For a
	// tenant-admin it is intersected with ConsumerIDs before the query is
	// built.
	Consumer   string
	Pipeline   string
	ErrorsOnly bool
	Limit      int
}

// llmlogArgs numbers the statement's arguments as the text is built.
type llmlogArgs []any

// add appends v and returns its placeholder.
func (a *llmlogArgs) add(v any) string {
	*a = append(*a, v)
	return "$" + strconv.Itoa(len(*a))
}

// llmlogListQuery returns the SQL text and its arguments for f — the one
// place that chooses the text by role (W06-17).
func llmlogListQuery(f llmlogListFilter) (string, []any, error) {
	if f.Limit < 1 {
		return "", nil, errLLMLogLimit
	}
	if f.ServerAdmin {
		sql, args := llmlogServerAdminQuery(f)
		return sql, args, nil
	}
	sql, args := llmlogTenantQuery(f)
	return sql, args, nil
}

// llmlogOptional renders the optional pipeline / errors_only predicates, each
// prefixed with AND; empty when neither filter is set.
func llmlogOptional(f llmlogListFilter, args *llmlogArgs) string {
	var b strings.Builder
	if f.Pipeline != "" {
		b.WriteString("\n\t\t   AND pipeline = " + args.add(f.Pipeline))
	}
	if f.ErrorsOnly {
		b.WriteString("\n\t\t   AND error IS NOT NULL")
	}
	return b.String()
}

// llmlogServerAdminQuery is the unfiltered list, narrowed by consumer_id only
// when f.Consumer is set. No lower bound: a server-admin sees the whole log.
func llmlogServerAdminQuery(f llmlogListFilter) (string, []any) {
	var args llmlogArgs
	limit := args.add(f.Limit)
	where := "TRUE"
	if f.Consumer != "" {
		where = "consumer_id = " + args.add(f.Consumer) + "::uuid"
	}
	sql := `
		SELECT ` + llmlogListColumns + `
		  FROM context_llm_log
		 WHERE ` + where + llmlogOptional(f, &args) + `
		 ORDER BY created_at DESC
		 LIMIT ` + limit
	return sql, args
}

// llmlogTenantQuery is the UNION ALL of the key branch and the consumer
// branch, each a per-id top-N probe bounded by llmlogConsumerFloor.
// ?consumer_id= is intersected here: the consumer list shrinks to that one id
// when it is the tenant's own, to nothing otherwise, and the key list empties.
func llmlogTenantQuery(f llmlogListFilter) (string, []any) {
	keys, consumers := nonNilIDs(f.KeyIDs), nonNilIDs(f.ConsumerIDs)
	if f.Consumer != "" {
		keys = []string{}
		if slices.Contains(consumers, f.Consumer) {
			consumers = []string{f.Consumer}
		} else {
			consumers = []string{}
		}
	}
	var args llmlogArgs
	keyList, consumerList, limit := args.add(keys), args.add(consumers), args.add(f.Limit)
	optional := llmlogOptional(f, &args)
	branch := func(list, column string) string {
		return `
		SELECT l.*
		  FROM (SELECT DISTINCT probe_id FROM unnest(` + list + `::uuid[]) AS u(probe_id)) AS p
		 CROSS JOIN LATERAL (
		       SELECT ` + llmlogListColumns + `
		         FROM context_llm_log
		        WHERE ` + column + ` = p.probe_id
		          AND created_at > now() - interval '` + llmlogConsumerFloor + `'` + optional + `
		        ORDER BY created_at DESC
		        LIMIT ` + limit + `) AS l`
	}
	sql := `
		SELECT * FROM (` + branch(keyList, "api_key_id") + `
		 UNION ALL` + branch(consumerList, "consumer_id") + `
		) AS w
		 ORDER BY created_at DESC
		 LIMIT ` + limit
	return sql, args
}

// nonNilIDs returns ids, or an empty list for nil — a nil slice would bind as
// a NULL array; the probe reads nothing either way, the empty list says so.
func nonNilIDs(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}
