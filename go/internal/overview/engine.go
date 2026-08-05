package overview

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/GottZ/ctx/internal/louvain"
)

// Der Engine-Umschalter und der Zeitbudget-Guard (Achse 04 / S6+S7,
// design/04 §4.9 + §3.4).
//
// ══ WAS SICH ÄNDERT — und was ausdrücklich NICHT ══
//
// Der heutige Aufbau ist eine Kette von Notlösungen um EINE Eigenschaft herum:
// community.Modularize ist ein opaker Aufruf, der ctx nicht beobachten kann.
// Daraus folgen der dokumentierte Goroutine-Leak, der SIGKILL statt
// SIGTERM-Grace — und der Knoten-Cap als ERSATZ für ein Zeitbudget.
//
// Der eigene Mover prüft ctx zwischen Warteschlangen-Blöcken. Damit wird das
// Zeitbudget der Primär-Guard und die Knotenzahl das Not-Aus:
//
//	Primär-Guard   Knotenzahl (Proxy)      →  time_budget (die Größe selbst)
//	Abbruch        SIGKILL, kein Grund     →  sauberer Skip mit Journal-Zeile
//	Goroutine-Leak vorhanden               →  entfällt
//	Prozessgrenze  wegen Unterbrechbarkeit →  bleibt, wegen RSS-Isolation
//
// WAS DER WECHSEL NICHT ÄNDERT: reisst das Budget, wird das Ergebnis
// VOLLSTÄNDIG verworfen — graph_cluster_member bleibt unverändert, computed_at
// rückt nicht vor, die Karte friert ein. Exakt wie am Knoten-Cap. Getauscht
// wird die Auslösebedingung, nicht der Modus (SP-5). Eine halbe Partition wäre
// weder reproduzierbar noch matchbar.
//
// ══ DIE ENGINE WÄHLT DEN KEY, NICHT DEN WERT (UD-07-04) ══
//
// max_nodes (200k) gilt für gonum, max_nodes_ctx (5M) für den eigenen Kern.
// Ein engine-abhängiger DEFAULT wäre im Struct-Tag-Mechanismus nicht
// ausdrückbar und beim Hot-Switch mehrdeutig ("wandert der Cap mit? gilt ein
// explizit gesetzter Wert weiter?"). Zwei Keys mit statischem Default, und der
// EFFEKTIVE Wert steht in jeder Journal-Zeile (max_nodes_eff) — damit ist im
// Betrieb unterscheidbar, ob 200000 gewollt oder geerbt ist.

// EffectiveMaxNodes liefert den Cap, den DIESE Engine benutzt.
//
// Im Elternprozess gebraucht: er schreibt max_nodes_eff in die Journal-Zeile,
// BEVOR das Kind startet — und muss den Wert deshalb ohne das Kind kennen.
func EffectiveMaxNodes(engine string, maxNodes, maxNodesCtx int) int {
	if engine == EngineCtx {
		return maxNodesCtx
	}
	return maxNodes
}

// NormalizeEngine bildet den Konfigurationswert auf die beiden bekannten
// Engines ab. Ein UNBEKANNTER Wert ist ein Fehler und KEIN stiller
// gonum-Fallback (§5.2 SP-2): wer engine=leiden schreibt, hat eine Erwartung,
// und ein Fallback würde sie stillschweigend enttäuschen — mit einer Partition,
// die aussieht, als sei sie nach Wunsch gerechnet worden.
func NormalizeEngine(v string) (string, error) {
	switch v {
	case "", EngineGonum:
		return EngineGonum, nil
	case EngineCtx:
		return EngineCtx, nil
	default:
		return "", fmt.Errorf("overview: unknown clustering engine %q (known: %q, %q)", v, EngineGonum, EngineCtx)
	}
}

// clusterCtxEngine fährt den eigenen Kern auf der CSR und übersetzt das
// Ergebnis in die Form, die persist erwartet.
//
// Das Zeitbudget ist HIER und nicht im Aufrufer: der Mover ist die einzige
// Phase, die es beobachten kann, und ein Budget über Laden UND Rechnen würde
// eine langsame Platte als Rechenzeit ausweisen.
func clusterCtxEngine(ctx context.Context, uuids []string, g *csrGraph, resolution float64, budget time.Duration, refine bool) (clustering, error) {
	n := len(uuids)
	if n == 0 {
		return clustering{blockToCluster: map[string]string{}, intraDegree: map[string]float64{}}, nil
	}

	mctx := ctx
	if budget > 0 {
		var cancel context.CancelFunc
		mctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}

	lg := louvain.NewGraph(g.Off, g.Adj, g.W)
	res, err := louvain.Run(mctx, lg, louvain.Options{Resolution: resolution, Refine: refine})
	if err != nil {
		return clustering{}, err
	}

	// cluster_id bleibt die kleinste Member-UUID (cluster.go:296-307,
	// unverändert seit 057) — die Identität lebt in Achse 01, nicht hier. Der
	// eigene Kern ändert nur, WIE die Communities gefunden werden.
	minUUID := make([]string, res.Clusters)
	for i, c := range res.Memb {
		u := uuids[i]
		if minUUID[c] == "" || u < minUUID[c] {
			minUUID[c] = u
		}
	}
	b2c := make(map[string]string, n)
	for i, c := range res.Memb {
		b2c[uuids[i]] = minUUID[c]
	}

	// Intra-Cluster-Grad: eine Passe über die u<v-Hälfte der CSR, in
	// aufsteigender Ordnung — dieselbe Summationsreihenfolge wie im
	// gonum-Pfad, weil der Kern nach Grad-Ordnung gewählt und gehasht wird
	// (core_hash) und ein wackelndes letztes Bit ein Topic re-labeln würde,
	// das sich nie bewegt hat.
	deg := make(map[string]float64, n)
	for _, u := range uuids {
		deg[u] = 0
	}
	for u := 0; u < n; u++ {
		for k := g.Off[u]; k < g.Off[u+1]; k++ {
			v := int(g.Adj[k])
			if v <= u || res.Memb[v] != res.Memb[u] {
				continue
			}
			deg[uuids[u]] += g.W[k]
			deg[uuids[v]] += g.W[k]
		}
	}

	q := res.Q
	if math.IsNaN(q) || math.IsInf(q, 0) {
		q = 0
	}
	return clustering{
		blockToCluster: b2c, intraDegree: deg, modularity: q, clusterCount: res.Clusters,
		edgePairs: g.Pairs, dangling: g.Dangling, selfLoops: g.SelfLoops,
		levels: res.Levels, sweeps: res.Sweeps, sigmaDrift: res.SigmaDrift,
	}, nil
}

// sortedClusterIDs ist ein Testhelfer-freier Determinismus-Anker für Aufrufer,
// die eine stabile Cluster-Reihenfolge brauchen.
func sortedClusterIDs(b2c map[string]string) []string {
	seen := make(map[string]struct{}, len(b2c)/4+1)
	out := make([]string, 0, len(seen))
	for _, c := range b2c {
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
