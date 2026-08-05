// Achse 04 / Welle S5 — Gates S5-G1 … S5-G3.
//
// EIN BEFUND VORWEG, weil er die Form dieser Gates bestimmt. Der Entwurf
// verlangt für S5-G1 eine "Fixture mit bei reinem Louvain NACHWEISLICH
// unzusammenhängender Community". Gegen DIESEN Kern liess sie sich nicht
// herstellen: 400 Zufallsgraphen mit gepflanzter Gruppenstruktur, je über sechs
// Auflösungen (γ = 0,05 / 0,2 / 0,5 / 1 / 2 / 5) gefahren, ergaben in 2.400
// Läufen NULL unzusammenhängende Communities.
//
// Die plausible Ursache ist die Warteschlange aus S4: verlässt ein Knoten seine
// Community, landen seine Nachbarn SOFORT wieder in der Warteschlange und
// werden neu bewertet. Genau dieser Schritt fehlt einer Implementierung mit
// vollen Sweeps — dort vergeht bis zur Neubewertung ein ganzer Durchlauf, und
// in diesem Fenster entsteht der Zerfall, den Traag et al. beschreiben. Der
// Zusammenhangs-Defekt ist damit teilweise eine Folge von Defekt (2), und das
// Beheben von (2) hat ihn mit-entschärft.
//
// KONSEQUENZ, ehrlich gezogen: die Rot-Probe von S5 liegt auf der EINHEIT
// (splitDisconnected gegen eine konstruierte zerfallene Partition), nicht
// end-to-end. Das Refinement bleibt trotzdem gebaut — es ist billig, es ist
// deterministisch, und es macht aus "wir haben keinen Fall gefunden" ein
// "der Fall kann nicht ausgeliefert werden". Was NICHT behauptet wird: dass es
// hier ein beobachtetes Problem repariert.
package louvain

import (
	"context"
	"math/rand/v2"
	"reflect"
	"testing"
)

// disconnectedPartition baut einen Graphen mit einer Partition, deren eine
// Community GARANTIERT in zwei Teile zerfällt: zwei Dreiecke ohne jede Kante
// zwischen ihnen, per Hand in dieselbe Community gesteckt.
func disconnectedPartition() (*Graph, []int32) {
	edges := []tuple{
		{0, 1, 1}, {1, 2, 1}, {0, 2, 1}, // Dreieck A
		{3, 4, 1}, {4, 5, 1}, {3, 5, 1}, // Dreieck B — keine Kante nach A
		{6, 7, 1}, {7, 8, 1}, {6, 8, 1}, // Dreieck C, eigene Community
		{2, 6, 0.1}, // schwache Brücke A→C
	}
	g := buildGraph(9, edges)
	memb := []int32{0, 0, 0, 0, 0, 0, 1, 1, 1} // A und B in EINER Community
	return g, memb
}

// TestSplitDisconnected_G1 ist die Rot-Probe auf der Einheit.
func TestSplitDisconnected_G1(t *testing.T) {
	g, memb := disconnectedPartition()

	// ROT: ohne Refinement ist die Partition nachweislich zerfallen.
	if d := DisconnectedCommunities(g, memb); d == 0 {
		t.Fatal("die Fixture ist gar nicht zerfallen — das Gate belegt nichts")
	}

	split, count := splitDisconnected(g, memb)
	if count != 3 {
		t.Fatalf("erwartet 3 zusammenhängende Teile (A, B, C), erhalten %d: %v", count, split)
	}
	if d := DisconnectedCommunities(g, split); d != 0 {
		t.Fatalf("nach dem Split noch %d überzählige Komponenten", d)
	}
	// Die Zerlegung darf NUR trennen, nie zusammenlegen: zwei Knoten, die
	// vorher verschiedene Communities hatten, dürfen danach nicht dieselbe
	// haben.
	for i := range memb {
		for j := range memb {
			if memb[i] != memb[j] && split[i] == split[j] {
				t.Fatalf("Split hat %d und %d zusammengelegt — er darf nur trennen", i, j)
			}
		}
	}
}

// TestRefine_G2_QNeverDrops ist S5-G2.
//
// Theoretisch kann Q durch das Trennen nicht fallen (zwischen zwei Komponenten
// ist der A-Term 0, der Strafterm strikt negativ — dasselbe Argument, mit dem
// §4.4 den Komponenten-Vorpass als exakt beweist). Der Test prüft es empirisch
// über eine Batterie, statt es zu glauben.
func TestRefine_G2_QNeverDrops(t *testing.T) {
	for seed := uint64(1); seed <= 60; seed++ {
		g := randomStructured(seed)
		plain, err := Run(context.Background(), g, Options{Resolution: 1.0})
		if err != nil {
			t.Fatalf("seed %d plain: %v", seed, err)
		}
		refined, err := Run(context.Background(), g, Options{Resolution: 1.0, Refine: true})
		if err != nil {
			t.Fatalf("seed %d refined: %v", seed, err)
		}
		if refined.Q < plain.Q-1e-12 {
			t.Errorf("seed %d: Q fällt durch Refinement — %.15f → %.15f", seed, plain.Q, refined.Q)
		}
	}
}

// TestRefine_G1_NothingDisconnectedIsEverDelivered ist die end-to-end-Zusage.
//
// Sie ist eine INVARIANTE, keine Reparatur-Messung: der Kern liefert auf diesen
// Instanzen auch ohne Refinement zusammenhängende Communities (s. Paket-Kopf).
// Der Test steht trotzdem, weil er die Zusage bewacht — kippt eine spätere
// Welle die Zugreihenfolge oder die Reduktion, faellt er.
func TestRefine_G1_NothingDisconnectedIsEverDelivered(t *testing.T) {
	for seed := uint64(1); seed <= 60; seed++ {
		g := randomStructured(seed)
		res, err := Run(context.Background(), g, Options{Resolution: 1.0, Refine: true})
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if d := DisconnectedCommunities(g, res.Memb); d != 0 {
			t.Errorf("seed %d: %d überzählige Komponenten trotz Refinement", seed, d)
		}
	}
}

// TestRefine_G3_DeterminismHolds ist S5-G3: die Determinismus-Anker gelten mit
// Refinement unverändert.
func TestRefine_G3_DeterminismHolds(t *testing.T) {
	g, _ := ringOfCliques(12, 9)
	first, err := Run(context.Background(), g, Options{Resolution: 1.0, Refine: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i := 0; i < 50; i++ {
		got, err := Run(context.Background(), g, Options{Resolution: 1.0, Refine: true})
		if err != nil {
			t.Fatalf("Lauf %d: %v", i, err)
		}
		if !reflect.DeepEqual(first.Memb, got.Memb) || first.Q != got.Q {
			t.Fatalf("Lauf %d weicht ab", i)
		}
	}
	// Auf einer wohlgetrennten Struktur darf das Refinement NICHTS ändern —
	// die Cliquen sind bereits zusammenhängend. Ein Refinement, das hier die
	// Partition anfasst, trennt zu viel.
	plain, err := Run(context.Background(), g, Options{Resolution: 1.0})
	if err != nil {
		t.Fatalf("Run plain: %v", err)
	}
	if !reflect.DeepEqual(plain.Memb, first.Memb) {
		t.Fatal("Refinement ändert eine bereits zusammenhängende Partition")
	}
}

// randomStructured baut einen Zufallsgraphen mit gepflanzter Gruppenstruktur —
// dieselbe Familie, mit der die Suche nach einer zerfallenen Community lief.
func randomStructured(seed uint64) *Graph {
	rng := rand.New(rand.NewPCG(seed, 0xabc))
	n := 60 + rng.IntN(90)
	groups := 3 + rng.IntN(5)
	var edges []tuple
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			p := 0.04
			if i%groups == j%groups {
				p = 0.30
			}
			if rng.Float64() < p {
				edges = append(edges, tuple{i, j, 0.5 + rng.Float64()})
			}
		}
	}
	if len(edges) == 0 {
		edges = append(edges, tuple{0, 1, 1})
	}
	return buildGraph(n, edges)
}
