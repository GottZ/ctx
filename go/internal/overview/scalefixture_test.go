// Achse 04 / Welle S0 — Ziel-Scale-Fixture-Generator.
//
// KEIN Produktionscode: die Datei ist ein _test.go im Paket overview, damit sie
// die Louvain-Eingabeform DIREKT erzeugt ([]string nodeUUIDs + []rawEdge, beide
// paketprivat) statt sie über eine Adapterschicht nachzubauen. Konsumenten sind
// S1 (Diagnose-Gate) und S12 (Ziel-Scale-Lauf).
//
// Warum ein Generator und nicht seed-structured.sql: design/04 §6.7 hält fest,
// dass in der ersten Fassung KEIN Gate am Ziel-Scale lief. Der Auslegungsfall
// (9,8M Knoten / 98M Paare, Zeile B aus §6.1) existiert in keinem Bench-Korpus
// des Repos — er muss erzeugt werden, und zwar so, dass der Generator selbst
// bei 9,8M nicht am Speicher stirbt. Daher:
//
//   - UUIDs sind eine PURE FUNKTION des Index (uuidAt), nie ein gehaltenes
//     Array — Kantenerzeugung braucht keinen Knotenspeicher.
//   - Die UUIDs sind MONOTON im Index. Das ist kein Zufall, sondern die
//     Nachbildung von loadNodes' `ORDER BY cb.id` (cluster.go:558-560): der
//     reale Louvain-Input ist UUID-sortiert, und die Ladeposition ist
//     Determinismus-Achse 2 (cluster.go:29-34). Ein Generator, der Communities
//     auf zusammenhängende Index-Blöcke legt UND zufällige UUIDs vergibt,
//     würde eine Sortierung erzwingen, die bei 9,8M teurer ist als der ganze
//     Rest.
//   - Die Community-Zugehörigkeit läuft deshalb über eine
//     format-preserving-Permutation (Feistel, O(1) Speicher, wahlfreier
//     Zugriff) statt über zusammenhängende Blöcke. Folge: cluster_id
//     (= kleinste Member-UUID) ist ein ZUFÄLLIGES Mitglied der Community, wie
//     live — der minUUID-Verstärker aus §6.5 bleibt am Fixture messbar.
//   - Kanten werden gestreamt (stream), nicht materialisiert; materialize()
//     ist der explizit teure Pfad für die Bench-Arme, die []rawEdge brauchen.
//
// Zwei Szenarien, wörtlich aus design/04 §6.1: K1 = heutige Dichte
// (2,25 Paare/Knoten, organisch-heavy-tail) und K2 = Auslegungsfall
// (10 Paare/Knoten, flach). Die Topologie-Zielwerte stammen aus der
// Live-Messung §2.4: Ø-Grad 4,49 · Grad p50/p95/max 4/9/34 · 34 Komponenten ·
// größte Komponente 93,7 % · 0 Self-Loops · 264 dangling von 3.255.
package overview

import (
	"encoding/hex"
	"fmt"
	"math"
	"math/bits"
	"sort"
)

// Abschnitt: Deterministische Zufallsquelle.

// splitmix64 ist der Mischschritt von SplitMix64 (Steele/Lea/Flood). Er dient
// hier zwei Zwecken: als Zustandsübergang eines sequentiellen Stroms und als
// zustandslose Hash-Funktion für den wahlfreien Zugriff (uuidAt, Feistel).
func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	z := x
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// detRand ist ein sequentieller, seedbarer Strom. Bewusst NICHT math/rand:
// die Fixture muss über Go-Versionen hinweg byte-gleich bleiben, und die
// Implementierung der Standardbibliothek ist kein zugesicherter Vertrag.
type detRand struct{ s uint64 }

func newDetRand(seed uint64, stream uint64) *detRand {
	return &detRand{s: splitmix64(seed ^ (stream * 0x9e3779b97f4a7c15))}
}

func (r *detRand) next() uint64 {
	r.s = splitmix64(r.s)
	return r.s
}

// f64 liefert eine Zahl in (0,1) — die offenen Grenzen sind Absicht: pow(u, e)
// und u^(-1/alpha) werden auf dem Wert ausgewertet.
func (r *detRand) f64() float64 {
	return (float64(r.next()>>11) + 0.5) * (1.0 / 9007199254740992.0)
}

// intn liefert eine Zahl in [0,n) ohne Modulo-Bias-Korrektur — der Bias liegt
// bei n < 2^32 unter 2^-32 und ist für eine Fixture belanglos.
func (r *detRand) intn(n int) int {
	if n <= 1 {
		return 0
	}
	return int(r.next() % uint64(n)) //nolint:gosec // Fixture-Index, kein Sicherheitspfad
}

// Abschnitt: Format-preserving Permutation.

// feistel permutiert [0,n) bijektiv, deterministisch und mit O(1) Speicher
// (4-Runden-Feistel über 2·halfBits Bits plus cycle-walking für den Rest des
// Bereichs). Die Alternative — ein materialisiertes []int32 — kostete bei 9,8M
// Knoten 39 MB, die der Generator gerade nicht ausgeben will.
type feistel struct {
	n        int
	halfBits uint
	mask     uint64
	key      uint64
}

func newFeistel(n int, key uint64) *feistel {
	b := uint(bits.Len64(uint64(n - 1))) //nolint:gosec // n > 0 garantiert durch resolve
	if b < 2 {
		b = 2
	}
	if b%2 == 1 {
		b++
	}
	half := b / 2
	return &feistel{n: n, halfBits: half, mask: (1 << half) - 1, key: key}
}

// at bildet i auf seine Permutationsposition ab.
func (f *feistel) at(i int) int {
	x := uint64(i) //nolint:gosec // i ∈ [0,n)
	for {
		l := x >> f.halfBits
		r := x & f.mask
		for round := uint64(0); round < 4; round++ {
			l, r = r, l^(splitmix64(f.key^(round<<56)^r)&f.mask)
		}
		x = (l << f.halfBits) | r
		if x < uint64(f.n) { //nolint:gosec // n > 0
			return int(x) //nolint:gosec // x < n
		}
	}
}

// Abschnitt: Spezifikation.

type scaleShape int

const (
	// shapeOrganic bildet die Live-Topologie nach: potenzverteilte
	// Community-Größen und preferential attachment innerhalb der Community
	// (Hubs). Das ist die Form, gegen die design/04 §2.4 gemessen hat.
	shapeOrganic scaleShape = iota
	// shapeFlat ist die Gegenprobe: gleich große Communities, gleichverteilte
	// Endpunktwahl. Sie trennt im S1-Mikro-Bench den Effekt der
	// Community-GRÖSSE vom Effekt der Grad-Verteilung.
	shapeFlat
)

func (s scaleShape) String() string {
	if s == shapeFlat {
		return "flat"
	}
	return "organic"
}

// scaleSpec ist die vollständige Beschreibung eines synthetischen Korpus.
// Gleiche Spec ⇒ byte-gleiche Kantenfolge (Gate S0-G1).
type scaleSpec struct {
	Nodes        int     // Louvain-Knoten (Einheit K11: Wissens-Blöcke × 0,98)
	PairsPerNode float64 // K1 = 2,25 · K2 = 10 (§6.1)
	Shape        scaleShape
	CommunityAvg int     // mittlere Community-Größe; live 1.192/59 ≈ 20
	IntraFrac    float64 // Anteil der Kanten innerhalb einer Community
	FringeFrac   float64 // Knoten AUSSERHALB der Riesenkomponente (live 6,3 %)
	IsolatedFrac float64 // davon Grad 0 (live 18 von 75)
	DanglingFrac float64 // Kanten mit einem Endpunkt außerhalb des Schnitts
	TailExp      float64 // Schiefe der Endpunktwahl bei shapeOrganic (>1 = Hubs)
	InterFanout  int     // Nachbar-Communities je Community (seed-structured: 6)
	Seed         uint64
}

// specK1Organic ist das Szenario „heutige Dichte, organisch" aus §6.1 (K1).
func specK1Organic(nodes int, seed uint64) scaleSpec {
	return scaleSpec{Nodes: nodes, PairsPerNode: 2.25, Shape: shapeOrganic, Seed: seed}
}

// specK2Flat ist der Auslegungsfall aus §6.1 (K2): zehn Paare je Knoten, flach.
func specK2Flat(nodes int, seed uint64) scaleSpec {
	return scaleSpec{Nodes: nodes, PairsPerNode: 10, Shape: shapeFlat, Seed: seed}
}

// intraFillCap deckelt, welcher Anteil der MÖGLICHEN Paare einer Community
// tatsächlich belegt wird. Der Deckel ist kein Geschmack, sondern die Grenze,
// ab der eine kollisionsfreie Ziehung in Ablehnungs-Schleifen läuft: bei 25 %
// Füllung liegt die erwartete Zahl der Versuche je Kante bei 1,33.
const intraFillCap = 0.25

func (s scaleSpec) withDefaults() scaleSpec {
	if s.IntraFrac <= 0 {
		s.IntraFrac = 0.9
	}
	if s.CommunityAvg <= 0 {
		// Default ist die LIVE gemessene mittlere Cluster-Größe (1.192 Knoten
		// auf 59 Cluster ≈ 20, §2.4/§1). Sie wird nur angehoben, wenn die
		// geforderte Dichte in einer 20er-Community keinen Platz mehr hätte:
		// 10 Paare/Knoten (K2) bräuchten 180 der 190 möglichen Paare — eine
		// Clique, keine Community. Die Anhebung ist damit eine Folge der
		// Dichte-Vorgabe, nicht eine zweite freie Stellschraube.
		s.CommunityAvg = 20
		perNode := s.PairsPerNode * s.IntraFrac
		if need := int(math.Ceil(1 + 2*perNode/intraFillCap)); need > s.CommunityAvg {
			s.CommunityAvg = need
		}
	}
	if s.FringeFrac < 0 {
		s.FringeFrac = 0
	}
	if s.FringeFrac == 0 {
		s.FringeFrac = 0.063 // 100 % − 93,7 % (§2.4)
	}
	if s.IsolatedFrac <= 0 {
		s.IsolatedFrac = 0.24 // 18 von 75 Fringe-Knoten (§2.4)
	}
	if s.TailExp <= 0 {
		// Kalibriert gegen §2.4: bei 50k Knoten / K1 liefert 1,8 die
		// Grad-Kennzahlen p50 4 / p95 10 gegen die Live-Messung p50 4 / p95 9.
		// Der max-Grad skaliert bei einer Potenzverteilung mit n und ist
		// deshalb KEIN Kalibrierungsanker (live 34 bei 1.192 Knoten).
		s.TailExp = 1.8
	}
	if s.InterFanout <= 0 {
		s.InterFanout = 6
	}
	if s.Seed == 0 {
		s.Seed = 0x53_30_66_69_78 // "S0fix"
	}
	return s
}

func (s scaleSpec) String() string {
	return fmt.Sprintf("n=%d pairs/node=%.2f shape=%s commAvg=%d seed=%#x",
		s.Nodes, s.PairsPerNode, s.Shape, s.CommunityAvg, s.Seed)
}

// Abschnitt: Auflösung.

// scaleFixture ist die aufgelöste Spec: alle Mengen stehen fest, nichts ist
// materialisiert außer den Community-Grenzen (O(C), bei 9,8M/Avg 20 ≈ 2 MB).
type scaleFixture struct {
	spec     scaleSpec
	mainN    int     // Knoten der Riesenkomponente
	isolated int     // Fringe-Knoten mit Grad 0
	fringeP  int     // Fringe-Zweier-Komponenten
	commOff  []int32 // kumulative Community-Grenzen über [0, mainN)
	perm     *feistel
	step     uint64 // UUID-Präfix-Schrittweite (48-Bit-Raum / Nodes)

	backbone   int // Spannbaum-Kanten (mainN − C)
	ring       int // Ring-Kanten über die Communities (C)
	extraIntra int
	extraInter int
	dangling   int
}

// resolveScale rechnet die Spec in Mengen um. Ein Fehler ist ein
// SPEZIFIKATIONS-Fehler (Dichte reicht nicht für den Zusammenhang), kein
// Laufzeitfehler — er muss den Test hart stoppen, nicht stillschweigend eine
// dünnere Fixture liefern.
func resolveScale(spec scaleSpec) (*scaleFixture, error) {
	s := spec.withDefaults()
	if s.Nodes < 16 {
		return nil, fmt.Errorf("scalefixture: Nodes=%d zu klein (min 16)", s.Nodes)
	}
	if s.Nodes >= 1<<40 {
		return nil, fmt.Errorf("scalefixture: Nodes=%d sprengt den 48-Bit-UUID-Präfixraum", s.Nodes)
	}

	fringe := int(float64(s.Nodes) * s.FringeFrac)
	isolated := int(float64(fringe) * s.IsolatedFrac)
	if (fringe-isolated)%2 == 1 {
		isolated++ // ungerader Rest ⇒ ein Knoten mehr isoliert, nie ein Halbpaar
	}
	mainN := s.Nodes - fringe
	if mainN < 8 {
		return nil, fmt.Errorf("scalefixture: FringeFrac=%.3f lässt nur %d Kern-Knoten", s.FringeFrac, mainN)
	}

	f := &scaleFixture{
		spec:     s,
		mainN:    mainN,
		isolated: isolated,
		fringeP:  (fringe - isolated) / 2,
		perm:     newFeistel(s.Nodes, splitmix64(s.Seed^0x7065726d)),
		// Der Knotenschnitt belegt die UNTERE Hälfte des 48-Bit-Präfixraums;
		// die obere Hälfte gehört den dangling-Endpunkten. Ohne diese Trennung
		// liefe der letzte Knotenpräfix plus sein Intervall-Rauschen über
		// 2^48 hinaus, würde beim Byte-Auslesen abgeschnitten und sortierte
		// vor dem ersten Knoten — die Monotonie-Zusage von uuidAt wäre dahin
		// (an genau dieser Stelle rot gelaufen, bevor sie stand).
		step: (uint64(1) << 47) / uint64(s.Nodes), //nolint:gosec // Nodes > 0
	}
	f.commOff = buildCommOff(s, mainN)
	c := len(f.commOff) - 1

	mTarget := int(math.Round(float64(s.Nodes) * s.PairsPerNode))
	// dangling liegt AUF dem Paar-Budget, nicht darin: live sind es 264
	// Kanten mit einem Endpunkt außerhalb ZUSÄTZLICH zu 3.255 Kanten im
	// Schnitt (§2.4). PairsPerNode bleibt damit die Dichte IM Schnitt — die
	// Größe, gegen die §6.1/§6.3 rechnen.
	f.dangling = int(float64(mTarget) * s.DanglingFrac)
	if f.dangling > s.Nodes {
		// Die dangling-Endpunkte teilen sich die obere Präfix-Hälfte mit
		// derselben Schrittweite wie die Knoten — mehr als Nodes davon
		// liefen aus dem 48-Bit-Raum.
		return nil, fmt.Errorf("scalefixture: DanglingFrac=%.3f fordert %d Außen-Endpunkte bei nur %d Knoten",
			s.DanglingFrac, f.dangling, s.Nodes)
	}
	body := mTarget - f.fringeP
	if body < 0 {
		return nil, fmt.Errorf("scalefixture: PairsPerNode=%.2f deckt nicht einmal die Fringe-Paare", s.PairsPerNode)
	}
	f.backbone = mainN - c
	f.ring = 0
	if c >= 2 {
		f.ring = c
	}
	intraTarget := int(math.Round(float64(body) * s.IntraFrac))
	interTarget := body - intraTarget
	if intraTarget < f.backbone || interTarget < f.ring {
		return nil, fmt.Errorf(
			"scalefixture: Dichte zu gering für Zusammenhang — intra %d < Spannbaum %d oder inter %d < Ring %d "+
				"(PairsPerNode=%.2f, CommunityAvg=%d); CommunityAvg senken oder Dichte erhöhen",
			intraTarget, f.backbone, interTarget, f.ring, s.PairsPerNode, s.CommunityAvg)
	}
	f.extraIntra = intraTarget - f.backbone
	f.extraInter = interTarget - f.ring
	return f, nil
}

// buildCommOff verteilt mainN Knoten auf Communities. shapeFlat: gleich groß.
// shapeOrganic: Pareto(alpha=1,5) über eine FESTE Community-Zahl, danach exakt
// auf mainN normiert — die feste Zahl hält die mittlere Größe auf
// CommunityAvg, die Normierung hält die Summe exakt, und der Heavy-Tail
// überlebt beides.
func buildCommOff(s scaleSpec, mainN int) []int32 {
	c := mainN / s.CommunityAvg
	if c < 1 {
		c = 1
	}
	sizes := make([]int32, c)
	if s.Shape == shapeFlat || c == 1 {
		base := mainN / c
		for i := range sizes {
			sizes[i] = int32(base) //nolint:gosec // base ≤ mainN
		}
	} else {
		r := newDetRand(s.Seed, 0x63_6f_6d_6d) // "comm"
		raw := make([]float64, c)
		sum := 0.0
		for i := range raw {
			v := math.Pow(r.f64(), -1.0/1.5) // Pareto(xm=1, alpha=1,5), Mittel 3
			if v > 3000 {
				v = 3000 // Deckel: größte Community ≤ ~1000 × CommunityAvg
			}
			raw[i] = v
			sum += v
		}
		scale := float64(mainN) / sum
		for i, v := range raw {
			n := int32(math.Round(v * scale)) //nolint:gosec // durch Deckel begrenzt
			if n < 2 {
				n = 2
			}
			sizes[i] = n
		}
	}
	// Exakt-Reparatur: die Differenz wandert auf die größte Community. Sie ist
	// reine Rundung (Deckel + Mindestgröße binden bei CommunityAvg ≥ 6 nicht).
	total := 0
	maxI := 0
	for i, n := range sizes {
		total += int(n)
		if n > sizes[maxI] {
			maxI = i
		}
	}
	if d := mainN - total; d != 0 {
		sizes[maxI] += int32(d) //nolint:gosec // |d| ≤ c
		if sizes[maxI] < 2 {
			sizes[maxI] = 2
		}
	}
	off := make([]int32, len(sizes)+1)
	acc := int32(0)
	for i, n := range sizes {
		off[i] = acc
		acc += n
	}
	off[len(sizes)] = acc
	return off
}

func (f *scaleFixture) communities() int { return len(f.commOff) - 1 }

// member liefert den Knotenindex des k-ten Mitglieds von Community c.
func (f *scaleFixture) member(c, k int) int { return f.perm.at(int(f.commOff[c]) + k) }

// edgeBudget ist die GEPLANTE Kantenzahl und damit eine OBERE SCHRANKE, keine
// Zusage: eine gesättigte Community (drawPair findet kein freies Paar mehr)
// und ein zurückfallender Inter-Griff (c2 == c1) lassen einzelne Kanten aus.
// Gemessen wird deshalb immer der Strom, nie das Budget — die Differenz lag
// in der Kalibrierung bei < 0,1 ‰ und darf nicht wegdefiniert werden.
func (f *scaleFixture) edgeBudget() int {
	return f.backbone + f.ring + f.extraIntra + f.extraInter + f.fringeP + f.dangling
}

// Abschnitt: UUIDs.

// uuidAt bildet einen Knotenindex auf seine UUID ab — MONOTON: i < j ⇒
// uuidAt(i) < uuidAt(j) lexikographisch. Der 48-Bit-Präfix trägt die Ordnung
// (disjunkte Intervalle der Breite step), die restlichen 80 Bit sind Rauschen
// mit korrekt gesetzten v4-Version- und Varianten-Nibbles, damit die Werte in
// eine PostgreSQL-uuid-Spalte passen und wie gen_random_uuid() aussehen.
func (f *scaleFixture) uuidAt(i int) string {
	return f.uuidFromKey(uint64(i)*f.step, splitmix64(f.spec.Seed^uint64(i)*0x9e3779b97f4a7c15)) //nolint:gosec // i ≥ 0
}

// outsideUUID erzeugt eine UUID GARANTIERT außerhalb des Knotenschnitts: ihr
// Präfix liegt hinter dem letzten Knotenintervall. Das ist der dangling-Fall
// aus §2.4 (live 264 Kanten mit einem Endpunkt außerhalb).
func (f *scaleFixture) outsideUUID(j int) string {
	base := uint64(1) << 47                                                                    // obere Hälfte des Präfixraums, s. resolveScale
	return f.uuidFromKey(base+uint64(j)*f.step, splitmix64(f.spec.Seed^0xdead_0000^uint64(j))) //nolint:gosec // j ≥ 0
}

func (f *scaleFixture) uuidFromKey(prefix48, noise uint64) string {
	var b [16]byte
	h := splitmix64(noise)
	// Bytes 0..5: der monotone Präfix plus Rauschen INNERHALB des Intervalls.
	p := prefix48
	if f.step > 1 {
		p += h % f.step
	}
	for i := 0; i < 6; i++ {
		b[i] = byte(p >> (40 - 8*uint(i)))
	}
	b[6] = 0x40 | byte(noise&0x0f) // Version 4
	b[7] = byte(noise >> 8)
	b[8] = 0x80 | byte(noise>>16&0x3f) // Variante 10
	for i := 9; i < 16; i++ {
		b[i] = byte(h >> (8 * uint(i-9)))
	}
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}

// Abschnitt: Erzeugung.

// stream emittiert jede Kante genau einmal auf INDEX-Ebene (keine
// UUID-Formatierung, keine Allokation je Kante). b ≥ Nodes bedeutet: Endpunkt
// außerhalb des Knotenschnitts (dangling). Speicher: O(C).
//
// Die Reihenfolge ist Teil des Vertrags — Gate S0-G1 hasht genau diesen Strom.
// Jede Phase zieht aus einem EIGENEN Strom, damit eine Mengenänderung in einer
// Phase die anderen nicht verschiebt.
func (f *scaleFixture) stream(onEdge func(a, b int, w float64)) {
	c := f.communities()
	mainN := f.mainN

	// (1)+(2) Intra-Kanten je Community, in EINEM Durchgang mit einem
	// gemeinsamen seen-Set — der Grund ist eine Messung, keine Ästhetik: mit
	// unabhängigen Ziehungen fielen bei K2 33 % der emittierten Kanten in der
	// Deduplizierung von computeClustering (cluster.go:470) wieder zusammen,
	// und die Fixture verfehlte ihre eigene Dichte-Vorgabe um ein Drittel.
	// Das seen-Set ist per Community und lebt nur so lange wie sie — bei
	// 9,8M Knoten sind das Zehntausende Einträge, keine Millionen.
	//
	// (1) Spannbaum: jeder Knoten k>0 hängt an einem früheren. Bei
	// shapeOrganic ist die Elternwahl potenzverzerrt (preferential
	// attachment) — das erzeugt die Hubs, die §2.4 als max-Grad 34 bei p50 4
	// misst. Der Spannbaum garantiert außerdem, dass jede Community
	// zusammenhängend ist, ohne dass ein Zufallsprozess es tun müsste.
	//
	// (2) Zusätzliche Intra-Kanten, proportional zur Community-Größe verteilt
	// (largest-remainder über commOff — exakt und streaming, ohne zweite
	// Tabelle).
	r1 := newDetRand(f.spec.Seed, 1)
	r2 := newDetRand(f.spec.Seed, 2)
	seen := make(map[uint64]struct{}, 4096)
	for ci := 0; ci < c; ci++ {
		size := int(f.commOff[ci+1] - f.commOff[ci])
		clear(seen)
		for k := 1; k < size; k++ {
			p := f.biasedIndex(k, r1)
			seen[pairKey(p, k)] = struct{}{}
			onEdge(f.member(ci, k), f.member(ci, p), edgeWeight(r1))
		}
		lo := int64(f.extraIntra) * int64(f.commOff[ci]) / int64(mainN)
		hi := int64(f.extraIntra) * int64(f.commOff[ci+1]) / int64(mainN)
		if size < 2 {
			continue
		}
		for j := lo; j < hi; j++ {
			a, b, ok := f.drawPair(size, seen, r2)
			if !ok {
				continue // Community gesättigt — gezählt, nicht erfunden
			}
			onEdge(f.member(ci, a), f.member(ci, b), edgeWeight(r2))
		}
	}

	// (3) Ring über die Communities — die Garantie, dass der Kern GENAU EINE
	// Komponente ist. §2.4: die Riesenkomponente ist bei Ø-Grad 4,49 die
	// strukturell erzwungene Normalform, nicht ein Zufallsbefund.
	r3 := newDetRand(f.spec.Seed, 3)
	for ci := 0; ci < f.ring; ci++ {
		nx := (ci + 1) % c
		onEdge(f.pickIn(ci, r3), f.pickIn(nx, r3), edgeWeight(r3))
	}

	// (4) Zusätzliche Inter-Kanten mit begrenztem Fan-out — der Supergraph
	// bleibt spärlich (seed-structured.sql: 6 Nachbarn), sonst misst S1 gegen
	// einen dichten Zufallsgraphen statt gegen die Zielstruktur.
	r4 := newDetRand(f.spec.Seed, 4)
	for j := 0; j < f.extraInter; j++ {
		c1 := r4.intn(c)
		c2 := (c1 + 1 + r4.intn(f.spec.InterFanout)) % c
		if c2 == c1 {
			continue
		}
		onEdge(f.pickIn(c1, r4), f.pickIn(c2, r4), edgeWeight(r4))
	}

	// (5) Fringe: isolierte Knoten (Grad 0, keine Kante) und Zweier-Komponenten.
	r5 := newDetRand(f.spec.Seed, 5)
	base := mainN + f.isolated
	for j := 0; j < f.fringeP; j++ {
		onEdge(f.perm.at(base+2*j), f.perm.at(base+2*j+1), edgeWeight(r5))
	}

	// (6) dangling: ein Endpunkt außerhalb des Schnitts. computeClustering
	// verwirft sie still (cluster.go:462-466) — die Fixture belegt, dass der
	// Zähler in S3 sie wiederfindet.
	r6 := newDetRand(f.spec.Seed, 6)
	for j := 0; j < f.dangling; j++ {
		onEdge(f.perm.at(r6.intn(mainN)), f.spec.Nodes+j, edgeWeight(r6))
	}
}

// biasedIndex zieht einen Index aus [0,n): potenzverzerrt zur 0 hin bei
// shapeOrganic (Hubs), gleichverteilt bei shapeFlat.
func (f *scaleFixture) biasedIndex(n int, r *detRand) int {
	if n <= 1 {
		return 0
	}
	var k int
	if f.spec.Shape == shapeOrganic {
		k = int(float64(n) * math.Pow(r.f64(), f.spec.TailExp))
	} else {
		k = int(float64(n) * r.f64())
	}
	if k >= n {
		k = n - 1
	}
	return k
}

// pairKey kanonisiert ein ungeordnetes Indexpaar.
func pairKey(a, b int) uint64 {
	if a > b {
		a, b = b, a
	}
	return uint64(a)<<32 | uint64(b) //nolint:gosec // Community-lokale Indizes < 2^32
}

// drawPair zieht ein noch nicht belegtes Paar aus einer Community. Die
// Versuchsgrenze ist der Preis der Kollisionsfreiheit: bei der durch
// intraFillCap garantierten Füllung ≤ 25 % scheitert sie mit
// Wahrscheinlichkeit 0,25^16; bei einer per Hand übersteuerten, gesättigten
// Spec meldet sie ehrlich „kein Platz mehr" statt eine Endlosschleife zu drehen.
func (f *scaleFixture) drawPair(size int, seen map[uint64]struct{}, r *detRand) (int, int, bool) {
	for attempt := 0; attempt < 16; attempt++ {
		a := f.biasedIndex(size, r)
		b := f.biasedIndex(size, r)
		if a == b {
			continue
		}
		k := pairKey(a, b)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		return a, b, true
	}
	return 0, 0, false
}

func (f *scaleFixture) pickIn(c int, r *detRand) int {
	size := int(f.commOff[c+1] - f.commOff[c])
	return f.member(c, f.biasedIndex(size, r))
}

// edgeWeight bildet raw_confidence eines dream-Links nach (live in [0,1];
// cluster.go:367/373 nutzt genau diese Spalte als Kantengewicht).
func edgeWeight(r *detRand) float64 { return 0.5 + 0.5*r.f64() }

// nodeUUIDs materialisiert die Knotenliste in derselben Ordnung, die loadNodes
// liefert (aufsteigend nach UUID — hier durch die Monotonie von uuidAt bereits
// gegeben, ohne Sortierung). Kosten: ~56 B/Knoten (String-Header + 36 B
// Backing) ⇒ bei 9,8M ≈ 550 MB. Deshalb NICHT der Standardpfad.
func (f *scaleFixture) nodeUUIDs() []string {
	out := make([]string, f.spec.Nodes)
	for i := range out {
		out[i] = f.uuidAt(i)
	}
	return out
}

// materialize erzeugt die vollständige Louvain-Eingabeform. Die rawEdge-Strings
// TEILEN das Backing der Knotenliste (kein zweites Formatieren) — das ist der
// Unterschied zwischen ~40 B und ~112 B je Kante (§6.3 M1).
func (f *scaleFixture) materialize() ([]string, []rawEdge) {
	nodes := f.nodeUUIDs()
	edges := make([]rawEdge, 0, f.edgeBudget())
	n := f.spec.Nodes
	f.stream(func(a, b int, w float64) {
		dst := ""
		if b >= n {
			dst = f.outsideUUID(b - n)
		} else {
			dst = nodes[b]
		}
		edges = append(edges, rawEdge{src: nodes[a], dst: dst, weight: w})
	})
	return nodes, edges
}

// Abschnitt: Vermessung (Gate S0-G1).

// scaleMetrics sind die Kennzahlen, gegen die design/04 §2.4 vermessen wurde.
type scaleMetrics struct {
	Nodes         int
	EdgesEmitted  int
	DistinctPairs int // −1 = nicht gezählt (zu groß)
	Dangling      int
	SelfLoops     int
	MeanDegree    float64
	DegP50        int
	DegP95        int
	DegMax        int
	Components    int
	GiantNodes    int
	GiantShare    float64
	Communities   int
	CommMean      float64
	CommMedian    int
	CommMax       int
}

func (m scaleMetrics) String() string {
	return fmt.Sprintf(
		"n=%d edges=%d distinct=%d dangling=%d selfloops=%d  deg: mean=%.2f p50=%d p95=%d max=%d  "+
			"comp=%d giant=%d (%.1f%%)  comms=%d (mean %.1f, median %d, max %d)",
		m.Nodes, m.EdgesEmitted, m.DistinctPairs, m.Dangling, m.SelfLoops,
		m.MeanDegree, m.DegP50, m.DegP95, m.DegMax,
		m.Components, m.GiantNodes, m.GiantShare*100,
		m.Communities, m.CommMean, m.CommMedian, m.CommMax)
}

// distinctPairLimit deckelt die Deduplizierungs-Zählung: sie braucht 8 B je
// Kante und einen Sort. Darüber bleibt DistinctPairs = −1, statt dass die
// Vermessung selbst zur OOM-Quelle wird.
const distinctPairLimit = 20_000_000

// measure fährt den Kantenstrom EINMAL und leitet alle Kennzahlen daraus ab.
// Speicher: 8 B/Knoten (Grad + Union-Find) plus optional 8 B/Kante.
func (f *scaleFixture) measure() scaleMetrics {
	n := f.spec.Nodes
	deg := make([]int32, n)
	uf := newUnionFind(n)
	var keys []uint64
	if f.edgeBudget() <= distinctPairLimit {
		keys = make([]uint64, 0, f.edgeBudget())
	}
	m := scaleMetrics{Nodes: n, Communities: f.communities()}
	f.stream(func(a, b int, w float64) {
		m.EdgesEmitted++
		if b >= n {
			m.Dangling++
			return // dangling zählt weder Grad noch Komponente (cluster.go verwirft sie)
		}
		if a == b {
			m.SelfLoops++
			return
		}
		deg[a]++
		deg[b]++
		uf.union(a, b)
		if keys != nil {
			lo, hi := a, b
			if lo > hi {
				lo, hi = hi, lo
			}
			keys = append(keys, uint64(lo)<<32|uint64(hi)) //nolint:gosec // Indizes < 2^32
		}
	})

	m.DistinctPairs = -1
	if keys != nil {
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		d := 0
		for i, k := range keys {
			if i == 0 || k != keys[i-1] {
				d++
			}
		}
		m.DistinctPairs = d
	}

	sorted := make([]int32, n)
	copy(sorted, deg)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var sum int64
	for _, d := range deg {
		sum += int64(d)
	}
	m.MeanDegree = float64(sum) / float64(n)
	m.DegP50 = int(sorted[n/2])
	m.DegP95 = int(sorted[int(float64(n)*0.95)])
	m.DegMax = int(sorted[n-1])

	comps, giant := uf.stats()
	m.Components = comps
	m.GiantNodes = giant
	m.GiantShare = float64(giant) / float64(n)

	csz := make([]int, f.communities())
	for i := range csz {
		csz[i] = int(f.commOff[i+1] - f.commOff[i])
	}
	sort.Ints(csz)
	m.CommMean = float64(f.mainN) / float64(len(csz))
	m.CommMedian = csz[len(csz)/2]
	m.CommMax = csz[len(csz)-1]
	return m
}

type unionFind struct{ p []int32 }

func newUnionFind(n int) *unionFind {
	p := make([]int32, n)
	for i := range p {
		p[i] = int32(i) //nolint:gosec // n < 2^31
	}
	return &unionFind{p: p}
}

func (u *unionFind) find(x int) int {
	for int(u.p[x]) != x {
		u.p[x] = u.p[u.p[x]] // Pfadhalbierung
		x = int(u.p[x])
	}
	return x
}

func (u *unionFind) union(a, b int) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	if ra < rb {
		u.p[rb] = int32(ra) //nolint:gosec // n < 2^31
	} else {
		u.p[ra] = int32(rb) //nolint:gosec // n < 2^31
	}
}

// stats zählt Komponenten und die Größe der größten. Isolierte Knoten zählen
// als eigene Komponente — genau die Konvention, mit der §2.4 auf 34
// Komponenten / 93,7 % kommt.
func (u *unionFind) stats() (components, giant int) {
	sizes := make(map[int]int, 64)
	for i := range u.p {
		sizes[u.find(i)]++
	}
	for _, s := range sizes {
		if s > giant {
			giant = s
		}
	}
	return len(sizes), giant
}
