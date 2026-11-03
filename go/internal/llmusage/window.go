// Package llmusage reads rolling usage windows of context_llm_log from the
// hourly rollup context_llm_usage_hourly (migration 159) instead of the raw
// hypertable.
//
// A window keeps the exact semantics of the raw queries it replaces —
// "created_at > now − span", no upper bound — because the rollup is complete
// to the row: its trigger counts every log row in the row's own transaction,
// so every hour is exact, the running one included. Only the window's first
// hour is cut by the lower bound, and that partial hour comes from the raw
// rows:
//
//	since   = now − span
//	edgeEnd = date_trunc('hour', since, 'UTC') + 1 h
//	window  = Σ raw (since < created_at < edgeEnd) + Σ rollup (bucket_start ≥ edgeEnd)
//
// The sums never read more than one hour of raw rows, whatever the span (each
// relief the edge cannot answer reads one more, see windowSQL). edgeEnd is
// the NEXT full hour even when since lies exactly on one: the bucket that
// starts at since holds a row stamped exactly at since, which the strict
// lower bound excludes, so that hour has to come from the raw rows as well.
//
// The package is a leaf: it imports the pgxdb handle atoms and nothing of
// ctx's domain (no config, no handler, no store), so every reader — the
// tenant quota accountant (Window), the status rollups (Groups, TenantGroups)
// and the usage API later — can share it without an import cycle. The rule
// is a depguard gate (.golangci.yml, llmusage-leaf).
package llmusage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// Spec selects one rolling window of attributed rows.
type Spec struct {
	// Now is where the window ends. Rows stamped after it count too, as they
	// do in a raw "created_at > now() - interval" query. It is cut to
	// microseconds, the resolution of timestamptz.
	Now time.Time
	// Span is the window length; it must be positive.
	Span time.Duration
	// APIKeyIDs is the literal key list: a row counts when its api_key_id is
	// one of them (the semantics of api_key_id = ANY) — never a consumer row,
	// whose api_key_id is NULL by the CHECK of migration 158. An empty list
	// counts nothing and costs no query.
	APIKeyIDs []string
	// Locality restricts the window to one backend_locality ("external",
	// "local", …); empty counts every row, a NULL locality included.
	Locality string
}

// Totals is the content of one window.
type Totals struct {
	// Since is the window's exclusive lower bound (Now − Span).
	Since time.Time
	// EdgeEnd is the first full UTC hour after Since: the raw rows below
	// it, the rollup from it on.
	EdgeEnd time.Time
	// Calls counts every row, failed ones included.
	Calls int64
	// Cost sums cost_usd (NULL counts as nothing).
	Cost float64
	// CallsRelief is the earliest moment Calls can drop: the oldest counted
	// row's created_at + Span. Zero when the window holds no row — or when
	// the raw rows of its oldest rollup hour were deleted by hand (the
	// rollup does not subtract deletes).
	CallsRelief time.Time
	// CostRelief is the earliest moment Cost can drop: the oldest counted
	// row with cost_usd > 0, + Span. Zero when no row carries a cost. The
	// rollup hour is chosen by its cost sum, so an hour whose negative costs
	// cancel its positive ones is passed over and the relief comes later —
	// never earlier; provider costs are not negative in practice.
	CostRelief time.Time
}

// Bounds returns the window's exclusive lower bound and the end of its raw
// edge for a window of span ending at now. Both are UTC and whole
// microseconds; edgeEnd is the first full hour strictly after since, so the
// raw edge (since, edgeEnd) is never longer than one hour.
func Bounds(now time.Time, span time.Duration) (since, edgeEnd time.Time) {
	since = now.UTC().Truncate(time.Microsecond).Add(-span).Truncate(time.Microsecond)
	edgeEnd = since.Truncate(time.Hour).Add(time.Hour)
	return since, edgeEnd
}

// windowSQL reads one window in one statement. $1 the key list, $2 since,
// $3 edgeEnd; the /*loc*/ marker is the row's locality condition — true, or
// backend_locality = $4.
//
// Access paths, pinned by TestQuotaSpendPlan under the custom plan of a
// one-shot statement and the generic plan a cached prepared statement can
// switch to: each key is its own LATERAL probe with api_key_id = key, fenced
// by OFFSET 0 so the planner cannot flatten it into a join, so the raw edge
// is read through idx_llm_log_apikey (api_key_id, created_at) bounded on both
// sides and the rollup through idx_llm_usage_key (api_key_id, bucket_start).
// With api_key_id = ANY($1) the planner preferred the indexes that lead with
// the time column — uq_llm_usage_hourly and the log's created_at index, both
// well correlated with the heap because rows arrive in time order — and read
// every key's rows in the range: the generic plan the whole instance's edge
// hour. The time-leading uq_llm_usage_hourly can still win the 30-day range
// on a rollup of a few dozen tenants (it filters the key inside the index);
// its price grows with every other tenant's rows, the probe's does not, so
// the plan moves to idx_llm_usage_key as the rollup grows. The DISTINCT keeps
// the = ANY semantics for a list that names a key twice.
//
// The locality condition sits in the aggregates' FILTER, not in the WHERE of
// a probe, so both renderings share one access path; as a WHERE predicate on
// the rollup it was a third indexable condition of uq_llm_usage_hourly and
// pulled the generic plan there.
//
// The oldest counted row comes from the edge when the edge holds one — its
// min(created_at) is a by-product of the sum. Otherwise it lies in the oldest
// counted rollup hour, and one bounded lookup over that single hour's raw
// rows finds it; COALESCE evaluates that lookup only when the edge has no
// such row.
//
// Sums stay NUMERIC until the end: the edge and the rollup add up exactly to
// what one SUM over the raw rows returns, and the single conversion to a
// float happens on the same value — the figures are byte-equal to the raw
// query's.
const windowSQL = `
WITH keys AS (
    SELECT DISTINCT id FROM unnest($1::uuid[]) AS u(id)
), edge AS (
    SELECT count(*) FILTER (WHERE /*loc*/) AS calls,
           COALESCE(sum(l.cost_usd) FILTER (WHERE /*loc*/), 0) AS cost,
           min(l.created_at) FILTER (WHERE /*loc*/) AS oldest_call,
           min(l.created_at) FILTER (WHERE l.cost_usd > 0 AND /*loc*/) AS oldest_cost
      FROM keys CROSS JOIN LATERAL (
           SELECT created_at, cost_usd, backend_locality
             FROM context_llm_log
            WHERE api_key_id = keys.id
              AND created_at > $2::timestamptz
              AND created_at < $3::timestamptz
           OFFSET 0) l
), roll AS (
    SELECT COALESCE(sum(r.calls) FILTER (WHERE /*loc*/), 0) AS calls,
           COALESCE(sum(r.cost_usd) FILTER (WHERE /*loc*/), 0) AS cost,
           min(r.bucket_start) FILTER (WHERE /*loc*/) AS first_call,
           min(r.bucket_start) FILTER (WHERE r.cost_usd > 0 AND /*loc*/) AS first_cost
      FROM keys CROSS JOIN LATERAL (
           SELECT bucket_start, calls, cost_usd, backend_locality
             FROM context_llm_usage_hourly
            WHERE api_key_id = keys.id
              AND bucket_start >= $3::timestamptz
           OFFSET 0) r
)
SELECT (edge.calls + roll.calls)::bigint,
       edge.cost + roll.cost,
       COALESCE(edge.oldest_call,
                (SELECT min(l.created_at)
                   FROM keys CROSS JOIN LATERAL (
                        SELECT created_at, backend_locality
                          FROM context_llm_log
                         WHERE api_key_id = keys.id
                           AND created_at >= roll.first_call
                           AND created_at < roll.first_call + interval '1 hour'
                        OFFSET 0) l
                  WHERE /*loc*/)),
       COALESCE(edge.oldest_cost,
                (SELECT min(l.created_at)
                   FROM keys CROSS JOIN LATERAL (
                        SELECT created_at, backend_locality
                          FROM context_llm_log
                         WHERE api_key_id = keys.id
                           AND created_at >= roll.first_cost
                           AND created_at < roll.first_cost + interval '1 hour'
                           AND cost_usd > 0
                        OFFSET 0) l
                  WHERE /*loc*/))
  FROM edge, roll`

// The two renderings of windowSQL: every locality, or one ($4).
var (
	windowAllSQL      = strings.ReplaceAll(windowSQL, "/*loc*/", "true")
	windowLocalitySQL = strings.ReplaceAll(windowSQL, "/*loc*/", "backend_locality = $4")
)

// Window reads one rolling window from the rollup plus its raw edge hour.
func Window(ctx context.Context, q pgxdb.Rower, spec Spec) (Totals, error) {
	if spec.Span <= 0 {
		return Totals{}, errors.New("llmusage: window span must be positive")
	}
	var out Totals
	out.Since, out.EdgeEnd = Bounds(spec.Now, spec.Span)
	if len(spec.APIKeyIDs) == 0 {
		return out, nil
	}
	sql, args := windowAllSQL, []any{spec.APIKeyIDs, out.Since, out.EdgeEnd}
	if spec.Locality != "" {
		sql, args = windowLocalitySQL, append(args, spec.Locality)
	}
	var oldestCall, oldestCost *time.Time
	if err := q.QueryRow(ctx, sql, args...).Scan(&out.Calls, &out.Cost, &oldestCall, &oldestCost); err != nil {
		return Totals{}, fmt.Errorf("llmusage: window %s: %w", spec.Span, err)
	}
	if oldestCall != nil {
		out.CallsRelief = oldestCall.UTC().Add(spec.Span)
	}
	if oldestCost != nil {
		out.CostRelief = oldestCost.UTC().Add(spec.Span)
	}
	return out, nil
}
