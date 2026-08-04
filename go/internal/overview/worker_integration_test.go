//go:build integration

package overview_test

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/overview"
	"github.com/GottZ/ctx/internal/testdb"
)

// dumpDeterministicRows snapshots every deterministic column of the four
// overview tables (member/node/edge/meta) as sorted text rows. computed_at is
// the ONE deliberately excluded column — it is now() of the respective run
// and volatile by definition; everything else must be byte-identical between
// the in-process and the worker rebuild.
func dumpDeterministicRows(t *testing.T, pool *pgxpool.Pool) map[string][]string {
	t.Helper()
	queries := map[string]string{
		"member": `SELECT block_id::text || '|' || cluster_id::text
		             FROM graph_cluster_member ORDER BY block_id`,
		"node": `SELECT cluster_id::text || '|' || scope || '|' || size::text || '|' ||
		                category_counts::text || '|' || repr_block_id::text || '|' ||
		                coalesce(repr_title,'') || '|' || repr_quality::text
		             FROM graph_cluster_node ORDER BY cluster_id, scope`,
		"edge": `SELECT cluster_a::text || '|' || cluster_b::text || '|' || scope_s || '|' ||
		                scope_t || '|' || link_count::text || '|' || weight_sum::text
		             FROM graph_cluster_edge ORDER BY cluster_a, cluster_b, scope_s, scope_t`,
		"meta": `SELECT scope || '|' || modularity::text || '|' || cluster_n::text || '|' ||
		                node_n::text || '|' || edge_n::text || '|' || resolution::text
		             FROM graph_overview_meta ORDER BY scope`,
	}
	out := make(map[string][]string, len(queries))
	for name, q := range queries {
		rows, err := pool.Query(context.Background(), q)
		if err != nil {
			t.Fatalf("dump %s: %v", name, err)
		}
		var lines []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatalf("scan %s: %v", name, err)
			}
			lines = append(lines, line)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("dump %s: %v", name, err)
		}
		out[name] = lines
	}
	return out
}

// TestWorkerRoundtrip_IdenticalDBRows is the E-A golden gate (design/05 §4.7):
// the worker path — the production Options through the real IPC encode→decode
// chain into overview.RunWorker (= Rebuild + Stats encode) — writes exactly
// the DB rows the in-process path writes, and reports the identical Stats.
// The only production delta outside this test is fork/exec + env-DSN glue,
// covered by the events helper-process spawn test and cmd/ctxd respectively.
//
// Fixture: the W1 two-triangles-plus-weak-bridge corpus (the established
// scope-partitioning shape). Both runs are GLOBAL (nil ScopeFilter) full
// replaces, so run 2 overwrites run 1 wholesale — the comparison also re-pins
// cross-process determinism (fixed seeds + ordered loads).
func TestWorkerRoundtrip_IdenticalDBRows(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const (
		A = "019d0000-0000-7000-9000-00000000000a"
		B = "019d0000-0000-7000-9000-00000000000b"
		C = "019d0000-0000-7000-9000-00000000000c"
		D = "019d0000-0000-7000-9000-00000000000d"
		E = "019d0000-0000-7000-9000-00000000000e"
		F = "019d0000-0000-7000-9000-00000000000f"
	)
	insBlock(t, pool, A, "private", "learnings", "A")
	insBlock(t, pool, B, "private", "learnings", "B")
	insBlock(t, pool, C, "shared", "decisions", "C")
	insBlock(t, pool, D, "work", "infrastructure", "D")
	insBlock(t, pool, E, "work", "infrastructure", "E")
	insBlock(t, pool, F, "work", "infrastructure", "F")
	insLink(t, pool, A, B, 0.9)
	insLink(t, pool, B, C, 0.9)
	insLink(t, pool, A, C, 0.9)
	insLink(t, pool, D, E, 0.9)
	insLink(t, pool, E, F, 0.9)
	insLink(t, pool, D, F, 0.9)
	insLink(t, pool, C, F, 0.05)

	opts := overview.Options{Resolution: 1.0, VisibleTypes: []string{"knowledge"}, OverviewTypes: []string{"knowledge"}}

	// Reference: the in-process path (the pre-E-A call shape).
	statsInProc, err := overview.Rebuild(ctx, pool, opts)
	if err != nil {
		t.Fatalf("in-process rebuild: %v", err)
	}
	if statsInProc.NodeCount != 6 {
		t.Fatalf("fixture sanity: NodeCount = %d, want 6", statsInProc.NodeCount)
	}
	inProcRows := dumpDeterministicRows(t, pool)

	// Worker path: the same Options through the REAL IPC chain (encode →
	// strict decode → RunWorker → Stats decode), against the same schema.
	var optsWire, statsWire bytes.Buffer
	if err := overview.EncodeWorkerOptions(&optsWire, opts); err != nil {
		t.Fatalf("encode options: %v", err)
	}
	decodedOpts, err := overview.DecodeWorkerOptions(&optsWire)
	if err != nil {
		t.Fatalf("decode options: %v", err)
	}
	if err := overview.RunWorker(ctx, pool, decodedOpts, &statsWire); err != nil {
		t.Fatalf("worker rebuild: %v", err)
	}
	statsWorker, err := overview.DecodeWorkerStats(&statsWire)
	if err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	workerRows := dumpDeterministicRows(t, pool)

	// DeepEqual since W-A: Stats carries the per-scope CandidateCount map and
	// is no longer comparable with ==. The map is part of the equivalence
	// claim — the worker must report the SAME per-scope candidate tally.
	if !reflect.DeepEqual(statsWorker, statsInProc) {
		t.Errorf("worker stats diverge from in-process stats:\n  in-process: %+v\n  worker    : %+v", statsInProc, statsWorker)
	}
	for _, table := range []string{"member", "node", "edge", "meta"} {
		if !reflect.DeepEqual(inProcRows[table], workerRows[table]) {
			t.Errorf("%s rows diverge between in-process and worker rebuild:\n  in-process: %v\n  worker    : %v",
				table, inProcRows[table], workerRows[table])
		}
	}
	// Guard against a silently empty comparison: the fixture MUST have produced rows.
	if len(inProcRows["member"]) != 6 {
		t.Fatalf("fixture sanity: %d member rows, want 6", len(inProcRows["member"]))
	}
	fmt.Println("worker/in-process golden compare over",
		len(inProcRows["member"]), "members,", len(inProcRows["node"]), "nodes,",
		len(inProcRows["edge"]), "edges,", len(inProcRows["meta"]), "meta rows — identical")
}
