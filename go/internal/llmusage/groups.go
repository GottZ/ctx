package llmusage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// Group is one (backend, pipeline) aggregate of a rolling window — in the
// tenant view one (scope, backend, pipeline) aggregate. It is the content of
// a status llm_24h row plus the attribution count behind llm_24h_complete.
type Group struct {
	// Scope is the tenant scope the row's knowledge key belongs to; empty in
	// the server view.
	Scope string
	// Backend is backend_name, falling back to host ('' when the row has
	// neither), and Pipeline the row's pipeline — both cut to 100 characters,
	// the rollup's dimension width (migration 159).
	Backend, Pipeline string
	// Calls counts every row, failed ones included; Errors the failed ones.
	Calls, Errors int64
	// UnattributedOK counts the successful rows without a backend_name: a
	// telemetry gap. A failed row without one is a known failure (it ended
	// before a backend was chosen) and does not count here.
	UnattributedOK int64
	// AvgMs is Σduration_ms / (rows with a duration), rounded half away
	// from zero — what avg(duration_ms)::int returns over the raw rows; 0
	// when no row carries a duration.
	AvgMs int64
	// PromptTokens and CompletionTokens sum the token columns (NULL counts
	// as nothing).
	PromptTokens, CompletionTokens int64
	// Cost sums cost_usd (NULL counts as nothing).
	Cost float64
}

// The two group readers share one shape, as Window does: the raw edge
// (since, edgeEnd) and the rollup from edgeEnd on, each aggregated to the
// output's dimensions, then added in one outer aggregate. The edge applies the
// rollup trigger's normalisation (llm_usage_rollup_ins, migration 159) to the
// raw rows — backend_name falling back to host and then to the empty string,
// both texts cut to 100 characters, unattributed_ok = no backend_name and no
// error — so that an edge group and a rollup group of the same dimensions meet
// in one output row. The one place the figures can differ from an aggregate
// over the raw rows is that cut: a name longer than 100 characters is shown
// cut, and two names that differ only after the 100th character share a row
// (the rollup cannot tell them apart; the longest live name has 24).
//
// Sums stay NUMERIC until the last step, as in windowSQL: the edge and the
// rollup add up to exactly the value one aggregate over the raw rows has, and
// avg_ms is that NUMERIC quotient cast to an integer, the cast that rounds
// avg(duration_ms)::int over the raw rows — the same division (numeric_div of
// the same two values) and the same rounding. cost_usd becomes float8 once,
// from the same NUMERIC value as before.
//
// Order: calls descending, then backend and pipeline, so that rows with as
// many calls keep one order from tick to tick (the raw query ordered by calls
// alone and left ties to the plan). The tenant view orders by scope first.
//
// Access paths (pinned by TestStatusLLM24hPlan in internal/handler): the
// edge is the hypertable's created_at range bounded on both sides, at most one
// hour of raw rows; the rollup is the bucket_start range from edgeEnd through
// idx_llm_usage_bucket.
// The tenant view joins both to context_api_keys and context_tenant_scopes,
// exactly as the raw query did: a row counts for the scopes of its key's
// tenant, a row without a knowledge key — a consumer's row (its api_key_id is
// NULL by the CHECK of migration 158; the rollup keeps it in consumer_id) or
// background work — counts for none.
const groupsSQL = `
WITH edge AS (
    SELECT left(COALESCE(l.backend_name, l.host, ''), 100) AS backend,
           left(l.pipeline, 100) AS pipeline,
           count(*) AS calls,
           count(*) FILTER (WHERE l.error IS NOT NULL) AS errors,
           count(*) FILTER (WHERE l.backend_name IS NULL AND l.error IS NULL) AS unattributed_ok,
           sum(l.prompt_tokens) AS prompt_tokens,
           sum(l.completion_tokens) AS completion_tokens,
           sum(l.duration_ms) AS duration_ms_sum,
           count(l.duration_ms) AS duration_n,
           sum(l.cost_usd) AS cost_usd
      FROM context_llm_log l
     WHERE l.created_at > $1::timestamptz
       AND l.created_at < $2::timestamptz
     GROUP BY 1, 2
), roll AS (
    SELECT r.backend_name AS backend,
           r.pipeline,
           sum(r.calls) AS calls,
           sum(r.errors) AS errors,
           sum(r.unattributed_ok) AS unattributed_ok,
           sum(r.prompt_tokens) AS prompt_tokens,
           sum(r.completion_tokens) AS completion_tokens,
           sum(r.duration_ms_sum) AS duration_ms_sum,
           sum(r.duration_n) AS duration_n,
           sum(r.cost_usd) AS cost_usd
      FROM context_llm_usage_hourly r
     WHERE r.bucket_start >= $2::timestamptz
     GROUP BY 1, 2
)
SELECT g.backend, g.pipeline,
       sum(g.calls)::bigint AS calls,
       sum(g.errors)::bigint AS errors,
       sum(g.unattributed_ok)::bigint AS unattributed_ok,
       COALESCE(sum(g.duration_ms_sum) / NULLIF(sum(g.duration_n), 0), 0)::bigint AS avg_ms,
       COALESCE(sum(g.prompt_tokens), 0)::bigint AS prompt_tokens,
       COALESCE(sum(g.completion_tokens), 0)::bigint AS completion_tokens,
       COALESCE(sum(g.cost_usd), 0)::float8 AS cost_usd
  FROM (SELECT * FROM edge UNION ALL SELECT * FROM roll) g
 GROUP BY g.backend, g.pipeline
 ORDER BY calls DESC, g.backend, g.pipeline`

// tenantGroupsSQL is groupsSQL per tenant scope; see there.
const tenantGroupsSQL = `
WITH edge AS (
    SELECT cts.scope,
           left(COALESCE(l.backend_name, l.host, ''), 100) AS backend,
           left(l.pipeline, 100) AS pipeline,
           count(*) AS calls,
           count(*) FILTER (WHERE l.error IS NOT NULL) AS errors,
           count(*) FILTER (WHERE l.backend_name IS NULL AND l.error IS NULL) AS unattributed_ok,
           sum(l.prompt_tokens) AS prompt_tokens,
           sum(l.completion_tokens) AS completion_tokens,
           sum(l.duration_ms) AS duration_ms_sum,
           count(l.duration_ms) AS duration_n,
           sum(l.cost_usd) AS cost_usd
      FROM context_llm_log l
      JOIN context_api_keys cak ON l.api_key_id = cak.id
      JOIN context_tenant_scopes cts ON cak.tenant_id = cts.tenant_id
     WHERE l.created_at > $1::timestamptz
       AND l.created_at < $2::timestamptz
     GROUP BY 1, 2, 3
), roll AS (
    SELECT cts.scope,
           r.backend_name AS backend,
           r.pipeline,
           sum(r.calls) AS calls,
           sum(r.errors) AS errors,
           sum(r.unattributed_ok) AS unattributed_ok,
           sum(r.prompt_tokens) AS prompt_tokens,
           sum(r.completion_tokens) AS completion_tokens,
           sum(r.duration_ms_sum) AS duration_ms_sum,
           sum(r.duration_n) AS duration_n,
           sum(r.cost_usd) AS cost_usd
      FROM context_llm_usage_hourly r
      JOIN context_api_keys cak ON r.api_key_id = cak.id
      JOIN context_tenant_scopes cts ON cak.tenant_id = cts.tenant_id
     WHERE r.bucket_start >= $2::timestamptz
     GROUP BY 1, 2, 3
)
SELECT g.scope, g.backend, g.pipeline,
       sum(g.calls)::bigint AS calls,
       sum(g.errors)::bigint AS errors,
       sum(g.unattributed_ok)::bigint AS unattributed_ok,
       COALESCE(sum(g.duration_ms_sum) / NULLIF(sum(g.duration_n), 0), 0)::bigint AS avg_ms,
       COALESCE(sum(g.prompt_tokens), 0)::bigint AS prompt_tokens,
       COALESCE(sum(g.completion_tokens), 0)::bigint AS completion_tokens,
       COALESCE(sum(g.cost_usd), 0)::float8 AS cost_usd
  FROM (SELECT * FROM edge UNION ALL SELECT * FROM roll) g
 GROUP BY g.scope, g.backend, g.pipeline
 ORDER BY g.scope, calls DESC, g.backend, g.pipeline`

// Groups reads the server view of a rolling window: every row — knowledge
// keys, consumers and background work alike — grouped by backend and
// pipeline, from the rollup plus the window's raw edge.
func Groups(ctx context.Context, q pgxdb.Querier, now time.Time, span time.Duration) ([]Group, error) {
	return readGroups(ctx, q, groupsSQL, false, now, span)
}

// TenantGroups reads the tenant view of a rolling window: the rows of
// knowledge keys, grouped by their tenant's scope, backend and pipeline.
// Consumer and background rows carry no knowledge key and count for no scope.
func TenantGroups(ctx context.Context, q pgxdb.Querier, now time.Time, span time.Duration) ([]Group, error) {
	return readGroups(ctx, q, tenantGroupsSQL, true, now, span)
}

func readGroups(ctx context.Context, q pgxdb.Querier, sql string, perScope bool, now time.Time, span time.Duration) ([]Group, error) {
	if span <= 0 {
		return nil, errors.New("llmusage: window span must be positive")
	}
	since, edgeEnd := Bounds(now, span)
	rows, err := q.Query(ctx, sql, since, edgeEnd)
	if err != nil {
		return nil, fmt.Errorf("llmusage: groups %s: %w", span, err)
	}
	defer rows.Close()
	out := []Group{}
	for rows.Next() {
		var g Group
		dest := []any{&g.Backend, &g.Pipeline, &g.Calls, &g.Errors, &g.UnattributedOK, &g.AvgMs,
			&g.PromptTokens, &g.CompletionTokens, &g.Cost}
		if perScope {
			dest = append([]any{&g.Scope}, dest...)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("llmusage: groups %s: scan: %w", span, err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("llmusage: groups %s: %w", span, err)
	}
	return out, nil
}
