// Package louvain ist der eigene Louvain-Rechenkern (Achse 04 / Welle S4,
// design/04 §4.3). DB-freies Leaf-Package: Eingabe ist eine CSR, Ausgabe eine
// Partition. Keine Abhängigkeit auf store, config oder pgx.
//
// ══ WARUM ÜBERHAUPT EIN EIGENER KERN ══
//
// gonums Louvain trägt ZWEI unabhängige Defekte, beide in der Quelle sichtbar
// und einer davon von den Autoren ausdrücklich als bewusste Abwägung benannt:
//
//	(1) undirectedLocalMover.deltaQ (louvain_undirected.go:567-584) summiert
//	    σ_tot bei JEDER Auswertung neu, indem es über jedes Mitglied jeder
//	    benachbarten Community läuft. Der Kommentar :578-583 sagt es selbst:
//	    "sigma_totC could be kept for each community and updated for moves,
//	    changing the calculation of sigma_totC here from O(n_c) to O(1), but in
//	    practice the time savings do not appear to be compelling".
//	    Kosten je Sweep: P = Σ_v Σ_{C ∈ conn(v)} |C| statt O(m).
//
//	(2) localMovingHeuristic (:483-496) mischt und durchläuft in JEDER Iteration
//	    ALLE Knoten neu. Es gibt keine Warteschlange der von einem Zug
//	    berührten Nachbarn. Eine Ebene, in der noch genau ein verbessernder Zug
//	    existiert, kostet trotzdem einen vollen Sweep.
//
// ══ WAS S1 DAZU GEMESSEN HAT — und warum BEIDE behoben werden ══
//
// Das S1-Gate hat (1) bestätigt und (2) sichtbar gemacht: bei KONSTANTER
// Knoten- und Kantenzahl (50.000 / 112.500) variiert gonums Laufzeit um Faktor
// 6,3 — allein mit der Community-Struktur. Die Lehrbuch-Lesart "O(m) je Sweep"
// ist damit empirisch widerlegt.
//
// ABER: der Prädiktor P allein erklärt die Laufzeit NICHT. Zwei Messpunkte mit
// praktisch identischem P (1,585e8 und 1,572e8) unterschieden sich um Faktor
// 1,78 in der Wall-Clock. Der Rest steckt in der Sweep-ZAHL, also in Defekt (2).
// Deshalb behebt dieser Kern beide — inkrementelles σ_tot UND Warteschlange.
// Ein Kern, der nur σ_tot anfasst, hebt nur den P-Anteil und verfehlt das
// S4-G1-Ziel plausibel.
//
// ══ WAS HIER NICHT PASSIERT ══
//
// Kein DB-Zugriff, keine UUID, kein Scope. Die Übersetzung Community-Index →
// cluster_id (minUUID) bleibt, wo sie ist.
package louvain

import (
	"context"
	"fmt"
	"math"
	"math/bits"
)

// Graph ist eine symmetrische, gewichtete CSR. Jede ungerichtete Kante {u,v}
// erscheint zweimal — in Adj[Off[u]:Off[u+1]] und in Adj[Off[v]:Off[v+1]].
// Self-Loops gehören NICHT hinein (der Aufrufer verwirft sie beim Laden).
type Graph struct {
	Off []uint32  // len == N+1
	Adj []uint32  // len == 2E
	W   []float64 // len == 2E, parallel zu Adj
	Deg []float64 // gewichteter Grad k_i, len == N
	M2  float64   // Σ k_i == 2m
}

// N ist die Knotenzahl.
func (g *Graph) N() int { return len(g.Off) - 1 }

// NewGraph berechnet Deg und M2 aus Off/Adj/W.
func NewGraph(off []uint32, adj []uint32, w []float64) *Graph {
	g := &Graph{Off: off, Adj: adj, W: w}
	n := g.N()
	if n < 0 {
		return &Graph{Off: []uint32{0}}
	}
	g.Deg = make([]float64, n)
	for u := 0; u < n; u++ {
		// off[u+1] ist immer gueltig: u < n == len(off)-1, also u+1 <= n <
		// len(off). gosec kann die Invariante nicht beweisen und meldet G602 —
		// die Alternative waere eine Laufzeitpruefung in der heissesten
		// Schleife des Kerns, fuer eine Bedingung, die der Typ N() garantiert.
		lo, hi := off[u], off[u+1] //nolint:gosec // G602 false positive: u+1 <= len(off)-1 by construction
		for _, x := range w[lo:hi] {
			g.Deg[u] += x
		}
		g.M2 += g.Deg[u]
	}
	return g
}

// Options steuert den Lauf.
type Options struct {
	// Resolution ist γ. <= 0 → 1.0.
	Resolution float64
	// MaxLevels und MaxMovesPerLevel sind FIXE DECKEL, keine
	// Konvergenzkriterien mit Toleranz (Determinismus-Anker A3). Ein Lauf, der
	// sie reisst, ist ein Fehler und kein "gutes Genug" — eine halbe Partition
	// wäre weder reproduzierbar noch matchbar.
	MaxLevels        int // 0 → defaultMaxLevels
	MaxMovesPerLevel int // 0 → 50·N
	// Refine schaltet die Leiden-Refinement-Phase (S5).
	Refine bool
}

const (
	defaultMaxLevels = 32
	// queueCheckStride bestimmt, wie oft der Mover ctx prüft. Der Grund, aus
	// dem der SIGKILL-Pfad überhaupt entfallen kann: gonums Modularize ist ein
	// undurchdringlicher Aufruf, dieser Mover ist es nicht.
	queueCheckStride = 4096
	// sigmaDriftTol ist die Schranke aus Anker A4 (§4.6): relativ zu M2, weil
	// ein absoluter Wert bei M2 in der Größenordnung 10⁸ (K2 @10M) enger wäre
	// als der akkumulierte Rechenfehler und damit wirkungslos.
	sigmaDriftTol = 1e-9
)

// Result ist die Ausgabe eines Laufs.
type Result struct {
	// Memb bildet die ORIGINALknoten auf die Community der obersten Ebene ab,
	// dicht nummeriert und aufsteigend nach kleinstem Originalknoten-Index —
	// eine totale, eingabeunabhängige Ordnung.
	Memb []int32
	// Q ist das GLOBALE Q über die zusammengesetzte Partition, in einer Passe
	// über die volle CSR mit dem globalen γ gerechnet (§4.4). Damit behält
	// graph_overview_meta.modularity exakt seine heutige Semantik.
	Q float64
	// SigmaDrift ist die maximale RELATIVE σ-Abweichung über alle Ebenen
	// (Gate S4-G6). Sie geht ins Lauf-Journal.
	SigmaDrift float64
	Levels     int
	Sweeps     int
	Moves      int
	Clusters   int
}

// Run führt Louvain aus.
func Run(ctx context.Context, g *Graph, opts Options) (Result, error) {
	gamma := opts.Resolution
	if gamma <= 0 {
		gamma = 1.0
	}
	maxLevels := opts.MaxLevels
	if maxLevels <= 0 {
		maxLevels = defaultMaxLevels
	}
	n := g.N()
	res := Result{Memb: make([]int32, n)}
	if n == 0 {
		return res, nil
	}
	if g.M2 == 0 {
		// Kantenloser Graph: jeder Knoten ist sein eigener Cluster. Q ist
		// undefiniert (0/0) und wird als 0 gemeldet — dieselbe Konvention wie
		// im gonum-Pfad.
		for i := range res.Memb {
			res.Memb[i] = int32(i) //nolint:gosec // n < 2^31
		}
		res.Clusters = n
		res.Levels = 1
		return res, nil
	}

	// mapping bildet ORIGINALknoten → aktuellen Ebenen-Knoten ab. Es wird nach
	// jeder Reduktion nachgezogen, damit die Ausgabe immer über die
	// Originalknoten indiziert ist.
	mapping := make([]int32, n)
	for i := range mapping {
		mapping[i] = int32(i) //nolint:gosec // n < 2^31
	}

	cur := g
	for level := 0; level < maxLevels; level++ {
		// Budget-Pruefung am EBENEN-EINSTIEG, zusaetzlich zur Pruefung alle
		// queueCheckStride Entnahmen im Mover.
		//
		// Ohne sie hat der Guard eine Granularitaets-Untergrenze: ein Graph mit
		// weniger Knoten als der Stride erreicht die Pruefung im Mover NIE und
		// laeuft trotz abgelaufenem Budget durch. Gefunden vom S67-G3-Gate, das
		// mit einem 400-Knoten-Korpus und einem 1-ns-Budget genau das zeigte —
		// harmlos bei 400 Knoten, aber ein Guard, der erst ab 4096 Knoten
		// greift, ist kein Guard, sondern eine Zusage mit Kleingedrucktem.
		if err := ctx.Err(); err != nil {
			return Result{}, fmt.Errorf("louvain: aborted before level %d: %w", level, err)
		}
		lvl, err := moveLevel(ctx, cur, gamma, opts)
		if err != nil {
			return Result{}, err
		}
		res.Sweeps += lvl.sweeps
		res.Moves += lvl.moves
		res.Levels++
		if lvl.drift > res.SigmaDrift {
			res.SigmaDrift = lvl.drift
		}
		if lvl.drift > sigmaDriftTol {
			// FEHLER, nicht WARN (Anker A4). σ geht direkt in ΔQ ein; driftet
			// es, driften die Tie-Breaks mit, und der Golden-Hash würde rot,
			// ohne dass jemand die Ursache hätte.
			return Result{}, fmt.Errorf(
				"louvain: sigma drift %.3e exceeds %.0e·M2 at level %d — incremental sigma is not holding",
				lvl.drift, sigmaDriftTol, level)
		}
		// S5: Refinement VOR dem Durchziehen des Mappings und vor der
		// Reduktion — die Reduktion soll auf den zusammenhängenden Teilmengen
		// arbeiten, nicht auf den zerfallenen Communities. Dadurch ist jede
		// Ebene aus zusammenhängenden Einheiten aufgebaut, und weil eine
		// Community der Ebene L+1 eine im reduzierten Graphen ZUSAMMENHÄNGENDE
		// Menge von Ebene-L-Einheiten ist, überträgt sich die Garantie bis in
		// die ausgelieferte Partition.
		if opts.Refine {
			refined, refinedCount := splitDisconnected(cur, lvl.memb)
			lvl.memb, lvl.clusters = refined, refinedCount
		}
		// Mapping durchziehen, BEVOR über den Abbruch entschieden wird: auch
		// die letzte Ebene hat gültige Zuordnungen.
		for i := range mapping {
			mapping[i] = lvl.memb[mapping[i]]
		}
		if !lvl.changed || lvl.clusters == cur.N() {
			// Keine Verbesserung mehr — die Ebene hat nichts zusammengelegt.
			break
		}
		cur = reduce(cur, lvl.memb, lvl.clusters)
	}

	// Dichte, deterministische Endnummerierung: Communities werden in der
	// Reihenfolge ihres KLEINSTEN Originalknoten-Index durchnummeriert. Ohne
	// diese Kanonisierung hinge die Nummerierung an der Reduktionsreihenfolge,
	// und der Golden-Hash würde bei einer rein internen Umstellung rot.
	res.Memb, res.Clusters = canonicalize(mapping)
	res.Q = Modularity(g, res.Memb, gamma)
	return res, nil
}

type levelResult struct {
	memb     []int32
	clusters int
	sweeps   int
	moves    int
	changed  bool
	drift    float64
}

// moveLevel ist die lokale Verschiebungsphase EINER Ebene.
//
// Der Unterschied zu gonum steckt in zwei Zeilen dieser Funktion und ist der
// ganze Grund für dieses Paket:
//
//   - σ_tot wird INKREMENTELL gepflegt (sigma[alt] -= k_v; sigma[neu] += k_v)
//     statt bei jeder Auswertung über alle Mitglieder neu summiert. Damit
//     kostet eine Zugbewertung O(deg(v)) statt O(Σ_C |C|).
//   - Die WARTESCHLANGE trägt nur die von einem Zug berührten Nachbarn. Damit
//     kostet eine Ebene O(m · Züge-pro-Knoten) statt O(Sweeps · Σ_v Σ_C |C|),
//     und ein einzelner verbleibender Zug kostet einen Zug, keinen Sweep.
func moveLevel(ctx context.Context, g *Graph, gamma float64, opts Options) (levelResult, error) {
	n := g.N()
	memb := make([]int32, n)
	// σ wird am ANFANG JEDER EBENE aus memb + Deg neu aufsummiert — O(N),
	// gegen O(m · Züge) vernachlässigbar. Das begrenzt die Drift auf eine Ebene
	// (Politik-Punkt 1 aus §4.3).
	sigma := make([]float64, n)
	comp := make([]float64, n) // Neumaier-Korrekturterme
	for i := 0; i < n; i++ {
		memb[i] = int32(i) //nolint:gosec // n < 2^31
		sigma[i] = g.Deg[i]
	}

	maxDeg := 0
	for u := 0; u < n; u++ {
		if d := int(g.Off[u+1] - g.Off[u]); d > maxDeg {
			maxDeg = d
		}
	}
	kin := newSlotMap(maxDeg)

	queue := make([]uint32, n)
	inQ := newBitset(n)
	for i := 0; i < n; i++ {
		queue[i] = uint32(i) //nolint:gosec // n < 2^31
		inQ.set(i)
	}

	maxMoves := opts.MaxMovesPerLevel
	if maxMoves <= 0 {
		maxMoves = 50 * n
	}
	lr := levelResult{}
	processed := 0
	for head := 0; head < len(queue); head++ {
		v := int(queue[head])
		inQ.clear(v)
		processed++
		if processed%queueCheckStride == 0 {
			if err := ctx.Err(); err != nil {
				return levelResult{}, fmt.Errorf("louvain: aborted after %d moves: %w", lr.moves, err)
			}
			// Die Warteschlange wächst; der verarbeitete Kopf wird
			// weggeschnitten, damit ein langer Lauf nicht unbegrenzt Speicher
			// hält. head wird entsprechend zurückgesetzt.
			if head > 1<<20 {
				queue = append(queue[:0], queue[head+1:]...)
				head = -1
			}
		}
		if lr.moves >= maxMoves {
			return levelResult{}, fmt.Errorf(
				"louvain: move cap %d reached — the level is not converging (A3: a capped run is an error, not a partial result)", maxMoves)
		}

		own := memb[v]
		kv := g.Deg[v]

		// kIn füllen: EINE Passe über die Adjazenz, O(deg(v)).
		kin.reset()
		for k := g.Off[v]; k < g.Off[v+1]; k++ {
			kin.add(memb[g.Adj[k]], g.W[k])
		}

		// σ der eigenen Community OHNE v — der Term, der den Vergleich
		// "bleiben" gegen "gehen" fair macht.
		sigmaOwnExcl := sigma[own] - kv
		kinOwn := kin.get(own)
		baseGain := deltaQ(kinOwn, kv, sigmaOwnExcl, gamma, g.M2)

		best := own
		bestGain := baseGain
		for _, c := range kin.touched() {
			if c == own {
				continue
			}
			gain := deltaQ(kin.get(c), kv, sigma[c], gamma, g.M2)
			// TIE-BREAK: relative Toleranz, nicht absolut. Ein fixes 1e-12
			// wäre bei M2 in der Größenordnung 10⁸ enger als der akkumulierte
			// Rechenfehler und damit wirkungslos (Anker A2). Bei Gleichstand
			// gewinnt der KLEINERE Community-Index — eine totale Ordnung ohne
			// PRNG.
			if better(gain, bestGain, c, best) {
				best, bestGain = c, gain
			}
		}
		if best == own {
			continue
		}

		// Zug ausführen. σ inkrementell, mit Neumaier-Kompensation: float64-
		// Addition ist nicht assoziativ, und über ~10⁹ Kantenberührungen driftet
		// die Summe von der exakten ab. Da σ direkt in ΔQ eingeht, driften die
		// Tie-Breaks mit.
		neumaierAdd(sigma, comp, int(own), -kv)
		neumaierAdd(sigma, comp, int(best), kv)
		memb[v] = best
		lr.moves++
		lr.changed = true

		// Nur die BERÜHRTEN Nachbarn zurück in die Warteschlange — Nachbarn
		// ausserhalb der neuen Community, die nicht schon drin sind. Das ist
		// Defekt (2) behoben.
		for k := g.Off[v]; k < g.Off[v+1]; k++ {
			u := int(g.Adj[k])
			if memb[u] == best || inQ.isSet(u) {
				continue
			}
			inQ.set(u)
			queue = append(queue, uint32(u)) //nolint:gosec // u < n < 2^31
		}
	}
	// Sweeps sind hier keine Volldurchläufe mehr, sondern verarbeitete
	// Warteschlangen-Einträge. Der Wert wird als solcher ins Journal gemeldet;
	// eine "Sweep-Zahl" im gonum-Sinn existiert in diesem Kern nicht.
	lr.sweeps = processed

	// σ-KONSISTENZ (Anker A4 / Gate S4-G6): am Ende der Ebene wird σ neu
	// aufsummiert und gegen den inkrementellen Wert geprüft. Ohne diese Probe
	// wäre der Golden-Hash auf langen Läufen ein Drift-Detektor ohne Diagnose.
	fresh := make([]float64, n)
	for i := 0; i < n; i++ {
		fresh[memb[i]] += g.Deg[i]
	}
	for c := 0; c < n; c++ {
		if d := math.Abs(sigma[c] + comp[c] - fresh[c]); d > lr.drift {
			lr.drift = d
		}
	}
	lr.drift /= g.M2 // relativ, s. sigmaDriftTol

	lr.memb, lr.clusters = canonicalize(memb)
	return lr, nil
}

// deltaQ ist der Gewinn-Term einer Community-Zuordnung.
//
// GEGEN FMA GEPINNT (Anker A1/Gate S4-G7). Die Go-Spezifikation erlaubt der
// Implementierung, mehrere Gleitkomma-Operationen zu einer fused operation
// zusammenzuziehen — auf arm64 kontrahiert, auf amd64 ohne FMA nicht. Ein
// Gleichstand innerhalb der Tie-Toleranz kippte dann ARCHITEKTURABHÄNGIG, und
// der Determinismus-Anker wäre ein Flake zwischen CI-, Deploy- und
// Entwicklerarchitektur. Die expliziten float64()-Konversionen sind die
// spec-konforme Methode, Fusion zu unterbinden; sie stehen hier NICHT aus
// Vorsicht, sondern weil sie das Gate tragen.
func deltaQ(kIn, kv, sigmaExcl, gamma, m2 float64) float64 {
	penalty := float64(gamma * kv)
	penalty = float64(penalty * sigmaExcl)
	penalty = float64(penalty / m2)
	return float64(kIn - penalty)
}

// tieTol ist die RELATIVE Gleichstandstoleranz (Anker A2).
const tieTol = 1e-15

// better entscheidet, ob (gain, c) den bisherigen Besten schlägt. Bei
// Gleichstand innerhalb der relativen Toleranz gewinnt der kleinere
// Community-Index — deterministisch, ohne PRNG.
func better(gain, bestGain float64, c, best int32) bool {
	tol := tieTol * math.Max(1, math.Abs(bestGain))
	if gain > bestGain+tol {
		return true
	}
	if gain < bestGain-tol {
		return false
	}
	return c < best
}

// neumaierAdd addiert kompensiert (Neumaier-Variante der Kahan-Summation): der
// Korrekturterm fängt den Anteil auf, der bei der Addition verlorengeht — auch
// dann, wenn der Summand GRÖSSER ist als die laufende Summe, was Kahan nicht
// leistet.
func neumaierAdd(sum, comp []float64, i int, x float64) {
	s := sum[i]
	t := s + x
	if math.Abs(s) >= math.Abs(x) {
		comp[i] += (s - t) + x
	} else {
		comp[i] += (x - t) + s
	}
	sum[i] = t
}

// canonicalize nummeriert Communities dicht und deterministisch durch: in der
// Reihenfolge ihres kleinsten Knoten-Index. Eine totale Ordnung, die nicht an
// der Reduktions- oder Zugreihenfolge hängt.
func canonicalize(memb []int32) ([]int32, int) {
	remap := make(map[int32]int32, len(memb)/4+1)
	out := make([]int32, len(memb))
	next := int32(0)
	for i, c := range memb {
		id, ok := remap[c]
		if !ok {
			id = next
			remap[c] = id
			next++
		}
		out[i] = id
	}
	return out, int(next)
}

// reduce baut den Graphen der nächsten Ebene: Communities werden Knoten,
// Kantengewichte zwischen ihnen summiert.
//
// Self-Loops (die inneren Kanten einer Community) landen bewusst NICHT in
// Adj: sie tragen zu keiner Zugentscheidung bei (eine Community-Selbstkante
// erscheint in keinem kIn eines FREMDEN Ziels), und Deg trägt ihren Beitrag
// bereits — k_c = Σ_{i∈c} k_i enthält jede innere Kante zweimal. Das Q wird
// ohnehin am Ende global über die ORIGINAL-CSR gerechnet (§4.4), nie aus den
// reduzierten Ebenen.
func reduce(g *Graph, memb []int32, clusters int) *Graph {
	n := g.N()
	// Grad zählen (obere Schranke: jede Kante bleibt oder wird Self-Loop).
	deg := make([]uint32, clusters)
	for u := 0; u < n; u++ {
		cu := memb[u]
		for k := g.Off[u]; k < g.Off[u+1]; k++ {
			if cv := memb[g.Adj[k]]; cv != cu {
				deg[cu]++
			}
		}
	}
	off := make([]uint32, clusters+1)
	var acc uint32
	for i, d := range deg {
		off[i] = acc
		acc += d
	}
	off[clusters] = acc

	adj := make([]uint32, acc)
	w := make([]float64, acc)
	fill := make([]uint32, clusters)
	for u := 0; u < n; u++ {
		cu := memb[u]
		for k := g.Off[u]; k < g.Off[u+1]; k++ {
			cv := memb[g.Adj[k]]
			if cv == cu {
				continue
			}
			adj[off[cu]+fill[cu]] = uint32(cv) //nolint:gosec // clusters < 2^31
			w[off[cu]+fill[cu]] = g.W[k]
			fill[cu]++
		}
	}

	// Verdichten: je Community nach Ziel sortieren und Mehrfacheinträge
	// summieren. STABIL, aus demselben Grund wie im CSR-Loader — float64-
	// Addition ist nicht assoziativ, und eine wackelnde Summationsreihenfolge
	// liesse das letzte ULP zwischen Läufen springen.
	newOff := make([]uint32, clusters+1)
	var out uint32
	for c := 0; c < clusters; c++ {
		lo, hi := off[c], off[c+1]
		sortAdjStable(adj[lo:hi], w[lo:hi])
		newOff[c] = out
		var prev uint32
		first := true
		for i := lo; i < hi; i++ {
			if !first && adj[i] == prev {
				w[out-1] += w[i]
				continue
			}
			adj[out] = adj[i]
			w[out] = w[i]
			out++
			prev = adj[i]
			first = false
		}
	}
	newOff[clusters] = out

	rg := &Graph{Off: newOff, Adj: adj[:out], W: w[:out], Deg: make([]float64, clusters), M2: g.M2}
	// Deg wird aus den MITGLIEDERN summiert, nicht aus der reduzierten
	// Adjazenz: nur so trägt es die inneren Kanten weiter, die oben bewusst
	// nicht in Adj gelandet sind. Aus demselben Grund bleibt M2 unverändert.
	for u := 0; u < n; u++ {
		rg.Deg[memb[u]] += g.Deg[u]
	}
	return rg
}

// sortAdjStable sortiert eine Adjazenzliste nach Ziel und zieht die Gewichte
// mit. Insertion Sort: die Listen sind kurz (mittlerer Grad live 4,49), und ein
// stabiler In-Place-Sort ohne Allokation ist hier billiger als sort.SliceStable.
func sortAdjStable(adj []uint32, w []float64) {
	for i := 1; i < len(adj); i++ {
		a, ww := adj[i], w[i]
		j := i - 1
		for j >= 0 && adj[j] > a {
			adj[j+1], w[j+1] = adj[j], w[j]
			j--
		}
		adj[j+1], w[j+1] = a, ww
	}
}

// Modularity rechnet das GLOBALE Q über die Original-CSR — eine Passe, O(m).
//
//	Q = Σ_c [ 2·W_in(c)/M2 − γ·(σ_c/M2)² ]
//
// W_in(c) zählt jede innere Kante EINMAL (die CSR trägt sie zweimal, deshalb
// wird über u<v gelaufen und mit 2 multipliziert).
func Modularity(g *Graph, memb []int32, gamma float64) float64 {
	if g.M2 == 0 {
		return 0
	}
	n := g.N()
	maxC := 0
	for _, c := range memb {
		if int(c) > maxC {
			maxC = int(c)
		}
	}
	win := make([]float64, maxC+1)
	sigma := make([]float64, maxC+1)
	for u := 0; u < n; u++ {
		sigma[memb[u]] += g.Deg[u]
		for k := g.Off[u]; k < g.Off[u+1]; k++ {
			v := int(g.Adj[k])
			if v <= u || memb[v] != memb[u] {
				continue
			}
			win[memb[u]] += g.W[k]
		}
	}
	var q float64
	for c := range win {
		frac := sigma[c] / g.M2
		q += 2*win[c]/g.M2 - gamma*frac*frac
	}
	if math.IsNaN(q) || math.IsInf(q, 0) {
		return 0
	}
	return q
}

// Abschnitt: Hilfsstrukturen.

// slotMap ist die kIn-Streuwerttabelle: offene Adressierung mit linearem
// Sondieren, Kapazität nach dem maximalen Grad bemessen.
//
// BEWUSST KEIN dichtes []float64 über alle Communities. Dicht indiziert kostet
// es C × 8 B mit C = N zu BEGINN jeder Ebene (jeder Knoten ist seine eigene
// Community) — bei 9,8M Knoten 78 MB Scratch, die nie mehr als deg(v) Slots
// gleichzeitig brauchen. Diese Struktur kostet O(max deg).
type slotMap struct {
	mask  uint32
	key   []int32
	val   []float64
	order []int32
}

func newSlotMap(maxDeg int) *slotMap {
	cap := 8
	if maxDeg > 0 {
		cap = 1 << uint(bits.Len(uint(maxDeg*2)))
	}
	m := &slotMap{
		mask:  uint32(cap - 1), //nolint:gosec // cap ist eine Zweierpotenz
		key:   make([]int32, cap),
		val:   make([]float64, cap),
		order: make([]int32, 0, maxDeg+1),
	}
	for i := range m.key {
		m.key[i] = -1
	}
	return m
}

// grow verdoppelt die Tabelle, wenn ein Knoten mehr verschiedene
// Nachbar-Communities hat als die Kapazität trägt. Der Fall kann bei
// maxDeg-basierter Bemessung nicht eintreten (Communities ≤ Grad), die
// Funktion ist die Absicherung gegen eine künftige Aufrufänderung.
func (m *slotMap) grow() {
	old := m.order
	oldVal := make([]float64, len(old))
	for i, k := range old {
		oldVal[i] = m.get(k)
	}
	cap := (len(m.key)) * 2
	m.mask = uint32(cap - 1) //nolint:gosec // Zweierpotenz
	m.key = make([]int32, cap)
	m.val = make([]float64, cap)
	for i := range m.key {
		m.key[i] = -1
	}
	m.order = m.order[:0]
	for i, k := range old {
		m.add(k, oldVal[i])
	}
}

func (m *slotMap) slot(c int32) uint32 {
	// Fibonacci-Hashing: billig, streut aufeinanderfolgende Community-Indizes
	// (der Normalfall zu Beginn einer Ebene) gleichmässig.
	return (uint32(c) * 2654435761) & m.mask //nolint:gosec // c >= 0
}

func (m *slotMap) add(c int32, w float64) {
	if len(m.order) >= len(m.key)/2 {
		m.grow()
	}
	i := m.slot(c)
	for {
		switch m.key[i] {
		case -1:
			m.key[i] = c
			m.val[i] = w
			m.order = append(m.order, c)
			return
		case c:
			m.val[i] += w
			return
		}
		i = (i + 1) & m.mask
	}
}

func (m *slotMap) get(c int32) float64 {
	i := m.slot(c)
	for {
		switch m.key[i] {
		case -1:
			return 0
		case c:
			return m.val[i]
		}
		i = (i + 1) & m.mask
	}
}

// reset räumt in O(berührte Slots) statt O(Kapazität) — der Punkt der
// order-Liste.
func (m *slotMap) reset() {
	for _, c := range m.order {
		i := m.slot(c)
		for m.key[i] != c {
			i = (i + 1) & m.mask
		}
		m.key[i] = -1
		m.val[i] = 0
	}
	m.order = m.order[:0]
}

// touched liefert die berührten Communities in EINFÜGEREIHENFOLGE. Die
// Reihenfolge darf das Ergebnis nicht beeinflussen — dafür sorgt der
// Index-Tie-Break in better(); Gate S4-G5 probt genau das.
func (m *slotMap) touched() []int32 { return m.order }

type bitset struct{ b []uint64 }

func newBitset(n int) *bitset { return &bitset{b: make([]uint64, (n+63)/64)} }

func (s *bitset) set(i int)        { s.b[i>>6] |= 1 << uint(i&63) }
func (s *bitset) clear(i int)      { s.b[i>>6] &^= 1 << uint(i&63) }
func (s *bitset) isSet(i int) bool { return s.b[i>>6]&(1<<uint(i&63)) != 0 }
