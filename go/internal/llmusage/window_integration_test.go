//go:build integration

// W06-4 gates (b) and (e) against a real PG18 + TimescaleDB with migration
// 159: a window read from the hourly rollup plus its raw edge hour equals the
// raw-row query it replaces, row for row, for random logs laid around the
// window boundaries; and the earliest relief is the oldest counted row's
// created_at + the span, to the microsecond.
//
//	go test -tags=integration -p 1 ./internal/llmusage/ -count=1 -v
//
// CTX_W064_SEEDS overrides the number of parity seeds (default 500).
package llmusage_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/llmusage"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// rawSpendSQL is QuotaAccountant.spend as it stood before W06-4
// (internal/backends/quota.go at 43041709), verbatim except that now() is the
// bound $2, so that a seed can place rows exactly on the window boundaries.
const rawSpendSQL = `
		SELECT
		  COALESCE(SUM(cost_usd) FILTER (
		      WHERE created_at > $2::timestamptz - interval '24 hours' AND backend_locality = 'external'), 0),
		  COALESCE(SUM(cost_usd) FILTER (
		      WHERE backend_locality = 'external'), 0),
		  COUNT(*) FILTER (WHERE created_at > $2::timestamptz - interval '24 hours')::int
		FROM context_llm_log
		WHERE api_key_id = ANY($1::uuid[])
		  AND created_at > $2::timestamptz - interval '30 days'`

// rawWindowSQL is one window over the raw rows: the reference for every
// field of llmusage.Totals.
const rawWindowSQL = `
SELECT count(*), COALESCE(sum(cost_usd), 0), min(created_at), min(created_at) FILTER (WHERE cost_usd > 0)
  FROM context_llm_log
 WHERE api_key_id = ANY($1::uuid[])
   AND created_at > $2::timestamptz - $3::interval
   AND ($4 = '' OR backend_locality = $4)`

// spanInterval is how the raw queries spell each span.
var spanInterval = map[time.Duration]string{
	24 * time.Hour:      "24 hours",
	30 * 24 * time.Hour: "30 days",
}

type logRow struct {
	at                            time.Time
	key, consumer, locality, cost *string
	errText                       *string
}

func sp(s string) *string { return &s }

func randUUID(r *rand.Rand) string {
	var b [16]byte
	for i := range b {
		b[i] = byte(r.UintN(256))
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// scenario is one parity seed: a clock reading, the tenant's keys and a log.
type scenario struct {
	now    time.Time
	tenant []string
	rows   []logRow
}

// genScenario lays rows around the four boundaries that matter — since and
// the edge end of the 24-hour and the 30-day window — at the boundary itself,
// one microsecond either side and within seconds and an hour of it, plus rows
// anywhere in the 31 days, a few in the future and some long gone. A quarter
// of the clocks sit exactly on a full hour, so since does too.
func genScenario(r *rand.Rand) scenario {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	now := base.Add(time.Duration(r.Int64N(4000)) * time.Hour)
	switch r.IntN(8) {
	case 0, 1: // on the hour
	case 2:
		now = now.Add(time.Microsecond)
	case 3:
		now = now.Add(time.Hour - time.Microsecond)
	default:
		now = now.Add(time.Duration(r.Int64N(int64(time.Hour/time.Microsecond))) * time.Microsecond)
	}
	sc := scenario{now: now}
	for i := 0; i < 1+r.IntN(3); i++ {
		sc.tenant = append(sc.tenant, randUUID(r))
	}
	foreign := randUUID(r)
	consumer := randUUID(r)

	var anchors []time.Time
	for _, span := range []time.Duration{24 * time.Hour, 30 * 24 * time.Hour} {
		since, edgeEnd := llmusage.Bounds(now, span)
		anchors = append(anchors, since, edgeEnd, since.Truncate(time.Hour))
	}
	jitter := func() time.Duration {
		switch r.IntN(6) {
		case 0:
			return 0
		case 1:
			return time.Microsecond
		case 2:
			return -time.Microsecond
		case 3:
			return time.Duration(r.Int64N(180_000_000)-90_000_000) * time.Microsecond // ±90 s
		default:
			return time.Duration(r.Int64N(7_200_000_000)-3_600_000_000) * time.Microsecond // ±1 h
		}
	}
	localities := []*string{sp("external"), sp("external"), sp("external"), sp("local"), sp("local"), sp("lan"), nil}
	n := 10 + r.IntN(60)
	for i := 0; i < n; i++ {
		var at time.Time
		switch k := r.IntN(10); {
		case k < 6:
			at = anchors[r.IntN(len(anchors))].Add(jitter())
		case k == 6:
			at = now.Add(-time.Duration(r.Int64N(int64(31*24*time.Hour/time.Microsecond))) * time.Microsecond)
		case k == 7:
			at = now.Add(-time.Duration(r.Int64N(int64(25*time.Hour/time.Microsecond))) * time.Microsecond)
		case k == 8:
			at = now.Add(time.Duration(r.Int64N(int64(4*time.Hour/time.Microsecond))-int64(2*time.Hour/time.Microsecond)) * time.Microsecond)
		default:
			at = now.Add(-31*24*time.Hour - time.Duration(r.Int64N(int64(10*24*time.Hour/time.Microsecond)))*time.Microsecond)
		}
		row := logRow{at: at, locality: localities[r.IntN(len(localities))]}
		switch k := r.IntN(10); {
		case k < 6:
			row.key = sp(sc.tenant[r.IntN(len(sc.tenant))])
		case k == 6:
			row.key = sp(foreign)
		case k < 9:
			row.consumer = sp(consumer) // a /v1 consumer row: api_key_id NULL
		}
		switch r.IntN(4) {
		case 0:
		case 1:
			row.cost = sp("0")
		default:
			row.cost = sp(fmt.Sprintf("%d.%08d", r.IntN(1000), r.IntN(100_000_000)))
		}
		if r.IntN(5) == 0 {
			row.errText = sp("boom")
		}
		sc.rows = append(sc.rows, row)
	}
	return sc
}

func insertRows(ctx context.Context, tx pgx.Tx, rows []logRow) error {
	at := make([]time.Time, len(rows))
	var key, consumer, loc, cost, errText []*string
	for i, r := range rows {
		at[i] = r.at
		key, consumer, loc, cost, errText = append(key, r.key), append(consumer, r.consumer),
			append(loc, r.locality), append(cost, r.cost), append(errText, r.errText)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO context_llm_log (created_at, pipeline, model, host, duration_ms,
		                             api_key_id, consumer_id, backend_locality, cost_usd, error)
		SELECT t, 'w064-parity', 'm', 'h', 10, k::uuid, c::uuid, loc, cost::numeric, err
		  FROM unnest($1::timestamptz[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[])
		       AS u(t, k, c, loc, cost, err)`, at, key, consumer, loc, cost, errText)
	return err
}

// edgeRows counts the rows of keys inside a window's raw edge.
func edgeRows(sc scenario, keys []string, span time.Duration) int {
	since, edgeEnd := llmusage.Bounds(sc.now, span)
	member := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		member[k] = struct{}{}
	}
	n := 0
	for _, r := range sc.rows {
		if r.key == nil {
			continue
		}
		if _, ok := member[*r.key]; !ok {
			continue
		}
		if r.at.After(since) && r.at.Before(edgeEnd) {
			n++
		}
	}
	return n
}

func reliefOf(oldest *time.Time, span time.Duration) time.Time {
	if oldest == nil {
		return time.Time{}
	}
	return oldest.UTC().Add(span)
}

// compareSeed checks every window and the spend trio for one key set.
func compareSeed(ctx context.Context, tx pgx.Tx, now time.Time, keys []string) ([]string, error) {
	var diffs []string
	for span, iv := range spanInterval {
		for _, loc := range []string{"", "external", "local"} {
			got, err := llmusage.Window(ctx, tx, llmusage.Spec{Now: now, Span: span, APIKeyIDs: keys, Locality: loc})
			if err != nil {
				return nil, err
			}
			var calls int64
			var cost float64
			var oldestCall, oldestCost *time.Time
			if err := tx.QueryRow(ctx, rawWindowSQL, keys, now, iv, loc).Scan(&calls, &cost, &oldestCall, &oldestCost); err != nil {
				return nil, fmt.Errorf("raw window: %w", err)
			}
			want := llmusage.Totals{Calls: calls, Cost: cost, CallsRelief: reliefOf(oldestCall, span), CostRelief: reliefOf(oldestCost, span)}
			if got.Calls != want.Calls || got.Cost != want.Cost ||
				!got.CallsRelief.Equal(want.CallsRelief) || !got.CostRelief.Equal(want.CostRelief) {
				diffs = append(diffs, fmt.Sprintf("span %s locality %q: rollup {calls %d cost %v relief %s / %s}, raw {calls %d cost %v relief %s / %s}",
					span, loc, got.Calls, got.Cost, got.CallsRelief, got.CostRelief, want.Calls, want.Cost, want.CallsRelief, want.CostRelief))
			}
		}
	}

	// The spend trio, composed exactly as QuotaAccountant.spend composes it.
	day, err := llmusage.Window(ctx, tx, llmusage.Spec{Now: now, Span: 24 * time.Hour, APIKeyIDs: keys})
	if err != nil {
		return nil, err
	}
	dayExt, err := llmusage.Window(ctx, tx, llmusage.Spec{Now: now, Span: 24 * time.Hour, APIKeyIDs: keys, Locality: "external"})
	if err != nil {
		return nil, err
	}
	monthExt, err := llmusage.Window(ctx, tx, llmusage.Spec{Now: now, Span: 30 * 24 * time.Hour, APIKeyIDs: keys, Locality: "external"})
	if err != nil {
		return nil, err
	}
	var dayCost, monthCost float64
	var dayCalls int
	if err := tx.QueryRow(ctx, rawSpendSQL, keys, now).Scan(&dayCost, &monthCost, &dayCalls); err != nil {
		return nil, fmt.Errorf("raw spend: %w", err)
	}
	if dayCost != dayExt.Cost || monthCost != monthExt.Cost || dayCalls != int(day.Calls) {
		diffs = append(diffs, fmt.Sprintf("spend: rollup (dayCost %v, monthCost %v, dayCalls %d), raw (%v, %v, %d)",
			dayExt.Cost, monthExt.Cost, day.Calls, dayCost, monthCost, dayCalls))
	}
	return diffs, nil
}

func seedCount(t *testing.T) int {
	if v := os.Getenv("CTX_W064_SEEDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("CTX_W064_SEEDS=%q", v)
		}
		return n
	}
	return 500
}

// TestWindowParity (gate b): over random logs laid around the boundaries of
// both windows, every window (24 h and 30 d, every locality, the tenant's keys
// and a single key) equals the raw-row query, and the spend trio equals the
// spend SQL of before W06-4. Each seed runs in its own transaction and rolls
// back, so the rollup the trigger wrote goes with it.
func TestWindowParity(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	var tz string
	if err := pool.QueryRow(ctx, "SHOW TimeZone").Scan(&tz); err != nil {
		t.Fatal(err)
	}
	// Create every chunk the seeds can touch once, up front: a chunk created
	// inside a seed's transaction goes with its rollback and would be created
	// again by the next seed (DDL per seed). The anchor rows carry no key, so
	// no window ever counts them.
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_llm_log (created_at, pipeline, model, host)
		SELECT g, 'w064-chunk-anchor', 'm', 'h'
		  FROM generate_series($1::timestamptz, $2::timestamptz, interval '3 days') g`,
		time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("chunk anchors: %v", err)
	}
	seeds := seedCount(t)
	start := time.Now()
	var edgeSeeds, onHourSeeds, rowsTotal, badSeeds int
	var first []string
	for seed := 0; seed < seeds; seed++ {
		sc := genScenario(rand.New(rand.NewPCG(uint64(seed), 0x0604)))
		rowsTotal += len(sc.rows)
		if since, _ := llmusage.Bounds(sc.now, 24*time.Hour); since.Equal(since.Truncate(time.Hour)) {
			onHourSeeds++
		}
		if edgeRows(sc, sc.tenant, 24*time.Hour)+edgeRows(sc, sc.tenant, 30*24*time.Hour) > 0 {
			edgeSeeds++
		}
		diffs, err := runSeed(ctx, pool, sc)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if len(diffs) > 0 {
			badSeeds++
			if len(first) < 5 {
				first = append(first, fmt.Sprintf("seed %d (now %s): %s", seed, sc.now.Format(time.RFC3339Nano), diffs[0]))
			}
		}
	}
	t.Logf("session TimeZone %s; %d seeds, %d log rows, %d seeds with tenant rows in a raw edge, %d seeds with since on a full hour; %d seeds differ (%s)",
		tz, seeds, rowsTotal, edgeSeeds, onHourSeeds, badSeeds, time.Since(start).Round(time.Millisecond))
	if badSeeds > 0 {
		t.Errorf("%d of %d seeds differ from the raw rows; first:\n%s", badSeeds, seeds, strings.Join(first, "\n"))
	}
	if edgeSeeds < seeds/4 {
		t.Errorf("only %d of %d seeds put a tenant row into a raw edge — the generator no longer probes the boundary", edgeSeeds, seeds)
	}
}

func runSeed(ctx context.Context, pool *pgxpool.Pool, sc scenario) ([]string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The reference's interval '30 days' is calendar arithmetic in the
	// session's time zone — 720 hours only in UTC, which is what ctx's
	// sessions run (live: Etc/UTC). Pin it, so that a container in a DST zone
	// cannot turn a 719- or 721-hour reference into a false difference; the
	// window itself is always exactly the span.
	if _, err := tx.Exec(ctx, "SET LOCAL TimeZone = 'UTC'"); err != nil {
		return nil, err
	}
	if err := insertRows(ctx, tx, sc.rows); err != nil {
		return nil, fmt.Errorf("insert: %w", err)
	}
	diffs, err := compareSeed(ctx, tx, sc.now, sc.tenant)
	if err != nil {
		return nil, err
	}
	if len(sc.tenant) > 1 {
		more, err := compareSeed(ctx, tx, sc.now, sc.tenant[:1])
		if err != nil {
			return nil, err
		}
		diffs = append(diffs, more...)
	}
	return diffs, nil
}

// TestWindowReliefExact (gate e): the earliest relief is the oldest counted
// row's created_at + span, to the microsecond — from the edge when the edge
// holds the oldest row, from the oldest rollup hour (not its start) when it
// does not, and for the cost only from a row that carries a cost.
func TestWindowReliefExact(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	at := func(s string) time.Time {
		v, err := time.Parse("2006-01-02 15:04:05.999999", s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	const day = 24 * time.Hour
	key := "0196f0a0-0000-7000-8000-000000000001"
	cases := []struct {
		name               string
		now                string
		rows               []logRow
		locality           string
		span               time.Duration
		calls              int64
		callsRelief, costR string // "" = zero
	}{
		{name: "oldest row in the edge", now: "2026-11-03 10:20:00", span: day,
			rows: []logRow{
				{at: at("2026-11-02 10:19:59.999999"), key: sp(key)}, // before since: not counted
				{at: at("2026-11-02 10:30:00.000042"), key: sp(key)},
				{at: at("2026-11-02 12:15:00"), key: sp(key), cost: sp("0.5")},
				{at: at("2026-11-03 09:20:00"), key: sp(key)},
			},
			calls: 3, callsRelief: "2026-11-03 10:30:00.000042", costR: "2026-11-03 12:15:00"},
		{name: "edge empty: the oldest row of the oldest rollup hour, not the hour's start", now: "2026-11-03 10:20:00", span: day,
			rows: []logRow{
				{at: at("2026-11-02 09:00:00"), key: sp(key), cost: sp("1")}, // outside
				{at: at("2026-11-02 11:42:17.123456"), key: sp(key)},
				{at: at("2026-11-02 11:05:00.000007"), key: sp(key)},
				{at: at("2026-11-02 14:00:00"), key: sp(key), cost: sp("0")},
				{at: at("2026-11-02 15:30:00"), key: sp(key), cost: sp("0.00000001")},
			},
			calls: 4, callsRelief: "2026-11-03 11:05:00.000007", costR: "2026-11-03 15:30:00"},
		{name: "cost relief skips rows without a cost and other localities", now: "2026-11-03 10:20:00", span: day, locality: "external",
			rows: []logRow{
				{at: at("2026-11-02 10:40:00"), key: sp(key), locality: sp("external")},                   // no cost
				{at: at("2026-11-02 10:45:00"), key: sp(key), locality: sp("local"), cost: sp("2")},       // other locality
				{at: at("2026-11-02 10:50:00"), key: sp(key), locality: sp("external"), cost: sp("0")},    // zero cost
				{at: at("2026-11-02 13:10:00"), key: sp(key), locality: sp("external"), cost: sp("0.25")}, // the one
			},
			calls: 3, callsRelief: "2026-11-03 10:40:00", costR: "2026-11-03 13:10:00"},
		{name: "since on a full hour: a row exactly at since is out, one microsecond later is in", now: "2026-11-03 10:00:00", span: day,
			rows: []logRow{
				{at: at("2026-11-02 10:00:00"), key: sp(key), cost: sp("9")},
				{at: at("2026-11-02 10:00:00.000001"), key: sp(key), cost: sp("1")},
			},
			calls: 1, callsRelief: "2026-11-03 10:00:00.000001", costR: "2026-11-03 10:00:00.000001"},
		{name: "30 days: relief thirty days after the oldest cost row", now: "2026-11-03 10:20:00", span: 30 * day, locality: "external",
			rows: []logRow{
				{at: at("2026-10-04 10:25:00"), key: sp(key), locality: sp("external"), cost: sp("0.1")},
				{at: at("2026-10-20 08:00:00"), key: sp(key), locality: sp("external"), cost: sp("0.1")},
			},
			calls: 2, callsRelief: "2026-11-03 10:25:00", costR: "2026-11-03 10:25:00"},
		{name: "consumer rows and an empty window: no relief", now: "2026-11-03 10:20:00", span: day,
			rows: []logRow{
				{at: at("2026-11-03 09:00:00"), consumer: sp("0196f0a0-0000-7000-8000-0000000000c1"), cost: sp("3")},
			},
			calls: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if err := insertRows(ctx, tx, c.rows); err != nil {
				t.Fatal(err)
			}
			got, err := llmusage.Window(ctx, tx, llmusage.Spec{Now: at(c.now), Span: c.span, APIKeyIDs: []string{key}, Locality: c.locality})
			if err != nil {
				t.Fatal(err)
			}
			want := func(s string) time.Time {
				if s == "" {
					return time.Time{}
				}
				return at(s)
			}
			if got.Calls != c.calls || !got.CallsRelief.Equal(want(c.callsRelief)) || !got.CostRelief.Equal(want(c.costR)) {
				t.Errorf("calls %d relief %s / %s, want %d relief %s / %s",
					got.Calls, got.CallsRelief, got.CostRelief, c.calls, want(c.callsRelief), want(c.costR))
			}
		})
	}
}
