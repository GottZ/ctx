// Gate C5-A (Entscheid C5-3, Checkpoint #5): die Zerlegung von G3.
//
// WAS HIER ROT WAR. Bis zu dieser Welle beantwortete das Journal auf einen
// G3-Verwurf genau eine Zahl: rej_g3. Ob das Modell FALSCH ADRESSIERT hat (das
// Zitat steht woertlich da, nur in einem anderen Chunk) oder ob es gar nicht
// dastand, war daraus nicht ableitbar — beide Faelle lieferten dasselbe
// Histogramm map[... g3:1 ...]. Die Sonden unten konstruieren die vier Faelle
// einzeln und verlangen, dass sie sich unterscheiden.
//
// JEDER FALL WIRD ISOLIERT GEZEIGT, nach dem Muster von
// distill_extract_test.go: die Zeile faellt an GENAU G3 und an keinem anderen
// Tor (distillGateFailures ueber alle sieben Screens). Sonst koennte eine Sonde
// gruen sein, weil eine andere Pruefung frueher zuschlaegt, und sie wuerde ueber
// die Zerlegung nichts aussagen.
//
//	go test ./internal/events/ -run TestDistillG3 -count=1 -v
package events

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/derived"
)

// Below: die Fixtures. Zwei vollstaendige Parts und ein LUECKENHAFTER — der
// dritte ist die Gegenprobe zur Lauf-Regel (siehe distillNewG3Index).

const (
	// Part 1, drei aufeinanderfolgende Chunks (1, 2, 3).
	g3P1C1 = "### Message 12 — user\n\n" +
		"Die Migration 147 hat einen deterministischen Tiebreak in die FTS-Arme eingebaut.\n"
	g3P1C2 = "### Message 13 — assistant\n\n" +
		"Der Retrieval-Pfad faltet vier Arme per Reciprocal Rank Fusion zusammen.\n"
	g3P1C3 = "### Message 14 — user\n\n" +
		"Das Damping des Insight-Typs steht seit Migration 146 auf 0,60.\n"

	// Part 2, ein Chunk — das FREMDE Material desselben Calls.
	g3P2C1 = "### Message 20 — assistant\n\n" +
		"Der Selektor zieht die Kandidaten in zwei Stufen aus dem Korpus.\n"

	// Part 3, Chunk 1 und Chunk 3: Chunk 2 hat das Prompt-Budget nicht mehr
	// gesetzt (distillBuildPrompt:574-580). Zwischen den beiden steht im Prompt
	// eine LUECKE, keine Naht.
	g3P3C1 = "### Message 30 — user\n\n" +
		"Der Janitor raeumt die Journal-Zeilen nach der Retention-Frist fort.\n"
	g3P3C3 = "### Message 32 — user\n\n" +
		"Die Kopie traegt die Sicherungstabellen der frueheren Wellen.\n"
)

const (
	// g3Claim1 nennt nur Inhaltswoerter aus g3P1C1 — dem Chunk, den die Sonden
	// ADRESSIEREN. Damit ist G7s Deckung 1,0 und der Claim maskiert die
	// Zitat-Pruefung nicht.
	g3Claim1 = "Die Migration 147 hat einen Tiebreak in die FTS-Arme eingebaut."
	// g3Claim3 tut dasselbe fuer g3P3C1.
	g3Claim3 = "Der Janitor raeumt die Journal-Zeilen fort."
)

// g3Shown ist der Prompt-Zustand eines Calls: drei Parts, sechs Chunks, eine
// Luecke.
func g3Shown() distillShown {
	return distillShown{
		text: map[distillChunkKey]string{
			{block: "1", chunk: 1}: g3P1C1,
			{block: "1", chunk: 2}: g3P1C2,
			{block: "1", chunk: 3}: g3P1C3,
			{block: "2", chunk: 1}: g3P2C1,
			{block: "3", chunk: 1}: g3P3C1,
			{block: "3", chunk: 3}: g3P3C3,
		},
		blockIDs: []string{"1", "2", "3"},
	}
}

// g3Assert ist die gemeinsame Form jeder Sonde: die Zeile faellt an genau G3,
// distillScreen sagt dasselbe, und die Zerlegung bucht sie unter `want`.
func g3Assert(t *testing.T, name string, in distillInsight, want string) {
	t.Helper()
	shown := g3Shown()

	fails := distillGateFailures(in, shown)
	if !fails["g3"] {
		t.Fatalf("%s: G3 feuert gar nicht (faellt an: %v) — die Sonde misst nicht, was sie soll",
			name, dxKeys(fails))
	}
	if len(fails) != 1 {
		t.Fatalf("%s: faellt an %v, want genau {g3} — ein anderes Tor maskiert die Sonde",
			name, dxKeys(fails))
	}
	if got, bad := distillScreen(in, shown); !bad || got != "g3" {
		t.Fatalf("%s: distillScreen = (%q, %v), want (\"g3\", true)", name, got, bad)
	}

	kept, rejects, g3 := distillGate([]distillInsight{in}, shown)
	if len(kept) != 0 || rejects["g3"] != 1 {
		t.Fatalf("%s: distillGate kept %d, rejects %v", name, len(kept), rejects)
	}
	if g3[want] != 1 {
		t.Fatalf("%s: G3-Zerlegung = %v, want %s=1 — der Fall ist nicht zaehlbar",
			name, g3, want)
	}
	if sum := dxSum(g3); sum != 1 {
		t.Fatalf("%s: G3-Zerlegung summiert %d, want 1 — ein Fall faellt in zwei Eimer: %v",
			name, sum, g3)
	}
}

// TestDistillG3Classification ist die Welle in vier Zeilen: vier konstruierte
// Faelle, vier verschiedene Eimer.
func TestDistillG3Classification(t *testing.T) {
	// (a) chunk — ADRESSIERUNGSFEHLER innerhalb des Parts. Das Zitat steht
	// woertlich in Chunk 2, genannt ist Chunk 1. Das ist die Hypothese aus §15
	// des C4-R-Berichts: mehrere gezeigte Segmente teilen sich block="N" und
	// unterscheiden sich nur in chunk="M".
	g3Assert(t, "Zitat im Nachbar-Chunk desselben Parts", distillInsight{
		Claim: g3Claim1,
		Quote: "Der Retrieval-Pfad faltet vier Arme per Reciprocal Rank Fusion zusammen.",
		Block: "1", Chunk: 1, Kind: derived.KindFinding,
	}, "chunk")

	// (b) span — das Zitat laeuft ueber die Grenze zwischen Chunk 1 und Chunk 2
	// desselben Parts. Es steht in KEINEM einzelnen Chunk und trotzdem woertlich
	// im Material: die Chunks eines Parts setzen sich byte-identisch zum
	// Part-Body zusammen. Ein Zwei-Wege-Schnitt wuerde diesen Fall unter
	// "nirgends" buchen und damit als Halluzination lesen.
	g3Assert(t, "Zitat ueber eine Chunk-Grenze desselben Parts", distillInsight{
		Claim: g3Claim1,
		Quote: "in die FTS-Arme eingebaut. ### Message 13 — assistant",
		Block: "1", Chunk: 1, Kind: derived.KindFinding,
	}, "span")

	// (c) part — das Zitat steht woertlich da, aber in einem ANDEREN Part
	// desselben Calls. Auch das ist ein Adressierungsfehler und keine
	// Halluzination; ohne diesen Eimer waere "nirgends" nicht wahr.
	g3Assert(t, "Zitat in einem fremden Part desselben Calls", distillInsight{
		Claim: g3Claim1,
		Quote: "Der Selektor zieht die Kandidaten in zwei Stufen aus dem Korpus.",
		Block: "1", Chunk: 1, Kind: derived.KindFinding,
	}, "part")

	// (d) none — das Zitat steht in nichts, was der Call gezeigt hat.
	g3Assert(t, "Zitat nirgends im gezeigten Material", distillInsight{
		Claim: g3Claim1,
		Quote: "Der Waerter meldet den Abbruch nach der dritten Wiederholung an.",
		Block: "1", Chunk: 1, Kind: derived.KindFinding,
	}, "none")
}

// TestDistillG3SpanDoesNotGlueOverAGap ist die GEGENPROBE zur Lauf-Regel und
// die eigentliche Falsifikation von (b).
//
// Part 3 zeigt Chunk 1 und Chunk 3; Chunk 2 hat das Budget nicht gesetzt.
// Zwischen den beiden gezeigten Chunks steht im Prompt eine Luecke. Ein Zitat,
// das genau ueber diese Klebestelle laeuft, hat im Material NIE gestanden — es
// darf nicht als "span" durchgehen, sonst wuerde die Welle eine Chunk-Grenze
// melden, die es nicht gibt, und die naechste Iteration wuerde am Chunking
// drehen statt am Generator.
//
// Baut man denselben Satz ueber zwei AUFEINANDERFOLGENDE Chunks (Sonde (b)),
// zaehlt dieselbe Funktion ihn als "span" — die beiden Sonden zusammen sind der
// Beleg, dass die Regel an der Konsekutivitaet haengt und nicht am Zufall.
func TestDistillG3SpanDoesNotGlueOverAGap(t *testing.T) {
	g3Assert(t, "Zitat ueber eine LUECKE zwischen Chunk 1 und Chunk 3", distillInsight{
		Claim: g3Claim3,
		Quote: "Retention-Frist fort. ### Message 32 — user",
		Block: "3", Chunk: 1, Kind: derived.KindFinding,
	}, "none")
}

// TestDistillG3PrecedenceIsTheSmallestErrorFirst haelt die Reihenfolge der drei
// Fragen fest. Ein Zitat, das in einem Nachbar-Chunk UND in einem fremden Part
// steht, ist beides — die Zerlegung muss den kleineren Adressierungsfehler
// nennen, sonst wandert die Klasse mit der Materiallage statt mit dem Fehler.
func TestDistillG3PrecedenceIsTheSmallestErrorFirst(t *testing.T) {
	// Derselbe Satz in Chunk 2 von Part 1 UND in Part 2.
	dup := "Der Selektor zieht die Kandidaten in zwei Stufen aus dem Korpus."
	shown := g3Shown()
	shown.text[distillChunkKey{block: "1", chunk: 2}] = g3P1C2 + dup + "\n"

	in := distillInsight{
		Claim: g3Claim1, Quote: dup,
		Block: "1", Chunk: 1, Kind: derived.KindFinding,
	}
	_, rejects, g3 := distillGate([]distillInsight{in}, shown)
	if rejects["g3"] != 1 {
		t.Fatalf("die Sonde trifft G3 nicht: %v", rejects)
	}
	if g3["chunk"] != 1 {
		t.Fatalf("G3-Zerlegung = %v, want chunk=1 — die Praezedenz nennt nicht den "+
			"kleinsten Adressierungsfehler zuerst", g3)
	}
}

// TestDistillG3DecompositionIsExact ist die zweite Gleichung der Welle ueber
// einen GEMISCHTEN Stapel: die vier Eimer summieren sich auf rej_g3, und die
// Zeilen, die an anderen Toren fallen, lassen sie unberuehrt.
//
// Ohne sie waere die Zerlegung eine Auswahl: ein g3-Fall, den niemand
// klassifiziert, saehe im Betrieb aus wie ein Fall, den es nicht gibt.
func TestDistillG3DecompositionIsExact(t *testing.T) {
	shown := g3Shown()
	batch := []distillInsight{
		// passiert alle Tore
		{Claim: g3Claim1, Quote: "Die Migration 147 hat einen deterministischen Tiebreak in die FTS-Arme eingebaut.",
			Block: "1", Chunk: 1, Kind: derived.KindFinding},
		// g3 → chunk
		{Claim: g3Claim1, Quote: "Der Retrieval-Pfad faltet vier Arme per Reciprocal Rank Fusion zusammen.",
			Block: "1", Chunk: 1, Kind: derived.KindFinding},
		// g3 → span
		{Claim: g3Claim1, Quote: "in die FTS-Arme eingebaut. ### Message 13 — assistant",
			Block: "1", Chunk: 1, Kind: derived.KindFinding},
		// g3 → part
		{Claim: g3Claim1, Quote: "Der Selektor zieht die Kandidaten in zwei Stufen aus dem Korpus.",
			Block: "1", Chunk: 1, Kind: derived.KindFinding},
		// g3 → none
		{Claim: g3Claim1, Quote: "Der Waerter meldet den Abbruch nach der dritten Wiederholung an.",
			Block: "1", Chunk: 1, Kind: derived.KindFinding},
		// G1 — die Adresse gehoert nicht zu diesem Call
		{Claim: g3Claim1, Quote: "Der Retrieval-Pfad faltet vier Arme per Reciprocal Rank Fusion zusammen.",
			Block: "9", Chunk: 1, Kind: derived.KindFinding},
		// G2 — unter der Laengen-Schwelle
		{Claim: g3Claim1, Quote: "zu kurz", Block: "1", Chunk: 1, Kind: derived.KindFinding},
	}

	kept, rejects, g3 := distillGate(batch, shown)
	t.Logf("kept=%d rejects=%v g3=%v", len(kept), rejects, g3)

	if len(kept) != 1 {
		t.Fatalf("kept = %d, want 1", len(kept))
	}
	if rejects["g3"] != 4 || rejects["g1"] != 1 || rejects["g2"] != 1 {
		t.Fatalf("rejects = %v, want g3=4 g1=1 g2=1", rejects)
	}
	if sum := dxSum(g3); sum != rejects["g3"] {
		t.Fatalf("G3-Zerlegung summiert %d, rej_g3 = %d — die Zerlegung ist unvollstaendig: %v",
			sum, rejects["g3"], g3)
	}
	for _, k := range distillG3Keys {
		if g3[k] != 1 {
			t.Errorf("g3[%q] = %d, want 1 — der Stapel traegt jeden Fall genau einmal (%v)",
				k, g3[k], g3)
		}
	}
}

// TestDistillG3EmptyQuoteIsNotEvidence haelt den benannten Sonderfall fest. G2
// macht ihn ueber das Tor unerreichbar (32 Runen Minimum), deshalb steht er hier
// direkt an der Klassifikation: ein leerer Nadel-String liegt unter
// strings.Contains in JEDEM Chunk, und ein Instrument, das daraus "chunk"
// meldet, wuerde eine Adressierung behaupten, die niemand vorgenommen hat.
func TestDistillG3EmptyQuoteIsNotEvidence(t *testing.T) {
	ix := distillNewG3Index(g3Shown())
	for _, quote := range []string{"", "   ", "\n\t "} {
		in := distillInsight{Claim: g3Claim1, Quote: quote, Block: "1", Chunk: 1, Kind: derived.KindFinding}
		if got := ix.classify(in); got != "none" {
			t.Errorf("classify(%q) = %q, want none", quote, got)
		}
	}
}

// TestDistillG3ForeignChunkIsSearchedBeforeItsComposedRun ist der permanente
// Nachzug zu Review-Finding 1 (Sonde RV4): Normalize ist über die
// Naht-Konkatenation NICHT distributiv — NFKC verschmilzt ein kombinierendes
// Zeichen am Anfang eines Chunks mit dem letzten Zeichen seines Vorgängers zu
// einer Rune, die kein einzelner Chunk trug. Ein Zitat, das unter G3s eigener
// Vergleichsform wörtlich in einem Chunk eines FREMDEN Parts steht, muss
// deshalb „part" heißen, auch wenn der zusammengesetzt-normalisierte Lauf es
// versteckt. Vor dem Nachzug buchte die Zerlegung genau diesen Fall als
// „none" — ein Adressierungsfehler wäre als Halluzination gezählt worden, im
// Widerspruch zur Zusage der Migration (150:42-45).
func TestDistillG3ForeignChunkIsSearchedBeforeItsComposedRun(t *testing.T) {
	foreign1 := "### Message 40 — assistant\n\nDer Selektor zieht die Kandidaten in zwei Stufe"
	foreign2 := "́n aus dem Korpus.\n"
	quote := "Der Selektor zieht die Kandidaten in zwei Stufe"

	// Beide Vorbedingungen der Konstruktion werden gemessen statt angenommen:
	// (1) das Zitat steht unter G3s Vergleichsform im einzelnen fremden Chunk,
	// (2) der komponierte Lauf versteckt es — sonst prüfte die Sonde nichts.
	if !strings.Contains(derived.Normalize(foreign1), derived.Normalize(quote)) {
		t.Fatalf("Fixture-Fehler: das Zitat steht nicht im fremden Chunk")
	}
	if strings.Contains(derived.Normalize(foreign1+foreign2), derived.Normalize(quote)) {
		t.Fatalf("Fixture-Fehler: der Lauf versteckt das Zitat nicht — die NFKC-Naht ist wirkungslos")
	}

	shown := distillShown{
		text: map[distillChunkKey]string{
			{block: "1", chunk: 1}: g3P1C1,
			{block: "4", chunk: 1}: foreign1,
			{block: "4", chunk: 2}: foreign2,
		},
		blockIDs: []string{"1", "4"},
	}
	in := distillInsight{
		Claim: g3Claim1, Quote: quote,
		Block: "1", Chunk: 1, Kind: derived.KindFinding,
	}
	fails := distillGateFailures(in, shown)
	if !fails["g3"] || len(fails) != 1 {
		t.Fatalf("die Sonde fällt an %v, want genau {g3}", dxKeys(fails))
	}
	_, rejects, g3 := distillGate([]distillInsight{in}, shown)
	if rejects["g3"] != 1 {
		t.Fatalf("die Sonde trifft G3 nicht: %v", rejects)
	}
	if g3["part"] != 1 {
		t.Fatalf("G3-Zerlegung = %v, want part=1 — der fremde Chunk wurde nur über den "+
			"komponierten Lauf gesucht", g3)
	}
}
