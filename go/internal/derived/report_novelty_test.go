package derived

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// Gate C5-A (Entscheid C5-2, Checkpoint #5): die novelty-VERTEILUNG im Report.
//
// WAS HIER ROT WAR. Bis zu dieser Welle nannte GateReport genau eine Zahl über
// novelty: den Median. Welle C4-R hat gemessen, was der nicht sieht — Lauf 2
// schrieb 7 Claims mit novelty 0 und 18,8 % unter GoodhartMinNovelty, während
// der Median bei 0,4286 stand und das vorab festgelegte Kriterium (Median
// ≥ 0,30) formal erfüllte. Die Verteilung musste von Hand aus den Roh-Antworten
// nachgerechnet werden, weil das Instrument sie nicht führte.
//
// Die Sonden unten bauen genau diese Lage nach und verlangen, dass sie am
// Report ABLESBAR ist. Ein Gate wird dabei NICHT gesetzt: das Urteil (judge)
// bleibt unverändert, C5-2 stellt erst das Wellen-Kriterium um.
//
//	go test ./internal/derived/ -run 'TestNovelty|TestMedian|TestQuantile' -count=1 -v

// lowClaimText nennt vier Token aus honestQuote und ein eigenes: novelty = 1/5
// = 0,2 — über null, aber über GoodhartMinNovelty. Es ist der mittlere Rang, an
// dem sich Median und p25 trennen lassen.
const lowClaimText = "alpha beta gamma delta novum"

func lowClaim() Claim {
	return Claim{
		Claim:    lowClaimText,
		Quote:    honestQuote,
		SourceID: "00000000-0000-0000-0000-000000000003",
		Kind:     "finding",
	}
}

// tailCharge ist die C4-R-Lage in klein: ein Median satt über der Schwelle,
// ein eingebrochener Schwanz. 4 Kopien (novelty 0), 4 schwache Claims (0,2),
// 12 ehrliche (0,4) — n = 20, Verankerungs-Rate 1,0.
func tailCharge() []Item {
	claims := make([]Claim, 0, 20)
	claims = append(claims, repeat(copyClaim(), 4)...)
	claims = append(claims, repeat(lowClaim(), 4)...)
	claims = append(claims, repeat(honestClaim(), 12)...)
	return []Item{keptItem(claims...)}
}

// TestNoveltyDistributionIsReported ist die Welle: der Report führt die
// Verteilung, und das C5-2-Kriterium (p10 ≥ 0,15 UND Anteil novelty 0 ≤ 1 %)
// ist aus ihm ohne Nachrechnen ablesbar.
func TestNoveltyDistributionIsReported(t *testing.T) {
	r := Report(tailCharge())

	if r.NoveltyN != 20 {
		t.Fatalf("NoveltyN = %d, want 20", r.NoveltyN)
	}
	if r.NoveltyN != r.ClaimsKept {
		t.Fatalf("NoveltyN = %d, ClaimsKept = %d — die Verteilung beschreibt eine "+
			"andere Menge als die, über die berichtet wird", r.NoveltyN, r.ClaimsKept)
	}
	for _, c := range []struct {
		name string
		got  float64
		want float64
	}{
		{"p10", r.NoveltyP10, 0},
		{"p25", r.NoveltyP25, 0.2},
		{"Median", r.MedianNovelty, 0.4},
		{"Anteil < 0,15", r.NoveltyBelowFloorShare, 0.2},
		{"Anteil = 0", r.NoveltyZeroShare, 0.2},
	} {
		if !closeTo(c.got, c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	// UND DAS IST DER BEFUND, DEN DIE WELLE SICHTBAR MACHT: dieselbe Charge
	// besteht das Goodhart-Urteil (der Median trägt), verfehlt aber das
	// C5-2-Kriterium in beiden Hälften. Vor dieser Welle war die zweite Aussage
	// aus dem Report nicht zu treffen.
	if !r.Passed {
		t.Fatalf("Fixture-Fehler: die Charge sollte am Median-Urteil durchgehen: %s", r.Reason)
	}
	if r.NoveltyP10 >= GoodhartMinNovelty || r.NoveltyZeroShare <= 0.01 {
		t.Fatalf("Fixture-Fehler: die Charge sollte das C5-2-Kriterium verfehlen "+
			"(p10 %.4f, Anteil 0 %.4f)", r.NoveltyP10, r.NoveltyZeroShare)
	}
}

// TestNoveltyDistributionIsRendered: die Verteilung steht in der
// Menschen-Ausgabe, nicht nur im JSON. Die Mess-Wellen lesen beides — die
// Konsole beim Lauf, das JSON beim Vergleich zweier Läufe.
func TestNoveltyDistributionIsRendered(t *testing.T) {
	out := Report(tailCharge()).String()
	t.Logf("Report-Ausgabe:\n%s", out)

	for _, want := range []string{
		"novelty-Verteilung (n=20)", "p10 0.0000", "p25 0.2000",
		"Median 0.4000", "Anteil < 0.15: 0.2000", "Anteil = 0: 0.2000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("die Ausgabe nennt %q nicht:\n%s", want, out)
		}
	}
	// Der Median bleibt zusätzlich auf seiner alten Zeile: zwei Läufe über die
	// Welle hinweg müssen vergleichbar bleiben.
	if !strings.Contains(out, "Median-novelty 0.4000") {
		t.Errorf("die alte Median-Zeile ist verschwunden:\n%s", out)
	}
}

// TestNoveltyDistributionIsInTheJSON: die fünf Felder sind Draht-Format. Ein
// Wert, den nur String() kennt, wäre für den Lauf-Vergleich nicht da.
func TestNoveltyDistributionIsInTheJSON(t *testing.T) {
	b, err := json.Marshal(Report(tailCharge()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{
		"novelty_n", "novelty_p10", "novelty_p25",
		"novelty_below_floor_share", "novelty_zero_share",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("das JSON führt %q nicht: %s", key, b)
		}
	}
}

// TestNoveltyDistributionOnAnEmptyCharge: eine leere Charge misst nichts, und
// jede der fünf Zahlen sagt 0 — nicht NaN. Ein NaN vergleicht sich gegen jede
// Schwelle als false, und das C5-2-Kriterium würde auf genau den entarteten
// Chargen stumm, für die es gedacht ist.
func TestNoveltyDistributionOnAnEmptyCharge(t *testing.T) {
	r := Report(nil)
	if r.NoveltyN != 0 {
		t.Errorf("NoveltyN = %d, want 0", r.NoveltyN)
	}
	for name, got := range map[string]float64{
		"p10": r.NoveltyP10, "p25": r.NoveltyP25,
		"BelowFloor": r.NoveltyBelowFloorShare, "Zero": r.NoveltyZeroShare,
	} {
		if got != 0 {
			t.Errorf("%s = %v, want exactly 0", name, got)
		}
	}
}

// TestNoveltyDistributionDoesNotJudge ist die Gegenprobe zur Nicht-Änderung:
// das Urteil hängt weiterhin am Median allein. Eine Charge, die das
// C5-2-Kriterium verfehlt, aber den Median hält, bleibt "bestanden" — C5-2
// stellt das WELLEN-Kriterium um, nicht das Schreibpfad-Gate. Fiele diese Sonde,
// wäre unbemerkt ein Per-Claim-Floor eingebaut worden, den erst der Befund
// entscheiden darf.
func TestNoveltyDistributionDoesNotJudge(t *testing.T) {
	r := Report(tailCharge())
	if !r.Passed {
		t.Fatalf("das Urteil hat sich geändert: %s", r.Reason)
	}
	if !strings.Contains(r.Reason, "Median-novelty") {
		t.Fatalf("die Begründung nennt nicht mehr den Median: %q", r.Reason)
	}
	for _, s := range []string{"p10", "p25", "Anteil"} {
		if strings.Contains(r.Reason, s) {
			t.Fatalf("die Begründung urteilt über %q — das ist ein Gate, kein Report: %q", s, r.Reason)
		}
	}
}

// TestMedianIsTheHalfQuantile pinnt die Delegation: median ist quantile bei
// q = 0,5 und liefert für JEDE Größe genau das, was die frühere Formel lieferte
// (mittlerer Wert bei ungerader Anzahl, Mittel der beiden mittleren bei
// gerader). Ohne diese Sonde könnte die Umstellung die Zahl verschieben, an der
// alle früheren Wellen gemessen wurden.
func TestMedianIsTheHalfQuantile(t *testing.T) {
	sets := [][]float64{
		{}, {0.7}, {0.2, 0.8}, {0.9, 0.1, 0.5},
		{0.4, 0.1, 0.3, 0.2}, {5, 1, 4, 2, 3}, {0, 0, 0.4, 0.4, 0.4, 0.4},
	}
	for _, xs := range sets {
		want := legacyMedian(xs)
		if got := median(xs); got != want {
			t.Errorf("median(%v) = %v, alte Formel %v", xs, got, want)
		}
		if got := quantile(xs, 0.5); got != want {
			t.Errorf("quantile(%v, 0.5) = %v, alte Formel %v", xs, got, want)
		}
	}

	// Die Behauptung ist BYTE-EXAKTHEIT, nicht Nähe — also wird sie ohne
	// Toleranz und erschöpfend über kleine Brüche geprüft. Die allgemeine
	// Interpolationsform a+0,5·(b−a) weicht von (a+b)/2 auf einem messbaren
	// Anteil solcher Paare um 1 ULP ab (Review C5-A Finding 2); der
	// Halbschritt-Zweig in quantile macht die Delegation exakt, und dieser
	// Sweep fällt rot, wenn ihn jemand entfernt.
	for d := 1; d <= 24; d++ {
		for i := 0; i <= d; i++ {
			for j := i; j <= d; j++ {
				a, b := float64(i)/float64(d), float64(j)/float64(d)
				want := (a + b) / 2
				if got := median([]float64{a, b}); got != want {
					t.Fatalf("median([%v %v]) = %v, alte Formel %v (Abweichung %g)",
						a, b, got, want, got-want)
				}
			}
		}
	}
}

// legacyMedian ist die Formel, die report.go vor dieser Welle trug — hier als
// unabhängige Referenz und nicht als Aufruf der neuen Funktion, sonst prüfte die
// Sonde sich selbst.
func legacyMedian(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// TestQuantileInterpolatesAndClamps hält die Definition fest, auf die sich das
// C5-2-Kriterium bezieht: lineare Interpolation zwischen den beiden
// benachbarten Ordnungsstatistiken (numpy-Default / R Typ 7), an den Rändern auf
// die Stichprobe geklemmt statt extrapoliert.
func TestQuantileInterpolatesAndClamps(t *testing.T) {
	xs := []float64{0, 1, 2, 3}
	for _, c := range []struct {
		q    float64
		want float64
	}{
		{0, 0}, {0.25, 0.75}, {0.5, 1.5}, {0.75, 2.25}, {1, 3},
		// Außerhalb von [0,1] wird geklemmt, nicht extrapoliert — ein p10 unter
		// dem kleinsten gemessenen Wert wäre eine erfundene Messung.
		{-1, 0}, {2, 3},
	} {
		if got := quantile(xs, c.q); !closeTo(got, c.want) {
			t.Errorf("quantile(%v, %v) = %v, want %v", xs, c.q, got, c.want)
		}
	}
	// Ein einzelner Wert ist jedes Quantil seiner selbst.
	for _, q := range []float64{0, 0.1, 0.5, 0.9, 1} {
		if got := quantile([]float64{0.42}, q); !closeTo(got, 0.42) {
			t.Errorf("quantile([0.42], %v) = %v, want 0.42", q, got)
		}
	}
}

// TestQuantileDoesNotReorderItsInput: der Report ist rein und deterministisch,
// und eine Faltung, die die Slice ihres Aufrufers sortiert, würde das von der
// Aufrufreihenfolge abhängig machen.
func TestQuantileDoesNotReorderItsInput(t *testing.T) {
	xs := []float64{0.9, 0.1, 0.5, 0.3}
	before := append([]float64(nil), xs...)
	_ = quantile(xs, 0.25)
	_ = median(xs)
	for i := range xs {
		if xs[i] != before[i] {
			t.Fatalf("die Eingabe wurde umsortiert: %v, war %v", xs, before)
		}
	}
}
