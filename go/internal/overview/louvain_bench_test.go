//go:build integration

// Achse 04 / Welle S4 — Gate S4-G1 (Geschwindigkeit) und S4-G2 (Qualität).
//
// S4-G1 ist das HARTE Gate der Welle: der eigene Kern muss bei 200k Knoten /
// K1-Dichte mindestens 10× schneller sein als gonum. Die Referenz ist keine
// Schätzung, sondern die S1-Messung auf derselben Fixture: 40,0 s Median.
//
// Es steht hier im Paket overview und nicht in internal/louvain, weil es die
// S0-Fixturen braucht — und die leben hier. Der Kern selbst bleibt ein
// DB-freies Leaf-Package ohne Konsumenten (design/04 §7 S4).
package overview

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/louvain"
)

// fixtureToLouvain baut die CSR-Eingabe des eigenen Kerns aus einer
// S0-Fixture. Sie durchläuft dieselben Schritte wie der Produktions-Loader
// (zählen, füllen, verdichten), nur ohne DB — die Fixture IST der Snapshot.
func fixtureToLouvain(f *scaleFixture) *louvain.Graph {
	g := buildCSRFromFixture(f)
	return louvain.NewGraph(g.Off, g.Adj, g.W)
}

// TestLouvainBenchG1_SpeedVsGonum ist S4-G1.
func TestLouvainBenchG1_SpeedVsGonum(t *testing.T) {
	if os.Getenv(benchEnv) == "" {
		t.Skip(benchEnv + " ungesetzt — S4-G1 übersprungen")
	}
	fmt.Printf("\n=== S4-G1 — eigener Kern gegen gonum, konstante Dichte, Median aus 3 ===\n")
	fmt.Printf("%-14s %10s %11s %12s %12s %9s %10s %10s %9s\n",
		"Punkt", "Knoten", "Paare", "gonum", "ctx", "Faktor", "Q gonum", "Q ctx", "ΔQ")

	type row struct {
		label                string
		nodes, pairs         int
		gonumMS, ctxMS       int64
		gonumQ, ctxQ         float64
		gonumCl, ctxCl       int
		levels, moves, sweep int
	}
	var rows []row

	for _, tc := range []struct {
		label string
		spec  scaleSpec
	}{
		{"K1-50k", specK1Organic(50_000, 7)},
		{"K1-200k", specK1Organic(200_000, 7)},
		{"K1-400k", specK1Organic(400_000, 7)},
		{"K2-200k", specK2Flat(200_000, 7)},
	} {
		f, err := resolveScale(tc.spec)
		if err != nil {
			t.Fatalf("resolve %s: %v", tc.label, err)
		}
		lg := fixtureToLouvain(f)

		// Eigener Kern: 3 Läufe, Median.
		ctxDurs := make([]time.Duration, 0, 3)
		var lastRes louvain.Result
		for i := 0; i < 3; i++ {
			start := time.Now()
			res, err := louvain.Run(context.Background(), lg, louvain.Options{Resolution: 1.0})
			if err != nil {
				t.Fatalf("%s: louvain.Run: %v", tc.label, err)
			}
			ctxDurs = append(ctxDurs, time.Since(start))
			lastRes = res
		}
		sort.Slice(ctxDurs, func(i, j int) bool { return ctxDurs[i] < ctxDurs[j] })

		// gonum auf DEMSELBEN Graphen — über den Produktionspfad
		// computeClusteringCSR, damit der Vergleich den echten Ist-Pfad trifft
		// und nicht eine Bench-eigene Nachbildung.
		nodes := f.nodeUUIDs()
		csr := buildCSRFromFixture(f)
		gonumDurs := make([]time.Duration, 0, 3)
		var lastCl clustering
		for i := 0; i < 3; i++ {
			start := time.Now()
			lastCl = computeClusteringCSR(nodes, csr, 1.0)
			gonumDurs = append(gonumDurs, time.Since(start))
		}
		sort.Slice(gonumDurs, func(i, j int) bool { return gonumDurs[i] < gonumDurs[j] })

		r := row{
			label: tc.label, nodes: f.spec.Nodes, pairs: csr.Pairs,
			gonumMS: gonumDurs[1].Milliseconds(), ctxMS: ctxDurs[1].Milliseconds(),
			gonumQ: lastCl.modularity, ctxQ: lastRes.Q,
			gonumCl: lastCl.clusterCount, ctxCl: lastRes.Clusters,
			levels: lastRes.Levels, moves: lastRes.Moves, sweep: lastRes.Sweeps,
		}
		rows = append(rows, r)
		factor := float64(r.gonumMS) / float64(max64(r.ctxMS, 1))
		fmt.Printf("%-14s %10d %11d %11dms %11dms %8.1fx %10.4f %10.4f %9.4f\n",
			r.label, r.nodes, r.pairs, r.gonumMS, r.ctxMS, factor, r.gonumQ, r.ctxQ, r.ctxQ-r.gonumQ)
	}

	fmt.Printf("\nEbenen / Züge / Warteschlangen-Entnahmen des eigenen Kerns:\n")
	for _, r := range rows {
		fmt.Printf("  %-14s Ebenen=%d Züge=%d Entnahmen=%d  Cluster gonum=%d ctx=%d\n",
			r.label, r.levels, r.moves, r.sweep, r.gonumCl, r.ctxCl)
	}

	// Das Abbruch-Kriterium des Entwurfs, am 200k-Punkt.
	for _, r := range rows {
		if r.label != "K1-200k" {
			continue
		}
		factor := float64(r.gonumMS) / float64(max64(r.ctxMS, 1))
		fmt.Printf("\n[S4-G1] 200k/K1: gonum %d ms, ctx %d ms ⇒ Faktor %.1fx (Kriterium ≥ 10x)\n",
			r.gonumMS, r.ctxMS, factor)
		if factor < 10 {
			t.Errorf("S4-G1 ROT: nur %.1fx schneller bei 200k — Entscheid §4.2 fällt", factor)
		}
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// specStructured bildet .project/bench-graph/seed-structured.sql nach — die
// Ground-Truth, die design/04 S4-G2 benennt: Communities von 1.000 Knoten,
// 3 Kanten je Knoten, 90 % intra, inter-Ziel eine der naechsten 6 Communities.
//
// Genau diese Struktur ist eine WOHLGETRENNTE gepflanzte Partition: die
// Communities sind innen dicht genug, dass die Modularitaet sie als Optimum
// sieht. Das ist die Bedingung, unter der ein ARI-Vergleich ueberhaupt etwas
// aussagt (s. groundTruthUsable).
func specStructured(nodes int, seed uint64) scaleSpec {
	s := specK1Organic(nodes, seed)
	s.Shape = shapeFlat
	s.PairsPerNode = 3
	s.CommunityAvg = 1000
	s.IntraFrac = 0.9
	s.InterFanout = 6
	s.FringeFrac = 0.001 // seed-structured.sql kennt keinen Rand
	return s
}

// groundTruthUsableARI ist die Schwelle, ab der eine Fixture ueberhaupt als
// Ground-Truth taugt.
//
// Der Grund ist ein Befund und keine Bequemlichkeit: auf den K1/K2-Skalen-
// Fixturen erreichen BEIDE Engines nur ARI 0,12 bis 0,44 — sie finden dort
// uebereinstimmend ~2.000 Communities der mittleren Groesse 25, egal wie gross
// die gepflanzten sind. Die gepflanzte Struktur ist dort schlicht NICHT das
// Modularitaets-Optimum (dieselbe Beobachtung, die schon der S1-Mikro-Bench
// gemacht hat). Ein ARI-Vergleich zweier Heuristiken gegen eine Referenz, die
// keine von beiden anstrebt, misst Rauschen.
//
// Dasselbe gilt fuer die 200k-Stufe der strukturierten Fixture: bei 2m ≈ 1,2M
// liegt die Aufloesungsgrenze der Modularitaet (√2m ≈ 1.100) genau auf der
// gepflanzten Community-Groesse, und BEIDE Engines legen zusammen (143 bzw. 144
// gefundene gegen 200 gepflanzte, ARI ≈ 0,15). Das ist eine Eigenschaft der
// Zielfunktion, nicht der Implementierung.
const groundTruthUsableARI = 0.70

// S4-G2 laeuft ueber ein SEED-ENSEMBLE und bewertet den MITTELWERT.
//
// Das ist eine bewusste Schaerfung des Entwurfs-Kriteriums ("ARI_ctx <
// ARI_gonum ⇒ rot"), und sie steht auf einer Messung: ueber acht Seeds
// derselben Fixture gewinnt der eigene Kern VIER Mal und gonum vier Mal
// (0,7754/0,7918 · 0,8108/0,7966 · 0,7601/0,7334 · 0,8103/0,8107 ·
// 0,7473/0,7601 · 0,7918/0,7778 · 0,7938/0,7802 · 0,8034/0,8041). Ein
// Ein-Seed-Vergleich zweier Heuristiken auf derselben Zielfunktion ist damit
// nachweislich ein Muenzwurf — ein Gate darauf zu bauen hiesse, die Welle vom
// Zufall freigeben oder blockieren zu lassen.
//
// Bewertet wird deshalb der Mittelwert ueber das Ensemble, mit einer Toleranz,
// die kleiner ist als die beobachtete Ein-Seed-Streuung (±0,027) und trotzdem
// jede systematische Verschlechterung faengt.
var g2Seeds = []uint64{7, 11, 23, 42, 99, 1234, 555, 8080}

const g2ARITolerance = 0.01

// TestLouvainBenchG2_QualityVsGroundTruth ist S4-G2.
//
// Gemessen wird gegen eine Fixture mit BEKANNTER Community-Struktur — Q allein
// genuegt nicht: die Aufloesungsgrenze der Modularitaet belohnt zu grobes
// Clustern, ein Kern koennte also mit einem HOEHEREN Q die Struktur schlechter
// treffen. ARI vergleicht gegen die Wahrheit.
func TestLouvainBenchG2_QualityVsGroundTruth(t *testing.T) {
	if os.Getenv(benchEnv) == "" {
		t.Skip(benchEnv + " ungesetzt — S4-G2 uebersprungen")
	}

	// (a) Das Ensemble auf der Fixture, die design/04 benennt.
	fmt.Printf("\n=== S4-G2 (a) — Seed-Ensemble auf seed-structured (50k, 1.000er-Communities, 90 %% intra) ===\n")
	fmt.Printf("%-8s %10s %9s %10s %9s %10s %9s\n", "Seed", "ARI gonum", "ARI ctx", "Q gonum", "Q ctx", "Cl gonum", "Cl ctx")
	var sumG, sumC float64
	var wins int
	for _, seed := range g2Seeds {
		f, err := resolveScale(specStructured(50_000, seed))
		if err != nil {
			t.Fatalf("resolve seed %d: %v", seed, err)
		}
		truth := fixtureGroundTruth(f)
		csr := buildCSRFromFixture(f)
		nodes := f.nodeUUIDs()
		cl := computeClusteringCSR(nodes, csr, 1.0)
		res, err := louvain.Run(context.Background(), louvain.NewGraph(csr.Off, csr.Adj, csr.W), louvain.Options{Resolution: 1.0})
		if err != nil {
			t.Fatalf("louvain.Run seed %d: %v", seed, err)
		}
		ag := louvain.ARI(partitionToDense(nodes, cl), truth)
		ac := louvain.ARI(res.Memb, truth)
		sumG += ag
		sumC += ac
		if ac > ag {
			wins++
		}
		fmt.Printf("%-8d %10.4f %9.4f %10.5f %9.5f %10d %9d\n",
			seed, ag, ac, cl.modularity, res.Q, cl.clusterCount, res.Clusters)
	}
	meanG := sumG / float64(len(g2Seeds))
	meanC := sumC / float64(len(g2Seeds))
	fmt.Printf("Mittel: ARI gonum %.4f · ctx %.4f (Δ %+.4f) — ctx besser in %d von %d Seeds\n",
		meanG, meanC, meanC-meanG, wins, len(g2Seeds))
	if meanG < groundTruthUsableARI && meanC < groundTruthUsableARI {
		t.Fatalf("S4-G2 UNBRAUCHBAR: keine Engine erreicht ARI %.2f — die Fixture taugt nicht als Ground-Truth", groundTruthUsableARI)
	}
	if meanC < meanG-g2ARITolerance {
		t.Errorf("S4-G2 ROT: mittlerer ARI ctx %.4f < gonum %.4f", meanC, meanG)
	}

	// (b) Die uebrigen Fixturen als KONTEXT — berichtet, nicht bewertet, mit
	// der Begruendung in der Rolle-Spalte.
	fmt.Printf("\n=== S4-G2 (b) — Kontext-Fixturen ===\n")
	fmt.Printf("%-22s %9s %9s %9s %10s %9s %9s %9s %s\n",
		"Fixture", "Knoten", "Q gonum", "Q ctx", "ARI gonum", "ARI ctx", "Cl gonum", "Cl ctx", "Rolle")
	for _, tc := range []struct {
		label string
		spec  scaleSpec
	}{
		{"seed-structured-200k", specStructured(200_000, 7)},
		{"K1-organisch-50k", specK1Organic(50_000, 7)},
		{"K2-flach-50k", specK2Flat(50_000, 7)},
	} {
		f, err := resolveScale(tc.spec)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		truth := fixtureGroundTruth(f)
		csr := buildCSRFromFixture(f)
		nodes := f.nodeUUIDs()
		cl := computeClusteringCSR(nodes, csr, 1.0)
		res, err := louvain.Run(context.Background(), louvain.NewGraph(csr.Off, csr.Adj, csr.W), louvain.Options{Resolution: 1.0})
		if err != nil {
			t.Fatalf("louvain.Run: %v", err)
		}
		ag := louvain.ARI(partitionToDense(nodes, cl), truth)
		ac := louvain.ARI(res.Memb, truth)
		role := "Struktur ist nicht das Q-Optimum — beide Engines gleich weit daneben"
		fmt.Printf("%-22s %9d %9.4f %9.4f %10.4f %9.4f %9d %9d %s\n",
			tc.label, f.spec.Nodes, cl.modularity, res.Q, ag, ac, cl.clusterCount, res.Clusters, role)
	}
}

// fixtureGroundTruth liefert die GEPFLANZTE Community je Knotenindex.
func fixtureGroundTruth(f *scaleFixture) []int32 {
	truth := make([]int32, f.spec.Nodes)
	for i := range truth {
		truth[i] = -1
	}
	c := f.communities()
	for ci := 0; ci < c; ci++ {
		size := int(f.commOff[ci+1] - f.commOff[ci])
		for k := 0; k < size; k++ {
			truth[f.member(ci, k)] = int32(ci) //nolint:gosec // c < 2^31
		}
	}
	// Die Fringe-Knoten (isoliert und Zweier-Komponenten) bekommen eigene
	// Communities — sie GEHÖREN zu keiner gepflanzten Gruppe, und sie
	// stillschweigend in eine zu werfen würde den ARI beschönigen.
	next := int32(c) //nolint:gosec // c < 2^31
	for i := range truth {
		if truth[i] < 0 {
			truth[i] = next
			next++
		}
	}
	return truth
}

// partitionToDense übersetzt die uuid-basierte Partition in dichte Indizes
// über die Knotenreihenfolge — die Form, die ARI erwartet.
func partitionToDense(nodes []string, cl clustering) []int32 {
	ids := make(map[string]int32, len(nodes)/8+1)
	out := make([]int32, len(nodes))
	for i, u := range nodes {
		cid := cl.blockToCluster[u]
		id, ok := ids[cid]
		if !ok {
			id = int32(len(ids)) //nolint:gosec // Clusterzahl < 2^31
			ids[cid] = id
		}
		out[i] = id
	}
	return out
}
