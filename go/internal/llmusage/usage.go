package llmusage

import (
	"context"
	"errors"
	"time"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// W06-16 (design/06 §4.7 row "/api/usage", §4.8; design/05 V-06-a/b/c):
// the series reader of GET /api/usage and the per-consumer reader of
// GET /api/usage/consumers.
//
// Contract (RC-D S1 — every function below is a fail-closed stub that
// returns ErrNotImplemented or the empty value):
//
//   - A series is a run of whole UTC buckets (hour or day) from From
//     (inclusive, cut down to the bucket) to To (exclusive, cut up to the
//     bucket). Buckets come from the hourly rollup context_llm_usage_hourly
//     only — the rollup is exact per hour, so whole buckets need no raw edge.
//     Refusals come from the ledger context_llm_rejections_hourly.
//   - null ≠ 0: a bucket that starts before the coverage of its source
//     (Cover) is reported with Complete = false and nil counters — never 0.
//     A covered bucket without rows is a true 0 with Complete = true. The
//     running bucket (end after now) carries its partial values and
//     Complete = false. Refusals before Cover.RejectionsSince are nil.
//   - Latencies: avg = duration_ms_sum / duration_n (nil when n = 0);
//     p50/p95 from the log2 histograms by Quantile (nil when the histogram is
//     empty). Hist reports whether histograms back the percentiles.
//   - Tenant filter: ConsumerIDs non-nil restricts the rows to those
//     consumers (consumer_id = ANY, idx_llm_usage_consumer, one LATERAL probe
//     per consumer like windowSQL); internal rows (api_key_id set, or no
//     identity) never appear then. nil = every row (server-admin only).
//   - Exposure: Target and DispatchClass of a refusal, and Group =
//     GroupInstance, are filled only when ServerAdmin is set.

// ErrNotImplemented is the answer of the RC-D S1 stubs.
var ErrNotImplemented = errors.New("llmusage: not implemented (RC-D S1 stub)")

// ErrSpan is the refusal of a series request whose To − From exceeds
// MaxSpan (400 at the API).
var ErrSpan = errors.New("llmusage: span exceeds 92 days")

// ErrRange is the refusal of a series request with From ≥ To or a zero bound.
var ErrRange = errors.New("llmusage: empty or invalid range")

// MaxSpan bounds one series request (design/06 §4.8 "to − from ≤ 92 d",
// marked hardcoded-legitim there: it bounds the rows a request can read,
// it is not a policy).
const MaxSpan = 92 * 24 * time.Hour

// Bucket is the series resolution.
type Bucket string

// The resolutions of GET /api/usage?bucket=.
const (
	BucketHour Bucket = "hour"
	BucketDay  Bucket = "day"
)

// GroupBy is the series dimension of GET /api/usage?group=.
type GroupBy string

// The dimensions of GET /api/usage?group= (design/06 §4.8). GroupInstance
// joins consumer_id → context_model_consumers.peer_instance_id and is
// server-admin only.
const (
	GroupConsumer GroupBy = "consumer"
	GroupAlias    GroupBy = "alias"
	GroupPipeline GroupBy = "pipeline"
	GroupSource   GroupBy = "source"
	GroupBackend  GroupBy = "backend"
	GroupClass    GroupBy = "class"
	GroupInstance GroupBy = "instance"
)

// SeriesQuery selects one series request.
type SeriesQuery struct {
	From, To time.Time
	Bucket   Bucket
	Group    GroupBy
	// ConsumerIDs restricts the rows to these consumers; nil = every row
	// (server-admin), non-nil empty = no row and no statement.
	ConsumerIDs []string
	// Source restricts to "internal" or "gateway" (the rollup's generated
	// column); "" = both.
	Source string
	// ServerAdmin unlocks Target/DispatchClass of refusals and GroupInstance.
	ServerAdmin bool
	// Now is the reading time (running bucket, coverage); zero = time.Now.
	Now time.Time
}

// Cover is where each source starts to be complete. Zero = no coverage
// (every bucket nil). UsageSince = LEAST(min(bucket_start) of the rollup,
// date_trunc('hour', applied_at of migration 159)); RejectionsSince =
// applied_at of migration 167 (the ledger has no backfill).
type Cover struct {
	UsageSince, RejectionsSince time.Time
}

// Refusal is one ledger reason of a bucket: n, the mean wait of the
// refusals that waited (nil when none waited), and — server-admin only —
// the target and dispatch class of capacity refusals.
type Refusal struct {
	Reason    string   `json:"reason"`
	Detail    string   `json:"detail,omitempty"`
	N         int64    `json:"n"`
	WaitMsAvg *float64 `json:"wait_ms_avg"`
	// Target and DispatchClass are omitted (never "") for a non-server-admin.
	Target        *string `json:"target,omitempty"`
	DispatchClass *string `json:"dispatch_class,omitempty"`
}

// Point is one bucket of one series. Every counter is a pointer: nil means
// "not recorded" (Complete = false), 0 means "recorded, nothing happened".
type Point struct {
	BucketStart      time.Time `json:"bucket_start"`
	Complete         bool      `json:"complete"`
	Calls            *int64    `json:"calls"`
	Errors           *int64    `json:"errors"`
	EstimatedCalls   *int64    `json:"estimated_calls"`
	PromptTokens     *int64    `json:"prompt_tokens"`
	CompletionTokens *int64    `json:"completion_tokens"`
	// GPUMs is duration_ms_sum: the summed wall time of the calls on their
	// backend (the rollup has no separate GPU clock).
	GPUMs    *int64   `json:"gpu_ms"`
	AvgMs    *float64 `json:"avg_ms"`
	P50Ms    *float64 `json:"p50_ms"`
	P95Ms    *float64 `json:"p95_ms"`
	QueueP50 *float64 `json:"queue_p50_ms"`
	QueueP95 *float64 `json:"queue_p95_ms"`
	TTFTP50  *float64 `json:"ttft_p50_ms"`
	TTFTP95  *float64 `json:"ttft_p95_ms"`
	CostUSD  *float64 `json:"cost_usd"`
	// Rejected is nil before Cover.RejectionsSince, an empty list for a
	// covered bucket without refusals.
	Rejected *[]Refusal `json:"rejected"`
}

// Series is the run of points of one group key.
type Series struct {
	Key    string  `json:"key"`
	Points []Point `json:"points"`
}

// Result is one series answer.
type Result struct {
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Bucket   Bucket    `json:"bucket"`
	Group    GroupBy   `json:"group"`
	Complete bool      `json:"complete"`
	Hist     bool      `json:"hist"`
	Series   []Series  `json:"series"`
}

// Align validates q and cuts its bounds to whole UTC buckets: From down,
// To up (the running bucket included). It refuses a span over MaxSpan
// (ErrSpan), an empty range (ErrRange) and an unknown bucket or group.
// S1 stub.
func Align(q SeriesQuery) (SeriesQuery, error) {
	_ = q
	return SeriesQuery{}, ErrNotImplemented
}

// ReadCover reads the coverage of both sources in one statement. S1 stub.
func ReadCover(ctx context.Context, q pgxdb.Rower) (Cover, error) {
	_, _ = ctx, q
	return Cover{}, ErrNotImplemented
}

// ReadSeries reads one aligned series request. S1 stub.
func ReadSeries(ctx context.Context, q pgxdb.Querier, sq SeriesQuery, c Cover) (Result, error) {
	_, _, _, _ = ctx, q, sq, c
	return Result{}, ErrNotImplemented
}

// SeriesSQL returns the statement text ReadSeries runs for sq (the plan
// gate EXPLAINs it). S1 stub: "".
func SeriesSQL(sq SeriesQuery) string {
	_ = sq
	return ""
}

// Quantile is the q-quantile (0 < q < 1) of a 16-bucket log2 histogram with
// the bucket bounds of design/05 §3.5 (base 60 ms; bucket 0 interpolated
// linearly from 0, the others geometrically between their bounds, the
// overflow bucket 15 answers its lower bound). ok = false for an empty or
// malformed histogram — the caller renders null, never 0. S1 stub.
func Quantile(hist []int64, q float64) (ms float64, ok bool) {
	_, _ = hist, q
	return 0, false
}

// ConsumerDay is the rollup part of one consumer × op of
// GET /api/usage/consumers: the last bucket and the 24-hour totals
// (one batch per tenant, a 30-second generation in the handler, V-06-f).
type ConsumerDay struct {
	ConsumerID string
	Op         string
	LastBucket *time.Time
	Calls24h   int64
	Tokens24h  int64
	Errors24h  int64
	Refused24h int64
}

// ReadConsumerDays reads ConsumerDay for every listed consumer in one
// statement (rollup + ledger, last 24 h). nil or empty list = no statement.
// S1 stub.
func ReadConsumerDays(ctx context.Context, q pgxdb.Querier, consumerIDs []string, now time.Time) ([]ConsumerDay, error) {
	_, _, _, _ = ctx, q, consumerIDs, now
	return nil, ErrNotImplemented
}

// SplitRow is one row of the status field usage_split_24h (server-admin
// only): calls and GPU wall time per target × source × dispatch class over
// the last 24 hours.
type SplitRow struct {
	Target string `json:"target"`
	Source string `json:"source"`
	Class  string `json:"class"`
	Calls  int64  `json:"calls"`
	GPUMs  int64  `json:"gpu_ms"`
}

// ReadSplit24h reads usage_split_24h from the rollup plus the raw edge hour
// (one statement, the Groups pattern). S1 stub.
func ReadSplit24h(ctx context.Context, q pgxdb.Querier, now time.Time) ([]SplitRow, error) {
	_, _, _ = ctx, q, now
	return nil, ErrNotImplemented
}
