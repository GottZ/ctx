//go:build integration

// Achse 04 / Welle S3 — Gates S3-G1 … S3-G6 (design/04 §6.7).
//
// Die Welle tauscht das SUBSTRAT, nicht die Engine. Genau deshalb ist ihr
// tragendes Gate ein IDENTITAETS-Gate: die CSR muss dieselbe Partition tragen
// wie der []rawEdge-Pfad, sonst ist der Speichergewinn erkauft und nicht
// gemessen.
package overview

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

var csrTypes = []string{"knowledge"}

// seedCSRCorpus legt einen Korpus mit genau den Randfaellen an, die §2.4 LIVE
// misst — nicht mit einem glatten Graphen, an dem jede Implementierung gruen
// waere: 264 dangling Kanten von 3.519 (7,5 %), 0 Self-Loops, isolierte Knoten,
// beide Richtungen desselben Paares.
func seedCSRCorpus(t *testing.T, pool *pgxpool.Pool, n int) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		var id string
		if err := pool.QueryRow(ctx, `
			INSERT INTO context_blocks (category, title, content, scope)
			VALUES ('csrbench', $1, 'x', 'private') RETURNING id::text`,
			fmt.Sprintf("node %04d", i)).Scan(&id); err != nil {
			t.Fatalf("seeding block %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	// Ein ARCHIVIERTER Block: jede Kante auf ihn ist dangling — der Live-Fall
	// (Link auf einen archivierten/meta-Block, den loadNodes nicht liefert).
	var archived string
	if err := pool.QueryRow(ctx, `
		INSERT INTO context_blocks (category, title, content, scope, is_archived)
		VALUES ('csrbench', 'archived', 'x', 'private', true) RETURNING id::text`).Scan(&archived); err != nil {
		t.Fatalf("seeding archived block: %v", err)
	}

	link := func(a, b string, conf float64) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
			VALUES ($1::uuid, $2::uuid, 'topical', $3, $3, 'private') ON CONFLICT DO NOTHING`,
			a, b, conf); err != nil {
			t.Fatalf("linking: %v", err)
		}
	}
	// Drei dichte Gruppen plus schwache Bruecken — Louvain hat etwas zu finden.
	group := n / 3
	for i := 0; i < n-2; i++ {
		if (i+1)%group != 0 {
			link(ids[i], ids[i+1], 0.9)
		}
		if i+2 < n && (i+2)%group != 0 {
			link(ids[i], ids[i+2], 0.7)
		}
	}
	for g := 1; g*group < n; g++ {
		link(ids[g*group-1], ids[g*group], 0.2) // Bruecke
	}
	// BEIDE Richtungen desselben Paares: die einzige Form paralleler Kanten,
	// die das Schema zulaesst (PK source,target). Ihre Gewichte muessen
	// aufsummiert werden, und zwar in Cursor-Reihenfolge.
	link(ids[1], ids[0], 0.5)
	link(ids[2], ids[1], 0.4)
	// dangling in beide Richtungen.
	link(ids[0], archived, 0.8)
	link(archived, ids[3], 0.6)
	// Der letzte Knoten bleibt ISOLIERT (Grad 0) — Singleton-Cluster.
	return ids
}

// TestCSR_G1_PartitionIsIdenticalToTheLegacyPath ist das tragende Gate.
//
// Byte-Identitaet, nicht "aehnlich": derselbe Knotenschnitt, dieselben
// Kantengewichte, dieselbe Einfuegereihenfolge und derselbe feste Seed muessen
// dieselbe Partition ergeben. Verglichen wird die VOLLE Partition per
// reflect.DeepEqual, nicht die Clusterzahl — zwei verschiedene Partitionen
// koennen muehelos gleich viele Cluster haben.
//
// Verglichen wird ausserdem intraDegree: es entscheidet ueber den Cluster-Kern,
// und ein abweichendes letztes ULP dort wuerde core_hash kippen und ein Topic
// re-labeln, das sich nie bewegt hat.
func TestCSR_G1_PartitionIsIdenticalToTheLegacyPath(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedCSRCorpus(t, pool, 300)

	legacyNodes, legacyScopes, err := loadNodes(ctx, pool, csrTypes, nil)
	if err != nil {
		t.Fatalf("loadNodes: %v", err)
	}
	legacyEdges, err := loadEdges(ctx, pool, nil)
	if err != nil {
		t.Fatalf("loadEdges: %v", err)
	}
	legacy := computeClustering(legacyNodes, legacyEdges, 1.0)

	csrNodes, csrScopes, g, err := loadCSR(ctx, pool, csrTypes, nil)
	if err != nil {
		t.Fatalf("loadCSR: %v", err)
	}
	csr := computeClusteringCSR(csrNodes, g, 1.0)

	if !reflect.DeepEqual(legacyNodes, csrNodes) {
		t.Fatalf("Knotenordnung weicht ab: %d vs %d Knoten", len(legacyNodes), len(csrNodes))
	}
	if !reflect.DeepEqual(legacyScopes, csrScopes) {
		t.Fatal("Scope-Abbildung weicht ab")
	}
	if !reflect.DeepEqual(legacy.blockToCluster, csr.blockToCluster) {
		t.Fatalf("PARTITION WEICHT AB — CSR: %d Cluster / Q=%.9f, Ist: %d / Q=%.9f",
			csr.clusterCount, csr.modularity, legacy.clusterCount, legacy.modularity)
	}
	if !reflect.DeepEqual(legacy.intraDegree, csr.intraDegree) {
		t.Fatal("intraDegree weicht ab — der Cluster-Kern und damit core_hash wuerden kippen")
	}
	if legacy.modularity != csr.modularity {
		t.Fatalf("Q weicht ab: %.17g vs %.17g", legacy.modularity, csr.modularity)
	}
	if legacy.edgePairs != csr.edgePairs || legacy.dangling != csr.dangling || legacy.selfLoops != csr.selfLoops {
		t.Fatalf("Zaehler weichen ab: Paare %d/%d dangling %d/%d selfloops %d/%d",
			legacy.edgePairs, csr.edgePairs, legacy.dangling, csr.dangling, legacy.selfLoops, csr.selfLoops)
	}
	t.Logf("S3-G1: %d Knoten / %d Paare / %d dangling — Partition byte-identisch, Q=%.9f",
		len(csrNodes), csr.edgePairs, csr.dangling, csr.modularity)
}

// TestCSR_G4_CountersMatchThePsqlProbe ist S3-G4: die Zaehler des Loaders
// stimmen mit einer UNABHAENGIGEN SQL-Gegenprobe ueberein.
//
// Unabhaengig heisst: die Gegenprobe zaehlt nicht nach, was Go gezaehlt hat,
// sondern rechnet die Mengen aus dem Schema — sonst belegte sie nur, dass Go
// mit sich selbst einig ist.
func TestCSR_G4_CountersMatchThePsqlProbe(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedCSRCorpus(t, pool, 120)

	_, _, g, err := loadCSR(ctx, pool, csrTypes, nil)
	if err != nil {
		t.Fatalf("loadCSR: %v", err)
	}

	// dangling: Kanten, bei denen mindestens ein Endpunkt NICHT im Schnitt liegt.
	var wantDangling, wantSelfLoops, wantPairs int
	cut := `SELECT id FROM context_blocks WHERE NOT is_archived AND type_name = ANY($1)`
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM context_dream_links l
		 WHERE l.relationship <> 'supersedes'
		   AND (l.source_block_id NOT IN (`+cut+`) OR l.target_block_id NOT IN (`+cut+`))`,
		csrTypes).Scan(&wantDangling); err != nil {
		t.Fatalf("dangling probe: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM context_dream_links l
		 WHERE l.relationship <> 'supersedes' AND l.source_block_id = l.target_block_id
		   AND l.source_block_id IN (`+cut+`)`, csrTypes).Scan(&wantSelfLoops); err != nil {
		t.Fatalf("self-loop probe: %v", err)
	}
	// Paare: DISTINCT ueber das ungeordnete Paar, beide Endpunkte im Schnitt.
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM (
		  SELECT DISTINCT least(l.source_block_id, l.target_block_id) a,
		                  greatest(l.source_block_id, l.target_block_id) b
		    FROM context_dream_links l
		   WHERE l.relationship <> 'supersedes'
		     AND l.source_block_id <> l.target_block_id
		     AND l.source_block_id IN (`+cut+`) AND l.target_block_id IN (`+cut+`)) q`,
		csrTypes).Scan(&wantPairs); err != nil {
		t.Fatalf("pair probe: %v", err)
	}

	if g.Dangling != wantDangling {
		t.Errorf("dangling_n: Loader %d, SQL %d", g.Dangling, wantDangling)
	}
	if g.SelfLoops != wantSelfLoops {
		t.Errorf("selfloop_n: Loader %d, SQL %d", g.SelfLoops, wantSelfLoops)
	}
	if g.Pairs != wantPairs {
		t.Errorf("edge_n: Loader %d, SQL %d", g.Pairs, wantPairs)
	}
	if wantDangling == 0 {
		t.Fatal("die Fixture erzeugt keine dangling-Kanten — das Gate belegt nichts")
	}
	t.Logf("S3-G4: dangling=%d selfloops=%d pairs=%d, alle gegen SQL bestaetigt", g.Dangling, g.SelfLoops, g.Pairs)
}

// TestCSR_G6_SnapshotIsolationHolds ist S3-G6.
//
// Die Probe schreibt eine Kante, WAEHREND der Loader zwischen seinen beiden
// Passen steht. Mit REPEATABLE READ sieht Passe 2 dieselbe Menge wie Passe 1 —
// ohne sie schriebe Passe 2 ueber die Off-Grenze des Nachbarknotens, und die
// Partition waere falsch OHNE Fehlermeldung.
//
// Der Test greift dafuer in loadCSR hinein, indem er die beiden Passen von Hand
// gegen dieselbe Transaktion faehrt und dazwischen aus einer ZWEITEN Verbindung
// schreibt. Anders ist der Effekt nicht beobachtbar: von aussen sieht ein
// korrupter Build wie ein normaler aus — das ist ja der Vorwurf.
func TestCSR_G6_SnapshotIsolationHolds(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	ids := seedCSRCorpus(t, pool, 60)

	tx, err := pool.BeginTx(ctx, txRepeatableReadReadOnly())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	nodes, _, err := csrLoadNodes(ctx, tx, csrTypes, nil)
	if err != nil {
		t.Fatalf("csrLoadNodes: %v", err)
	}
	before := 0
	if err := csrScanEdges(ctx, tx, nil, func(_, _ [16]byte, _ float64) { before++ }); err != nil {
		t.Fatalf("pass 1: %v", err)
	}

	// Ein FREMDER Schreiber zwischen den Passen — genau das, was Dreams
	// Replace-Sweep im Hintergrund tut.
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
		VALUES ($1::uuid, $2::uuid, 'topical', 0.5, 0.5, 'private') ON CONFLICT DO NOTHING`,
		ids[10], ids[40]); err != nil {
		t.Fatalf("concurrent insert: %v", err)
	}
	// Die neue Kante MUSS ausserhalb dieser Transaktion sichtbar sein, sonst
	// belegt die Probe nichts ueber die Isolation.
	outside := 0
	if err := csrScanEdgesPool(ctx, pool, func() { outside++ }); err != nil {
		t.Fatalf("outside count: %v", err)
	}
	if outside <= before {
		t.Fatalf("die Fremd-Kante ist nirgends sichtbar (%d <= %d) — die Probe belegt nichts", outside, before)
	}

	after := 0
	if err := csrScanEdges(ctx, tx, nil, func(_, _ [16]byte, _ float64) { after++ }); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if after != before {
		t.Fatalf("S3-G6 ROT: Passe 1 sah %d Kanten, Passe 2 sieht %d — der Snapshot haelt nicht", before, after)
	}
	_ = nodes
	t.Logf("S3-G6: beide Passen sehen %d Kanten, obwohl ausserhalb %d sichtbar sind", after, outside)
}

func csrScanEdgesPool(ctx context.Context, pool *pgxpool.Pool, tick func()) error {
	rows, err := pool.Query(ctx, csrEdgeQuery)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		tick()
	}
	return rows.Err()
}
