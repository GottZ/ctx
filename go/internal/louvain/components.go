package louvain

import (
	"context"
	"fmt"
	"math"
)

// Der Komponenten-Vorpass mit γ-Reskalierung (Achse 04 / S8, design/04 §4.4).
//
// ══ DER ÄQUIVALENZBEWEIS, und warum er die Reskalierung ERZWINGT ══
//
// Sei G die disjunkte Vereinigung der Komponenten G₁…G_p mit
// Kantengewichtssummen m₁…m_p, Σ m_t = m. Eine Community, die zwei Komponenten
// überspannt, lässt sich stets mit STRIKT POSITIVEM ΔQ trennen (der A-Term
// zwischen Komponenten ist 0, der −γ·k·k/2m-Term strikt negativ) — im Optimum
// überspannt also keine Community eine Komponente. Damit zerfällt die Summe:
//
//	Q = Σ_t (m_t/m) · Q_t(γ_t)      mit    γ_t = γ · m_t / m
//
// Die Gewichte m_t/m sind positive Konstanten, die Summanden hängen an
// disjunkten Variablenmengen ⇒ die globale Maximierung zerfällt EXAKT in p
// unabhängige Maximierungen mit reskaliertem γ.
//
// ZWEI KONSEQUENZEN, beide praktisch:
//
//  1. Der Vorpass ist EXAKT, nicht approximativ — die Zielfunktion ist
//     dieselbe. (Louvain bleibt eine Heuristik: die ZIELFUNKTION ist
//     identisch, der PFAD der Heuristik kann abweichen. Gate S8-G1 misst genau
//     diese Abweichung, statt sie wegzudefinieren.)
//  2. OHNE Reskalierung wäre der Vorpass FALSCH. γ_t < γ heisst: eine kleine
//     Komponente wird seltener zerschnitten — und genau das ist gewollt. Eine
//     6-Knoten-Komponente soll nicht in drei Communities zerfallen, nur weil
//     der übrige Korpus gross ist. Wer die Komponenten ohne Reskalierung
//     einzeln clustert, ändert LAUTLOS die Auflösung.
//
// ══ WAS ER PRAKTISCH BRINGT — ehrlich beziffert ══
//
// Live 6,3 %: 75 von 1.192 Knoten liegen ausserhalb der Riesenkomponente
// (§2.4, 93,7 %). Der Vorpass ist damit BEIWERK, kein Argument — und er wird
// asymptotisch schlechter, weil eine Riesenkomponente bei Ø-Grad 4,49 die
// strukturell erzwungene Normalform ist und mit wachsendem Korpus relativ
// GRÖSSER wird. Gebaut wird er trotzdem: er kostet O(m·α), er ist beweisbar
// exakt, und er macht die Komponentenzahl zur Messgrösse (component_n im
// Journal), die es vorher nicht gab.

// ComponentResult ergänzt Result um die Kennzahlen des Vorpasses.
type ComponentResult struct {
	Result
	// Components ist die Zahl der Zusammenhangskomponenten (isolierte Knoten
	// zählen als eigene) — die Konvention, mit der §2.4 auf 34 Komponenten und
	// 93,7 % kommt.
	Components int
	// QControl ist die Kontrollrechnung Σ_t (m_t/m)·Q_t(γ_t) (Gate S8-G5).
	// Sie MUSS mit dem global gerechneten Q übereinstimmen — sie ist die
	// empirische Bestätigung des Beweises oben.
	QControl float64
}

// RunComponents clustert komponentenweise mit reskaliertem γ.
//
// BERICHTET WIRD IMMER DAS GLOBALE Q, in einer Passe über die volle CSR mit
// dem GLOBALEN γ (§4.4). Damit behält graph_overview_meta.modularity exakt
// seine heutige Semantik, und Achse 02/03 lesen dieselbe Grösse wie vorher.
// Die Zerlegung läuft daneben als Kontrollrechnung mit.
func RunComponents(ctx context.Context, g *Graph, opts Options) (ComponentResult, error) {
	gamma := opts.Resolution
	if gamma <= 0 {
		gamma = 1.0
	}
	n := g.N()
	out := ComponentResult{Result: Result{Memb: make([]int32, n)}}
	if n == 0 {
		return out, nil
	}

	compOf, compCount := connectedComponents(g)
	out.Components = compCount
	if g.M2 == 0 {
		for i := range out.Memb {
			out.Memb[i] = int32(i) //nolint:gosec // n < 2^31
		}
		out.Clusters = n
		out.Levels = 1
		return out, nil
	}

	// Knoten je Komponente sammeln, in AUFSTEIGENDER Original-Index-Ordnung.
	// Die Ordnung ist load-bearing: sie bestimmt die Knotennummerierung des
	// Teilgraphen und damit — über den Index-Tie-Break des Movers — die
	// Partition. Eine Map-Iteration hier würde den Determinismus-Anker brechen.
	members := make([][]int32, compCount)
	for v := 0; v < n; v++ {
		c := compOf[v]
		members[c] = append(members[c], int32(v)) //nolint:gosec // n < 2^31
	}

	global := make([]int32, n)
	nextID := int32(0)
	for c := 0; c < compCount; c++ {
		if err := ctx.Err(); err != nil {
			return ComponentResult{}, fmt.Errorf("louvain: components aborted at %d/%d: %w", c, compCount, err)
		}
		sub, m2t := extractComponent(g, members[c])

		// m_t = 0: isolierter Knoten (live 18 Stück). Singleton-Community,
		// kein Lauf — und ausdrücklich KEIN Beitrag zur Kontrollrechnung, weil
		// sein Gewicht m_t/m gerade 0 ist.
		if m2t == 0 || len(members[c]) == 1 {
			for _, v := range members[c] {
				global[v] = nextID
			}
			nextID++
			out.Levels = maxInt(out.Levels, 1)
			continue
		}

		sopts := opts
		// γ_t = γ · m_t/m. Über M2 ausgedrückt, weil M2 = 2m ist und der
		// Faktor 2 sich herauskürzt.
		sopts.Resolution = gamma * m2t / g.M2
		res, err := Run(ctx, sub, sopts)
		if err != nil {
			return ComponentResult{}, fmt.Errorf("louvain: component %d/%d: %w", c, compCount, err)
		}
		for i, v := range members[c] {
			global[v] = nextID + res.Memb[i]
		}
		nextID += int32(res.Clusters) //nolint:gosec // Clusterzahl < 2^31

		out.Sweeps += res.Sweeps
		out.Moves += res.Moves
		out.Levels = maxInt(out.Levels, res.Levels)
		out.SigmaDrift = math.Max(out.SigmaDrift, res.SigmaDrift)
		// Kontrollrechnung: (m_t/m)·Q_t(γ_t). Q_t stammt aus dem Teillauf und
		// ist damit gegen das REskalierte γ gerechnet — genau die Grösse, die
		// im Beweis steht.
		out.QControl += (m2t / g.M2) * res.Q
	}

	out.Memb, out.Clusters = canonicalize(global)
	out.Q = Modularity(g, out.Memb, gamma)
	return out, nil
}

// connectedComponents ist ein Union-Find über die CSR, O(m·α).
//
// Isolierte Knoten zählen als eigene Komponente — dieselbe Konvention, mit der
// die Live-Messung auf 34 Komponenten und eine Riesenkomponente von 93,7 %
// kommt. Die Nummerierung folgt dem kleinsten enthaltenen Knoten-Index, damit
// sie nicht an der Besuchsreihenfolge hängt.
func connectedComponents(g *Graph) ([]int32, int) {
	n := g.N()
	parent := make([]int32, n)
	for i := range parent {
		parent[i] = int32(i) //nolint:gosec // n < 2^31
	}
	// Kein rekursiver Aufruf, deshalb eine schlichte Zuweisung: die
	// Pfadverkuerzung laeuft als Schleife. Rekursiv liefe sie bei 9,8M Knoten
	// in einer Riesenkomponente (live 93,7 %) in den Stapelueberlauf.
	find := func(x int32) int32 {
		for parent[x] != x {
			parent[x] = parent[parent[x]] // Pfadhalbierung
			x = parent[x]
		}
		return x
	}
	for u := 0; u < n; u++ {
		for k := g.Off[u]; k < g.Off[u+1]; k++ {
			a, b := find(int32(u)), find(int32(g.Adj[k])) //nolint:gosec // Indizes < 2^31
			if a == b {
				continue
			}
			if a < b {
				parent[b] = a
			} else {
				parent[a] = b
			}
		}
	}
	root := make([]int32, n)
	for i := 0; i < n; i++ {
		root[i] = find(int32(i)) //nolint:gosec // n < 2^31
	}
	return canonicalize(root)
}

// extractComponent baut den induzierten Teilgraphen einer Komponente und
// liefert dessen M2 (= 2·m_t).
//
// Die Knoten werden in AUFSTEIGENDER Original-Ordnung neu nummeriert; der
// Teilgraph erbt damit dieselbe Totalordnung, auf der der Tie-Break des Movers
// ruht.
func extractComponent(g *Graph, nodes []int32) (*Graph, float64) {
	// local bildet Original-Index → Teilgraph-Index ab. Als Slice über die
	// sortierte Knotenliste plus Binärsuche waere es O(log n) je Zugriff; hier
	// ist eine Map billiger und beeinflusst NICHTS am Ergebnis, weil ueber sie
	// nie iteriert wird.
	local := make(map[int32]int32, len(nodes))
	for i, v := range nodes {
		local[v] = int32(i) //nolint:gosec // len(nodes) < 2^31
	}
	off := make([]uint32, len(nodes)+1)
	for i, v := range nodes {
		off[i+1] = off[i] + (g.Off[v+1] - g.Off[v])
	}
	adj := make([]uint32, off[len(nodes)])
	w := make([]float64, off[len(nodes)])
	var m2 float64
	for i, v := range nodes {
		pos := off[i]
		for k := g.Off[v]; k < g.Off[v+1]; k++ {
			adj[pos] = uint32(local[int32(g.Adj[k])]) //nolint:gosec // Teilgraph-Index < 2^32
			w[pos] = g.W[k]
			m2 += g.W[k]
			pos++
		}
	}
	return NewGraph(off, adj, w), m2
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
