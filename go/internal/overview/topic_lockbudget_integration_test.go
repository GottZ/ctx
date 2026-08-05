//go:build integration

// W3-G11 — lock-hold budget and temp-spill baseline of the identity phase
// (design/01 §7 W3-G11, amendment A01-5).
//
// What this gate DOES deliver:
//   - (a) the measured wall time of the persist transaction, split into the
//     birth generation and the matching generation, printed as the BASELINE
//     that later waves (S9a CopyFrom-persist, S9b delta-persist) compare
//     against. It is an absolute baseline, not a before/after percentage:
//     "before" is a state that no longer exists in this tree, and a made-up
//     percentage would be a claim, not a measurement (W21).
//   - (b) the temp-spill probe with its red half. EXPLAIN (ANALYZE, BUFFERS)
//     over the ov_prev fill reports LOCAL buffer writes — evictions of dirty
//     temp-table blocks to disk, inside the advisory lock. The probe runs the
//     same statement on two virgin connections, once at the production
//     temp_buffers and once at the PostgreSQL minimum, and requires the
//     minimum to evict measurably more. Measured at 20.000 members: 288 versus
//     476 blocks. Note that the production value does not reach ZERO evictions
//     — the bulk-extension of the temp relation writes regardless — so the
//     honest claim of this gate is the DIFFERENCE, not an absence.
//
// What it does NOT deliver, named rather than faked:
//   - (c) the child-process peak RSS against GOMEMLIMIT. It needs the spawned
//     worker and a 200k fixture, which is the S1 profiling run of Achse 04
//     (masterplan R7 / UD-02-04); this gate would only produce a number for
//     the in-process test binary, which is not the process the limit applies
//     to.
//   - the 200k-member run. The fixture size is CTX_W3_G11_MEMBERS; the default
//     is sized so the spill probe is decisive while the gate stays inside a
//     normal test loop. The node-cap-scale run is one env var away and is the
//     form the Achse-04 budget work is expected to use.
package overview

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

func g11Members(t *testing.T) int {
	t.Helper()
	if v := os.Getenv("CTX_W3_G11_MEMBERS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 100 {
			t.Fatalf("CTX_W3_G11_MEMBERS=%q: want an integer >= 100", v)
		}
		return n
	}
	return 20000
}

// g11Explain returns the EXPLAIN (ANALYZE, BUFFERS) plan of the ov_prev fill
// under a given temp_buffers setting, in its own rolled-back transaction on a
// VIRGIN connection.
//
// The virgin connection is not cosmetic: PostgreSQL refuses to change
// temp_buffers once a session has touched a temp table (SQLSTATE 22023, the
// same code as a range violation), and every persist in this test file has
// touched one. A pooled connection would therefore either reject the probe or
// silently run at the wrong setting.
func g11Explain(t *testing.T, dsn, tempBuffers string) string {
	t.Helper()
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	fresh, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()

	tx, err := fresh.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL temp_buffers = '%s'`, tempBuffers)); err != nil {
		t.Fatalf("temp_buffers=%s: %v", tempBuffers, err)
	}
	if _, err := tx.Exec(ctx, createPrevSQL); err != nil {
		t.Fatalf("create ov_prev: %v", err)
	}
	rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+prevSnapshotGlobalSQL)
	if err != nil {
		t.Fatalf("EXPLAIN at temp_buffers=%s: %v", tempBuffers, err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString("  " + line + "\n")
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan.String()
}

// localWrites extracts the number of LOCAL buffer blocks a plan wrote out.
// Local buffers are the temp-table buffer pool: a write means a dirty temp
// block was evicted to disk — that is the spill this gate is about, and it is
// a different counter from the `temp read/written` of a sort or hash spill.
func localWrites(plan string) int {
	total := 0
	for _, line := range strings.Split(plan, "\n") {
		idx := strings.Index(line, "local ")
		if idx < 0 {
			continue
		}
		for _, field := range strings.Fields(line[idx:]) {
			if n, ok := strings.CutPrefix(field, "written="); ok {
				v, err := strconv.Atoi(n)
				if err == nil {
					total += v
				}
			}
		}
	}
	return total
}

func TestW3LockBudgetBaseline(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	const scope = "g11"
	n := g11Members(t)

	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("019e1111-0000-7000-9000-%012d", i)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, scope, category, title, content)
		SELECT u::uuid, $2, 'learnings', 'g11 ' || u, 'g11 fixture'
		  FROM unnest($1::text[]) AS u`, ids, scope); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A partition of ~50 clusters over n members — the live shape (59 clusters)
	// scaled up, not one giant community.
	const clusters = 50
	build := func(shift int) (map[string]string, map[string]string, map[string]float64) {
		assign := make(map[string]string, n)
		scopes := make(map[string]string, n)
		deg := make(map[string]float64, n)
		heads := make([]string, clusters)
		for c := 0; c < clusters; c++ {
			heads[c] = ids[(c*(n/clusters)+shift)%n]
		}
		for i, id := range ids {
			c := ((i + shift) / (n / clusters)) % clusters
			assign[id] = heads[c]
			scopes[id] = scope
			deg[id] = float64(i%7) + 1
		}
		return assign, scopes, deg
	}

	timed := func(label string, shift int) time.Duration {
		assign, scopes, deg := build(shift)
		start := time.Now()
		st, err := persist(ctx, pool,
			clustering{blockToCluster: assign, intraDegree: deg, clusterCount: clusters},
			Options{Resolution: 1.0, VisibleTypes: w3Types, ScopeFilter: []string{scope},
				TombstoneRetention: 45 * 24 * time.Hour},
			scopes, tallyScopes(scopes))
		d := time.Since(start)
		if err != nil {
			t.Fatalf("%s persist: %v", label, err)
		}
		t.Logf("[G11 baseline] %-22s members=%d clusters=%d  persist=%s  carried=%d born=%d split=%d retired=%d reattached=%d members_changed=%d members_reassigned=%d",
			label, n, clusters, d.Round(time.Millisecond),
			st.TopicsCarried, st.TopicsBorn, st.TopicsSplit, st.TopicsRetired, st.TopicsReattached,
			st.MembersChanged, st.MembersReassigned)
		return d
	}

	// (a) The documented lock-hold baseline. Generation 1 is all births (the
	// empty-ov_prev path), generation 2 is the real matching workload: a full
	// predecessor snapshot, a full overlap, both argmax passes and the churn
	// measurement. Generation 2 is the number later waves have to beat.
	timed("gen 1 (all births)", 0)
	matching := timed("gen 2 (full matching)", n/clusters/2)
	t.Logf("[G11 baseline] lock-hold budget of the identity phase = %s for %d members / %d clusters (temp_buffers=%s)",
		matching.Round(time.Millisecond), n, clusters, persistTempBuffers)

	// (b) Temp spill, with its red half.
	t.Run("temp_buffers keeps the identity working set off disk", func(t *testing.T) {
		production := g11Explain(t, dsn, persistTempBuffers)
		prodWrites := localWrites(production)
		t.Logf("EXPLAIN ov_prev fill @ temp_buffers=%s — local writes=%d:\n%s",
			persistTempBuffers, prodWrites, production)

		// RED: the PostgreSQL minimum (100 blocks = 800 kB, pg_settings
		// min_val). If the minimum does NOT evict more than the production
		// setting, the fixture is too small for this gate to prove anything —
		// and the gate says so instead of passing quietly.
		const minimal = "800kB"
		red := g11Explain(t, dsn, minimal)
		redWrites := localWrites(red)
		t.Logf("EXPLAIN ov_prev fill @ temp_buffers=%s (red probe) — local writes=%d:\n%s",
			minimal, redWrites, red)

		if redWrites <= prodWrites {
			t.Fatalf("red probe: temp_buffers=%s evicted %d local blocks, production (%s) evicted %d — no measurable difference at %d members; raise CTX_W3_G11_MEMBERS, the gate proves nothing at this size",
				minimal, redWrites, persistTempBuffers, prodWrites, n)
		}
		t.Logf("[G11 spill] SET LOCAL temp_buffers=%s saves %d temp-block evictions inside the advisory lock at %d members (%s ⇒ %d, minimum ⇒ %d)",
			persistTempBuffers, redWrites-prodWrites, n, persistTempBuffers, prodWrites, redWrites)
	})
}
