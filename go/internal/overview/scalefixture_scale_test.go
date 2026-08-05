//go:build integration

// Achse 04 / Welle S0 — die zwei Arme, die nicht in jeden `go test -short`
// gehören: die Ziel-Scale-Speicherprobe (Minuten) und der DB-Schreibpfad
// (Testcontainer). Beide sind opt-in über CTX_ROOTMAP_BENCH bzw. den
// Testcontainer-Tag; der Rest von S0-G1 läuft in scalefixture_gate_test.go
// unter -short.
package overview

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestScaleFixtureStreamMemory ist die Speicher-Disziplin-Probe: der Generator
// selbst darf am Ziel-Scale nicht sterben. §6.1 Zeile B — 10M Wissens-Blöcke
// sind 9,8M Louvain-Knoten, K1 = 22,05M Paare, K2 = 98M Paare.
//
//	CTX_ROOTMAP_BENCH=1 go test -tags=integration ./internal/overview/ \
//	  -run TestScaleFixtureStreamMemory -v -timeout 90m
func TestScaleFixtureStreamMemory(t *testing.T) {
	if os.Getenv("CTX_ROOTMAP_BENCH") == "" {
		t.Skip("CTX_ROOTMAP_BENCH ungesetzt — Ziel-Scale-Probe übersprungen")
	}
	const target = 9_800_000
	for _, spec := range []scaleSpec{specK1Organic(target, 7), specK2Flat(target, 7)} {
		f, err := resolveScale(spec)
		if err != nil {
			t.Fatalf("resolve %v: %v", spec, err)
		}
		runtime.GC()
		var edges, dangling int64
		f.stream(func(_, b int, _ float64) {
			edges++
			if b >= f.spec.Nodes {
				dangling++
			}
		})
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		fmt.Printf("[S0-scale] %v\n  Kanten=%d dangling=%d Communities=%d  VmHWM=%s Go-Sys=%s\n",
			spec, edges, dangling, f.communities(), mbFromKB(readVmHWMkB(t)), mbFromBytes(ms.Sys))
		if int(edges) > f.edgeBudget() || float64(edges) < 0.99*float64(f.edgeBudget()) {
			t.Errorf("Strom lieferte %d Kanten, Budget %d — außerhalb des Korridors", edges, f.edgeBudget())
		}
	}
}

// writeScaleFixtureDB schreibt die Fixture in eine Bench-/Testcontainer-DB.
//
// Drei Dinge, die dabei NICHT beliebig sind:
//
//  1. Die dangling-Endpunkte werden als ARCHIVIERTE Blöcke angelegt, nicht
//     weggelassen — context_dream_links trägt einen Fremdschlüssel auf
//     context_blocks (016_dream.sql:18-19). „Endpunkt außerhalb des Schnitts"
//     ist live genau das: ein Link auf einen archivierten/meta-Block, den
//     loadNodes nicht mehr liefert (cluster.go:457-458).
//  2. Geschrieben wird über CopyFrom in eine TEMP-Tabelle und von dort mit
//     ON CONFLICT DO NOTHING — der PK (source_block_id, target_block_id)
//     verträgt keine Wiederholung, und CopyFrom kennt kein ON CONFLICT.
//  3. Ausschließlich gegen eine DB, die NICHT context_store heißt. Der
//     Generator schreibt Millionen Zeilen; ein Fehlgriff auf die Live-DB wäre
//     nicht rückabwickelbar.
func writeScaleFixtureDB(t *testing.T, ctx context.Context, pool *pgxpool.Pool, f *scaleFixture, scope string) {
	t.Helper()
	var dbName string
	if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatalf("scalefixture-db: current_database: %v", err)
	}
	if dbName == "context_store" {
		t.Fatalf("scalefixture-db: VERWEIGERT — Ziel-DB ist context_store (Live). Nur Bench-/Testcontainer-DBs.")
	}

	n := f.spec.Nodes
	blockRows := make([][]any, 0, 4096)
	flushBlocks := func() {
		if len(blockRows) == 0 {
			return
		}
		_, err := pool.CopyFrom(ctx,
			pgx.Identifier{"context_blocks"},
			[]string{"id", "category", "title", "content", "scope", "is_archived"},
			pgx.CopyFromRows(blockRows))
		if err != nil {
			t.Fatalf("scalefixture-db: CopyFrom context_blocks: %v", err)
		}
		blockRows = blockRows[:0]
	}
	for i := 0; i < n; i++ {
		u := f.uuidAt(i)
		blockRows = append(blockRows, []any{u, "scalefixture", fmt.Sprintf("fixture node %d", i), "synthetic", scope, false})
		if len(blockRows) == 4096 {
			flushBlocks()
		}
	}
	for j := 0; j < f.dangling; j++ {
		u := f.outsideUUID(j)
		blockRows = append(blockRows, []any{u, "scalefixture", fmt.Sprintf("fixture outside %d", j), "synthetic", scope, true})
		if len(blockRows) == 4096 {
			flushBlocks()
		}
	}
	flushBlocks()

	if _, err := pool.Exec(ctx, `CREATE TEMP TABLE IF NOT EXISTS sf_links
		(source_block_id uuid, target_block_id uuid, raw_confidence real) ON COMMIT PRESERVE ROWS`); err != nil {
		t.Fatalf("scalefixture-db: temp table: %v", err)
	}
	linkRows := make([][]any, 0, 8192)
	flushLinks := func() {
		if len(linkRows) == 0 {
			return
		}
		if _, err := pool.CopyFrom(ctx, pgx.Identifier{"sf_links"},
			[]string{"source_block_id", "target_block_id", "raw_confidence"},
			pgx.CopyFromRows(linkRows)); err != nil {
			t.Fatalf("scalefixture-db: CopyFrom sf_links: %v", err)
		}
		linkRows = linkRows[:0]
	}
	f.stream(func(a, b int, w float64) {
		src := f.uuidAt(a)
		dst := ""
		if b >= n {
			dst = f.outsideUUID(b - n)
		} else {
			dst = f.uuidAt(b)
		}
		linkRows = append(linkRows, []any{src, dst, w})
		if len(linkRows) == 8192 {
			flushLinks()
		}
	})
	flushLinks()

	tag, err := pool.Exec(ctx, `
		INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
		SELECT source_block_id, target_block_id, 'topical', raw_confidence, raw_confidence, $1
		FROM sf_links
		WHERE source_block_id <> target_block_id
		ON CONFLICT DO NOTHING`, scope)
	if err != nil {
		t.Fatalf("scalefixture-db: insert links: %v", err)
	}
	t.Logf("scalefixture-db: %d Blöcke (+%d außerhalb), %d dream-Links in %q", n, f.dangling, tag.RowsAffected(), dbName)
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS sf_links`); err != nil {
		t.Logf("scalefixture-db: temp cleanup: %v", err)
	}
}

// TestScaleFixtureDBRoundtrip belegt, dass der DB-Schreibpfad dieselbe
// Louvain-Eingabe liefert wie der In-Memory-Pfad: nach dem Schreiben müssen
// loadNodes/loadEdges den Knotenschnitt und die Kantenmenge des Generators
// reproduzieren. Ohne diese Probe wäre der DB-Arm eine Behauptung.
func TestScaleFixtureDBRoundtrip(t *testing.T) {
	dsn := os.Getenv("CTX_BENCH_DSN")
	if dsn == "" {
		t.Skip("CTX_BENCH_DSN ungesetzt — DB-Arm von S0 übersprungen")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	spec := specK1Organic(2_000, 5)
	spec.DanglingFrac = 0.081
	f, err := resolveScale(spec)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	writeScaleFixtureDB(t, ctx, pool, f, "private")

	nodes, _, err := loadNodes(ctx, pool, []string{"knowledge"}, []string{"private"})
	if err != nil {
		t.Fatalf("loadNodes: %v", err)
	}
	edges, err := loadEdges(ctx, pool, []string{"private"})
	if err != nil {
		t.Fatalf("loadEdges: %v", err)
	}
	wantNodes, wantEdges := f.materialize()
	if len(nodes) != len(wantNodes) {
		t.Fatalf("loadNodes lieferte %d Knoten, Generator %d", len(nodes), len(wantNodes))
	}
	for i := range nodes {
		if nodes[i] != wantNodes[i] {
			t.Fatalf("Knoten %d: DB %q, Generator %q — die ORDER-BY-id-Ordnung stimmt nicht", i, nodes[i], wantNodes[i])
		}
	}
	// loadEdges filtert dangling über den scope-JOIN NICHT heraus, aber der
	// archivierte Block fällt aus loadNodes — computeClustering verwirft die
	// Kante. Verglichen wird deshalb die Partition, nicht die Rohliste.
	dbCl := computeClustering(nodes, edges, 1.0)
	memCl := computeClustering(wantNodes, wantEdges, 1.0)
	if dbCl.clusterCount != memCl.clusterCount || dbCl.modularity != memCl.modularity {
		t.Fatalf("Partition weicht ab: DB %d Cluster / Q=%.6f, Speicher %d / Q=%.6f",
			dbCl.clusterCount, dbCl.modularity, memCl.clusterCount, memCl.modularity)
	}
}
