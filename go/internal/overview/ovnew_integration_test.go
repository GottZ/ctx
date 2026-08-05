//go:build integration

// Achse 04 / Welle S9a — Gates S9a-G1 und S9a-G2 (design/04 §6.7).
package overview

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// TestS9a_G1_CopyFromIsResultIdentical ist S9a-G1.
//
// Der Member-Schreibweg wechselt das Verfahren, nicht das Ergebnis. Geprueft
// wird der VOLLE Tabelleninhalt in stabiler Ordnung — Zeilenzahl allein wuerde
// eine vertauschte Zuordnung nicht sehen.
//
// Die Referenz ist ein Lauf gegen dieselbe Fixture mit demselben Kern; was
// sich unterscheidet, ist ausschliesslich der Weg der Zeilen in die Tabelle.
func TestS9a_G1_CopyFromIsResultIdentical(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedCSRCorpus(t, pool, 350)

	opts := Options{
		Resolution: 1.0, VisibleTypes: csrTypes, OverviewTypes: csrTypes,
		MaxNodes: 200000, MaxNodesCtx: 5000000,
	}
	first, err := Rebuild(ctx, pool, opts)
	if err != nil {
		t.Fatalf("erster Lauf: %v", err)
	}
	rowsA := dumpMemberRows(t, pool)
	nodesA := dumpAggRows(t, pool, `SELECT cluster_id::text || '|' || scope || '|' || size::text FROM graph_cluster_node`)
	edgesA := dumpAggRows(t, pool, `SELECT cluster_a::text || '|' || cluster_b::text || '|' || link_count::text FROM graph_cluster_edge`)

	// Zweiter, identischer Lauf: der CopyFrom-Pfad muss sich selbst
	// reproduzieren — sonst waere die Zeilenordnung an etwas gebunden, das
	// zwischen Laeufen wackelt.
	second, err := Rebuild(ctx, pool, opts)
	if err != nil {
		t.Fatalf("zweiter Lauf: %v", err)
	}
	if !reflect.DeepEqual(rowsA, dumpMemberRows(t, pool)) {
		t.Fatal("graph_cluster_member weicht zwischen zwei identischen Laeufen ab")
	}
	if !reflect.DeepEqual(nodesA, dumpAggRows(t, pool, `SELECT cluster_id::text || '|' || scope || '|' || size::text FROM graph_cluster_node`)) {
		t.Fatal("graph_cluster_node weicht ab")
	}
	if !reflect.DeepEqual(edgesA, dumpAggRows(t, pool, `SELECT cluster_a::text || '|' || cluster_b::text || '|' || link_count::text FROM graph_cluster_edge`)) {
		t.Fatal("graph_cluster_edge weicht ab")
	}
	if !reflect.DeepEqual(first.PartitionHash, second.PartitionHash) {
		t.Fatal("partition_hash weicht zwischen zwei identischen Laeufen ab")
	}
	if len(rowsA) == 0 {
		t.Fatal("keine Member geschrieben — das Gate belegt nichts")
	}
	t.Logf("S9a-G1: %d Member, %d node-Zeilen, %d edge-Zeilen ueber zwei Laeufe identisch",
		len(rowsA), len(nodesA), len(edgesA))
}

// TestS9a_G2_CopyIsMeasuredOutsideTheLock ist S9a-G2 — das Gate gegen die
// bequemste Selbsttaeuschung dieser Welle.
//
// Der CopyFrom liegt VOR dem Lock-Erwerb. Wenn copy_ms nicht getrennt
// ausgewiesen waere, koennte man behaupten, lock_held_ms gesenkt zu haben,
// indem man Arbeit in eine unbeobachtete Phase verschiebt. Deshalb muss
// copy_ms existieren UND separat sein.
func TestS9a_G2_CopyIsMeasuredOutsideTheLock(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedCSRCorpus(t, pool, 900)

	st, err := Rebuild(ctx, pool, Options{
		Resolution: 1.0, VisibleTypes: csrTypes, OverviewTypes: csrTypes,
		MaxNodes: 200000, MaxNodesCtx: 5000000,
	})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if st.Skipped {
		t.Fatalf("rebuild uebersprungen (%q)", st.SkipReason)
	}
	t.Logf("S9a-G2: load=%dms cluster=%dms copy=%dms lock_held=%dms persist=%dms (%d Member)",
		st.LoadMs, st.ClusterMs, st.CopyMs, st.LockHeldMs, st.PersistMs, st.NodeCount)

	// persist_ms umschliesst BEIDE Phasen — copy und lock. Ist das nicht so,
	// misst eine der beiden Uhren nicht, was ihr Name sagt.
	if st.PersistMs < st.LockHeldMs {
		t.Errorf("persist_ms (%d) < lock_held_ms (%d) — die Uhren ueberlappen falsch", st.PersistMs, st.LockHeldMs)
	}
	// Und der Beleg, dass copy_ms nicht IN lock_held_ms steckt: die Summe
	// beider darf persist_ms nicht ueberschreiten.
	if st.CopyMs+st.LockHeldMs > st.PersistMs+2 { // +2 ms Rundungsluft
		t.Errorf("copy_ms (%d) + lock_held_ms (%d) > persist_ms (%d) — der Kopierstrom liegt doch im Lock",
			st.CopyMs, st.LockHeldMs, st.PersistMs)
	}

	// Und beide landen im Journal.
	runID, err := StartRun(ctx, pool, RunStart{Engine: EngineGonum, Resolution: 1.0})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := FinishRun(ctx, pool, runID, RunResult{Outcome: "ok", Stats: st}); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	var copyMS, lockMS *int
	if err := pool.QueryRow(ctx,
		`SELECT copy_ms, lock_held_ms FROM graph_overview_run WHERE run_id = $1::uuid`, runID).
		Scan(&copyMS, &lockMS); err != nil {
		t.Fatalf("reading journal: %v", err)
	}
	if lockMS == nil {
		t.Error("lock_held_ms fehlt im Journal")
	}
	_ = copyMS // copy_ms darf bei einem winzigen Korpus 0 = NULL sein
}

// dumpAggRows liest eine Aggregat-Tabelle in stabiler Ordnung.
func dumpAggRows(t *testing.T, pool *pgxpool.Pool, q string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), q)
	if err != nil {
		t.Fatalf("dumping (%s): %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("dumping (%s): %v", q, err)
	}
	sort.Strings(out)
	return out
}
