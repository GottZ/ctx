package overview

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"

	"gonum.org/v1/gonum/graph/community"
	"gonum.org/v1/gonum/graph/simple"
)

// computeClusteringCSR ist der Rechenkern auf dem CSR-Substrat (S3).
//
// Er baut den gonum-Graphen weiterhin — S3 tauscht das SUBSTRAT, nicht die
// Engine. Der eigene Mover kommt mit S4; bis dahin muss diese Funktion
// beweisen, dass die CSR dieselbe Partition trägt wie der []rawEdge-Pfad
// (Gate S3-G1), und dafür ist ein unveränderter gonum-Aufruf die einzige
// ehrliche Referenz. Eine Welle, die Substrat UND Engine gleichzeitig tauscht,
// könnte einen Unterschied keinem von beidem zuordnen.
//
// BYTE-IDENTITÄT, wo sie herkommt: die Kanten werden in der Reihenfolge
// (u aufsteigend, dann v aufsteigend, nur u<v) eingefügt. Das ist exakt die
// Ordnung, die der Ist-Pfad über sein sort.Slice der aggregierten Paare
// herstellt — dieselbe Einfügereihenfolge, dieselben float64-Gewichte,
// derselbe feste PCG-Seed ⇒ dieselbe Partition.
func computeClusteringCSR(uuids []string, g *csrGraph, resolution float64) clustering {
	n := len(uuids)
	if n == 0 {
		return clustering{blockToCluster: map[string]string{}, intraDegree: map[string]float64{}}
	}

	gr := simple.NewWeightedUndirectedGraph(0, 0)
	for i := 0; i < n; i++ {
		gr.AddNode(simple.Node(int64(i))) // isolierte Knoten auch → Singleton-Cluster
	}
	for u := 0; u < n; u++ {
		for k := g.Off[u]; k < g.Off[u+1]; k++ {
			v := int(g.Adj[k])
			if v <= u {
				continue // jede ungerichtete Kante genau einmal, in der u<v-Richtung
			}
			w := g.W[k]
			if w <= 0 {
				w = 1e-9 // Modularize paniert bei negativem Gewicht; 0 ist bedeutungslos
			}
			gr.SetWeightedEdge(simple.WeightedEdge{F: simple.Node(int64(u)), T: simple.Node(int64(v)), W: w})
		}
	}

	reduced := community.Modularize(gr, resolution, rand.NewPCG(louvainSeed1, louvainSeed2))
	comms := reduced.Communities()

	b2c := make(map[string]string, n)
	for _, members := range comms {
		var minUUID string
		for _, node := range members {
			if u := uuids[node.ID()]; minUUID == "" || u < minUUID {
				minUUID = u
			}
		}
		for _, node := range members {
			b2c[uuids[node.ID()]] = minUUID
		}
	}

	// Intra-Cluster-Grad, eine Passe über die CSR — wieder nur die u<v-Hälfte
	// und in aufsteigender Ordnung. Dieselbe Summationsreihenfolge wie im
	// Ist-Pfad, aus demselben Grund: der Cluster-Kern wird nach Grad-Ordnung
	// gewählt und gehasht, ein wackelndes letztes Bit könnte zwei Nachbarn
	// vertauschen, core_hash ändern und ein Topic re-labeln, das sich nie
	// bewegt hat.
	deg := make(map[string]float64, n)
	for _, u := range uuids {
		deg[u] = 0
	}
	for u := 0; u < n; u++ {
		for k := g.Off[u]; k < g.Off[u+1]; k++ {
			v := int(g.Adj[k])
			if v <= u {
				continue
			}
			ua, ub := uuids[u], uuids[v]
			if b2c[ua] != b2c[ub] {
				continue
			}
			deg[ua] += g.W[k]
			deg[ub] += g.W[k]
		}
	}

	q := community.Q(gr, comms, resolution)
	if math.IsNaN(q) || math.IsInf(q, 0) {
		q = 0 // 0-Kanten-Graph → undefiniertes Q; 0 melden statt NaN
	}
	return clustering{
		blockToCluster: b2c, intraDegree: deg, modularity: q, clusterCount: len(comms),
		edgePairs: g.Pairs, dangling: g.Dangling, selfLoops: g.SelfLoops,
	}
}

// clusterCSRWithCtx spiegelt clusterWithCtx fuer den CSR-Pfad: Modularize ist
// weiterhin ein undurchdringlicher gonum-Aufruf, der ctx nicht beobachten kann,
// also bleibt der dokumentierte Goroutine-Leak beim Abbruch — unveraendert und
// bewusst, denn S3 tauscht das Substrat und nicht die Engine. Er stirbt
// strukturell mit dem Kindprozess (E-B) und faellt mit S4 ganz weg, weil der
// eigene Mover zwischen den Sweeps ctx-beobachtbar ist.
func clusterCSRWithCtx(ctx context.Context, uuids []string, g *csrGraph, resolution float64) (clustering, error) {
	done := make(chan clustering, 1)
	go func() {
		done <- computeClusteringCSR(uuids, g, resolution)
	}()
	select {
	case <-ctx.Done():
		return clustering{}, fmt.Errorf("clustering aborted (goroutine keeps running until convergence): %w", ctx.Err())
	case cl := <-done:
		return cl, nil
	}
}
