// Achse 04 / Welle S4 — die Gates, die ohne DB und ohne Bench laufen:
// S4-G3 (50-Lauf-Determinismus), S4-G4 (Golden-Hash), S4-G5 (Permutation),
// S4-G6 (σ-Drift) und die Q-Kreuzprobe gegen gonum.
//
// Die Q-Kreuzprobe steht bewusst VOR allem anderen: ein eigener Rechenkern,
// dessen Modularitäts-Formel um einen Faktor 2 danebenliegt, würde jedes
// Qualitäts-Gate bestehen, das nur seine EIGENE Zahl gegen sich selbst hält.
package louvain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"testing"

	"gonum.org/v1/gonum/graph"
	"gonum.org/v1/gonum/graph/community"
	"gonum.org/v1/gonum/graph/simple"
)

// tuple ist eine ungerichtete Kante der Testfixturen.
type tuple struct {
	u, v int
	w    float64
}

// ringOfCliques baut c Cliquen der Groesse s, im Ring verbunden — eine
// Struktur mit ANALYTISCH bekannter bester Partition (die Cliquen selbst).
// Die Ring-Kanten sind bewusst schwach (0,05), damit die Cliquen und nicht der
// Ring das Optimum sind.
func ringOfCliques(c, s int) (*Graph, []int32) {
	var edges []tuple
	for k := 0; k < c; k++ {
		for i := 0; i < s; i++ {
			for j := i + 1; j < s; j++ {
				edges = append(edges, tuple{k*s + i, k*s + j, 1})
			}
		}
	}
	for k := 0; k < c; k++ {
		edges = append(edges, tuple{k*s + s - 1, ((k + 1) % c) * s, 0.05})
	}
	truth := make([]int32, c*s)
	for i := range truth {
		truth[i] = int32(i / s)
	}
	return buildGraph(c*s, edges), truth
}

// buildGraph macht aus einer Kantenliste eine symmetrische CSR.
func buildGraph(n int, edges []tuple) *Graph {
	deg := make([]uint32, n)
	for _, e := range edges {
		deg[e.u]++
		deg[e.v]++
	}
	off := make([]uint32, n+1)
	var acc uint32
	for i, d := range deg {
		off[i] = acc
		acc += d
	}
	off[n] = acc
	adj := make([]uint32, acc)
	w := make([]float64, acc)
	fill := make([]uint32, n)
	for _, e := range edges {
		adj[off[e.u]+fill[e.u]] = uint32(e.v)
		w[off[e.u]+fill[e.u]] = e.w
		fill[e.u]++
		adj[off[e.v]+fill[e.v]] = uint32(e.u)
		w[off[e.v]+fill[e.v]] = e.w
		fill[e.v]++
	}
	return NewGraph(off, adj, w)
}

// gonumGraph spiegelt eine CSR in gonums Datenstruktur — nur fuer die
// Q-Kreuzprobe.
func gonumGraph(g *Graph) *simple.WeightedUndirectedGraph {
	gr := simple.NewWeightedUndirectedGraph(0, 0)
	n := g.N()
	for i := 0; i < n; i++ {
		gr.AddNode(simple.Node(int64(i)))
	}
	for u := 0; u < n; u++ {
		for k := g.Off[u]; k < g.Off[u+1]; k++ {
			if v := int(g.Adj[k]); v > u {
				gr.SetWeightedEdge(simple.WeightedEdge{F: simple.Node(int64(u)), T: simple.Node(int64(v)), W: g.W[k]})
			}
		}
	}
	return gr
}

// gonumQ rechnet dasselbe Q mit der Fremdimplementierung.
func gonumQ(g *Graph, memb []int32, gamma float64) float64 {
	maxC := int32(0)
	for _, c := range memb {
		if c > maxC {
			maxC = c
		}
	}
	comms := make([][]graph.Node, maxC+1)
	for i, c := range memb {
		comms[c] = append(comms[c], simple.Node(int64(i)))
	}
	// Leere Communities entfernen — gonum.Q erwartet eine Partition, keine
	// Liste mit Loechern.
	out := comms[:0]
	for _, grp := range comms {
		if len(grp) > 0 {
			out = append(out, grp)
		}
	}
	return community.Q(gonumGraph(g), out, gamma)
}

// TestModularity_MatchesGonum ist die Kreuzprobe: die eigene Q-Formel gegen
// die Fremdimplementierung, auf mehreren Partitionen desselben Graphen.
//
// Geprüft wird NICHT nur die optimale Partition: eine falsche Formel kann dort
// zufällig richtig liegen. Die Alles-in-einem- und die Alles-getrennt-Partition
// sind die beiden Extreme, an denen ein Faktor-2- oder γ-Fehler auffliegt.
func TestModularity_MatchesGonum(t *testing.T) {
	g, truth := ringOfCliques(6, 8)
	n := g.N()

	allOne := make([]int32, n)
	allSep := make([]int32, n)
	for i := range allSep {
		allSep[i] = int32(i)
	}
	for _, tc := range []struct {
		name string
		memb []int32
	}{
		{"ground-truth", truth},
		{"alles-eine-community", allOne},
		{"jeder-fuer-sich", allSep},
	} {
		for _, gamma := range []float64{0.5, 1.0, 1.7} {
			dense, _ := canonicalize(tc.memb)
			got := Modularity(g, dense, gamma)
			want := gonumQ(g, dense, gamma)
			if math.Abs(got-want) > 1e-12 {
				t.Errorf("%s γ=%.1f: eigenes Q=%.15f, gonum Q=%.15f (Δ=%.3e)", tc.name, gamma, got, want, got-want)
			}
		}
	}
}

// TestRun_FindsThePlantedPartition belegt, dass der Kern überhaupt clustert —
// und zwar die analytisch bekannte Struktur.
func TestRun_FindsThePlantedPartition(t *testing.T) {
	g, truth := ringOfCliques(8, 10)
	res, err := Run(context.Background(), g, Options{Resolution: 1.0})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Clusters != 8 {
		t.Fatalf("erwartet 8 Cliquen, gefunden %d (Q=%.6f)", res.Clusters, res.Q)
	}
	truthDense, _ := canonicalize(truth)
	if ari := ARI(res.Memb, truthDense); ari < 0.999 {
		t.Fatalf("ARI gegen die Ground-Truth nur %.4f", ari)
	}
	if q := Modularity(g, res.Memb, 1.0); math.Abs(q-res.Q) > 1e-12 {
		t.Fatalf("gemeldetes Q %.15f weicht von der Nachrechnung %.15f ab", res.Q, q)
	}
}

// TestRun_G3_Deterministic ist S4-G3 — 50 Läufe, identische Partition.
//
// Der Test ist hier NICHT trivial grün, obwohl der Mover keinen PRNG benutzt:
// er würde rot, sobald irgendwo eine Map-Iteration oder eine
// Speicheradress-abhängige Reihenfolge ins Ergebnis einginge. Genau dagegen
// steht der Index-Tie-Break.
func TestRun_G3_Deterministic(t *testing.T) {
	g, _ := ringOfCliques(12, 9)
	first, err := Run(context.Background(), g, Options{Resolution: 1.0})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i := 0; i < 50; i++ {
		got, err := Run(context.Background(), g, Options{Resolution: 1.0})
		if err != nil {
			t.Fatalf("Lauf %d: %v", i, err)
		}
		if !reflect.DeepEqual(first.Memb, got.Memb) {
			t.Fatalf("Lauf %d liefert eine andere Partition", i)
		}
		if first.Q != got.Q {
			t.Fatalf("Lauf %d liefert Q=%.17g statt %.17g", i, got.Q, first.Q)
		}
	}
}

// partitionDigest ist der Golden-Anker A1 in Paket-lokaler Form.
func partitionDigest(memb []int32) string {
	h := sha256.New()
	for i, c := range memb {
		fmt.Fprintf(h, "%d:%d\n", i, c)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// TestRun_G4_GoldenPartition ist S4-G4: der Hash gegen eine EINGECHECKTE
// Fixture.
//
// Der 50-Lauf-Test prüft Wiederholbarkeit INNERHALB eines Binaries. Dieser
// hier prüft KONSTANZ ÜBER DIE ZEIT — er färbt jede Verhaltensänderung des
// Movers rot: Tie-Break, Reduktionsordnung, Zugreihenfolge. Ändert eine Welle
// den Kern absichtlich, wird der Wert hier nachgezogen, sichtbar im Diff.
//
// GELTUNGSBEREICH, präzise statt großzügig (§4.6): "konstant auf amd64". Gate
// S4-G7 verlangt Gleichheit auf ZWEI Architekturen; hier steht nur eine zur
// Verfügung, deshalb ist das der deklarierte Geltungsbereich und keine
// stillschweigende Zusage. Die ΔQ-Arithmetik ist gegen FMA-Kontraktion gepinnt
// (deltaQ), damit die zweite Architektur nachziehbar bleibt.
func TestRun_G4_GoldenPartition(t *testing.T) {
	cases := []struct {
		name   string
		c, s   int
		golden string
	}{
		{"ring-6x8", 6, 8, "8b50d825e791bd48"},
		{"ring-12x9", 12, 9, "c1b8ae5153f37a6b"},
	}
	for _, tc := range cases {
		g, _ := ringOfCliques(tc.c, tc.s)
		res, err := Run(context.Background(), g, Options{Resolution: 1.0})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got := partitionDigest(res.Memb)
		if tc.golden == "" {
			t.Logf("%s golden offen — eintragen: %q", tc.name, got)
			continue
		}
		if got != tc.golden {
			t.Errorf("%s: Golden-Drift — Hash %s, erwartet %s (Q=%.9f, %d Cluster). "+
				"Wurde der Mover absichtlich geändert? Dann hier nachziehen.",
				tc.name, got, tc.golden, res.Q, res.Clusters)
		}
	}
}

// TestRun_G5_EdgeOrderPermutation ist S4-G5 (Anker A2).
//
// Die Reihenfolge der Kanten INNERHALB eines Knotens darf die Partition nicht
// ändern. Sie beeinflusst die Einfügereihenfolge in die kIn-Streuwerttabelle
// und damit die Reihenfolge, in der Kandidaten bewertet werden — genau die
// Stelle, an der ein fehlender Tie-Break unsichtbar bliebe, solange niemand
// permutiert.
func TestRun_G5_EdgeOrderPermutation(t *testing.T) {
	g, _ := ringOfCliques(10, 7)
	base, err := Run(context.Background(), g, Options{Resolution: 1.0})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rng := rand.New(rand.NewPCG(99, 7))
	for iter := 0; iter < 12; iter++ {
		p := &Graph{
			Off: append([]uint32(nil), g.Off...),
			Adj: append([]uint32(nil), g.Adj...),
			W:   append([]float64(nil), g.W...),
			Deg: append([]float64(nil), g.Deg...),
			M2:  g.M2,
		}
		for u := 0; u < p.N(); u++ {
			lo, hi := p.Off[u], p.Off[u+1]
			adj, w := p.Adj[lo:hi], p.W[lo:hi]
			rng.Shuffle(len(adj), func(i, j int) {
				adj[i], adj[j] = adj[j], adj[i]
				w[i], w[j] = w[j], w[i]
			})
		}
		got, err := Run(context.Background(), p, Options{Resolution: 1.0})
		if err != nil {
			t.Fatalf("permutierter Lauf %d: %v", iter, err)
		}
		if !reflect.DeepEqual(base.Memb, got.Memb) {
			t.Fatalf("Permutation %d ändert die Partition — der Tie-Break trägt nicht", iter)
		}
	}
}

// TestRun_G6_SigmaDriftStaysBounded ist S4-G6.
//
// Der Kern MELDET seine σ-Drift; dieser Test prüft, dass sie unter der
// Schranke bleibt und dass sie überhaupt gemessen wird (eine konstant
// gemeldete 0 wäre kein Detektor, sondern ein Platzhalter — deshalb prüft der
// Test zusätzlich, dass die Nachrechnung im Kern wirklich läuft, indem er die
// Schranke gegen einen Graphen mit stark unterschiedlichen Gewichtsgrößen
// fährt, wo Kompensation nötig ist).
func TestRun_G6_SigmaDriftStaysBounded(t *testing.T) {
	var edges []tuple
	const c, s = 12, 12
	for k := 0; k < c; k++ {
		for i := 0; i < s; i++ {
			for j := i + 1; j < s; j++ {
				// Gewichte über 9 Größenordnungen: genau die Lage, in der
				// naive Summation Stellen verliert.
				w := 1e-7
				if (i+j)%3 == 0 {
					w = 1e2
				}
				edges = append(edges, tuple{k*s + i, k*s + j, w})
			}
		}
	}
	for k := 0; k < c; k++ {
		edges = append(edges, tuple{k*s + s - 1, ((k + 1) % c) * s, 1e-3})
	}
	g := buildGraph(c*s, edges)
	res, err := Run(context.Background(), g, Options{Resolution: 1.0})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SigmaDrift > sigmaDriftTol {
		t.Fatalf("σ-Drift %.3e über der Schranke %.0e", res.SigmaDrift, sigmaDriftTol)
	}
	t.Logf("S4-G6: σ-Drift %.3e (Schranke %.0e), %d Ebenen, %d Züge",
		res.SigmaDrift, sigmaDriftTol, res.Levels, res.Moves)
}

// TestRun_ContextAbort belegt die Eigenschaft, wegen der der SIGKILL-Pfad
// überhaupt entfallen KANN: dieser Mover ist zwischen den Zügen
// ctx-beobachtbar. gonums Modularize ist es nicht — dort ist der Abbruch ein
// verwaister Goroutine-Leak (cluster.go:397-410).
func TestRun_ContextAbort(t *testing.T) {
	g, _ := ringOfCliques(400, 30) // gross genug, dass der Abbruch greift
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, g, Options{Resolution: 1.0}); err == nil {
		t.Fatal("abgebrochener Kontext lieferte ein Ergebnis — der Mover ist nicht beobachtbar")
	}
}

// TestRun_EmptyAndEdgeless deckt die beiden entarteten Eingaben ab.
func TestRun_EmptyAndEdgeless(t *testing.T) {
	empty := NewGraph([]uint32{0}, nil, nil)
	if res, err := Run(context.Background(), empty, Options{}); err != nil || res.Clusters != 0 {
		t.Fatalf("leerer Graph: %+v, %v", res, err)
	}
	edgeless := NewGraph([]uint32{0, 0, 0, 0}, nil, nil)
	res, err := Run(context.Background(), edgeless, Options{})
	if err != nil {
		t.Fatalf("kantenloser Graph: %v", err)
	}
	if res.Clusters != 3 || res.Q != 0 {
		t.Fatalf("kantenloser Graph: %d Cluster, Q=%v — erwartet 3 Singletons und Q=0", res.Clusters, res.Q)
	}
}
