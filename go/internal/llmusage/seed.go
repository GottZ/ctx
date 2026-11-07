package llmusage

import (
	"context"
	"fmt"
	"time"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// ConsumerUsage is what one model consumer used of one gateway pipeline in
// the running UTC day and month (SeedConsumers): the tokens (prompt +
// completion) and calls of the day, the tokens of the month, and the
// provider-reported external cost of the month.
type ConsumerUsage struct {
	ConsumerID, TenantID, Pipeline   string
	DayTokens, DayCalls, MonthTokens int64
	MonthExternalCost                float64
}

// seedSQL is the boot seed of the meter (design/06 §4.2 "Boot-Seed", wave
// W06-13): every consumer row of the rollup since $2 (the UTC month start),
// with the day from $1 (the UTC day start) on as a FILTER, grouped by
// consumer, its tenant and the pipeline. The windows are calendar UTC and
// fall on whole hours, so the hourly rollup answers them exactly — no raw
// edge (W06-3: the trigger counts every log row in its own transaction, the
// running hour included). The join names the tenant: a consumer never
// changes it and is deleted only with its tenant (K24, migration 161
// trigger model_consumers_identity_immutable), so the join is exact and the
// rollup needs no tenant column. Token sums are clamped to the bigint range
// (an absurd sum must not fail the seed, which would keep /v1 closed).
// Access path: the time-leading idx_llm_usage_bucket over the month,
// filtering the partial consumer rows — measured in W06-13 on 1.17 M rollup
// rows (TestSeedPlanAtOneMillionRows).
const seedSQL = `
SELECT u.consumer_id::text, c.tenant_id::text, u.pipeline,
       LEAST(COALESCE(sum(u.prompt_tokens + u.completion_tokens) FILTER (WHERE u.bucket_start >= $1), 0), 9223372036854775807)::bigint,
       LEAST(COALESCE(sum(u.calls) FILTER (WHERE u.bucket_start >= $1), 0), 9223372036854775807)::bigint,
       LEAST(COALESCE(sum(u.prompt_tokens + u.completion_tokens), 0), 9223372036854775807)::bigint,
       COALESCE(sum(u.cost_usd) FILTER (WHERE u.backend_locality = 'external'), 0)::float8
  FROM context_llm_usage_hourly u
  JOIN context_model_consumers c ON c.id = u.consumer_id
 WHERE u.consumer_id IS NOT NULL AND u.bucket_start >= $2
 GROUP BY u.consumer_id, c.tenant_id, u.pipeline`

// SeedSQL is the statement of SeedConsumers with $1 the UTC day start and
// $2 the UTC month start — for the plan measurement.
func SeedSQL() string { return seedSQL }

// SeedConsumers reads what every model consumer used per gateway pipeline in
// the UTC day and month of now (design/06 §4.2 "Boot-Seed"). The meter maps
// the pipeline to its op (cmd/ctxd, llmlog.GatewayOp — this package is a
// leaf). One statement, O(consumer rows of the month) on the rollup.
func SeedConsumers(ctx context.Context, q pgxdb.Querier, now time.Time) ([]ConsumerUsage, error) {
	u := now.UTC()
	dayStart := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	monthStart := time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	rows, err := q.Query(ctx, seedSQL, dayStart, monthStart)
	if err != nil {
		return nil, fmt.Errorf("llmusage: seed consumers: %w", err)
	}
	defer rows.Close()
	var out []ConsumerUsage
	for rows.Next() {
		var c ConsumerUsage
		if err := rows.Scan(&c.ConsumerID, &c.TenantID, &c.Pipeline, &c.DayTokens, &c.DayCalls, &c.MonthTokens, &c.MonthExternalCost); err != nil {
			return nil, fmt.Errorf("llmusage: scan seed row: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("llmusage: seed consumers: %w", err)
	}
	return out, nil
}
