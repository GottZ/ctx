// Achse 04 / Welle S8 — Gates S8-G1, S8-G2, S8-G5.
package louvain

import (
	"context"
	"math"
	"math/rand/v2"
	"testing"
)

// disjointFixture baut einen Graphen aus mehreren KOMPONENTEN mit stark
// unterschiedlichen Grössen — die Live-Form (§2.4: eine Riesenkomponente von
// 93,7 %, dazu 33 kleine, davon 18 isolierte Knoten).
func disjointFixture(seed uint64) (*Graph, int) {
	rng := rand.New(rand.NewPCG(seed, 0x5a1))
	var edges []tuple
	next := 0
	comps := 0
	// Riesenkomponente: ein Ring aus Cliquen.
	const gc, gs = 14, 9
	for k := 0; k < gc; k++ {
		base := next + k*gs
		for i := 0; i < gs; i++ {
			for j := i + 1; j < gs; j++ {
				edges = append(edges, tuple{base + i, base + j, 0.5 + rng.Float64()})
			}
		}
	}
	for k := 0; k < gc; k++ {
		edges = append(edges, tuple{next + k*gs + gs - 1, next + ((k+1)%gc)*gs, 0.05})
	}
	next += gc * gs
	comps++
	// Ein paar kleine Komponenten.
	for _, sz := range []int{6, 4, 3, 2} {
		for i := 0; i < sz; i++ {
			for j := i + 1; j < sz; j++ {
				edges = append(edges, tuple{next + i, next + j, 0.8})
			}
		}
		next += sz
		comps++
	}
	// Und isolierte Knoten (m_t = 0).
	isolated := 5
	n := next + isolated
	comps += isolated
	return buildGraph(n, edges), comps
}

// TestComponents_S8G5_ControlSumMatchesGlobalQ ist S8-G5 — die empirische
// Bestätigung des Äquivalenzbeweises.
//
// Q = Σ_t (m_t/m)·Q_t(γ_t) muss mit dem GLOBAL gerechneten Q übereinstimmen.
// Weicht es ab, ist die γ-Reskalierung falsch implementiert — und zwar auf
// eine Weise, die keine Partition und kein Q-Vergleich zeigen würde.
func TestComponents_S8G5_ControlSumMatchesGlobalQ(t *testing.T) {
	for seed := uint64(1); seed <= 12; seed++ {
		g, wantComps := disjointFixture(seed)
		for _, gamma := range []float64{0.5, 1.0, 1.8} {
			res, err := RunComponents(context.Background(), g, Options{Resolution: gamma})
			if err != nil {
				t.Fatalf("seed %d γ=%.1f: %v", seed, gamma, err)
			}
			if res.Components != wantComps {
				t.Errorf("seed %d: %d Komponenten gefunden, %d erwartet", seed, res.Components, wantComps)
			}
			if d := math.Abs(res.Q - res.QControl); d > 1e-9 {
				t.Errorf("seed %d γ=%.1f: S8-G5 ROT — globales Q %.15f, Kontrollsumme %.15f (Δ=%.3e)",
					seed, gamma, res.Q, res.QControl, d)
			}
		}
	}
}

// TestComponents_S8G1_QDoesNotDrop ist S8-G1.
//
// Verglichen werden ZWEI nach DERSELBEN Vorschrift gerechnete globale Q (§4.4)
// — nicht zwei Grössen unterschiedlicher Herkunft. Abbruch-Kriterium des
// Entwurfs: Q_split < Q_global − 0,005.
func TestComponents_S8G1_QDoesNotDrop(t *testing.T) {
	const tol = 0.005
	for seed := uint64(1); seed <= 12; seed++ {
		g, _ := disjointFixture(seed)
		whole, err := Run(context.Background(), g, Options{Resolution: 1.0})
		if err != nil {
			t.Fatalf("seed %d whole: %v", seed, err)
		}
		split, err := RunComponents(context.Background(), g, Options{Resolution: 1.0})
		if err != nil {
			t.Fatalf("seed %d split: %v", seed, err)
		}
		if split.Q < whole.Q-tol {
			t.Errorf("seed %d: S8-G1 ROT — Q mit Vorpass %.6f < ohne %.6f (Toleranz %.3f)",
				seed, split.Q, whole.Q, tol)
		}
	}
}

// TestComponents_S8G2_DeviationIsReportedNotDefinedAway ist S8-G2.
//
// Louvain bleibt eine Heuristik: die ZIELFUNKTION ist durch den Beweis
// identisch, der PFAD kann abweichen. Der Test verlangt deshalb KEINE
// identische Partition — er misst die Abweichung und macht sie sichtbar. Eine
// Zusicherung "identisch" wäre falsch und würde beim ersten Gegenbeispiel
// entweder rot oder aufgeweicht.
func TestComponents_S8G2_DeviationIsReportedNotDefinedAway(t *testing.T) {
	var same, differ int
	var worstDelta float64
	for seed := uint64(1); seed <= 12; seed++ {
		g, _ := disjointFixture(seed)
		whole, err := Run(context.Background(), g, Options{Resolution: 1.0})
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		split, err := RunComponents(context.Background(), g, Options{Resolution: 1.0})
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if ari := ARI(whole.Memb, split.Memb); ari >= 0.9999 {
			same++
		} else {
			differ++
		}
		worstDelta = math.Max(worstDelta, math.Abs(split.Q-whole.Q))
	}
	t.Logf("S8-G2: %d von %d Fixturen partitionsgleich (ARI ≥ 0,9999), groesste Q-Abweichung %.2e",
		same, same+differ, worstDelta)
	if worstDelta > 0.005 {
		t.Errorf("Q-Abweichung %.4f ueber der S8-G1-Toleranz", worstDelta)
	}
}

// TestComponents_GammaRescalingIsLoadBearing ist die ROT-PROBE des Entwurfs.
//
// Die Behauptung: OHNE Reskalierung zerfaellt eine kleine Komponente, weil das
// GLOBALE γ relativ zu ihrer eigenen Kantenmasse viel zu gross ist — "eine
// 6-Knoten-Komponente soll nicht in drei Communities zerfallen, nur weil der
// uebrige Korpus gross ist" (§4.4).
//
// Die Fixture muss dafuer eine Komponente sein, die ZERFALLEN KANN: eine
// Clique kann es nicht, egal bei welchem γ. Deshalb eine Kette aus vier
// Dreiecken mit schwachen Bruecken — bei γ=1 findet Louvain die vier
// Dreiecke, bei γ_t bleibt sie EINE Community.
//
// Der unreskalierte Fall wird von Hand gefahren statt ueber ein Flag: ein Flag
// fuer einen nachweislich falschen Pfad ist Ballast.
func TestComponents_GammaRescalingIsLoadBearing(t *testing.T) {
	// Vier Dreiecke, im Ring schwach verbunden.
	var edges []tuple
	const tri, sz = 4, 3
	for k := 0; k < tri; k++ {
		b := k * sz
		edges = append(edges, tuple{b, b + 1, 1}, tuple{b + 1, b + 2, 1}, tuple{b, b + 2, 1})
	}
	for k := 0; k < tri; k++ {
		edges = append(edges, tuple{k*sz + 2, ((k + 1) % tri) * sz, 0.08})
	}
	sub := buildGraph(tri*sz, edges)

	// Der Korpus, in dem diese Komponente steckt, ist 200x so kantenschwer.
	// γ_t = γ · m_t/m ist dann 1/200 des globalen γ.
	const corpusFactor = 200.0
	gammaT := 1.0 / corpusFactor

	unscaled, err := Run(context.Background(), sub, Options{Resolution: 1.0})
	if err != nil {
		t.Fatalf("unscaled: %v", err)
	}
	scaled, err := Run(context.Background(), sub, Options{Resolution: gammaT})
	if err != nil {
		t.Fatalf("scaled: %v", err)
	}
	t.Logf("12-Knoten-Komponente in einem 200x groesseren Korpus: γ=1 ⇒ %d Cluster · γ_t=%.4g ⇒ %d Cluster",
		unscaled.Clusters, gammaT, scaled.Clusters)

	if unscaled.Clusters < 2 {
		t.Fatalf("bei globalem γ zerfaellt die Komponente gar nicht (%d Cluster) — die Rot-Probe belegt nichts",
			unscaled.Clusters)
	}
	if scaled.Clusters != 1 {
		t.Errorf("mit reskaliertem γ_t bleiben %d Cluster statt 1 — die Reskalierung wirkt nicht", scaled.Clusters)
	}
}
