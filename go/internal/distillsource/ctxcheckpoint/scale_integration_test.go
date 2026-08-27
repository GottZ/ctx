//go:build integration

// Gate A02-3, scale half: index usage and latency at the target scale.
//
// The wave briefing points this at a "1M copy". The measuring copy that exists
// on this host (ctx-messkopie-db) is a 1:1 clone of the live corpus — 7887
// blocks, measured — so it is not one, and it is read-only for this wave
// besides. The scale evidence is therefore produced here, in a container this
// test owns: one million checkpoint blocks, the real index from migration 135,
// and both the green and the red formulation of every query run against the
// same rows. That is reproducible by anyone running the suite, which a hand
// measurement against a borrowed database would not be.
//
// Row budget is overridable via CTX_A02_3_SCALE_N for a faster local loop; the
// default is the number the gate names.

package ctxcheckpoint_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/distillsource/ctxcheckpoint"
	"github.com/GottZ/ctx/internal/testdb"
)

// scaleRoots is the number of root sessions the fixture spreads its manifests
// over, and scaleParts the parts per manifest. The product with scaleN decides
// how selective the root predicate is, which is the whole point of the measure.
const (
	scaleRoots = 4_000
	scaleParts = 56 // the live maximum
)

func scaleN(t *testing.T) int {
	if v := os.Getenv("CTX_A02_3_SCALE_N"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1000 {
			t.Fatalf("CTX_A02_3_SCALE_N = %q: want an integer >= 1000", v)
		}
		return n
	}
	return 1_000_000
}

// scaleRoot is the root the measurements address. It must be a root the fixture
// actually fills, which the block layout below guarantees.
const scaleRoot = "root-7"

// scaleHorizon is the window the latency gate runs with: the design default for
// distill.ctx_session_horizon.
const scaleHorizon = 30 * 24 * time.Hour

// liveBlocksPerDay is the measured ingest rate of the live checkpoint corpus:
// 5961 blocks over 42.5 days. It is what turns a row COUNT into a corpus TIME
// SPAN, and that span is the quantity the horizon is selective against.
//
// The first version of this fixture ignored it and laid down one row per
// second, so a million rows spanned 11.6 days and the 30-day default covered
// the ENTIRE corpus — the horizon then had nothing to cut, which is what made
// it look ineffective. At the real rate a million blocks span ~19.6 years and a
// 30-day window is a fraction of a percent. Same query, opposite conclusion:
// the fixture's time compression WAS the finding.
const liveBlocksPerDay = 140.1

// scaleSpread returns the wall-clock span n rows occupy at the live ingest rate.
func scaleSpread(n int) time.Duration {
	return time.Duration(float64(n) / liveBlocksPerDay * 24 * float64(time.Hour))
}

// seedScale loads n checkpoint blocks laid out the way the corpus is: blocks
// come in groups of scaleParts, each group is one manifest LISTING its
// scaleParts-1 parts by id, and consecutive groups round-robin over scaleRoots
// roots.
//
// The ids are deterministic (the same trick the million-row fixtures in this
// tree use), which is what lets a manifest name its own parts inside a single
// generate_series insert. Without that the manifests carry empty part lists,
// Read resolves nothing, and the latency gate would measure an empty answer
// while looking green.
//
// Row triggers are disabled for the load — context_blocks carries three
// FOR EACH ROW triggers and the NOTIFY duplicate check in one of them is
// quadratic, which turns a bulk load into hours.
func seedScale(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int) time.Duration {
	t.Helper()
	start := time.Now()

	if _, err := pool.Exec(ctx, `ALTER TABLE context_blocks DISABLE TRIGGER USER`); err != nil {
		t.Fatalf("disable triggers: %v", err)
	}
	defer func() {
		if _, err := pool.Exec(ctx, `ALTER TABLE context_blocks ENABLE TRIGGER USER`); err != nil {
			t.Fatalf("enable triggers: %v", err)
		}
	}()

	// The body is short on purpose: this fixture measures PLAN SHAPE and row
	// access, not detoasting. A 36000-character body per row would make the
	// table 36 GB and measure the disk, not the index.
	const q = `
INSERT INTO context_blocks (id, category, title, content, scope, type_name, metadata, created_at)
SELECT ('019f2210-0000-7000-9000-' || lpad(to_hex(i), 12, '0'))::uuid,
       $3,
       'checkpoint scale ' || i,
       '# head' || E'\n\n' || '## Compaction source evidence' || E'\n\n' ||
       '## Direct transcript' || E'\n\n' || '### Message 1 — user' || E'\n' || repeat('x', 400),
       $4,
       'checkpoint',
       CASE WHEN i % $5 = 0
            THEN jsonb_build_object(
                   'root_session_id', 'root-' || ((i / $5) % $6)::text,
                   'source_block_ids',
                   (SELECT to_jsonb(array_agg(
                             '019f2210-0000-7000-9000-' || lpad(to_hex(p), 12, '0')
                             ORDER BY p))
                      FROM generate_series(i + 1, i + $5 - 1) AS s(p)))
            ELSE jsonb_build_object(
                   'root_session_id', 'root-' || ((i / $5) % $6)::text,
                   'part', '1')
       END,
       now() - ((($7 - i) * $8::double precision) || ' seconds')::interval
  FROM generate_series($1::bigint, $2::bigint) AS g(i)`

	// Seconds between two consecutive rows at the live ingest rate.
	secondsPerRow := scaleSpread(n).Seconds() / float64(n)

	const chunk = 50_000
	for lo := 0; lo < n; lo += chunk {
		hi := lo + chunk - 1
		if hi > n-1 {
			hi = n - 1
		}
		if _, err := pool.Exec(ctx, q, lo, hi, fxCategory, fxScope, scaleParts, scaleRoots, n, secondsPerRow); err != nil {
			t.Fatalf("seed rows %d..%d: %v", lo, hi, err)
		}
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	// The fixture is only worth measuring if the addressed root really holds
	// manifests with resolvable parts, AND if the horizon the tests use still
	// selects rows. Asserted, not assumed: the first version of this layout
	// produced neither, and the second one dated every row to January so a
	// now()-relative horizon selected nothing — both would have measured an
	// empty answer while reporting green.
	var manifests, listed, recent int
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM context_blocks
         WHERE type_name = 'checkpoint' AND scope = $1 AND category = $2
           AND metadata->>'root_session_id' = $3 AND metadata ? 'source_block_ids'),
       (SELECT COALESCE(sum(jsonb_array_length(metadata->'source_block_ids')), 0)
          FROM context_blocks
         WHERE type_name = 'checkpoint' AND scope = $1 AND category = $2
           AND metadata->>'root_session_id' = $3 AND metadata ? 'source_block_ids'),
       (SELECT count(DISTINCT metadata->>'root_session_id') FROM context_blocks
         WHERE type_name = 'checkpoint' AND scope = $1 AND category = $2
           AND metadata ? 'source_block_ids' AND created_at > now() - $4::interval)`,
		fxScope, fxCategory, scaleRoot, scaleHorizon.String()).Scan(&manifests, &listed, &recent); err != nil {
		t.Fatalf("fixture self-check: %v", err)
	}
	switch {
	case manifests == 0 || listed == 0:
		t.Fatalf("fixture self-check: root %q holds %d manifests listing %d parts — nothing to measure",
			scaleRoot, manifests, listed)
	case recent == 0:
		t.Fatalf("fixture self-check: no root falls inside the %s horizon — Sessions would measure an empty answer",
			scaleHorizon)
	}
	t.Logf("fixture self-check: root %q holds %d manifests listing %d parts; %d roots inside the %s horizon",
		scaleRoot, manifests, listed, recent, scaleHorizon)
	t.Logf("fixture time span: %d rows at %.1f blocks/day = %.1f days (%.2f years); horizon %s covers %.3f %% of it",
		n, liveBlocksPerDay, scaleSpread(n).Hours()/24, scaleSpread(n).Hours()/24/365.25,
		scaleHorizon, 100*scaleHorizon.Seconds()/scaleSpread(n).Seconds())
	return time.Since(start)
}

func explain(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	rows, err := pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+sql, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("explain scan: %v", err)
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows: %v", err)
	}
	return sb.String()
}

// TestReadUsesCheckpointRootIndexAtScale is the index gate.
//
// Green is the equality formulation the reader ships. Red is the
// IS DISTINCT FROM formulation: it is the tell-tale probe, because the
// IS NOT NULL line in the query is documentation rather than mechanism — PG18
// derives it from the strict equality and the two plans are identical. Only
// replacing the equality actually loses the index.
func TestReadUsesCheckpointRootIndexAtScale(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	pool := testdb.SetupTestDB(t)

	n := scaleN(t)
	t.Logf("fixture: %d rows in %s", n, seedScale(t, ctx, pool, n))

	const selectList = `
SELECT id::text,
       (EXTRACT(EPOCH FROM created_at) * 1000000)::BIGINT AS wm,
       ARRAY(SELECT jsonb_array_elements_text(metadata->'source_block_ids'))
  FROM context_blocks
 WHERE (metadata->>'root_session_id') IS NOT NULL
   AND `
	const tail = `
   AND type_name = 'checkpoint' AND scope = '` + fxScope + `' AND category = '` + fxCategory + `'
   AND NOT is_archived
   AND metadata ? 'source_block_ids'
   AND (EXTRACT(EPOCH FROM created_at) * 1000000)::BIGINT > 0
 ORDER BY created_at ASC, id ASC
 LIMIT 400`

	green := explain(t, ctx, pool, selectList+`metadata->>'root_session_id' = '`+scaleRoot+`'`+tail)
	red := explain(t, ctx, pool, selectList+`metadata->>'root_session_id' IS DISTINCT FROM '`+scaleRoot+`'`+tail)

	if !strings.Contains(green, "idx_blocks_checkpoint_root") {
		t.Errorf("green plan does not use idx_blocks_checkpoint_root:\n%s", green)
	}
	if strings.Contains(red, "idx_blocks_checkpoint_root") {
		t.Errorf("red plan (IS DISTINCT FROM) still uses the index — the probe is not discriminating:\n%s", red)
	}
	t.Logf("green plan:\n%s", green)
	t.Logf("red plan (IS DISTINCT FROM):\n%s", red)
}

// TestSessionsAvoidsSeqScanAtScale is the candidate-query gate — the one the
// first design draft did not have, and the gap through which a wrong finding
// fell.
//
// It probes THREE formulations, because two axes are at work and only measuring
// both tells them apart:
//
//   - redFull is the design's full aggregation over every checkpoint block
//     (no manifest filter, no horizon). That is the shape the finding was made
//     on, and it is the one that produces a Seq Scan.
//   - redNoHorizon is the shipped query WITHOUT the horizon. It avoids the Seq
//     Scan by itself, because restricting to manifests is already selective —
//     but its cost is proportional to the number of MANIFESTS IN THE CORPUS,
//     which is exactly the quantity that grows without bound.
//   - green is the shipped query.
//
// The gate is the plan shape: redFull carries a Seq Scan, green does not.
//
// The buffer counts are MEASURED and logged beside the plan shape. Whether the
// horizon reduces them depends on how much of the corpus the window selects —
// see TestSessionsHorizonCrossoverAtScale, which measures that directly. They
// are logged rather than asserted because the threshold that would make them an
// assertion is exactly the quantity under investigation.
func TestSessionsAvoidsSeqScanAtScale(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	pool := testdb.SetupTestDB(t)

	n := scaleN(t)
	t.Logf("fixture: %d rows in %s", n, seedScale(t, ctx, pool, n))

	const head = `
SELECT metadata->>'root_session_id' AS root
  FROM context_blocks
 WHERE (metadata->>'root_session_id') IS NOT NULL
   AND type_name = 'checkpoint' AND scope = '` + fxScope + `' AND category = '` + fxCategory + `'
   AND NOT is_archived
   AND `
	const manifestOnly = `metadata ? 'source_block_ids'
   AND `
	const tail = `
 GROUP BY 1
 ORDER BY max(created_at) DESC
 LIMIT 4`

	// Same window the shipped query would compute from Options.SessionHorizon,
	// expressed in SQL so the plan is comparable.
	horizon := fmt.Sprintf("created_at > now() - interval '%d seconds'", int(scaleHorizon.Seconds()))

	redFull := explain(t, ctx, pool, head+"TRUE"+tail)
	redNoHorizon := explain(t, ctx, pool, head+manifestOnly+"TRUE"+tail)
	green := explain(t, ctx, pool, head+manifestOnly+horizon+tail)

	if !strings.Contains(redFull, "Seq Scan on context_blocks") {
		t.Errorf("redFull (design's full aggregation) shows no Seq Scan — the probe is not discriminating:\n%s", redFull)
	}
	if strings.Contains(green, "Seq Scan on context_blocks") {
		t.Errorf("green plan (with horizon) still does a Seq Scan over context_blocks:\n%s", green)
	}

	fullBuf, redBuf, greenBuf := totalBuffers(t, redFull), totalBuffers(t, redNoHorizon), totalBuffers(t, green)
	t.Logf("buffers: redFull %d, redNoHorizon %d, green %d (horizon reduction: %.1f %%)",
		fullBuf, redBuf, greenBuf, 100*(1-float64(greenBuf)/float64(redBuf)))
	// Logged, not asserted: the reduction depends on how much of the corpus the
	// window selects, and that is the quantity TestSessionsHorizonCrossoverAtScale
	// measures. A threshold here would encode one point of that curve as if it
	// were a property of the query.
	if greenBuf >= redBuf {
		t.Logf("NOTE: the horizon reduced nothing (%d buffers with it, %d without) — "+
			"the window is above the crossover; see TestSessionsHorizonCrossoverAtScale",
			greenBuf, redBuf)
	}

	t.Logf("redFull plan (design's full aggregation):\n%s", redFull)
	t.Logf("redNoHorizon plan (manifest filter, no horizon):\n%s", redNoHorizon)
	t.Logf("green plan (shipped query):\n%s", green)
}

// totalBuffers reads the buffer count of a plan's TOP node — the accumulated
// total for the whole plan. Taking the maximum over all lines would double
// count, since every parent already includes its children.
func totalBuffers(t *testing.T, plan string) int {
	t.Helper()
	for _, line := range strings.Split(plan, "\n") {
		trimmed := strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(trimmed, "Buffers: shared ")
		if !ok {
			continue
		}
		total := 0
		for _, field := range strings.Fields(rest) {
			kind, num, ok := strings.Cut(field, "=")
			if !ok || (kind != "hit" && kind != "read") {
				continue
			}
			v, err := strconv.Atoi(strings.TrimSuffix(num, ","))
			if err != nil {
				t.Fatalf("parse buffer field %q: %v", field, err)
			}
			total += v
		}
		return total
	}
	t.Fatalf("no buffer line in plan:\n%s", plan)
	return 0
}

// TestLatencyBudgetAtScale is the latency gate: Read per manifest under 30 ms,
// Sessions under 20 ms with the cap. Both are measured as a median over
// repeated calls, because a single cold call measures the page cache.
//
// The budgets are the design's and are NOT adjusted here — the first round of
// this wave left this test red rather than move them, and the numbers below
// come from fixing the fixture, not the threshold.
//
// What changed is the fixture's time span. It now derives from the live ingest
// rate (liveBlocksPerDay), so a million rows span ~19.6 years and the 30-day
// horizon selects a fraction of a percent — the production shape. The earlier
// version laid down one row per second, compressing the same million rows into
// 11.6 days, where the 30-day default covered the entire corpus and the horizon
// had nothing to select; Sessions measured 38 ms there against the 20 ms
// budget. Same code, same query, different corpus geometry.
//
// TestSessionsHorizonCrossoverAtScale measures the dependency itself, so the
// pass here is not a lucky point but a located one.
func TestLatencyBudgetAtScale(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	pool := testdb.SetupTestDB(t)

	n := scaleN(t)
	t.Logf("fixture: %d rows in %s", n, seedScale(t, ctx, pool, n))

	opt := fxOpts()
	opt.MaxManifests = 1
	opt.SessionHorizon = scaleHorizon
	src, err := ctxcheckpoint.New(pool, opt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const reps = 20
	// Warm the cache once; the budget is about the steady state, and a cold
	// first call measures the disk. Both warmups double as assertions that
	// there is anything to measure at all — a latency taken over an empty
	// answer is the easiest way to report a green budget for nothing.
	warm, err := src.Read(ctx, scaleRoot, 0, 400, 4000)
	if err != nil {
		t.Fatalf("warmup Read: %v", err)
	}
	if len(warm.Items) == 0 {
		t.Fatalf("warmup Read returned no items for root %q — the budget would measure an empty answer", scaleRoot)
	}
	refs, err := src.Sessions(ctx)
	if err != nil {
		t.Fatalf("warmup Sessions: %v", err)
	}
	if len(refs) == 0 {
		t.Fatalf("warmup Sessions returned no candidate inside the %s horizon — the budget would measure an empty answer", scaleHorizon)
	}
	t.Logf("measuring against %d items per Read and %d candidates per Sessions", len(warm.Items), len(refs))

	readMedian := medianOf(t, reps, func() {
		if _, err := src.Read(ctx, scaleRoot, 0, 400, 4000); err != nil {
			t.Fatalf("Read: %v", err)
		}
	})
	sessMedian := medianOf(t, reps, func() {
		if _, err := src.Sessions(ctx); err != nil {
			t.Fatalf("Sessions: %v", err)
		}
	})

	t.Logf("median over %d calls at %d rows: Read %v, Sessions %v", reps, n, readMedian, sessMedian)
	if readMedian > 30*time.Millisecond {
		t.Errorf("Read median = %v, budget is 30ms", readMedian)
	}
	if sessMedian > 20*time.Millisecond {
		t.Errorf("Sessions median = %v, budget is 20ms", sessMedian)
	}
}

// TestSessionsHorizonCrossoverAtScale measures WHERE the horizon starts to bind
// the cost, on a fixture whose time span comes from the live ingest rate.
//
// This exists because the first round drew a general conclusion ("the horizon
// changes nothing") from a fixture that compressed a million rows into 11.6
// days. In that shape the 30-day default covered the entire corpus, so of
// course it cut nothing. At the real rate the same default covers a fraction of
// a percent, and the planner switches from the corpus-wide manifest bitmap to
// idx_context_created — two orders of magnitude cheaper.
//
// Each horizon is measured on its OWN pool so no statement cache carries a plan
// across, which is the production shape: one horizon value per deployment.
func TestSessionsHorizonCrossoverAtScale(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	pool := testdb.SetupTestDB(t)

	n := scaleN(t)
	t.Logf("fixture: %d rows in %s", n, seedScale(t, ctx, pool, n))

	horizons := []time.Duration{
		0, // no cap
		365 * 24 * time.Hour,
		90 * 24 * time.Hour,
		30 * 24 * time.Hour, // design default
		7 * 24 * time.Hour,
		24 * time.Hour,
	}
	span := scaleSpread(n)
	for _, h := range horizons {
		// A FRESH pool per horizon, and it is not a nicety: the shipped query
		// carries the cutoff as a parameter, so every horizon reuses the same
		// prepared statement and Postgres keeps serving the plan it chose for
		// the first one. Sharing a pool here measured 33-54 ms for EVERY
		// horizon including the ones that are two orders of magnitude more
		// selective — the same 30-day window that measures 4.8 ms on its own
		// pool in TestLatencyBudgetAtScale. A cached plan is the production
		// shape for ONE horizon, not for a sweep across six.
		hp, err := pgxpool.NewWithConfig(ctx, pool.Config().Copy())
		if err != nil {
			t.Fatalf("fresh pool for %s: %v", h, err)
		}
		opt := fxOpts()
		opt.SessionHorizon = h
		src, err := ctxcheckpoint.New(hp, opt)
		if err != nil {
			hp.Close()
			t.Fatalf("New: %v", err)
		}
		refs, err := src.Sessions(ctx) // warm
		if err != nil {
			hp.Close()
			t.Fatalf("Sessions(%s): %v", h, err)
		}
		med := medianOf(t, 20, func() {
			if _, err := src.Sessions(ctx); err != nil {
				t.Fatalf("Sessions(%s): %v", h, err)
			}
		})
		share := "n/a (no cap)"
		if h > 0 {
			share = fmt.Sprintf("%.3f %%", 100*h.Seconds()/span.Seconds())
		}
		verdict := "inside 20ms budget"
		if med > 20*time.Millisecond {
			verdict = "OVER 20ms budget"
		}
		t.Logf("horizon %-8s covers %-12s candidates=%d median=%v %s", h, share, len(refs), med, verdict)
		hp.Close()
	}
}

func medianOf(t *testing.T, reps int, fn func()) time.Duration {
	t.Helper()
	ds := make([]time.Duration, 0, reps)
	for range reps {
		start := time.Now()
		fn()
		ds = append(ds, time.Since(start))
	}
	for i := 1; i < len(ds); i++ {
		for j := i; j > 0 && ds[j] < ds[j-1]; j-- {
			ds[j], ds[j-1] = ds[j-1], ds[j]
		}
	}
	return ds[len(ds)/2]
}
