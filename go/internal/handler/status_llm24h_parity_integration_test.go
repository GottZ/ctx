//go:build integration

// W06-5 gate (b), as a property next to the golden cases: over random logs
// laid around the window's boundaries, both rollup readers (llmusage.Groups,
// TenantGroups, mapped to status rows by llm24hRowOf) serialise byte-equal to
// the raw-row reference queries — rows, order, llm_24h_complete and the
// whole tenant generation.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run TestStatusLLM24hParity -count=1 -v
//
// CTX_W065_SEEDS overrides the number of seeds (default 300).
package handler

import (
	"context"
	"encoding/json"
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

// llm24hParityRow is one random log row.
type llm24hParityRow struct {
	at                                  time.Time
	pipeline, host                      string
	backend, errText, cost, key, consum *string
	duration, ptok, ctok                *int
}

// llm24hScenario draws one clock and a log around its window: rows at since,
// edgeEnd and the hour before since — on the boundary, a microsecond either
// side, within 90 s or an hour of it — anywhere in the 24 hours, in the two
// hours after the clock, and before the window. Dimensions come from small
// sets, so that groups collect several rows from both sides of edgeEnd and
// tie on calls; a quarter of the clocks sit on a full hour, and half the
// seeds hold no successful row without a backend_name (complete stays true).
func llm24hScenario(r *rand.Rand, keys []string, consumer string) (time.Time, []llm24hParityRow) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(r.Int64N(3500)) * time.Hour)
	switch r.IntN(8) {
	case 0, 1:
	case 2:
		now = now.Add(time.Microsecond)
	case 3:
		now = now.Add(time.Hour - time.Microsecond)
	default:
		now = now.Add(time.Duration(r.Int64N(int64(time.Hour/time.Microsecond))) * time.Microsecond)
	}
	since, edgeEnd := llmusage.Bounds(now, llm24hSpan)
	anchors := []time.Time{since, edgeEnd, since.Truncate(time.Hour)}
	us := func(d time.Duration) int64 { return int64(d / time.Microsecond) }
	jitter := func() time.Duration {
		switch r.IntN(6) {
		case 0:
			return 0
		case 1:
			return time.Microsecond
		case 2:
			return -time.Microsecond
		case 3:
			return time.Duration(r.Int64N(us(180*time.Second))-us(90*time.Second)) * time.Microsecond
		default:
			return time.Duration(r.Int64N(us(2*time.Hour))-us(time.Hour)) * time.Microsecond
		}
	}
	pick := func(vals ...*string) *string { return vals[r.IntN(len(vals))] }
	s := func(v string) *string { return &v }
	i := func(v int) *int { return &v }
	hosts := []string{"spark", "herbert", ""}
	gaps := r.IntN(2) == 0 // half the seeds may hold a successful row without backend_name
	var rows []llm24hParityRow
	for n := 10 + r.IntN(50); n > 0; n-- {
		var at time.Time
		switch k := r.IntN(10); {
		case k < 6:
			at = anchors[r.IntN(len(anchors))].Add(jitter())
		case k < 8:
			at = now.Add(-time.Duration(r.Int64N(us(llm24hSpan))) * time.Microsecond)
		case k == 8:
			at = now.Add(time.Duration(r.Int64N(us(2*time.Hour))) * time.Microsecond)
		default:
			at = since.Add(-time.Duration(r.Int64N(us(48*time.Hour))) * time.Microsecond)
		}
		row := llm24hParityRow{
			at:       at,
			pipeline: []string{"dream-eval", "query-synthesize", "web-chat"}[r.IntN(3)],
			host:     hosts[r.IntN(len(hosts))],
			backend:  pick(s("spark-chat"), s("spark-chat"), s("spark-embed"), s(""), nil),
			errText:  pick(nil, nil, nil, nil, s("boom")),
			cost:     pick(nil, s("0"), s(fmt.Sprintf("%d.%08d", r.IntN(3), r.IntN(100_000_000)))),
		}
		if !gaps && row.backend == nil && row.errText == nil {
			row.errText = s("failed before a backend was chosen")
		}
		if r.IntN(5) > 0 {
			row.duration = i(r.IntN(4000))
		}
		if r.IntN(3) > 0 {
			row.ptok = i(r.IntN(500))
		}
		if r.IntN(3) > 0 {
			row.ctok = i(r.IntN(300))
		}
		switch k := r.IntN(10); {
		case k < 6:
			row.key = &keys[r.IntN(len(keys))]
		case k < 8:
			row.consum = &consumer
		}
		rows = append(rows, row)
	}
	return now, rows
}

func llm24hSeeds(t *testing.T) int {
	if v := os.Getenv("CTX_W065_SEEDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("CTX_W065_SEEDS=%q", v)
		}
		return n
	}
	return 300
}

// llm24hReaders runs both rollup readers and maps them as the collector does.
func llm24hReaders(ctx context.Context, tx pgx.Tx, now time.Time) (llm24hCase, error) {
	out := llm24hCase{Anchor: now.UTC().Format(time.RFC3339Nano), Server: []llm24hRow{}, Complete: true, Tenant: map[string][]llm24hRow{}}
	groups, err := llmusage.Groups(ctx, tx, now, llm24hSpan)
	if err != nil {
		return out, err
	}
	for _, g := range groups {
		if g.UnattributedOK > 0 {
			out.Complete = false
		}
		out.Server = append(out.Server, llm24hRowOf(g))
	}
	groups, err = llmusage.TenantGroups(ctx, tx, now, llm24hSpan)
	if err != nil {
		return out, err
	}
	for _, g := range groups {
		out.Tenant[g.Scope] = append(out.Tenant[g.Scope], llm24hRowOf(g))
	}
	return out, nil
}

func TestStatusLLM24hParity(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	ids := seedLLM24hIdentities(ctx, t, pool)
	keys := []string{ids.keyA1, ids.keyA2, ids.keyB, ids.keyC}
	// Create every chunk the seeds can touch once, then empty both tables: a
	// chunk created inside a seed's transaction goes with its rollback and
	// would be created again by the next seed. The rollup does not subtract a
	// DELETE, hence the TRUNCATE.
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_llm_log (created_at, pipeline, model, host)
		SELECT g, 'w065-chunk-anchor', 'm', 'h'
		  FROM generate_series('2026-05-20'::timestamptz, '2026-11-30'::timestamptz, interval '3 days') g;
		DELETE FROM context_llm_log;
		TRUNCATE context_llm_usage_hourly`); err != nil {
		t.Fatalf("chunks: %v", err)
	}

	seeds := llm24hSeeds(t)
	start := time.Now()
	var rowsTotal, edgeSeeds, onHourSeeds, tieSeeds, incomplete, bad int
	var first []string
	for seed := 0; seed < seeds; seed++ {
		now, rows := llm24hScenario(rand.New(rand.NewPCG(uint64(seed), 0x0605)), keys, ids.consumer)
		rowsTotal += len(rows)
		since, edgeEnd := llmusage.Bounds(now, llm24hSpan)
		if since.Equal(since.Truncate(time.Hour)) {
			onHourSeeds++
		}
		for _, r := range rows {
			if r.at.After(since) && r.at.Before(edgeEnd) {
				edgeSeeds++
				break
			}
		}
		ref, got, err := llm24hParitySeed(ctx, pool, now, rows)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		calls := map[int]int{}
		for _, r := range ref.Server {
			calls[r.Calls]++
		}
		for _, n := range calls {
			if n > 1 {
				tieSeeds++
				break
			}
		}
		if !ref.Complete {
			incomplete++
		}
		rb, _ := json.Marshal(ref)
		gb, _ := json.Marshal(got)
		if string(rb) != string(gb) {
			bad++
			if len(first) < 3 {
				first = append(first, fmt.Sprintf("seed %d (now %s):\n  rollup %s\n  raw    %s", seed, now.Format(time.RFC3339Nano), gb, rb))
			}
		}
	}
	t.Logf("%d seeds, %d log rows, %d seeds with rows in the raw edge, %d with since on a full hour, %d with tied calls, %d incomplete; %d seeds differ (%s)",
		seeds, rowsTotal, edgeSeeds, onHourSeeds, tieSeeds, incomplete, bad, time.Since(start).Round(time.Millisecond))
	if bad > 0 {
		t.Errorf("%d of %d seeds differ from the raw rows; first:\n%s", bad, seeds, strings.Join(first, "\n"))
	}
	if edgeSeeds < seeds/2 || tieSeeds < seeds/4 || incomplete < seeds/4 || seeds-incomplete < seeds/4 {
		t.Errorf("%d edge seeds, %d tie seeds and %d incomplete seeds of %d — the generator no longer probes the edge, the order or the flag",
			edgeSeeds, tieSeeds, incomplete, seeds)
	}
}

// llm24hParitySeed lays one seed in its own transaction, reads both sides and
// rolls back — the rollup rows the trigger wrote go with it.
func llm24hParitySeed(ctx context.Context, pool *pgxpool.Pool, now time.Time, rows []llm24hParityRow) (ref, got llm24hCase, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return ref, got, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, r := range rows {
		if _, err := tx.Exec(ctx, `
			INSERT INTO context_llm_log (created_at, pipeline, model, host, backend_name, duration_ms, error,
			                             prompt_tokens, completion_tokens, cost_usd, api_key_id, consumer_id)
			VALUES ($1, $2, 'm', $3, $4, $5, $6, $7, $8, $9::numeric, $10::uuid, $11::uuid)`,
			r.at, r.pipeline, r.host, r.backend, r.duration, r.errText, r.ptok, r.ctok, r.cost, r.key, r.consum); err != nil {
			return ref, got, fmt.Errorf("insert: %w", err)
		}
	}
	if ref, err = refLLM24h(ctx, tx, now); err != nil {
		return ref, got, err
	}
	got, err = llm24hReaders(ctx, tx, now)
	return ref, got, err
}
