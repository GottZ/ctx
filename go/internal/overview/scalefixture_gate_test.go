// Achse 04 / Welle S0 — Gate S0-G1 (design/04 §6.7).
//
// „Generator erzeugt einen Korpus mit der SPEZIFIZIERTEN
// Community-Größenverteilung und Dichte (Nachmessung gegen die Vorgabe);
// Abweichung > 5 % ⇒ Fixture unbrauchbar."
//
// Dazu die Determinismus-Achse aus dem Auftrag: gleicher Seed ⇒ byte-gleiche
// Kanten. Sie ist als GOLDEN HASH gepinnt, nicht nur als Zwei-Läufe-Vergleich:
// zwei Läufe desselben Binaries sind auch dann gleich, wenn der Generator seine
// Semantik ändert — der Golden-Hash ist die einzige Probe, die einen
// unbeabsichtigten Fixture-Drift zwischen zwei Commits sichtbar macht. Muster:
// graphcache.Fingerprint (validate.go:15), cluster_test.go 50-Lauf-Determinismus.
package overview

import (
	"hash/fnv"
	"math"
	"strings"
	"testing"
)

// streamHash ist der Determinismus-Fingerabdruck: FNV-1a über die
// Kantenfolge in EMISSIONS-Reihenfolge, inklusive Gewichts-Bitmuster. Jede
// Änderung an Reihenfolge, Endpunkten oder Gewichten kippt ihn.
func streamHash(f *scaleFixture) uint64 {
	h := fnv.New64a()
	var buf [24]byte
	f.stream(func(a, b int, w float64) {
		putU64(buf[0:8], uint64(a))  //nolint:gosec // Index ≥ 0
		putU64(buf[8:16], uint64(b)) //nolint:gosec // Index ≥ 0
		putU64(buf[16:24], math.Float64bits(w))
		_, _ = h.Write(buf[:])
	})
	return h.Sum64()
}

func putU64(b []byte, v uint64) {
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (56 - 8*uint(i)))
	}
}

// goldenStreamHash pinnt je Szenario den Kantenstrom einer kleinen Stufe. Die
// Werte sind aus dem ersten grünen Lauf übernommen; ändert eine Welle den
// Generator absichtlich, wird HIER nachgezogen — sichtbar im Diff, nie still.
var goldenStreamHash = map[string]uint64{
	"K1-organic-5000": 0x485626173060acb0,
	"K2-flat-5000":    0x9f47cec966fd7089,
}

func TestScaleFixtureDeterminism(t *testing.T) {
	cases := []struct {
		name string
		spec scaleSpec
	}{
		{"K1-organic-5000", specK1Organic(5_000, 7)},
		{"K2-flat-5000", specK2Flat(5_000, 7)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := resolveScale(tc.spec)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			b, err := resolveScale(tc.spec)
			if err != nil {
				t.Fatalf("resolve (2): %v", err)
			}
			ha, hb := streamHash(a), streamHash(b)
			if ha != hb {
				t.Fatalf("gleicher Seed, verschiedener Strom: %#x != %#x", ha, hb)
			}
			if want := goldenStreamHash[tc.name]; want != 0 && ha != want {
				t.Fatalf("Golden-Drift: stream hash %#x, erwartet %#x — "+
					"wurde der Generator absichtlich geändert? Dann goldenStreamHash nachziehen.", ha, want)
			}
			if want := goldenStreamHash[tc.name]; want == 0 {
				t.Logf("golden hash noch offen — eintragen: %q: %#x,", tc.name, ha)
			}

			// Ein anderer Seed MUSS einen anderen Strom liefern, sonst
			// belegt die Gleichheit oben nichts (die Probe würde auch bei
			// einem konstanten Hash grün sein).
			other := tc.spec
			other.Seed = tc.spec.Seed + 1
			c, err := resolveScale(other)
			if err != nil {
				t.Fatalf("resolve (seed+1): %v", err)
			}
			if hc := streamHash(c); hc == ha {
				t.Fatalf("Seed ist wirkungslos: %#x == %#x", hc, ha)
			}

			// Die materialisierte Form muss denselben Vertrag tragen — sie
			// ist die Eingabe von computeClustering, nicht der Indexstrom.
			n1, e1 := a.materialize()
			n2, e2 := b.materialize()
			if len(n1) != len(n2) || len(e1) != len(e2) {
				t.Fatalf("materialize-Längen weichen ab: %d/%d Knoten, %d/%d Kanten", len(n1), len(n2), len(e1), len(e2))
			}
			for i := range n1 {
				if n1[i] != n2[i] {
					t.Fatalf("Knoten %d weicht ab: %s != %s", i, n1[i], n2[i])
				}
			}
			for i := range e1 {
				if e1[i] != e2[i] {
					t.Fatalf("Kante %d weicht ab: %+v != %+v", i, e1[i], e2[i])
				}
			}
			// edgeBudget ist eine obere Schranke (gesättigte Communities).
			// Sie darf leicht überschätzen, aber nicht unterschätzen — eine
			// Unterschätzung hieße, dass materialize() reallokiert und die
			// Speicherrechnung von §6.3 nicht mehr trägt.
			if len(e1) > a.edgeBudget() || float64(len(e1)) < 0.99*float64(a.edgeBudget()) {
				t.Fatalf("edgeBudget()=%d, materialisiert %d — außerhalb des zugesagten Korridors", a.edgeBudget(), len(e1))
			}
		})
	}
}

// TestScaleFixtureUUIDForm belegt die zwei Eigenschaften, auf denen die
// Speicherdisziplin des Generators ruht: die UUIDs sind wohlgeformt (v4,
// Variante 10 — sie müssen in eine PostgreSQL-uuid-Spalte passen) und
// MONOTON im Index. Ohne die Monotonie wäre nodeUUIDs() nicht die Ordnung,
// die loadNodes' `ORDER BY cb.id` liefert, und die Fixture würde eine andere
// Determinismus-Achse messen als der Produktionspfad.
func TestScaleFixtureUUIDForm(t *testing.T) {
	f, err := resolveScale(specK1Organic(20_000, 3))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	prev := ""
	seen := make(map[string]struct{}, 20_000)
	for i := 0; i < f.spec.Nodes; i++ {
		u := f.uuidAt(i)
		if len(u) != 36 || u[8] != '-' || u[13] != '-' || u[18] != '-' || u[23] != '-' {
			t.Fatalf("uuidAt(%d)=%q ist keine UUID-Form", i, u)
		}
		if u[14] != '4' {
			t.Fatalf("uuidAt(%d)=%q: Version-Nibble ist %q, erwartet '4'", i, u, u[14])
		}
		if !strings.ContainsRune("89ab", rune(u[19])) {
			t.Fatalf("uuidAt(%d)=%q: Varianten-Nibble ist %q, erwartet 8/9/a/b", i, u, u[19])
		}
		if prev != "" && prev >= u {
			t.Fatalf("uuidAt nicht monoton bei %d: %q >= %q", i, prev, u)
		}
		if _, dup := seen[u]; dup {
			t.Fatalf("uuidAt(%d)=%q ist ein Duplikat", i, u)
		}
		seen[u] = struct{}{}
		prev = u
	}
	// dangling-Endpunkte liegen GARANTIERT hinter dem Knotenschnitt —
	// computeClustering muss sie verwerfen, nicht als Knoten sehen.
	for j := 0; j < 64; j++ {
		o := f.outsideUUID(j)
		if _, inCut := seen[o]; inCut {
			t.Fatalf("outsideUUID(%d)=%q liegt IM Knotenschnitt", j, o)
		}
		if prev >= o {
			t.Fatalf("outsideUUID(%d)=%q sortiert vor dem letzten Knoten %q", j, o, prev)
		}
	}
}

// TestScaleFixtureGateG1 ist S0-G1: Nachmessung gegen die Vorgabe.
func TestScaleFixtureGateG1(t *testing.T) {
	const tol = 0.05 // Abbruch-Kriterium des Gates (§6.7)

	cases := []struct {
		name string
		spec scaleSpec
	}{
		{"K1-organic-50k", specK1Organic(50_000, 7)},
		{"K2-flat-50k", specK2Flat(50_000, 7)},
		{"K1-organic-5k", specK1Organic(5_000, 7)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := resolveScale(tc.spec)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			m := f.measure()
			t.Logf("%v", m)

			// (a) DICHTE — gegen die DEDUPLIZIERTEN Paare gemessen, denn
			// computeClustering aggregiert parallele Links zu einem Paar
			// (cluster.go:470). Eine Fixture, die ihr Budget in Duplikaten
			// verbrennt, ist genau der Fall, den dieses Gate fangen soll.
			if m.DistinctPairs < 0 {
				t.Fatalf("DistinctPairs nicht gezählt — Gate kann die Dichte nicht prüfen")
			}
			gotPairsPerNode := float64(m.DistinctPairs) / float64(m.Nodes)
			if rel := math.Abs(gotPairsPerNode-f.spec.PairsPerNode) / f.spec.PairsPerNode; rel > tol {
				t.Errorf("Dichte verfehlt: %.4f Paare/Knoten, Vorgabe %.4f (%.1f %% ab)",
					gotPairsPerNode, f.spec.PairsPerNode, rel*100)
			}

			// (b) COMMUNITY-GRÖSSENVERTEILUNG gegen die Vorgabe.
			if rel := math.Abs(m.CommMean-float64(f.spec.CommunityAvg)) / float64(f.spec.CommunityAvg); rel > tol {
				t.Errorf("mittlere Community-Größe %.2f, Vorgabe %d (%.1f %% ab)", m.CommMean, f.spec.CommunityAvg, rel*100)
			}

			// (c) KOMPONENTEN-ANTEIL — die Live-Messung §2.4 (93,7 %). Sie
			// ist der Wert, mit dem §4.1d als tragender Mechanismus WIDERLEGT
			// wurde; eine Fixture, die ihn nicht trägt, könnte S8 grün
			// aussehen lassen, wo live nichts zu holen ist.
			wantGiant := 1 - f.spec.FringeFrac
			if math.Abs(m.GiantShare-wantGiant) > 0.005 {
				t.Errorf("Riesenkomponente %.4f, Vorgabe %.4f", m.GiantShare, wantGiant)
			}

			// (d) SELF-LOOPS — live 0 (§2.4). gonum verbietet sie im simple
			// graph; eine Fixture, die welche erzeugt, verschöbe die
			// dangling-/selfloop-Zähler von S3.
			if m.SelfLoops != 0 {
				t.Errorf("%d Self-Loops — live sind es 0", m.SelfLoops)
			}

			// (e) FORM der Grad- und Größenverteilung. KEINE Perzentil-Gleichheit
			// mit der Live-Messung: max-Grad und max-Community wachsen bei
			// einer Potenzverteilung mit n, und die Live-Zahl steht bei
			// 1.192 Knoten. Geprüft wird, dass die Form da IST bzw. NICHT da ist.
			switch f.spec.Shape {
			case shapeOrganic:
				if m.DegMax < 8*m.DegP50 {
					t.Errorf("kein Heavy-Tail im Grad: max=%d, p50=%d", m.DegMax, m.DegP50)
				}
				if m.DegP95 < 2*m.DegP50 {
					t.Errorf("Grad-p95 %d ist nicht ≥ 2× p50 %d — Verteilung zu flach", m.DegP95, m.DegP50)
				}
				if m.CommMax < 10*m.CommMedian {
					t.Errorf("kein Heavy-Tail in den Community-Größen: max=%d, median=%d", m.CommMax, m.CommMedian)
				}
			case shapeFlat:
				if m.DegMax > 3*m.DegP50 {
					t.Errorf("flache Vorgabe, aber max-Grad %d > 3× p50 %d", m.DegMax, m.DegP50)
				}
				if m.CommMax > 2*m.CommMedian {
					t.Errorf("flache Vorgabe, aber max-Community %d > 2× median %d", m.CommMax, m.CommMedian)
				}
			}
		})
	}
}

// TestScaleFixtureDangling belegt den Randfall, den §2.4 live misst (264
// Kanten mit einem Endpunkt außerhalb des Schnitts) und den cluster.go:462-466
// STILL verwirft — S3 baut darauf einen Zähler auf.
func TestScaleFixtureDangling(t *testing.T) {
	spec := specK1Organic(20_000, 11)
	spec.DanglingFrac = 0.081 // 264 / 3.255 (§2.4)
	f, err := resolveScale(spec)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	m := f.measure()
	if m.Dangling != f.dangling || m.Dangling == 0 {
		t.Fatalf("dangling: gemessen %d, aufgelöst %d", m.Dangling, f.dangling)
	}
	// Die Dichte IM Schnitt darf davon unberührt bleiben — dangling liegt AUF
	// dem Budget, nicht darin (DistinctPairs zählt nur Kanten mit beiden
	// Endpunkten im Schnitt, wie computeClustering).
	got := float64(m.DistinctPairs) / float64(m.Nodes)
	if rel := math.Abs(got-spec.PairsPerNode) / spec.PairsPerNode; rel > 0.05 {
		t.Errorf("dangling verwässert die Schnitt-Dichte: %.4f statt %.4f", got, spec.PairsPerNode)
	}

	nodes, edges := f.materialize()
	inCut := make(map[string]struct{}, len(nodes))
	for _, u := range nodes {
		inCut[u] = struct{}{}
	}
	outside := 0
	for _, e := range edges {
		_, okA := inCut[e.src]
		_, okB := inCut[e.dst]
		if !okA || !okB {
			outside++
		}
	}
	if outside != f.dangling {
		t.Fatalf("materialisiert %d Kanten mit Endpunkt außerhalb, aufgelöst %d", outside, f.dangling)
	}
	// Gegenprobe am ECHTEN Konsumenten: computeClustering muss genau diese
	// Kanten fallen lassen und trotzdem jeden Knoten zuordnen.
	cl := computeClustering(nodes, edges, 1.0)
	if len(cl.blockToCluster) != len(nodes) {
		t.Fatalf("computeClustering ordnet %d von %d Knoten zu", len(cl.blockToCluster), len(nodes))
	}
}
