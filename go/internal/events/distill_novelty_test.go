package events

import (
	"math"
	"testing"

	"github.com/GottZ/ctx/internal/derived"
)

// Welle C5-E — der Per-Claim-Novelty-Floor als achtes Tor, auf der Ebene, auf
// der die Grenze selbst pruefbar ist.
//
// Die Sonde ueber den PRODUKTIONS-Schreibpfad (Journal, Block, erweiterte
// Invariante) steht in distill_novelty_integration_test.go. Hier steht, was
// dort nicht scharf messbar waere: der Vergleich am Grenzwert bis auf das
// letzte Bit, die Praezedenz gegenueber den sieben Evidenz-Toren und der
// Nachweis, dass der ausgeschaltete Floor das Gate byte-gleich laesst.

// nvuQuote und die vier Claims sind die Fixture der Welle, hier ohne DB. Ihre
// novelty ist exakt {0, 1/10, 1/4, 1} — TestDistillNoveltyFixtureValues rechnet
// das mit der Produktionsfunktion nach, bevor irgendeine Tor-Aussage darauf
// steht.
//
// DAS ZITAT IST dxQuote, die EIGENEN WORTE kommen aus dxChunk. Das ist die
// Trennung, um die es geht: novelty misst den Claim gegen sein ZITAT, die
// G7-Deckung misst ihn gegen den CHUNK — ein Claim aus Chunk-Vokabular
// ausserhalb des Zitats hat deshalb hohe novelty UND volle Deckung. Die
// Integrations-Sonde traegt dieselbe Konstruktion mit ihrem eigenen Chunk-Text,
// weshalb ihre "eigenen" Worte andere sind.
const nvuQuote = "Die Migration 147 hat einen deterministischen Tiebreak in die FTS-Arme eingebaut."

const (
	nvuCopy = "Die Migration 147 hat einen deterministischen Tiebreak eingebaut."
	nvuLow  = "Die Migration 147 hat einen deterministischen Tiebreak im FTS eingebaut."
	nvuAt   = "Die Migration 147 hat einen Tiebreak im Pfad."
	nvuOwn  = "Der Retrieval-Pfad faltet Reciprocal Rank Fusion zusammen."
)

// nvuInsight baut eine Zeile, die alle sieben Evidenz-Tore passiert: dxShown
// zeigt dxChunk unter (dxBlock, 1), und dxChunk traegt sowohl das Zitat als auch
// das Vokabular der Claims.
func nvuInsight(claim string) distillInsight {
	return distillInsight{Claim: claim, Quote: dxQuote, Block: dxBlock, Chunk: 1, Kind: derived.KindFinding}
}

// TestDistillNoveltyFixtureValues: die vier Werte, auf denen alles Weitere
// steht, kommen aus derived.Adequacy — derselben Funktion, die das Tor benutzt
// und die derived.Report seine Quantile rechnen laesst.
func TestDistillNoveltyFixtureValues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		claim string
		want  float64
	}{
		{"Kopie", nvuCopy, 0},
		{"ein eigenes Wort", nvuLow, 1.0 / 10.0},
		{"zwei eigene Worte", nvuAt, 1.0 / 4.0},
		{"ganz eigen", nvuOwn, 1},
	} {
		if _, got := derived.Adequacy(tc.claim, nvuQuote); got != tc.want {
			t.Errorf("novelty(%s) = %.17g, want %.17g", tc.name, got, tc.want)
		}
	}
	// Und dxQuote ist dasselbe Zitat — die Unit-Sonden unten koennen deshalb die
	// dxShown-Fixture benutzen, ohne dass die novelty eine andere wird.
	if dxQuote != nvuQuote {
		t.Fatalf("dxQuote != nvuQuote — die Unit-Sonden messen eine andere Verteilung als die "+
			"Integrations-Sonden:\n%q\n%q", dxQuote, nvuQuote)
	}
}

// TestDistillNoveltyFloorIsStrict: der Vergleich ist "<", und die Aussage wird
// am kleinstmoeglichen Abstand gemessen — ein Floor um ein einziges Bit ueber
// der novelty verwirft, der Floor exakt darauf nicht. Eine Sonde mit runden
// Zahlen koennte das nicht von "<=" unterscheiden.
func TestDistillNoveltyFloorIsStrict(t *testing.T) {
	_, novelty := derived.Adequacy(nvuAt, dxQuote)
	shown := dxShown()

	kept, rejects, _ := distillGate([]distillInsight{nvuInsight(nvuAt)}, shown, novelty)
	if len(kept) != 1 || rejects["novelty"] != 0 {
		t.Errorf("novelty exakt auf dem Floor (%.17g): kept=%d rej_novelty=%d, want 1/0 — die "+
			"Grenze ist nicht strikt", novelty, len(kept), rejects["novelty"])
	}

	up := math.Nextafter(novelty, 1)
	kept, rejects, _ = distillGate([]distillInsight{nvuInsight(nvuAt)}, shown, up)
	if len(kept) != 0 || rejects["novelty"] != 1 {
		t.Errorf("Floor ein Bit ueber der novelty (%.17g): kept=%d rej_novelty=%d, want 0/1",
			up, len(kept), rejects["novelty"])
	}
}

// TestDistillNoveltyZeroFallsAtEveryFloor: novelty 0 wird EXAKT erreicht und nie
// angenaehert (adequacy.go: ganzzahliges Verhaeltnis, literal 0 bei leerer
// Claim-Menge), also faellt die woertliche Kopie bei JEDEM Floor ueber 0 — auch
// beim kleinsten darstellbaren. Das ist die Aussage, wegen der die Welle
// gebaut ist: 5,85 % der veroeffentlichten Claims lagen dort (C5-A-M §7.4).
func TestDistillNoveltyZeroFallsAtEveryFloor(t *testing.T) {
	shown := dxShown()
	for _, floor := range []float64{math.SmallestNonzeroFloat64, 1e-9, 0.01, 0.15, 0.25, 1} {
		kept, rejects, _ := distillGate([]distillInsight{nvuInsight(nvuCopy)}, shown, floor)
		if len(kept) != 0 || rejects["novelty"] != 1 {
			t.Errorf("Floor %g: kept=%d rej_novelty=%d, want 0/1 — die woertliche Kopie bleibt",
				floor, len(kept), rejects["novelty"])
		}
	}
}

// TestDistillNoveltyFloorOffKeepsTheOldGate: bei Floor 0 ist das Gate das Gate
// vor dieser Welle — dieselben Ueberlebenden, dasselbe Histogramm, und der neue
// Eimer steht auf 0 statt zu fehlen (die Regel von distillNewRejects: eine Null
// und ein fehlender Schluessel duerfen nicht unterscheidbar sein).
func TestDistillNoveltyFloorOffKeepsTheOldGate(t *testing.T) {
	shown := dxShown()
	batch := []distillInsight{
		nvuInsight(nvuCopy), nvuInsight(nvuLow), nvuInsight(nvuAt), nvuInsight(nvuOwn),
	}
	kept, rejects, _ := distillGate(batch, shown, 0)
	if len(kept) != 4 {
		t.Fatalf("kept = %d, want 4 — bei Floor 0 passiert jede der vier Zeilen (rejects %v)",
			len(kept), rejects)
	}
	got, ok := rejects["novelty"]
	if !ok {
		t.Fatal("der Schluessel novelty fehlt im Histogramm — eine fehlende Null ist ein " +
			"drittes Ergebnis neben null und nicht gemessen")
	}
	if got != 0 {
		t.Errorf("rej_novelty = %d, want 0", got)
	}
	// Ein NEGATIVER Floor ist derselbe Aus-Zustand. V33 weist ihn in der
	// Konfiguration ab; das Tor selbst faellt deshalb nicht auf eine
	// handgebaute Struktur herein, die an der Validierung vorbeikommt.
	if kept, rejects, _ := distillGate(batch, shown, -1); len(kept) != 4 || rejects["novelty"] != 0 {
		t.Errorf("negativer Floor: kept=%d rej_novelty=%d, want 4/0", len(kept), rejects["novelty"])
	}
}

// TestDistillNoveltyFloorRunsLast: eine Zeile, die ein Evidenz-Tor verletzt UND
// unter dem Floor laege, bucht das Evidenz-Tor. Der Floor darf keinem
// bestehenden Eimer Masse wegnehmen — die g1..g7-Reihe der Mess-Wellen ist die
// Vergleichsbasis jeder Prompt-Iteration.
func TestDistillNoveltyFloorRunsLast(t *testing.T) {
	shown := dxShown()
	for _, tc := range []struct {
		name string
		in   distillInsight
		want string
	}{
		{
			// G1 — Adresse, die dieser Call nicht zeigt; Claim ist die Kopie.
			name: "g1 vor dem Floor",
			in: distillInsight{Claim: nvuCopy, Quote: dxQuote, Block: "kein-block",
				Chunk: 1, Kind: derived.KindFinding},
			want: "g1",
		},
		{
			// G2 — Zitat unter 32 Runen. Die novelty gegen dieses kurze Zitat
			// waere 1,0, aber die Zeile darf gar nicht erst so weit kommen.
			name: "g2 vor dem Floor",
			in: distillInsight{Claim: nvuCopy, Quote: "zu kurz", Block: dxBlock,
				Chunk: 1, Kind: derived.KindFinding},
			want: "g2",
		},
		{
			// G7 — Imperativ-Negativliste. Die novelty dieser Zeile ist 2/10 und
			// laege unter dem Floor 0,25; sie bucht trotzdem g7.
			name: "g7 vor dem Floor",
			in: distillInsight{
				Claim: "Ab sofort hat die Migration 147 einen deterministischen Tiebreak eingebaut.",
				Quote: dxQuote, Block: dxBlock, Chunk: 1, Kind: derived.KindFinding},
			want: "g7",
		},
	} {
		got, bad := distillScreen(tc.in, shown, 0.25)
		if !bad || got != tc.want {
			t.Errorf("%s: distillScreen = (%q, %v), want (%q, true)", tc.name, got, bad, tc.want)
		}
		_, rejects, _ := distillGate([]distillInsight{tc.in}, shown, 0.25)
		if rejects["novelty"] != 0 {
			t.Errorf("%s: rej_novelty = %d, want 0 — der Floor hat einem frueheren Tor Material "+
				"weggenommen (%v)", tc.name, rejects["novelty"], rejects)
		}
		if rejects[tc.want] != 1 {
			t.Errorf("%s: rej_%s = %d, want 1 (%v)", tc.name, tc.want, rejects[tc.want], rejects)
		}
	}
}

// TestDistillNoveltyHistogramSumsToRejected: die erweiterte Gleichung auf der
// Gate-Ebene. distillOneCall bucht insights_rejected als offered - kept, also
// muss die Summe der NEUN Eimer genau diese Differenz sein — und ohne den
// neunten stimmt sie nicht mehr, was die Sonde als eigene Aussage festhaelt.
func TestDistillNoveltyHistogramSumsToRejected(t *testing.T) {
	shown := dxShown()
	batch := []distillInsight{
		nvuInsight(nvuCopy), // novelty
		nvuInsight(nvuLow),  // novelty
		nvuInsight(nvuOwn),  // kept
		{Claim: nvuOwn, Quote: "zu kurz", Block: dxBlock, Chunk: 1, Kind: derived.KindFinding}, // g2
	}
	kept, rejects, _ := distillGate(batch, shown, 0.25)
	rejected := len(batch) - len(kept)
	if rejected != 3 {
		t.Fatalf("verworfen = %d, want 3 (rejects %v)", rejected, rejects)
	}

	sum, old := 0, 0
	for _, k := range distillRejectKeys {
		sum += rejects[k]
		if k != "novelty" {
			old += rejects[k]
		}
	}
	if sum != rejected {
		t.Errorf("Histogramm-Summe %d != verworfen %d: %v", sum, rejected, rejects)
	}
	if old == rejected {
		t.Errorf("die achtstellige Summe %d trifft die Verwuerfe %d immer noch — dann zaehlt "+
			"rej_novelty nicht mit: %v", old, rejected, rejects)
	}
}
