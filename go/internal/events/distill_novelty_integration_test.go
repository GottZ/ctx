//go:build integration

// Gate C5-E (Entscheid C5-1/C5-2, Checkpoint #5): der PER-CLAIM-NOVELTY-FLOOR
// im Schreibpfad, erzeugt und abgelesen ueber den PRODUKTIONS-Weg.
//
// WOFUER DIE SONDE STEHT. Bis zu dieser Welle war novelty eine reine
// Report-Groesse (derived.Adequacy, derived.Report seit C5-A): die Mess-Welle
// C5-A-M hat auf dem root-Stand p10 = 0,0385 und 5,85 % der veroeffentlichten
// Claims bei novelty exakt 0 gemessen — woertliche Zitat-Kopien, die JEDES der
// sieben Tore passieren, weil jede einzelne von ihnen eine perfekt verankerte
// Zitation ist (adequacy.go: "G0-G7 cannot catch that"). Der Floor ist das
// achte Tor, das genau diese Klasse haelt.
//
// ROT-FORM gegen den Baum vor dieser Welle, beide Haelften ueber diesen Pfad:
//
//  1. VERHALTEN — die woertliche Kopie (nvCopy, novelty exakt 0) passiert alle
//     Tore und steht am Ende IM GESCHRIEBENEN BLOCK. Sonde 1 verlangt das
//     Gegenteil und faellt deshalb mit "insights_kept = 4, want 2".
//  2. BEOBACHTBARKEIT — nvRead liest rej_novelty und antwortet mit SQLSTATE
//     42703 (undefined_column), dem Rot-Muster von 149/150: der Verwurfsgrund
//     ist im Betrieb nicht zaehlbar, weil die Spalte fehlt.
//
// DIE FIXTURE RECHNET NICHT SELBST. Die vier novelty-Werte sind exakt
// {0, 1/10, 1/4, 1} — nachgeprueft in TestDistillNoveltyFixtureIsHonest gegen
// derived.Adequacy, also gegen dieselbe Funktion, die das Tor benutzt. Faellt
// diese Kontrolle, misst alles darunter eine erfundene Verteilung.
//
//	go test -tags=integration ./internal/events/ -run TestDistillNovelty -count=1 -v
package events

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/distillsource"
	"github.com/GottZ/ctx/internal/promptguard"
	"github.com/GottZ/ctx/internal/testdb"
)

// Der eine Chunk des Calls. Sein zweiter Satz traegt das Vokabular, aus dem die
// EIGENEN Worte der Claims kommen: novelty misst Claim gegen ZITAT, die
// G7-Deckung misst Claim gegen CHUNK — ein Claim aus Chunk-Woertern ausserhalb
// des Zitats hat deshalb hohe novelty UND volle Deckung, und genau das ist die
// Lage, in der der Floor etwas anderes entscheidet als G7.
const nvBody = "### Message 12 — user\n\n" +
	"Die Migration 147 hat einen deterministischen Tiebreak in die FTS-Arme eingebaut. " +
	"Der Selektor bewertet jeden Treffer nach seinem Rang und bleibt bei wachsendem " +
	"Korpus stabil. Die Auswertung laeuft ueber das Goldset und wird bei jeder " +
	"Aenderung des Selektors vollstaendig wiederholt.\n"

// Das Zitat aller Antwortzeilen: woertlich aus nvBody, ueber MinQuoteRunes (32).
// Es ist fuer JEDE Zeile dasselbe — die Zeilen unterscheiden sich einzig darin,
// wie viel eigenes Wort ihr Claim gegenueber diesem Zitat traegt.
const nvQuote = "Die Migration 147 hat einen deterministischen Tiebreak in die FTS-Arme eingebaut."

// Die vier Claims und ihre novelty. Die Token-Menge ist util.TokenSet
// (lowercase [a-z0-9äöüß]+, eine MENGE), novelty ist der Anteil der
// Claim-Tokens, die NICHT im Zitat stehen:
//
//	nvCopy 0/8   = 0,00   woertliche Kopie, kuerzer gesetzt
//	nvLow  1/10  = 0,10   ein eigenes Wort ("im")
//	nvAt   2/8   = 0,25   zwei eigene ("im", "selektor") — der Grenzfall
//	nvOwn  8/8   = 1,00   ganz aus dem zweiten Satz des Chunks
//
// Alle vier tragen volle G7-Deckung (jedes Inhaltswort ≥ 4 Runen steht im
// Chunk) und dasselbe Zitat, passieren also ohne Floor alle sieben Tore.
const (
	nvCopy = "Die Migration 147 hat einen deterministischen Tiebreak eingebaut."
	nvLow  = "Die Migration 147 hat einen deterministischen Tiebreak im FTS eingebaut."
	nvAt   = "Die Migration hat einen Tiebreak im Selektor eingebaut."
	nvOwn  = "Der Selektor bewertet jeden Treffer nach seinem Rang."
)

// nvImperative faellt an G7 (Imperativ-Negativliste "ab sofort") UND laege mit
// novelty 2/10 = 0,20 unter dem Sonden-Floor 0,25. Es ist die Praezedenz-Probe:
// der Floor ist das LETZTE Tor, also muss diese Zeile g7 buchen und rej_novelty
// unberuehrt lassen.
const nvImperative = "Ab sofort hat die Migration 147 einen deterministischen Tiebreak eingebaut."

// nvFloor ist der Floor der Kern-Sonden. 0,25 statt des Registry-Defaults 0,15,
// weil nvAt darauf EXAKT liegt: 2/8 ist in float64 exakt darstellbar, der
// Vergleich am Grenzwert ist damit eine Aussage ueber "<" gegen "<=" und keine
// ueber Rundung. Der Default 0,15 hat seine eigene Sonde (Sonde 4).
const nvFloor = 0.25

// nvSource haendigt genau EINEN Chunk aus. Mehr braucht diese Welle nicht: der
// Floor entscheidet je Claim, nicht je Adresse, und ein zweiter Chunk wuerde nur
// die G3-Achse ins Bild holen.
func nvSource() *fakeDistillSource {
	served := false
	return &fakeDistillSource{
		sessions: []distillsource.Ref{{Session: dfRoot, Watermark: 100}},
		head:     map[string]int64{dfRoot: 100},
		hasNew:   map[string]bool{dfRoot: true},
		readFn: func(after int64) (distillsource.Batch, error) {
			if served {
				return distillsource.Batch{Watermark: after, Complete: true}, nil
			}
			served = true
			return distillsource.Batch{
				Items: []distillsource.Item{{
					Text: nvBody,
					Attrs: []promptguard.Attr{
						{Name: "block", Value: a8Block1},
						{Name: "chunk", Value: "1"},
					},
					Origin:      distillsource.Origin{BlockID: a8Block1, ChunkIndex: 1, Role: "user"},
					Sensitivity: backends.SensCredentials,
					Untrusted:   true,
				}},
				Watermark: 100,
				Complete:  true,
			}, nil
		},
	}
}

// nvConfig ist a8Config mit einem gesetzten Floor. Der Wert steht EXPLIZIT, weil
// dfConfig eine handgebaute Struktur ist: die Registry-Defaults erreichen sie
// nicht, und der Go-Nullwert 0 ist hier der Aus-Schalter (a8Config bleibt
// deshalb bei Floor aus — siehe den Kommentar dort).
func nvConfig(floor float64) *config.Config {
	c := a8Config()
	c.Distill.NoveltyFloor = floor
	return c
}

// nvAnswer antwortet mit den vier Claims an DERSELBEN Adresse und mit
// DEMSELBEN Zitat, aus dem gerenderten Prompt gehoben.
func nvAnswer(req a8Request) (string, int) {
	return nvAnswerWith(req, nvCopy, nvLow, nvAt, nvOwn)
}

// nvAnswerWith baut die Antwort aus beliebigen Claims — eine Zeile je Claim,
// alle auf die erste Adresse des Prompts, alle mit nvQuote.
func nvAnswerWith(req a8Request, claims ...string) (string, int) {
	addrs := a8Addrs(req.User)
	if len(addrs) == 0 {
		return a8Answer(), http.StatusOK
	}
	a := addrs[0]
	quote := nvQuoteFrom(req.User, a)
	lines := make([]map[string]any, 0, len(claims))
	for _, claim := range claims {
		lines = append(lines, map[string]any{
			"claim": claim, "quote": quote,
			"block": a.block, "chunk": a.chunk, "kind": "finding",
		})
	}
	return a8Answer(lines...), http.StatusOK
}

// nvQuoteFrom hebt nvQuote aus dem gerenderten Prompt — und gibt "" zurueck,
// wenn es dort nicht steht. Der Stub darf nichts behaupten, was der Arm ihm
// nicht gezeigt hat (round-2 blocker #2); die Fixture-Kontrolle unten macht das
// Fehlen sichtbar, statt es in einem gruenen Lauf verschwinden zu lassen.
func nvQuoteFrom(prompt string, addr a8Addr) string {
	head := `block="` + addr.block + `" chunk="` + strconv.Itoa(addr.chunk) + `"`
	start := strings.Index(prompt, head)
	if start < 0 {
		return ""
	}
	body := prompt[start:]
	if i := strings.Index(body, ">\n"); i >= 0 {
		body = body[i+2:]
	}
	if i := strings.Index(body, "</untrusted_block"); i >= 0 {
		body = body[:i]
	}
	if !strings.Contains(body, nvQuote) {
		return ""
	}
	return nvQuote
}

// nvJournal ist die Ablesung dieser Welle: das Histogramm in seiner NEUEN,
// neunstelligen Form.
type nvJournal struct {
	calls    int
	kept     int
	rejected int
	rej      map[string]int
}

// nvRead liest die juengste Lauf-Zeile.
//
// ROT-FORM: gegen den Baum vor dieser Welle antwortet genau dieses Statement mit
// SQLSTATE 42703 (undefined_column) — rej_novelty gibt es nicht, und das ist der
// Beleg, dass der Verwurfsgrund im Betrieb nicht zaehlbar war.
func nvRead(t *testing.T, pool *pgxpool.Pool, key string) nvJournal {
	t.Helper()
	var j nvJournal
	var g1, g2, g3, g4, g5, g6, g7, schema, novelty int
	if err := pool.QueryRow(context.Background(), `
		SELECT calls, insights_kept, insights_rejected,
		       rej_g1, rej_g2, rej_g3, rej_g4, rej_g5, rej_g6, rej_g7, rej_schema,
		       rej_novelty
		  FROM distill_run WHERE source_key = $1 ORDER BY started_at DESC LIMIT 1`, key).
		Scan(&j.calls, &j.kept, &j.rejected,
			&g1, &g2, &g3, &g4, &g5, &g6, &g7, &schema, &novelty); err != nil {
		t.Fatalf("read the run row: %v", err)
	}
	j.rej = map[string]int{
		"g1": g1, "g2": g2, "g3": g3, "g4": g4, "g5": g5, "g6": g6, "g7": g7,
		"schema": schema, "novelty": novelty,
	}
	return j
}

// nvRun faehrt EINEN Tick mit dem gegebenen Floor und der gegebenen Antwort und
// liefert die Journal-Zeile.
func nvRun(t *testing.T, pool *pgxpool.Pool, floor float64,
	answer func(a8Request) (string, int),
) nvJournal {
	t.Helper()
	n6Reset(t, pool)
	stub := a8NewStub(t, answer)
	s := a8Scheduler(pool, nvConfig(floor), nvSource(), a8Pool(stub.srv.URL))
	s.distillOnce(context.Background(), dfNoDemand)
	return nvRead(t, pool, distillSourceKey(dfLabel, dfScope, dfRoot))
}

func TestDistillNoveltyFloor(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	// SONDE 1 — der Kern. Zwei der vier Zeilen fallen am Floor, und das Journal
	// benennt den Grund. Das ist die Zeile, die gegen den Baum vor dieser Welle
	// rot steht: dort passieren alle vier, und die Kopie steht am Ende im Block.
	t.Run("die Kopie faellt am Floor und wird gezaehlt", func(t *testing.T) {
		j := nvRun(t, pool, nvFloor, nvAnswer)
		t.Logf("calls=%d kept=%d rejected=%d rej=%v", j.calls, j.kept, j.rejected, j.rej)

		if j.calls != 1 {
			t.Fatalf("calls = %d, want 1 — die Sonde misst sonst nicht den einen Call", j.calls)
		}
		if j.kept != 2 {
			t.Fatalf("insights_kept = %d, want 2 — nvCopy (novelty 0) und nvLow (0,10) muessen "+
				"am Floor %.2f fallen, nvAt (0,25) und nvOwn (1,00) bleiben", j.kept, nvFloor)
		}
		if j.rej["novelty"] != 2 {
			t.Fatalf("rej_novelty = %d, want 2 — der Verwurfsgrund ist nicht zaehlbar (%v)",
				j.rej["novelty"], j.rej)
		}
		// Und die sieben Tore bleiben unberuehrt: der Floor nimmt keinem von
		// ihnen Material weg, er kommt nach ihnen.
		for _, k := range []string{"g1", "g2", "g3", "g4", "g5", "g6", "g7", "schema"} {
			if j.rej[k] != 0 {
				t.Errorf("rej_%s = %d, want 0 — die Fixture faellt nur am Floor (%v)", k, j.rej[k], j.rej)
			}
		}

		// Die HARTE Aussage der Welle: die Kopie steht nicht in der Ebene.
		blocks := a9Blocks(t, pool)
		if len(blocks) != 1 {
			t.Fatalf("geschriebene Bloecke = %d, want 1", len(blocks))
		}
		body := blocks[0].content
		if strings.Contains(body, nvCopy) {
			t.Errorf("der Block traegt die woertliche Kopie — der Floor haelt sie nicht auf")
		}
		if strings.Contains(body, nvLow) {
			t.Errorf("der Block traegt den Claim unter dem Floor (novelty 0,10)")
		}
		if !strings.Contains(body, nvOwn) {
			t.Errorf("der Block traegt den eigenen Claim (novelty 1,00) NICHT — der Floor "+
				"verwirft zu viel:\n%s", body)
		}
		if !strings.Contains(body, nvAt) {
			t.Errorf("der Block traegt den Grenzfall (novelty = Floor) NICHT — die Grenze ist "+
				"nicht strikt:\n%s", body)
		}
	})

	// SONDE 2 — der Aus-Schalter. Floor 0 laesst jede Zeile durch, auch die
	// Kopie. Ohne diese Sonde waere "0 = aus" eine Behauptung im Kommentar.
	t.Run("Floor 0 ist aus", func(t *testing.T) {
		j := nvRun(t, pool, 0, nvAnswer)
		t.Logf("kept=%d rejected=%d rej=%v", j.kept, j.rejected, j.rej)
		if j.kept != 4 {
			t.Fatalf("insights_kept = %d, want 4 — bei Floor 0 darf keine Zeile am Floor fallen", j.kept)
		}
		if j.rej["novelty"] != 0 {
			t.Fatalf("rej_novelty = %d, want 0 — der ausgeschaltete Floor zaehlt", j.rej["novelty"])
		}
		if j.rejected != 0 {
			t.Fatalf("insights_rejected = %d, want 0 — ohne Floor passieren alle vier Zeilen "+
				"die sieben Tore, und genau das ist der rote Zustand dieser Welle", j.rejected)
		}
	})

	// SONDE 3 — die Grenze ist STRIKT. Derselbe Claim, ein um 0,01 hoeherer
	// Floor: nvAt faellt jetzt. Ohne diese Gegenprobe waere "novelty = Floor
	// bleibt" auch dann gruen, wenn der Grenzfall aus einem anderen Grund nie
	// verworfen wuerde.
	t.Run("novelty exakt am Floor bleibt, knapp darunter faellt", func(t *testing.T) {
		j := nvRun(t, pool, math.Nextafter(nvFloor, 1), nvAnswer)
		t.Logf("kept=%d rej=%v", j.kept, j.rej)
		if j.kept != 1 {
			t.Fatalf("insights_kept = %d, want 1 — bei einem Floor knapp UEBER 0,25 faellt auch "+
				"der Grenzfall, nur nvOwn (1,00) bleibt", j.kept)
		}
		if j.rej["novelty"] != 3 {
			t.Fatalf("rej_novelty = %d, want 3", j.rej["novelty"])
		}
	})

	// SONDE 4 — der Registry-Default wirkt. 0,15 ist der Wert, den
	// derived.GoodhartMinNovelty als Report-Grenze traegt; der Floor misst
	// dieselbe Grenze, und diese Sonde bindet die beiden Zahlen im
	// Schreibpfad aneinander statt nur in einem Kommentar.
	t.Run("der Default 0,15 haelt Kopie und Schwanz", func(t *testing.T) {
		j := nvRun(t, pool, derived.GoodhartMinNovelty, nvAnswer)
		t.Logf("kept=%d rej=%v", j.kept, j.rej)
		if j.kept != 2 || j.rej["novelty"] != 2 {
			t.Fatalf("kept=%d rej_novelty=%d, want 2/2 — bei %.2f fallen novelty 0 und 0,10",
				j.kept, j.rej["novelty"], derived.GoodhartMinNovelty)
		}
	})

	// SONDE 5 — die Praezedenz. Eine Zeile, die G7 UND den Floor verletzt, bucht
	// g7: der Floor ist das letzte Tor. Die Reihenfolge ist keine Kosmetik — sie
	// haelt die g1..g7-Reihe der frueheren Mess-Wellen vergleichbar, weil kein
	// Verwurf aus einem bestehenden Eimer in den neuen wandert.
	t.Run("ein Verwurf an G7 bucht g7, nicht novelty", func(t *testing.T) {
		j := nvRun(t, pool, nvFloor, func(req a8Request) (string, int) {
			return nvAnswerWith(req, nvImperative, nvOwn)
		})
		t.Logf("kept=%d rejected=%d rej=%v", j.kept, j.rejected, j.rej)
		if j.rej["g7"] != 1 {
			t.Errorf("rej_g7 = %d, want 1 — die Imperativ-Zeile faellt nicht an G7 (%v)",
				j.rej["g7"], j.rej)
		}
		if j.rej["novelty"] != 0 {
			t.Errorf("rej_novelty = %d, want 0 — der Floor hat einem frueheren Tor Material "+
				"weggenommen (%v)", j.rej["novelty"], j.rej)
		}
		if j.kept != 1 {
			t.Errorf("insights_kept = %d, want 1", j.kept)
		}
	})

	// SONDE 6 — die Gleichung, in ihrer NEUEN Form, und die alte als Gegenprobe.
	// Der Lauf mischt einen Tor-Verwurf (G2, Zitat unter 32 Runen) mit zwei
	// Floor-Verwuerfen, damit beide Summanden echt sind.
	t.Run("die erweiterte Invariante haelt und braucht ihren neuen Summanden", func(t *testing.T) {
		j := nvRun(t, pool, nvFloor, func(req a8Request) (string, int) {
			addrs := a8Addrs(req.User)
			if len(addrs) == 0 {
				return a8Answer(), http.StatusOK
			}
			a := addrs[0]
			answer, status := nvAnswerWith(req, nvCopy, nvLow, nvOwn)
			if status != http.StatusOK {
				return answer, status
			}
			// Eine fuenfte Zeile mit zu kurzem Zitat — sie faellt an G2.
			var parsed struct {
				Insights []map[string]any `json:"insights"`
			}
			if err := json.Unmarshal([]byte(answer), &parsed); err != nil {
				return answer, status
			}
			parsed.Insights = append(parsed.Insights, map[string]any{
				"claim": nvOwn, "quote": "zu kurz",
				"block": a.block, "chunk": a.chunk, "kind": "finding",
			})
			return a8Answer(parsed.Insights...), http.StatusOK
		})
		t.Logf("kept=%d rejected=%d rej=%v", j.kept, j.rejected, j.rej)

		if j.rejected == 0 {
			t.Fatal("insights_rejected = 0 — die Sonde hat nichts verworfen und misst nichts")
		}
		if j.rej["novelty"] == 0 {
			t.Fatal("rej_novelty = 0 — ohne Floor-Verwurf belegt die Gleichung nichts Neues")
		}
		if got := n6Sum(j.rej); got != j.rejected {
			t.Fatalf("Histogramm-Summe %d != insights_rejected %d — die Zerlegung ist "+
				"unvollstaendig: %v", got, j.rejected, j.rej)
		}
		// Und die ALTE Form ist ab dieser Welle nachweislich falsch: ohne den
		// neunten Summanden fehlt der Gleichung genau die Zahl, die der Floor
		// beisteuert. Das ist die Falsifikation der Erweiterung an Ort und
		// Stelle — sie faellt rot, wenn jemand rej_novelty aus der Summe nimmt.
		old := 0
		for _, k := range []string{"g1", "g2", "g3", "g4", "g5", "g6", "g7", "schema"} {
			old += j.rej[k]
		}
		if old == j.rejected {
			t.Fatalf("die alte, achtstellige Summe %d trifft insights_rejected %d immer noch — "+
				"dann zaehlt rej_novelty nicht mit, oder die Sonde hat keinen Floor-Verwurf: %v",
				old, j.rejected, j.rej)
		}
		if old+j.rej["novelty"] != j.rejected {
			t.Fatalf("alte Summe %d + rej_novelty %d != insights_rejected %d: %v",
				old, j.rej["novelty"], j.rejected, j.rej)
		}
	})
}

// TestDistillNoveltyFixtureIsHonest ist die Fixture-Kontrolle: die vier
// novelty-Werte, die alle Sonden oben voraussetzen, sind mit der
// PRODUKTIONS-Funktion nachgerechnet, und das Zitat steht woertlich im
// gerenderten Prompt. Faellt diese Sonde, messen die Sonden oben eine erfundene
// Verteilung statt des Tores.
func TestDistillNoveltyFixtureIsHonest(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	for _, tc := range []struct {
		name  string
		claim string
		want  float64
	}{
		{"nvCopy", nvCopy, 0},
		{"nvLow", nvLow, 1.0 / 10.0},
		{"nvAt", nvAt, 1.0 / 4.0},
		{"nvOwn", nvOwn, 1},
		{"nvImperative", nvImperative, 2.0 / 10.0},
	} {
		_, got := derived.Adequacy(tc.claim, nvQuote)
		if got != tc.want {
			t.Errorf("novelty(%s) = %.6f, want %.6f — die Fixture rechnet anders als das Tor",
				tc.name, got, tc.want)
		}
	}
	// nvAt liegt EXAKT auf dem Floor der Kern-Sonden — die Grenzwert-Aussage
	// haengt daran, dass hier nichts gerundet wird.
	if _, n := derived.Adequacy(nvAt, nvQuote); n != nvFloor {
		t.Errorf("novelty(nvAt) = %.17g, nvFloor = %.17g — der Grenzfall liegt nicht auf der Grenze",
			n, nvFloor)
	}

	n6Reset(t, pool)
	stub := a8NewStub(t, nvAnswer)
	s := a8Scheduler(pool, nvConfig(nvFloor), nvSource(), a8Pool(stub.srv.URL))
	s.distillOnce(context.Background(), dfNoDemand)

	seen := stub.seen()
	if len(seen) != 1 {
		t.Fatalf("der Stub sah %d Prompts, want 1", len(seen))
	}
	prompt := seen[0].User
	if addrs := a8Addrs(prompt); len(addrs) != 1 {
		t.Fatalf("der Prompt zeigt %d Adressen, want 1: %v", len(addrs), addrs)
	}
	if !strings.Contains(prompt, nvQuote) {
		t.Fatalf("das Zitat der Fixture steht nicht im gerenderten Prompt — der Stub behauptet " +
			"Material, das der Arm nie gezeigt hat")
	}
	// Und die eigenen Worte der hohen Claims stehen im CHUNK, nicht im Zitat:
	// nur so trennt die Fixture den Floor von der G7-Deckung.
	for _, w := range []string{"Selektor", "bewertet", "Treffer", "Rang"} {
		if !strings.Contains(prompt, w) {
			t.Errorf("%q steht nicht im gezeigten Material — nvOwn haette keine G7-Deckung", w)
		}
		if strings.Contains(nvQuote, w) {
			t.Errorf("%q steht im Zitat — dann misst nvOwn keine novelty", w)
		}
	}
}
