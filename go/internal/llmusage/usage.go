package llmusage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/jackc/pgx/v5"
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

// ErrParam is the refusal of an unknown bucket, group or source (400).
var ErrParam = errors.New("llmusage: unknown bucket, group or source")

// ErrForbiddenGroup is the refusal of GroupInstance for a non-server-admin
// (403 at the API).
var ErrForbiddenGroup = errors.New("llmusage: group=instance is server-admin only")

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
	GroupPipelineName GroupBy = "pipeline"
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

// ConsumerDay is the rollup part of one consumer × pipeline of
// GET /api/usage/consumers: the last bucket of the last 30 days and the
// 24-hour totals (one batch per tenant, a 30-second generation in the
// handler, V-06-f). The handler maps the pipeline to its op
// (llmlog.GatewayOp; this package is a leaf).
type ConsumerDay struct {
	ConsumerID string
	Pipeline   string
	LastBucket *time.Time
	Calls24h   int64
	Tokens24h  int64
	Errors24h  int64
}

// ConsumerRefusals is the ledger part of one consumer × op: the refusals of
// the last 24 hours.
type ConsumerRefusals struct {
	ConsumerID string
	Op         string
	Refused24h int64
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

// bucketStep is the length of one bucket.
func bucketStep(b Bucket) time.Duration {
	if b == BucketDay {
		return 24 * time.Hour
	}
	return time.Hour
}

// floorBucket cuts t down to its UTC bucket.
func floorBucket(t time.Time, b Bucket) time.Time {
	u := t.UTC()
	if b == BucketDay {
		y, m, d := u.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	return u.Truncate(time.Hour)
}

// Align validates q and cuts its bounds to whole UTC buckets: From down,
// To up (the running bucket included). It refuses a span To − From over
// MaxSpan (ErrSpan, measured on the request's own bounds), an empty range
// (ErrRange), an unknown bucket, group or source (ErrParam) and
// GroupInstance without ServerAdmin (ErrForbiddenGroup). A zero Now becomes
// time.Now.
func Align(q SeriesQuery) (SeriesQuery, error) {
	switch q.Bucket {
	case BucketHour, BucketDay:
	default:
		return SeriesQuery{}, fmt.Errorf("%w: bucket %q", ErrParam, q.Bucket)
	}
	switch q.Group {
	case GroupConsumer, GroupAlias, GroupPipelineName, GroupSource, GroupBackend, GroupClass:
	case GroupInstance:
		if !q.ServerAdmin {
			return SeriesQuery{}, ErrForbiddenGroup
		}
	default:
		return SeriesQuery{}, fmt.Errorf("%w: group %q", ErrParam, q.Group)
	}
	switch q.Source {
	case "", "internal", "gateway":
	default:
		return SeriesQuery{}, fmt.Errorf("%w: source %q", ErrParam, q.Source)
	}
	if q.From.IsZero() || q.To.IsZero() || !q.From.Before(q.To) {
		return SeriesQuery{}, ErrRange
	}
	if q.To.Sub(q.From) > MaxSpan {
		return SeriesQuery{}, ErrSpan
	}
	if q.Now.IsZero() {
		q.Now = time.Now()
	}
	q.Now = q.Now.UTC()
	q.From = floorBucket(q.From, q.Bucket)
	to := floorBucket(q.To, q.Bucket)
	if to.Before(q.To.UTC()) {
		to = to.Add(bucketStep(q.Bucket))
	}
	q.To = to
	return q, nil
}

// coverSQL reads both coverages in one statement: the rollup is complete
// from its first hour or from the hour migration 159 (the rollup and its
// backfill) was applied, whichever is earlier; the refusal ledger from the
// moment migration 167 was applied (it has no backfill). LEAST ignores a
// NULL; both NULL reads as no coverage.
const coverSQL = `
SELECT LEAST((SELECT min(bucket_start) FROM context_llm_usage_hourly),
             (SELECT date_trunc('hour', applied_at, 'UTC') FROM _migrations WHERE version = 159)),
       (SELECT applied_at FROM _migrations WHERE version = 167)`

// ReadCover reads the coverage of both sources in one statement.
func ReadCover(ctx context.Context, q pgxdb.Rower) (Cover, error) {
	var usage, rejections *time.Time
	if err := q.QueryRow(ctx, coverSQL).Scan(&usage, &rejections); err != nil {
		return Cover{}, fmt.Errorf("llmusage: read cover: %w", err)
	}
	var c Cover
	if usage != nil {
		c.UsageSince = usage.UTC()
	}
	if rejections != nil {
		c.RejectionsSince = rejections.UTC()
	}
	return c, nil
}

// histColumns are the rollup's three log2 histograms (design/05 §3.5).
var histColumns = [...]string{"latency_hist", "queue_wait_hist", "ttft_hist"}

// histBuckets is the bucket count of the histogram contract (design/05 §3.5).
const histBuckets = 16

// bucketExpr is the bucket of a rollup or ledger row.
func bucketExpr(b Bucket) string {
	if b == BucketDay {
		return "date_trunc('day', u.bucket_start, 'UTC')"
	}
	return "u.bucket_start"
}

// keyExpr is the series key of a rollup row for group g.
func keyExpr(g GroupBy) string {
	switch g {
	case GroupAlias:
		return "u.model_alias"
	case GroupPipelineName:
		return "u.pipeline"
	case GroupSource:
		return "u.source"
	case GroupBackend:
		return "u.backend_name"
	case GroupClass:
		return "u.dispatch_class"
	case GroupInstance:
		return "COALESCE((SELECT mc.peer_instance_id::text FROM context_model_consumers mc WHERE mc.id = u.consumer_id), '')"
	}
	return "COALESCE(u.consumer_id::text, '')"
}

// fromClause is the row source of table: every row of the window, or — for
// a consumer list — one probe per consumer, fenced by OFFSET 0 so the
// planner cannot flatten it into a join and each probe rides the
// consumer index (the windowSQL pattern; V-06-a).
func fromClause(table string, sq SeriesQuery) string {
	if sq.ConsumerIDs == nil {
		return table + ` u WHERE u.bucket_start >= $1 AND u.bucket_start < $2`
	}
	return `unnest($3::uuid[]) AS c(id)
  CROSS JOIN LATERAL (SELECT * FROM ` + table + ` r
                       WHERE r.consumer_id = c.id AND r.bucket_start >= $1 AND r.bucket_start < $2
                       OFFSET 0) u`
}

// SeriesSQL returns the statement text ReadSeries runs for sq (the plan
// gate EXPLAINs it): $1 From, $2 To (timestamptz), $3 ConsumerIDs (uuid[]
// as text[]) when sq.ConsumerIDs is non-nil; bucket, group, source and the
// exposure are fixed in the text. Every counter is summed per bucket and
// key; the histograms per bucket index (no SQL aggregate exists for
// bigint[]).
func SeriesSQL(sq SeriesQuery) string {
	var b strings.Builder
	b.WriteString("SELECT " + bucketExpr(sq.Bucket) + " AS b, " + keyExpr(sq.Group) + ` AS k,
  sum(u.calls)::bigint, sum(u.errors)::bigint, sum(u.estimated_calls)::bigint,
  sum(u.prompt_tokens)::bigint, sum(u.completion_tokens)::bigint,
  sum(u.duration_ms_sum)::bigint, sum(u.duration_n)::bigint, sum(u.cost_usd)::float8`)
	for _, col := range histColumns {
		for i := 1; i <= histBuckets; i++ {
			fmt.Fprintf(&b, ", sum(u.%s[%d])::bigint", col, i)
		}
	}
	b.WriteString("\nFROM " + fromClause("context_llm_usage_hourly", sq))
	if sq.Source != "" {
		// The value is one of two literals Align admitted.
		b.WriteString("\n  AND u.source = '" + sq.Source + "'")
	}
	b.WriteString("\nGROUP BY 1, 2")
	return b.String()
}

// refusalSQL reads the ledger of the window: per bucket, key, reason and
// detail — and target and dispatch class only for a server-admin. A key
// expression the ledger cannot answer (pipeline, source, instance; class
// and backend for a non-server-admin) yields no statement.
func refusalSQL(sq SeriesQuery) (string, bool) {
	var key string
	switch sq.Group {
	case GroupConsumer:
		key = "COALESCE(u.consumer_id::text, '')"
	case GroupAlias:
		key = "u.model_alias"
	case GroupClass:
		key = "u.dispatch_class"
	case GroupBackend:
		key = "u.target"
	default:
		return "", false
	}
	if !sq.ServerAdmin && (sq.Group == GroupClass || sq.Group == GroupBackend) {
		return "", false
	}
	exposed := "'', ''"
	if sq.ServerAdmin {
		exposed = "u.target, u.dispatch_class"
	}
	return "SELECT " + bucketExpr(sq.Bucket) + " AS b, " + key + ` AS k, u.reason, u.detail, ` + exposed + `,
  sum(u.n)::bigint, sum(u.wait_ms_sum)::bigint, sum(u.wait_n)::bigint
FROM ` + fromClause("context_llm_rejections_hourly", sq) + `
GROUP BY 1, 2, 3, 4, 5, 6`, true
}

// cell is the sums of one bucket × key.
type cell struct {
	calls, errors, estimated, prompt, completion, durSum, durN int64
	cost                                                       float64
	hist                                                       [len(histColumns)][]int64
}

// ReadSeries reads one aligned series request: the rollup in one statement,
// the ledger in a second one (none when the group has no ledger dimension or
// the consumer list is empty). Series are the keys with at least one row in
// the window, sorted by key; every series has one point per bucket from
// From to To.
func ReadSeries(ctx context.Context, q pgxdb.Querier, sq SeriesQuery, c Cover) (Result, error) {
	res := Result{From: sq.From, To: sq.To, Bucket: sq.Bucket, Group: sq.Group, Complete: true, Hist: true, Series: []Series{}}
	if sq.ConsumerIDs != nil && len(sq.ConsumerIDs) == 0 {
		// The same answer as a visible consumer without rows (below): a
		// foreign or unknown consumer is no oracle.
		res.Complete = emptyComplete(sq, c)
		return res, nil
	}
	args := []any{sq.From, sq.To}
	if sq.ConsumerIDs != nil {
		args = append(args, sq.ConsumerIDs)
	}
	cells, err := readCells(ctx, q, SeriesSQL(sq), args)
	if err != nil {
		return Result{}, err
	}

	refusals := map[string]map[time.Time][]Refusal{}
	sql, ok := refusalSQL(sq)
	if ok {
		if err := readRefusals(ctx, q, sql, args, sq.ServerAdmin, refusals); err != nil {
			return Result{}, err
		}
	}

	keys := make([]string, 0, len(cells)+len(refusals))
	for k := range cells {
		keys = append(keys, k)
	}
	for k := range refusals {
		if cells[k] == nil {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	step := bucketStep(sq.Bucket)
	for _, k := range keys {
		s := Series{Key: k}
		for at := sq.From; at.Before(sq.To); at = at.Add(step) {
			p := point(at, at.Add(step), sq.Now, c, cells[k][at])
			if ok && !at.Before(c.RejectionsSince) && !c.RejectionsSince.IsZero() {
				list := refusals[k][at]
				if list == nil {
					list = []Refusal{}
				}
				p.Rejected = &list
			}
			res.Complete = res.Complete && p.Complete
			s.Points = append(s.Points, p)
		}
		res.Series = append(res.Series, s)
	}
	if len(keys) == 0 {
		res.Complete = emptyComplete(sq, c)
	}
	return res, nil
}

// emptyComplete is the completeness of a window without series: covered
// from its first bucket on and over.
func emptyComplete(sq SeriesQuery, c Cover) bool {
	return !c.UsageSince.IsZero() && !sq.From.Before(c.UsageSince) && !sq.To.After(sq.Now)
}

// readCells runs the rollup statement and sums its rows per key and bucket.
func readCells(ctx context.Context, q pgxdb.Querier, sql string, args []any) (map[string]map[time.Time]*cell, error) {
	cells := map[string]map[time.Time]*cell{}
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("llmusage: read series: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			b   time.Time
			k   string
			cl  cell
			raw [len(histColumns) * histBuckets]int64
		)
		dest := []any{&b, &k, &cl.calls, &cl.errors, &cl.estimated, &cl.prompt, &cl.completion, &cl.durSum, &cl.durN, &cl.cost}
		for i := range raw {
			dest = append(dest, &raw[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("llmusage: scan series: %w", err)
		}
		for h := range histColumns {
			cl.hist[h] = append([]int64(nil), raw[h*histBuckets:(h+1)*histBuckets]...)
		}
		if cells[k] == nil {
			cells[k] = map[time.Time]*cell{}
		}
		cells[k][b.UTC()] = &cl
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("llmusage: series rows: %w", err)
	}
	return cells, nil
}

// readRefusals scans the ledger statement into out.
func readRefusals(ctx context.Context, q pgxdb.Querier, sql string, args []any, admin bool, out map[string]map[time.Time][]Refusal) error {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("llmusage: read refusals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			b                         time.Time
			k, reason, detail, tg, dc string
			n, waitSum, waitN         int64
		)
		if err := rows.Scan(&b, &k, &reason, &detail, &tg, &dc, &n, &waitSum, &waitN); err != nil {
			return fmt.Errorf("llmusage: scan refusals: %w", err)
		}
		r := Refusal{Reason: reason, Detail: detail, N: n}
		if waitN > 0 {
			avg := float64(waitSum) / float64(waitN)
			r.WaitMsAvg = &avg
		}
		if admin {
			if tg != "" {
				r.Target = &tg
			}
			if dc != "" {
				r.DispatchClass = &dc
			}
		}
		if out[k] == nil {
			out[k] = map[time.Time][]Refusal{}
		}
		out[k][b.UTC()] = append(out[k][b.UTC()], r)
	}
	return rows.Err()
}

// point builds the point of the bucket [start, end) from its sums (nil = no
// row): before the coverage every counter is null and the point incomplete;
// a covered bucket without rows is a real 0; the running bucket carries its
// partial values and is incomplete.
func point(start, end, now time.Time, c Cover, cl *cell) Point {
	p := Point{BucketStart: start}
	if c.UsageSince.IsZero() || start.Before(c.UsageSince) {
		return p
	}
	p.Complete = !end.After(now)
	if cl == nil {
		cl = &cell{}
	}
	i64 := func(v int64) *int64 { return &v }
	p.Calls, p.Errors, p.EstimatedCalls = i64(cl.calls), i64(cl.errors), i64(cl.estimated)
	p.PromptTokens, p.CompletionTokens, p.GPUMs = i64(cl.prompt), i64(cl.completion), i64(cl.durSum)
	cost := cl.cost
	p.CostUSD = &cost
	if cl.durN > 0 {
		avg := float64(cl.durSum) / float64(cl.durN)
		p.AvgMs = &avg
	}
	q := func(h []int64, x float64) *float64 {
		if v, ok := Quantile(h, x); ok {
			return &v
		}
		return nil
	}
	p.P50Ms, p.P95Ms = q(cl.hist[0], 0.5), q(cl.hist[0], 0.95)
	p.QueueP50, p.QueueP95 = q(cl.hist[1], 0.5), q(cl.hist[1], 0.95)
	p.TTFTP50, p.TTFTP95 = q(cl.hist[2], 0.5), q(cl.hist[2], 0.95)
	return p
}

// histBaseMs is the upper bound of bucket 0 (design/05 §3.5).
const histBaseMs = 60.0

// Quantile is the q-quantile (0 < q < 1) of a 16-bucket log2 histogram with
// the bucket bounds of design/05 §3.5 (base 60 ms; bucket 0 interpolated
// linearly from 0, the others geometrically between their bounds, the
// overflow bucket 15 answers its lower bound). ok = false for an empty or
// malformed histogram — the caller renders null, never 0.
func Quantile(hist []int64, q float64) (ms float64, ok bool) {
	if len(hist) != histBuckets || !(q > 0 && q < 1) {
		return 0, false
	}
	var total int64
	for _, v := range hist {
		if v < 0 {
			return 0, false
		}
		total += v
	}
	if total == 0 {
		return 0, false
	}
	rank := q * float64(total)
	var cum int64
	for i, v := range hist {
		if v == 0 || float64(cum+v) < rank {
			cum += v
			continue
		}
		f := (rank - float64(cum)) / float64(v)
		switch i {
		case 0:
			return f * histBaseMs, true
		case histBuckets - 1:
			return histBaseMs * math.Pow(2, float64(i-1)), true
		}
		return histBaseMs * math.Pow(2, float64(i-1)+f), true
	}
	return histBaseMs * math.Pow(2, histBuckets-2), true
}

// consumerDaysSQL reads the rollup part of the consumers list: per consumer
// and pipeline the last bucket of the last 30 days and the sums of the last
// 24 hours — one probe per consumer over idx_llm_usage_consumer.
const consumerDaysSQL = `
SELECT u.consumer_id::text, u.pipeline, max(u.bucket_start),
       COALESCE(sum(u.calls) FILTER (WHERE u.bucket_start >= $3), 0)::bigint,
       COALESCE(sum(u.prompt_tokens + u.completion_tokens) FILTER (WHERE u.bucket_start >= $3), 0)::bigint,
       COALESCE(sum(u.errors) FILTER (WHERE u.bucket_start >= $3), 0)::bigint
FROM unnest($1::uuid[]) AS c(id)
CROSS JOIN LATERAL (SELECT * FROM context_llm_usage_hourly r
                     WHERE r.consumer_id = c.id AND r.bucket_start >= $2
                     OFFSET 0) u
GROUP BY 1, 2`

// consumerRefusalsSQL reads the ledger part: refusals per consumer and op
// over the last 24 hours (idx_llm_rejections_consumer).
const consumerRefusalsSQL = `
SELECT u.consumer_id::text, u.op, sum(u.n)::bigint
FROM unnest($1::uuid[]) AS c(id)
CROSS JOIN LATERAL (SELECT * FROM context_llm_rejections_hourly r
                     WHERE r.consumer_id = c.id AND r.bucket_start >= $2
                     OFFSET 0) u
GROUP BY 1, 2`

// consumerLastWindow bounds the last-bucket scan of the consumers list (the
// llmlog list's 30-day floor, design/06 §4.8).
const consumerLastWindow = 30 * 24 * time.Hour

// ReadConsumerDays reads the rollup part (one statement) and the ledger
// part (one statement) of the consumers list for every listed consumer. An
// empty list costs no statement. The 24 hours are whole hours: the buckets
// from the hour 23 hours before now's hour on.
func ReadConsumerDays(ctx context.Context, q pgxdb.Querier, consumerIDs []string, now time.Time) ([]ConsumerDay, []ConsumerRefusals, error) {
	if len(consumerIDs) == 0 {
		return nil, nil, nil
	}
	hour := now.UTC().Truncate(time.Hour)
	day := hour.Add(-23 * time.Hour)
	rows, err := q.Query(ctx, consumerDaysSQL, consumerIDs, hour.Add(-consumerLastWindow), day)
	if err != nil {
		return nil, nil, fmt.Errorf("llmusage: read consumer days: %w", err)
	}
	days, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ConsumerDay, error) {
		var d ConsumerDay
		var last time.Time
		if err := r.Scan(&d.ConsumerID, &d.Pipeline, &last, &d.Calls24h, &d.Tokens24h, &d.Errors24h); err != nil {
			return d, err
		}
		last = last.UTC()
		d.LastBucket = &last
		return d, nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("llmusage: scan consumer days: %w", err)
	}
	rows, err = q.Query(ctx, consumerRefusalsSQL, consumerIDs, day)
	if err != nil {
		return nil, nil, fmt.Errorf("llmusage: read consumer refusals: %w", err)
	}
	refused, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ConsumerRefusals, error) {
		var x ConsumerRefusals
		return x, r.Scan(&x.ConsumerID, &x.Op, &x.Refused24h)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("llmusage: scan consumer refusals: %w", err)
	}
	return days, refused, nil
}

// splitSQL is usage_split_24h: calls and summed backend wall time per
// target × source × dispatch class over the buckets of the last 24 hours
// (idx_llm_usage_bucket), the running hour included.
const splitSQL = `
SELECT backend_name, source, dispatch_class, sum(calls)::bigint, sum(duration_ms_sum)::bigint
FROM context_llm_usage_hourly
WHERE bucket_start >= $1
GROUP BY 1, 2, 3
ORDER BY 1, 2, 3`

// ReadSplit24h reads usage_split_24h from the rollup in one statement: the
// 24 buckets from the hour 23 hours before now's hour on (whole hours — the
// status figure, not an exact rolling window).
func ReadSplit24h(ctx context.Context, q pgxdb.Querier, now time.Time) ([]SplitRow, error) {
	rows, err := q.Query(ctx, splitSQL, now.UTC().Truncate(time.Hour).Add(-23*time.Hour))
	if err != nil {
		return nil, fmt.Errorf("llmusage: read split: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (SplitRow, error) {
		var x SplitRow
		return x, r.Scan(&x.Target, &x.Source, &x.Class, &x.Calls, &x.GPUMs)
	})
	if err != nil {
		return nil, fmt.Errorf("llmusage: scan split: %w", err)
	}
	if out == nil {
		out = []SplitRow{}
	}
	return out, nil
}
