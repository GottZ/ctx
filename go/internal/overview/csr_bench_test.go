//go:build integration

// Achse 04 / Welle S3 — Gates S3-G3 (RSS-Delta) und S3-G5 (Pass-Kosten).
//
// Beide sind MESSUNGEN, keine Zusicherungen: sie liefern die Zahlen, die im
// Commit-Body stehen, und brechen nichts ab. Opt-in ueber CTX_ROOTMAP_BENCH wie
// die S1-Arme, je Messpunkt ein eigener Kindprozess — VmHWM ist ein
// Hochwassermarker, mehrere Substrate im selben Prozess lieferten die Zahl des
// teuersten statt die des jeweiligen. Genau dieser Fehler ist im ersten
// S1-Anlauf passiert.
package overview

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const csrSubstrateEnv = "CTX_ROOTMAP_BENCH_SUBSTRATE"

// buildCSRFromFixture baut die CSR direkt aus dem S0-Kantenstrom, ohne DB.
//
// Sie bildet den Zwei-Pass-Build nach (zaehlen, fuellen, verdichten) und
// erzeugt damit dieselbe Datenstruktur, die loadCSR aus dem Cursor baut. Der
// DB-Weg ist fuer eine SPEICHER-Messung ungeeignet: der pgx-Puffer und der
// Cursor-Zustand liegen dann mit im VmHWM und verwaschen genau den Unterschied,
// den G3 messen soll.
func buildCSRFromFixture(f *scaleFixture) *csrGraph {
	n := f.spec.Nodes
	g := &csrGraph{Off: make([]uint32, n+1)}
	deg := make([]uint32, n)
	f.stream(func(a, b int, _ float64) {
		if b >= n {
			g.Dangling++
			return
		}
		if a == b {
			g.SelfLoops++
			return
		}
		deg[a]++
		deg[b]++
	})
	var acc uint32
	for i, d := range deg {
		g.Off[i] = acc
		acc += d
	}
	g.Off[n] = acc
	g.Adj = make([]uint32, acc)
	g.W = make([]float64, acc)
	fill := make([]uint32, n)
	f.stream(func(a, b int, w float64) {
		if b >= n || a == b {
			return
		}
		g.Adj[g.Off[a]+fill[a]] = uint32(b) //nolint:gosec // Fixture-Index < 2^32
		g.W[g.Off[a]+fill[a]] = w
		fill[a]++
		g.Adj[g.Off[b]+fill[b]] = uint32(a) //nolint:gosec // Fixture-Index < 2^32
		g.W[g.Off[b]+fill[b]] = w
		fill[b]++
	})
	csrCompact(g, n)
	return g
}

// TestCSRBenchSubstrateRSS ist S3-G3: der Speicher-Unterschied der beiden
// Eingabeformen bei identischem Graphen.
//
// Verglichen wird das SUBSTRAT, nicht der ganze Rebuild: []rawEdge mit eigenen
// Strings (die Form, die pgx' Scan je Zeile erzeugt) plus die Symmetrisierungs-
// Map und die idx-Map, gegen Off/Adj/W. Alles andere — gonum-Graph, persist —
// ist in beiden Pfaden identisch und wuerde den Unterschied nur verduennen.
func TestCSRBenchSubstrateRSS(t *testing.T) {
	if os.Getenv(benchEnv) == "" {
		t.Skip(benchEnv + " ungesetzt — S3-G3 uebersprungen")
	}
	if kind := os.Getenv(csrSubstrateEnv); kind != "" {
		n, _ := strconv.Atoi(os.Getenv(stageNodesEnv))
		runSubstrateChild(t, kind, n)
		return
	}

	fmt.Printf("\n=== S3-G3 — Speicher je Eingabeform, identischer Graph (je eigener Prozess) ===\n")
	fmt.Printf("%-10s %10s %14s %14s %12s %10s\n", "Knoten", "Paare", "[]rawEdge+Maps", "CSR Off/Adj/W", "Delta", "Faktor")
	for _, n := range []int{50_000, 200_000, 400_000} {
		legacy := runSubstrateStage(t, "legacy", n)
		csr := runSubstrateStage(t, "csr", n)
		if legacy.rssKb == 0 || csr.rssKb == 0 {
			continue
		}
		delta := legacy.rssKb - csr.rssKb
		fmt.Printf("%-10d %10d %14s %14s %12s %9.2fx\n",
			n, csr.pairs, mbFromKB(legacy.rssKb), mbFromKB(csr.rssKb), mbFromKB(delta),
			float64(legacy.rssKb)/float64(csr.rssKb))
	}
}

type substrateResult struct {
	rssKb int64
	pairs int
}

func runSubstrateStage(t *testing.T, kind string, n int) substrateResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCSRBenchSubstrateRSS$", "-test.v", "-test.timeout=0") //nolint:gosec // eigenes Testbinary
	cmd.Env = append(os.Environ(), benchEnv+"=1", csrSubstrateEnv+"="+kind, stageNodesEnv+"="+strconv.Itoa(n))
	out, err := cmd.CombinedOutput()
	if err != nil && ctx.Err() != nil {
		t.Logf("substrate %s/%d: Budget gerissen", kind, n)
		return substrateResult{}
	}
	var res substrateResult
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "SUBSTRATE ") {
			continue
		}
		for _, kv := range strings.Fields(strings.TrimPrefix(line, "SUBSTRATE ")) {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			switch k {
			case "rss_kb":
				res.rssKb, _ = strconv.ParseInt(v, 10, 64)
			case "pairs":
				res.pairs, _ = strconv.Atoi(v)
			}
		}
	}
	if res.rssKb == 0 {
		t.Fatalf("substrate %s/%d lieferte keine SUBSTRATE-Zeile:\n%s", kind, n, tailLines(string(out), 15))
	}
	return res
}

func runSubstrateChild(t *testing.T, kind string, n int) {
	f, err := resolveScale(specK1Organic(n, 7))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	pairs := 0
	switch kind {
	case "legacy":
		// Der Ist-Pfad in seiner ganzen Breite: Kantenliste mit eigenen
		// Strings, Symmetrisierungs-Map, Index-Map.
		nodes, edges := materializeOwned(f)
		idx := make(map[string]int64, len(nodes))
		for i, u := range nodes {
			idx[u] = int64(i)
		}
		type pair struct{ a, b int64 }
		agg := make(map[pair]float64, len(edges))
		for _, e := range edges {
			a, okA := idx[e.src]
			b, okB := idx[e.dst]
			if !okA || !okB || a == b {
				continue
			}
			if a > b {
				a, b = b, a
			}
			agg[pair{a, b}] += e.weight
		}
		pairs = len(agg)
		runtime.KeepAlive(edges)
		runtime.KeepAlive(agg)
		runtime.KeepAlive(idx)
	case "csr":
		g := buildCSRFromFixture(f)
		pairs = g.Pairs
		runtime.KeepAlive(g)
	}
	fmt.Printf("SUBSTRATE rss_kb=%d pairs=%d\n", ReadVmHWMkB(), pairs)
}

// TestCSRBenchPassCost ist S3-G5: was der ZWEITE Cursor-Durchlauf kostet.
//
// Der Entwurf laesst offen, ob der zweite Pass entfallen soll (ungeordnet
// streamen und in Go sortieren, das graphcache-Muster). Diese Messung liefert
// die Zahl, an der das entschieden wird — sie entscheidet es nicht selbst.
//
// Sie braucht CTX_BENCH_DSN mit einem gefuellten Korpus; gegen einen
// Testcontainer waere sie eine Aussage ueber eine leere Tabelle.
func TestCSRBenchPassCost(t *testing.T) {
	if os.Getenv(benchEnv) == "" {
		t.Skip(benchEnv + " ungesetzt — S3-G5 uebersprungen")
	}
	dsn := os.Getenv("CTX_BENCH_DSN")
	if dsn == "" {
		t.Skip("CTX_BENCH_DSN ungesetzt — S3-G5 braucht einen gefuellten Korpus")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	tx, err := pool.BeginTx(ctx, txRepeatableReadReadOnly())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	nodes, _, err := csrLoadNodes(ctx, tx, []string{"knowledge", "audit-trail"}, nil)
	if err != nil {
		t.Fatalf("csrLoadNodes: %v", err)
	}
	var t1, t2 time.Duration
	var rows int
	for i := 0; i < 3; i++ {
		start := time.Now()
		n := 0
		if err := csrScanEdges(ctx, tx, nil, func(_, _ [16]byte, _ float64) { n++ }); err != nil {
			t.Fatalf("pass: %v", err)
		}
		d := time.Since(start)
		rows = n
		if i == 0 {
			t1 = d
		} else {
			t2 += d / 2
		}
	}
	fmt.Printf("\n=== S3-G5 — Kosten des zweiten Cursor-Durchlaufs ===\n")
	fmt.Printf("Knoten=%d Kantenzeilen=%d\n", len(nodes), rows)
	fmt.Printf("Pass 1 (kalt): %s · Pass 2+3 (Median warm): %s · Aufschlag zweiter Pass: %.1f %%\n",
		benchMS(t1), benchMS(t2), 100*float64(t2)/float64(t1))
	fmt.Printf("Abbruch-Kriterium des Entwurfs: > 40 %% ⇒ ungeordnetes Streaming bauen (graphcache-Muster)\n")
}
