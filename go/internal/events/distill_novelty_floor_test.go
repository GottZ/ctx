// Nachzug zu Review C5-E Finding 1: der Novelty-Floor darf nur auf Claims
// feuern, die der Tokenisierer SIEHT.
//
// util.TokenSet kennt nur [a-z0-9äöüß]. Ein Claim in kyrillischer (oder
// griechischer, CJK-, arabischer) Schrift tokenisiert zur leeren Menge,
// derived.Adequacy antwortet mit dem literalen 0 des Leere-Mengen-Kontrakts,
// und ohne den Guard löschte der Floor Substanz als „wörtliche Kopie", von der
// kein einziges Token im Zitat steht — während alle sieben Alt-Tore passieren
// (distillCoverage splittet auf unicode.IsLetter und sieht volle Deckung).
package events

import (
	"testing"

	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/util"
)

const (
	// nfChunk trägt das kyrillische Zitat wörtlich; das Zitat hat ≥ 32 Runen
	// (G2) und der Claim besteht aus Wörtern des Zitats (G7-Deckung voll).
	nfQuote = "Селектор вытягивает кандидатов из корпуса в два этапа"
	nfClaim = "Селектор вытягивает кандидатов в два этапа"
	nfChunk = "### Message 50 — assistant\n\n" + nfQuote + ".\n"
)

func nfShown() distillShown {
	return distillShown{
		text:     map[distillChunkKey]string{{block: "1", chunk: 1}: nfChunk},
		blockIDs: []string{"1"},
	}
}

// TestDistillNoveltyFloorSkipsClaimsOutsideTheTokenAlphabet pinnt den Guard am
// Prädikat und am ganzen Screen.
func TestDistillNoveltyFloorSkipsClaimsOutsideTheTokenAlphabet(t *testing.T) {
	// Vorbedingungen der Konstruktion, gemessen statt angenommen: die
	// Claim-Token-Menge ist leer, und Adequacy antwortet mit der literalen 0,
	// die den Fehlbuchungs-Pfad überhaupt erst öffnet.
	if n := len(util.TokenSet(nfClaim)); n != 0 {
		t.Fatalf("Fixture-Fehler: der kyrillische Claim tokenisiert zu %d Tokens, want 0", n)
	}
	if _, novelty := derived.Adequacy(nfClaim, nfQuote); novelty != 0 {
		t.Fatalf("Fixture-Fehler: Adequacy-novelty = %v, want die literale 0 der leeren Menge", novelty)
	}

	in := distillInsight{Claim: nfClaim, Quote: nfQuote, Block: "1", Chunk: 1, Kind: derived.KindFinding}

	if distillBelowNoveltyFloor(in, 0.15) {
		t.Fatal("das Prädikat verwirft einen Claim, den der Tokenisierer nicht sieht — " +
			"eine leere Token-Menge ist kein Kopie-Beweis (Review C5-E Finding 1)")
	}
	if key, bad := distillScreen(in, nfShown(), 0.15); bad {
		t.Fatalf("distillScreen = (%q, true), want gehalten — der Claim fällt an einem Tor, "+
			"obwohl Material und Deckung stehen", key)
	}

	// Gegenprobe: ein lateinischer Claim, den der Tokenisierer sieht und der
	// eine wörtliche Kopie seines Zitats ist, fällt am selben Floor weiter —
	// der Guard öffnet kein Loch für sichtbare Kopien.
	latQuote := "Der Selektor zieht die Kandidaten in zwei Stufen aus dem Korpus."
	lat := distillInsight{Claim: latQuote, Quote: latQuote, Block: "1", Chunk: 1, Kind: derived.KindFinding}
	if !distillBelowNoveltyFloor(lat, 0.15) {
		t.Fatal("die sichtbare wörtliche Kopie passiert den Floor — der Guard ist zu breit")
	}
}
