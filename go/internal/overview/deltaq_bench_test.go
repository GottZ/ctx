//go:build integration

// Achse 04 / Welle S1 — Diagnose- und ENTSCHEIDUNGS-Gate für die
// deltaQ-Hypothese (design/04 §2.1, §6.7 S1-G1…S1-G3, DECISIONS UD-01-04).
//
// Die Hypothese, aus der Primärquelle: gonums `undirectedLocalMover.deltaQ`
// (gonum@v0.17.0/graph/community/louvain_undirected.go:530-606) iteriert über
// JEDES MITGLIED jeder benachbarten Community; der Kommentar :578-583 benennt
// das als bewusste Entscheidung („sigma_totC could be kept for each community
// … but in practice the time savings do not appear to be compelling"). Die
// Kosten eines Sweeps sind damit
//
//	P = Σ_v Σ_{C ∈ conn(v)} |C|          conn(v) = Communities der Nachbarn + eigene
//
// statt der Lehrbuch-Kosten O(m). §2.1 stellt ausdrücklich fest, dass zwei von
// drei vergleichbaren Bench-Punkten P stützen und EINER dagegen steht (400k:
// Zufallsarm 465,5 s gegen strukturierten Arm 351 s — unter P wäre das
// umgekehrte Vorzeichen zu erwarten), und dass der Report die zur Auflösung
// nötige Community-Größenverteilung des strukturierten Arms nicht enthält.
//
// Genau diese Lücke schließt dieser Bench. Er ist KEIN Bestätigungs-Gate: fällt
// die Hypothese, wird die S-Linie neu geschnitten (§7 S1, Re-Planungs-Klausel).
//
// Aufbau — drei Arme, weil sie verschiedene Fragen beantworten:
//
//	(i)  LADDER  50k → 800k bei KONSTANTER Dichte. §6.2 verwirft die vorhandene
//	     Faktorreihe ausdrücklich als Extrapolationsbasis: sie lief auf
//	     induzierten Präfixen, deren Kantendichte ~quadratisch mit n wächst,
//	     misst also zwei Effekte gleichzeitig. Hier wachsen Knoten und Kanten
//	     gemeinsam.
//	(ii) COMMSIZE  N und m KONSTANT, mittlere Community-Größe variabel. Das ist
//	     die entscheidende Probe: unter P muss die Wall-Clock mit |C| wachsen,
//	     obwohl sich am Graphen NICHTS ändert außer der Community-Struktur.
//	     Ist sie flach, ist P widerlegt.
//	(iii) RSS  VmHWM je Stufe, im EIGENEN Prozess gemessen (S1-G3). VmHWM ist
//	     ein Hochwassermarker und sinkt nie — mehrere Stufen im selben Prozess
//	     lieferten die Zahl der teuersten, nicht die der jeweiligen. Deshalb
//	     spawnt der Eltern-Lauf je Messpunkt ein Kind (os.Args[0] + Stage-Env)
//	     und liest dessen VmHWM aus. Das ist zugleich die Form, in der der
//	     Rebuild live läuft (worker-Kindprozess, cluster.go:405-410).
//
// Läufe: je 3, Median. Zeitbudget je Messpunkt 20 min — reißt es, ist der Punkt
// als „nicht terminiert in Budget" ausgewiesen. Das IST ein Ergebnis (§6.2: der
// einzige Messpunkt bei realistischer Dichte lautet „nach >25 min nicht
// konvergiert").
//
// Aufruf:
//
//	CTX_ROOTMAP_BENCH=1 go test -tags=integration ./internal/overview/ \
//	  -run 'TestDeltaQBench|TestDeltaQPredictor' -v -timeout 300m
package overview

import (
	"bufio"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gonum.org/v1/gonum/graph/community"
	"gonum.org/v1/gonum/graph/simple"
)

// Abschnitt: Prädiktor P.

// deltaQPredictor berechnet P = Σ_v Σ_{C ∈ conn(v)} |C| exakt über die
// Fixture-Adjazenz und eine gegebene Partition.
//
// Die Definition folgt der Quelle Zeile für Zeile: `connected` sammelt
// `l.memberships[vid]` über alle Nachbarn plus die eigene Mitgliedschaft
// (louvain_undirected.go:596-606), und die innere Schleife :567-584 läuft über
// alle Mitglieder jeder dieser Communities. Mehrfachkanten zählen NICHT
// mehrfach — `connected` ist eine Menge; die Stempel-Tabelle bildet das ab.
//
// Gemessen wird gegen die ENDGÜLTIGE Partition. Das ist die obere Kante des
// Sweep-Kostenverlaufs (zu Beginn jeder Ebene ist jede Community ein Singleton,
// dann gilt P ≈ 2m + N) und damit der Wert, gegen den die Hypothese am
// schärfsten prüfbar ist.
// Die Kantenquelle ist bewusst eine FUNKTION und keine *scaleFixture: die
// Kalibrierung S1-G2 braucht eine analytisch bekannte Clique-Fixture, die der
// Generator gar nicht erzeugen kann (er erzwingt — zu Recht — einen
// Community-Ring, der die Isolation zerstören würde).
func deltaQPredictor(n int, edges func(func(a, b int, w float64)), comm []int32, commSize []int64) float64 {
	off := make([]int32, n+2)
	edges(func(a, b int, _ float64) {
		if b >= n || a == b {
			return
		}
		off[a+1]++
		off[b+1]++
	})
	for i := 1; i < len(off); i++ {
		off[i] += off[i-1]
	}
	adj := make([]int32, off[n])
	fill := make([]int32, n)
	edges(func(a, b int, _ float64) {
		if b >= n || a == b {
			return
		}
		adj[off[a]+fill[a]] = int32(b) //nolint:gosec // Index < 2^31
		fill[a]++
		adj[off[b]+fill[b]] = int32(a) //nolint:gosec // Index < 2^31
		fill[b]++
	})

	stamp := make([]int32, len(commSize))
	for i := range stamp {
		stamp[i] = -1
	}
	var p float64
	for v := 0; v < n; v++ {
		c0 := comm[v]
		stamp[c0] = int32(v) //nolint:gosec // v < 2^31
		p += float64(commSize[c0])
		for _, u := range adj[off[v]:off[v+1]] {
			cu := comm[u]
			if stamp[cu] != int32(v) { //nolint:gosec // v < 2^31
				stamp[cu] = int32(v) //nolint:gosec // v < 2^31
				p += float64(commSize[cu])
			}
		}
	}
	return p
}

// densePartition übersetzt die uuid-basierte Partition von computeClustering in
// dichte Community-Indizes über die Knoten-Reihenfolge der Fixture.
func densePartition(nodes []string, cl clustering) (comm []int32, size []int64) {
	ids := make(map[string]int32, len(nodes)/8+1)
	comm = make([]int32, len(nodes))
	for i, u := range nodes {
		cid := cl.blockToCluster[u]
		id, ok := ids[cid]
		if !ok {
			id = int32(len(ids)) //nolint:gosec // Clusterzahl < 2^31
			ids[cid] = id
			size = append(size, 0)
		}
		comm[i] = id
		size[id]++
	}
	return comm, size
}

// Abschnitt: Eingabeform.

// materializeOwned baut dieselbe Eingabe wie scaleFixture.materialize, aber mit
// EIGENEN Strings je Kanten-Endpunkt.
//
// Das ist kein Detail, sondern die Voraussetzung dafür, dass die RSS-Messung
// mit dem §6.3(a)-Modell vergleichbar ist: dort steht M1 mit „~112 B je
// gerichteter Kante" — das sind zwei je eigenständig allokierte 36-Byte-UUIDs
// plus zwei String-Header plus das Gewicht, also genau das, was pgx' Scan in
// loadEdges (cluster.go:619-624) je Zeile erzeugt. materialize() teilt
// stattdessen das Backing der Knotenliste und wäre um den Faktor ~2,5
// billiger — eine Zahl, die den Anker ~254 MB @200k nicht reproduzieren kann.
func materializeOwned(f *scaleFixture) ([]string, []rawEdge) {
	nodes := f.nodeUUIDs()
	edges := make([]rawEdge, 0, f.edgeBudget())
	n := f.spec.Nodes
	f.stream(func(a, b int, w float64) {
		dst := ""
		if b >= n {
			dst = f.outsideUUID(b - n)
		} else {
			dst = strings.Clone(nodes[b])
		}
		edges = append(edges, rawEdge{src: strings.Clone(nodes[a]), dst: dst, weight: w})
	})
	return nodes, edges
}

// benchModularizeLevels zählt die Reduktionsebenen eines gonum-Laufs.
//
// Warum überhaupt separat: `Modularize` gibt den reduzierten Graphen zurück,
// dessen Ebenen über Expanded() begehbar sind — computeClustering wirft ihn
// weg (cluster.go:497-498). Die Sweep-ZAHL bleibt auch damit unzugänglich:
// `localMovingHeuristic` (louvain_undirected.go:483-496) führt keinen Zähler
// und ist von außen nicht instrumentierbar, ohne gonum zu forken. Der Bench
// weist Sweeps deshalb als „n/v" aus statt eine Zahl zu erfinden.
//
// Der Graph wird aus dem INDEX-Strom der Fixture gebaut (keine UUID-Strings) —
// das ist billiger als computeClustering und für die Ebenen-Zählung äquivalent,
// weil die Fixture konstruktionsbedingt dedupliziert ist (S0-G1: distinct/emitted
// > 99,9 %).
func benchModularizeLevels(f *scaleFixture) (levels, clusters int, q float64, dur time.Duration) {
	n := f.spec.Nodes
	g := simple.NewWeightedUndirectedGraph(0, 0)
	for i := 0; i < n; i++ {
		g.AddNode(simple.Node(int64(i)))
	}
	f.stream(func(a, b int, w float64) {
		if b >= n || a == b {
			return
		}
		g.SetWeightedEdge(simple.WeightedEdge{F: simple.Node(int64(a)), T: simple.Node(int64(b)), W: w})
	})
	t0 := time.Now()
	reduced := community.Modularize(g, 1.0, rand.NewPCG(louvainSeed1, louvainSeed2))
	dur = time.Since(t0)
	comms := reduced.Communities()
	q = community.Q(g, comms, 1.0)
	// FALLE, an der diese Funktion zweimal mit SIGSEGV stand: Expanded()
	// gibt `g.parent` als INTERFACE zurück (louvain_undirected.go:165-167).
	// Auf der untersten Ebene ist der ZEIGER nil, das Interface aber nicht —
	// jede Schleifenbedingung, die gegen eine Interface-Variable auf nil
	// prüft, läuft eine Ebene zu weit und dereferenziert den nil-Empfänger.
	// Die Laufvariable ist deshalb KONKRET typisiert; nur dann ist `!= nil`
	// ein echter Zeigervergleich.
	cur, _ := reduced.(*community.ReducedUndirected)
	for cur != nil {
		levels++
		next, _ := cur.Expanded().(*community.ReducedUndirected)
		cur = next
	}
	return levels, len(comms), q, dur
}

// Abschnitt: Messpunkt.

// stageResult ist eine Zeile der Entscheidungs-Vorlage.
type stageResult struct {
	Label      string
	Nodes      int
	Pairs      int
	CommAvg    int
	WallMS     int64
	Q          float64
	Clusters   int
	P          float64
	VmHWMkB    int64
	VmHWMTotal int64
	Levels     int
	Timeout    bool
	MeanCSize  float64
}

const (
	benchEnv      = "CTX_ROOTMAP_BENCH"
	stageKindEnv  = "CTX_ROOTMAP_BENCH_STAGE"
	stageNodesEnv = "CTX_ROOTMAP_BENCH_NODES"
	stageCommEnv  = "CTX_ROOTMAP_BENCH_COMMAVG"
	stageBudget   = 20 * time.Minute
)

// stageSpec kennt drei Kennungen:
//
//	K1  — heutige Dichte, organisch-heavy-tail (§6.1 K1). Die Leiter.
//	K2  — Auslegungsfall 10 Paare/Knoten, flach (§6.1 K2). Die Leiter.
//	K1F — K1-DICHTE, aber FLACHE Form. Nur für den Mikro-Bench: dort soll
//	      ausschließlich die Community-GRÖSSE variieren. Mit der organischen
//	      Form variierte zusätzlich die Größen-VERTEILUNG (Pareto um den
//	      Mittelwert) und die Grad-Verteilung — der Bench würde drei Dinge
//	      gleichzeitig ändern und könnte die deltaQ-Hypothese weder stützen
//	      noch fällen. Genau dieser Mangel ist der Grund, aus dem §2.1 den
//	      2026-06-Report für den strukturierten Arm nicht auswerten kann.
func stageSpec(kind string, nodes, commAvg int) scaleSpec {
	var s scaleSpec
	switch kind {
	case "K2":
		s = specK2Flat(nodes, 7)
	case "K1F":
		s = specK1Organic(nodes, 7)
		s.Shape = shapeFlat
	default:
		s = specK1Organic(nodes, 7)
	}
	if commAvg > 0 {
		s.CommunityAvg = commAvg
	}
	return s
}

// runStageInProcess ist der KIND-Pfad: genau ein Messpunkt, danach eine
// maschinenlesbare RESULT-Zeile auf stdout. Der Prozess endet direkt danach,
// damit sein VmHWM ausschließlich diesen einen Punkt beschreibt.
func runStageInProcess(t *testing.T, kind string, nodes, commAvg int) {
	t.Helper()
	spec := stageSpec(kind, nodes, commAvg)
	f, err := resolveScale(spec)
	if err != nil {
		fmt.Printf("RESULT error=%q\n", err.Error())
		return
	}
	nodeUUIDs, edges := materializeOwned(f)
	runtime.GC()

	t0 := time.Now()
	cl := computeClustering(nodeUUIDs, edges, 1.0)
	wall := time.Since(t0)

	// VmHWM SOFORT lesen. Der erste Anlauf dieses Benchs las den Wert am Ende
	// des Kindes und maß damit auch den Prädiktor-CSR und den ZWEITEN
	// gonum-Graphen der Ebenen-Zählung mit — eine Zahl, die kein
	// §6.3(a)-Vergleichspunkt ist. VmHWM ist ein Hochwassermarker und sinkt
	// nie; hier gelesen beschreibt er genau load+symmetrize+Modularize, also
	// den Pfad, den das Modell rechnet.
	clusterHWM := readVmHWMkB(t)

	// Der Prädiktor läuft NACH der Zeitmessung und auf der Index-Ebene — er
	// darf weder die Wall-Clock noch den RSS-Anker verfälschen.
	comm, size := densePartition(nodeUUIDs, cl)
	nodeUUIDs, edges = nil, nil //nolint:ineffassign // Speicher vor dem Prädiktor freigeben
	runtime.GC()
	p := deltaQPredictor(f.spec.Nodes, f.stream, comm, size)

	// Die Ebenen-Zählung baut einen zweiten Graphen und kostet noch einmal
	// einen vollen Modularize-Lauf. Sie ist deshalb auf die kleinen Stufen
	// begrenzt und läuft ZULETZT.
	levels := 0
	if nodes <= 50_000 {
		levels, _, _, _ = benchModularizeLevels(f)
	}
	fmt.Printf("RESULT wall_ms=%d q=%.6f clusters=%d p=%.0f vmhwm_kb=%d vmhwm_total_kb=%d levels=%d mean_csize=%.2f pairs=%d\n",
		wall.Milliseconds(), cl.modularity, cl.clusterCount, p, clusterHWM, readVmHWMkB(t), levels,
		float64(f.mainN)/float64(f.communities()), f.edgeBudget())
}

// runStageChild spawnt einen Kindprozess für genau einen Messpunkt und liest
// dessen RESULT-Zeile. Reißt das Zeitbudget, wird der Punkt als „nicht
// terminiert in Budget" zurückgegeben — nicht als Fehler: die Nicht-Terminierung
// ist die Aussage.
func runStageChild(t *testing.T, testName, kind string, nodes, commAvg int) stageResult {
	t.Helper()
	res := stageResult{Nodes: nodes, CommAvg: commAvg}
	ctx, cancel := context.WithTimeout(context.Background(), stageBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+testName+"$", "-test.v", "-test.timeout=0") //nolint:gosec // os.Args[0] ist das eigene Testbinary
	cmd.Env = append(os.Environ(),
		benchEnv+"=1",
		stageKindEnv+"="+kind,
		stageNodesEnv+"="+strconv.Itoa(nodes),
		stageCommEnv+"="+strconv.Itoa(commAvg))
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		res.Timeout = true
		return res
	}
	line := ""
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		if strings.HasPrefix(strings.TrimSpace(sc.Text()), "RESULT ") {
			line = strings.TrimSpace(sc.Text())
		}
	}
	if line == "" {
		t.Fatalf("Kindprozess lieferte keine RESULT-Zeile (err=%v):\n%s", err, tailLines(string(out), 20))
	}
	for _, kv := range strings.Fields(strings.TrimPrefix(line, "RESULT ")) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch k {
		case "wall_ms":
			res.WallMS, _ = strconv.ParseInt(v, 10, 64)
		case "q":
			res.Q, _ = strconv.ParseFloat(v, 64)
		case "clusters":
			res.Clusters, _ = strconv.Atoi(v)
		case "p":
			res.P, _ = strconv.ParseFloat(v, 64)
		case "vmhwm_kb":
			res.VmHWMkB, _ = strconv.ParseInt(v, 10, 64)
		case "vmhwm_total_kb":
			res.VmHWMTotal, _ = strconv.ParseInt(v, 10, 64)
		case "levels":
			res.Levels, _ = strconv.Atoi(v)
		case "mean_csize":
			res.MeanCSize, _ = strconv.ParseFloat(v, 64)
		case "pairs":
			res.Pairs, _ = strconv.Atoi(v)
		case "error":
			t.Fatalf("Kindprozess-Fehler: %s", v)
		}
	}
	return res
}

func tailLines(s string, n int) string {
	ls := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}

// medianOf3 fährt drei Kindprozesse und nimmt den Median der Wall-Clock. Die
// übrigen Felder stammen aus dem Median-Lauf, damit die Zeile in sich
// konsistent bleibt (VmHWM und Wall-Clock desselben Prozesses).
func medianOf3(t *testing.T, testName, label, kind string, nodes, commAvg int) stageResult {
	t.Helper()
	runs := make([]stageResult, 0, 3)
	for i := 0; i < 3; i++ {
		r := runStageChild(t, testName, kind, nodes, commAvg)
		if r.Timeout {
			r.Label = label
			fmt.Printf("  %-22s Lauf %d: NICHT TERMINIERT in %s — Punkt als nicht terminiert ausgewiesen\n", label, i+1, stageBudget)
			return r
		}
		runs = append(runs, r)
		fmt.Printf("  %-22s Lauf %d: %6d ms  Q=%.4f  Cluster=%d  P=%.3g  VmHWM(cluster)=%s  VmHWM(total)=%s\n",
			label, i+1, r.WallMS, r.Q, r.Clusters, r.P, mbFromKB(r.VmHWMkB), mbFromKB(r.VmHWMTotal))
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].WallMS < runs[j].WallMS })
	m := runs[1]
	m.Label = label
	return m
}

// printLadderTable trennt zwei Größen, die beim ersten Aufriss dieselbe Spalte
// teilten und dadurch falsch zu lesen waren: „|C| soll" ist die GEPFLANZTE
// mittlere Community-Größe der Fixture, „|C| ist" die im gefundenen
// Partitionsergebnis (Knoten / Cluster). Die beiden laufen auseinander — genau
// das ist ein Befund und darf nicht in einer Spalte verschwinden.
func printLadderTable(title string, rows []stageResult) {
	fmt.Printf("\n=== %s ===\n", title)
	fmt.Printf("%-22s %9s %11s %9s %8s %9s %9s %8s %12s %10s\n",
		"Punkt", "Knoten", "Paare", "|C| soll", "|C| ist", "Wall", "Q", "Cluster", "P", "Peak-RSS")
	for _, r := range rows {
		wall := fmt.Sprintf("%.2fs", float64(r.WallMS)/1000)
		if r.Timeout {
			wall = ">20min"
		}
		found := 0.0
		if r.Clusters > 0 {
			found = float64(r.Nodes) / float64(r.Clusters)
		}
		fmt.Printf("%-22s %9d %11d %9.1f %8.1f %9s %9.4f %8d %12.4g %10s\n",
			r.Label, r.Nodes, r.Pairs, r.MeanCSize, found, wall, r.Q, r.Clusters, r.P, mbFromKB(r.VmHWMkB))
	}
}

// Abschnitt: Arm (i) — Skalen-Leiter bei konstanter Dichte (S1-G1a, S1-G3).

func TestDeltaQBenchLadder(t *testing.T) {
	if os.Getenv(benchEnv) == "" {
		t.Skip(benchEnv + " ungesetzt — S1-Diagnose übersprungen")
	}
	if kind := os.Getenv(stageKindEnv); kind != "" {
		n, _ := strconv.Atoi(os.Getenv(stageNodesEnv))
		ca, _ := strconv.Atoi(os.Getenv(stageCommEnv))
		runStageInProcess(t, kind, n, ca)
		return
	}

	type point struct {
		kind  string
		nodes int
	}
	points := []point{
		{"K1", 50_000}, {"K1", 200_000}, {"K1", 400_000}, {"K1", 800_000},
		{"K2", 50_000}, {"K2", 200_000},
	}
	rows := make([]stageResult, 0, len(points))
	for _, p := range points {
		label := fmt.Sprintf("%s-%d", p.kind, p.nodes)
		fmt.Printf("[S1-ladder] %s\n", label)
		r := medianOf3(t, "TestDeltaQBenchLadder", label, p.kind, p.nodes, 0)
		rows = append(rows, r)
		if r.Timeout {
			fmt.Printf("[S1-ladder] %s riss das Budget — größere Stufen derselben Dichte werden übersprungen\n", label)
			break
		}
	}
	printLadderTable("S1-G1a — Skalen-Leiter, KONSTANTE Dichte (Median aus 3, je eigener Prozess)", rows)

	// S1-G3: der 256-MiB-Anker aus §6.3(a)/R7. Der Container fährt seit
	// 05.08. 512 MiB (DECISIONS UD-02-04 Stufe 1); die Zahl bleibt der
	// Vergleichspunkt, gegen den das Modell kalibriert wurde.
	for _, r := range rows {
		if r.Label == "K1-200000" {
			fmt.Printf("\n[S1-G3] Anker-Vergleich @200k Knoten / %d Paare:\n", r.Pairs)
			fmt.Printf("  gemessen (Kind-VmHWM, eigener Prozess): %s\n", mbFromKB(r.VmHWMkB))
			fmt.Printf("  §6.3(a)-Modell @200k / 450k Paare:      ~254 MB\n")
			fmt.Printf("  Container-Limit heute:                  512 MiB (UD-02-04 Stufe 1, vorher 256)\n")
		}
	}
}

// Abschnitt: Arm (ii) — die entscheidende Probe (S1-G1b).

// TestDeltaQBenchCommunitySize hält N UND die Kantenzahl konstant und variiert
// ausschließlich die mittlere Community-Größe.
//
// Unter der Hypothese P wächst die Wall-Clock ungefähr proportional zu Ø|C|
// (P ≈ (2m + N) · Ø|C| bei wenigen Nachbar-Communities je Knoten). Ist sie
// flach, ist P widerlegt — und §4.2 (eigener Rechenkern) verliert seine
// Begründung, unabhängig davon, ob ein eigener Kern aus anderen Gründen lohnt.
//
// Die Fixture ist FLACH gehalten (shapeFlat): gleich große Communities und
// gleichverteilte Endpunktwahl trennen den Effekt der GRÖSSE vom Effekt der
// Grad-Verteilung. Genau diese Trennung fehlt dem 2026-06-Report, dessen
// strukturierter Arm keine Größenverteilung ausweist (§2.1).
func TestDeltaQBenchCommunitySize(t *testing.T) {
	if os.Getenv(benchEnv) == "" {
		t.Skip(benchEnv + " ungesetzt — S1-Diagnose übersprungen")
	}
	if kind := os.Getenv(stageKindEnv); kind != "" {
		n, _ := strconv.Atoi(os.Getenv(stageNodesEnv))
		ca, _ := strconv.Atoi(os.Getenv(stageCommEnv))
		runStageInProcess(t, kind, n, ca)
		return
	}

	// Die Community-Größe ist nach oben durch mainN/4 begrenzt: darunter
	// bliebe weniger als eine Handvoll Communities übrig, der Ring über sie
	// würde entarten und die Inter-Kanten fielen aus — die Kantenzahl wäre
	// dann NICHT mehr konstant, und der Bench misst zwei Dinge statt einem.
	const nodes = 50_000
	rows := make([]stageResult, 0, 6)
	for _, ca := range []int{25, 100, 500, 2_000, 6_000, 11_000} {
		label := fmt.Sprintf("|C|~%d", ca)
		fmt.Printf("[S1-commsize] %s\n", label)
		r := medianOf3(t, "TestDeltaQBenchCommunitySize", label, "K1F", nodes, ca)
		rows = append(rows, r)
		if r.Timeout {
			break
		}
	}
	printLadderTable("S1-G1b — KONSTANTE Knoten- und Kantenzahl, variable Community-Größe", rows)

	// Die Auswertung gehört in den Lauf, nicht in eine Nacherzählung: das
	// Verhältnis Wall-Clock zu P muss unter der Hypothese ungefähr konstant
	// sein. Wandert es über Größenordnungen, misst die Wall-Clock etwas
	// anderes als P.
	fmt.Printf("\n%-14s %12s %12s %12s %14s %14s\n",
		"Punkt", "Wall (ms)", "|C| soll", "|C| ist", "P", "ns je P-Einheit")
	for _, r := range rows {
		if r.Timeout || r.P == 0 {
			continue
		}
		found := 0.0
		if r.Clusters > 0 {
			found = float64(r.Nodes) / float64(r.Clusters)
		}
		fmt.Printf("%-14s %12d %12.1f %12.1f %14.4g %14.4f\n",
			r.Label, r.WallMS, r.MeanCSize, found, r.P, float64(r.WallMS)*1e6/r.P)
	}
}

// Abschnitt: Arm (iii) — Kalibrierung des Prädiktors (S1-G2).

// TestDeltaQPredictorCalibration ist die ROT-PROBE des Mikro-Benchs: misst der
// Prädiktor wirklich P, oder misst er etwas anderes?
//
// Die Fixture ist analytisch bekannt: c disjunkte Cliquen der Größe s, keine
// Kante zwischen ihnen. Für jeden Knoten ist conn(v) = {eigene Community}, also
// gilt exakt P = n · s. Weicht der gemessene Wert ab, ist S1-G1 wertlos
// (§6.7 S1-G2).
//
// Zweite Probe im selben Test: eine Kette aus zwei Cliquen mit genau einer
// Brückenkante. Dort ist P = n·s + 2·s — die beiden Brückenknoten sehen
// zusätzlich die Nachbar-Community. Die Probe fängt genau den Fehler, den eine
// naive Implementierung macht: Nachbar-Communities doppelt oder gar nicht zu zählen.
func TestDeltaQPredictorCalibration(t *testing.T) {
	// Die Cliquen-Fixture wird von Hand gebaut, nicht vom Generator: sie muss
	// analytisch exakt sein, und resolveScale erzwingt (zu Recht) einen
	// Community-Ring, der die Isolation der Cliquen zerstören würde.
	const (
		c = 40
		s = 25
		n = c * s
	)
	nodes := make([]string, n)
	stub, err := resolveScale(specK1Organic(n, 1))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for i := range nodes {
		nodes[i] = stub.uuidAt(i)
	}
	comm := make([]int32, n)
	size := make([]int64, c)
	for i := 0; i < n; i++ {
		comm[i] = int32(i / s) //nolint:gosec // c < 2^31
		size[i/s]++
	}

	cliqueEdges := func(onEdge func(a, b int, w float64)) {
		for k := 0; k < c; k++ {
			for i := 0; i < s; i++ {
				for j := i + 1; j < s; j++ {
					onEdge(k*s+i, k*s+j, 1)
				}
			}
		}
	}
	got := deltaQPredictor(n, cliqueEdges, comm, size)
	want := float64(n) * float64(s)
	if got != want {
		t.Fatalf("S1-G2 ROT: Prädiktor liefert P=%.0f, analytisch %.0f (%d Cliquen à %d)", got, want, c, s)
	}
	t.Logf("S1-G2: reine Cliquen — P=%.0f = n·s (%d·%d)", got, n, s)

	// Brücke zwischen Clique 0 und Clique 1: Knoten 0 und Knoten s.
	bridgedEdges := func(onEdge func(a, b int, w float64)) {
		cliqueEdges(onEdge)
		onEdge(0, s, 1)
	}
	got2 := deltaQPredictor(n, bridgedEdges, comm, size)
	want2 := want + 2*float64(s)
	if got2 != want2 {
		t.Fatalf("S1-G2 ROT: mit Brücke P=%.0f, analytisch %.0f", got2, want2)
	}
	t.Logf("S1-G2: eine Brückenkante — P=%.0f = n·s + 2s ✓", got2)
}
