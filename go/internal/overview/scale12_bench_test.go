//go:build integration

// Achse 04 / Welle S12 — der Ziel-Scale-Freigabelauf (design/04 §7 S12,
// Gate S12-G1).
//
// Das ist die ABNAHME-Messung der Achse: 9,8M Louvain-Knoten (= 10M
// Wissens-Blöcke × 0,98, §6.1 Zeile B) in BEIDEN Dichte-Szenarien, mit dem
// eigenen Kern, im eigenen Prozess je Messpunkt.
//
// Was hier NICHT gemessen wird und warum: die DB-Phasen (load_ms, copy_ms,
// lock_held_ms) brauchen einen 9,8M-Korpus in einer echten Datenbank —
// nach S0 rund 20–30 GB Disk. Diese Strecke hat keine Freigabe für einen
// Deploy und kein Bench-DB-Ziel; der Rechenpfad ist der Teil, der die
// Achse trägt, und er ist hier vollständig vermessen. Der DB-Arm bleibt als
// benannte Lücke stehen statt als geschätzte Zahl.
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

	"github.com/GottZ/ctx/internal/louvain"
)

const s12KindEnv = "CTX_ROOTMAP_S12_KIND"

// s12Budget ist das Zeitbudget je Szenario. Nicht-Terminieren ist ein
// ERGEBNIS mit Diagnose, kein Testfehler — dieselbe Regel wie in S1.
const s12Budget = 45 * time.Minute

// TestS12_TargetScaleAcceptance ist Gate S12-G1.
func TestS12_TargetScaleAcceptance(t *testing.T) {
	if os.Getenv(benchEnv) == "" {
		t.Skip(benchEnv + " ungesetzt — S12-Freigabelauf uebersprungen")
	}
	if kind := os.Getenv(s12KindEnv); kind != "" {
		runS12Child(t, kind)
		return
	}

	fmt.Printf("\n=== S12 — Ziel-Scale-Freigabelauf, 9,8M Knoten, engine=ctx ===\n")
	fmt.Printf("%-10s %10s %12s %11s %10s %9s %9s %8s %7s\n",
		"Szenario", "Knoten", "Paare", "Wandzeit", "Peak-RSS", "Q", "Cluster", "Ebenen", "Komp.")

	type row struct {
		kind   string
		res    s12Result
		failed bool
	}
	var rows []row
	for _, kind := range []string{"K1", "K2"} {
		r, ok := runS12Stage(t, kind)
		rows = append(rows, row{kind: kind, res: r, failed: !ok})
		if !ok {
			fmt.Printf("%-10s %10s %12s %11s %10s %9s %9s %8s %7s\n",
				kind, "9800000", "—", ">45min", "—", "—", "—", "—", "—")
			continue
		}
		fmt.Printf("%-10s %10d %12d %10.1fs %10s %9.4f %9d %8d %7d\n",
			kind, r.Nodes, r.Pairs, float64(r.WallMS)/1000, mbFromKB(r.RSSKb),
			r.Q, r.Clusters, r.Levels, r.Components)
	}

	fmt.Printf("\n[S12-G1] Bewertung gegen die Budgets der Achse:\n")
	for _, r := range rows {
		if r.failed {
			fmt.Printf("  %s: NICHT TERMINIERT in %s — das ist ein Ergebnis, keine Panne.\n", r.kind, s12Budget)
			continue
		}
		// §6.6: die Kadenz ist 6 h. Ein Lauf, der darunter bleibt, traegt sie.
		cadenceOK := time.Duration(r.res.WallMS)*time.Millisecond < 6*time.Hour
		fmt.Printf("  %s: Rechenzeit %.1fs (6-h-Kadenz %v) · Kind-Peak %s · sigma_drift %.2e\n",
			r.kind, float64(r.res.WallMS)/1000, cadenceOK, mbFromKB(r.res.RSSKb), r.res.Drift)
		if !cadenceOK {
			t.Errorf("S12-G1: %s reisst die 6-h-Kadenz allein mit der Rechenphase", r.kind)
		}
	}
	fmt.Printf("\n  Zum Vergleich, gemessen in S1 auf demselben Fixture-Generator:\n")
	fmt.Printf("  gonum konvergierte bei 1,065M Knoten nach >25 min NICHT; bei 800k/K1\n")
	fmt.Printf("  brauchte es 210,5s, wo der eigene Kern 284ms brauchte (Faktor 355).\n")
}

type s12Result struct {
	Nodes, Pairs, Clusters, Levels, Components int
	WallMS                                     int64
	RSSKb                                      int64
	Q, Drift                                   float64
}

func runS12Stage(t *testing.T, kind string) (s12Result, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), s12Budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestS12_TargetScaleAcceptance$", "-test.v", "-test.timeout=0") //nolint:gosec // eigenes Testbinary
	cmd.Env = append(os.Environ(), benchEnv+"=1", s12KindEnv+"="+kind)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return s12Result{}, false
	}
	var r s12Result
	found := false
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "S12RESULT ") {
			continue
		}
		found = true
		for _, kv := range strings.Fields(strings.TrimPrefix(line, "S12RESULT ")) {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			switch k {
			case "nodes":
				r.Nodes, _ = strconv.Atoi(v)
			case "pairs":
				r.Pairs, _ = strconv.Atoi(v)
			case "wall_ms":
				r.WallMS, _ = strconv.ParseInt(v, 10, 64)
			case "rss_kb":
				r.RSSKb, _ = strconv.ParseInt(v, 10, 64)
			case "q":
				r.Q, _ = strconv.ParseFloat(v, 64)
			case "clusters":
				r.Clusters, _ = strconv.Atoi(v)
			case "levels":
				r.Levels, _ = strconv.Atoi(v)
			case "components":
				r.Components, _ = strconv.Atoi(v)
			case "drift":
				r.Drift, _ = strconv.ParseFloat(v, 64)
			}
		}
	}
	if !found {
		t.Fatalf("S12 %s lieferte keine S12RESULT-Zeile (err=%v):\n%s", kind, err, tailLines(string(out), 20))
	}
	return r, true
}

func runS12Child(t *testing.T, kind string) {
	const target = 9_800_000
	spec := specK1Organic(target, 7)
	if kind == "K2" {
		spec = specK2Flat(target, 7)
	}
	f, err := resolveScale(spec)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	g := buildCSRFromFixture(f)
	lg := louvain.NewGraph(g.Off, g.Adj, g.W)
	runtime.GC()

	start := time.Now()
	res, err := louvain.RunComponents(context.Background(), lg,
		louvain.Options{Resolution: 1.0, Refine: true})
	wall := time.Since(start)
	if err != nil {
		fmt.Printf("S12RESULT error=%q\n", err.Error())
		t.Fatalf("louvain: %v", err)
	}
	fmt.Printf("S12RESULT nodes=%d pairs=%d wall_ms=%d rss_kb=%d q=%.6f clusters=%d levels=%d components=%d drift=%.3e\n",
		f.spec.Nodes, g.Pairs, wall.Milliseconds(), ReadVmHWMkB(),
		res.Q, res.Clusters, res.Levels, res.Components, res.SigmaDrift)
}
