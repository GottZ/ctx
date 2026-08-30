//go:build integration

// Gate C5-A (Entscheid C5-3, Checkpoint #5): die G3-Zerlegung im JOURNAL,
// erzeugt ueber den PRODUKTIONS-SCHREIBPFAD.
//
// WARUM DIESE SONDE NEBEN DER UNIT-SONDE STEHT. distill_g3class_test.go zeigt,
// dass die Klassifikation die vier Faelle trennt; sie sagt nichts darueber, ob
// die Zahlen den Lauf ueberleben. Der Massstab der Welle ist aber, dass ein
// Mess-Lauf auf der Mess-Kopie die Anteile JE LAUF zaehlbar liefert — und das
// heisst: durch distillGate, in den Batch-Ledger, per UPDATE in die Lauf-Zeile,
// per SQL wieder heraus. Genau diese Strecke laeuft hier, mit einem Stub
// anstelle des Modells und Produktionscode auf jedem anderen Meter.
//
// Die Einsichten werden NICHT von Hand in die Tabelle gesetzt: der Stub liest
// die Adressen aus dem gerenderten Prompt und antwortet mit Zitaten, die er aus
// demselben Prompt hebt. Er kann damit nur das behaupten, was der Arm ihm
// wirklich gezeigt hat.
//
// ROT-FORM: gegen den Baum vor dieser Welle antwortet g3Read mit SQLSTATE 42703
// (undefined_column) — die vier Spalten gibt es nicht, und das ist der Beleg,
// dass die Zerlegung im Betrieb nicht ablesbar war.
//
//	go test -tags=integration ./internal/events/ -run TestDistillG3Journal -count=1 -v
package events

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/distillsource"
	"github.com/GottZ/ctx/internal/promptguard"
	"github.com/GottZ/ctx/internal/testdb"
)

// Die drei Chunks des Calls. Jeder liegt ueber MinRowRunes (200), und die drei
// Texte teilen keinen Satz — sonst waere nicht entscheidbar, aus welchem Chunk
// ein Zitat stammt, und die Sonde wuerde die Klassifikation nicht pruefen,
// sondern raten.
const (
	// Part 1, Chunk 1 — der ADRESSIERTE Chunk aller fuenf Antwortzeilen.
	g3jC1 = "### Message 12 — user\n\n" +
		"Die Migration 147 hat einen deterministischen Tiebreak in die FTS-Arme eingebaut. " +
		"Der Retrieval-Pfad faltet vier Arme per Reciprocal Rank Fusion zusammen und bewertet " +
		"jeden Treffer nach seinem Rang statt nach seinem Score. Das Verfahren bleibt bei " +
		"wachsendem Korpus stabil.\n"

	// Part 1, Chunk 2 — der NACHBAR. Aufeinanderfolgender Index, also im
	// Material eine Naht und keine Luecke.
	g3jC2 = "### Message 13 — assistant\n\n" +
		"Das Damping des Insight-Typs steht seit Migration 146 auf 0,60 und wird beim Start " +
		"aus der Konfiguration gelesen. Die Auswertung laeuft ueber das Goldset und wird bei " +
		"jeder Aenderung des Selektors vollstaendig wiederholt.\n"

	// Part 2, Chunk 1 — das FREMDE Part desselben Calls.
	g3jC3 = "### Message 20 — user\n\n" +
		"Der Selektor zieht die Kandidaten in zwei Stufen aus dem Korpus und legt die Auswahl " +
		"im Journal ab. Die zweite Stufe prueft die Laenge jeder Zeile gegen die " +
		"Substanz-Schwelle und verwirft alles darunter.\n"

	// g3jClaim nennt nur Inhaltswoerter aus g3jC1, dem adressierten Chunk. G7
	// hat damit volle Deckung und maskiert die Zitat-Pruefung nicht.
	g3jClaim = "Die Migration 147 hat einen deterministischen Tiebreak eingebaut."

	// g3jInvented steht in keinem der drei Chunks.
	g3jInvented = "Der Waerter meldet den Abbruch nach der dritten Wiederholung an den Scheduler."
)

// g3jSource haendigt einen Batch mit drei Chunks aus: zwei aufeinanderfolgende
// eines Parts und einen eines zweiten. a8Source kann das nicht — es gibt genau
// einen Chunk je Block, und dann sind "Nachbar-Chunk" und "Naht" im Call gar
// nicht darstellbar.
func g3jSource() *fakeDistillSource {
	served := false
	type chunk struct {
		id   string
		idx  int
		text string
	}
	chunks := []chunk{
		{a8Block1, 1, g3jC1},
		{a8Block1, 2, g3jC2},
		{a8Block2, 1, g3jC3},
	}
	return &fakeDistillSource{
		sessions: []distillsource.Ref{{Session: dfRoot, Watermark: 100}},
		head:     map[string]int64{dfRoot: 100},
		hasNew:   map[string]bool{dfRoot: true},
		readFn: func(after int64) (distillsource.Batch, error) {
			if served {
				return distillsource.Batch{Watermark: after, Complete: true}, nil
			}
			served = true
			items := make([]distillsource.Item, 0, len(chunks))
			for _, c := range chunks {
				items = append(items, distillsource.Item{
					Text: c.text,
					Attrs: []promptguard.Attr{
						{Name: "block", Value: c.id},
						{Name: "chunk", Value: strconv.Itoa(c.idx)},
					},
					Origin:      distillsource.Origin{BlockID: c.id, ChunkIndex: c.idx, Role: "user"},
					Sensitivity: backends.SensCredentials,
					Untrusted:   true,
				})
			}
			return distillsource.Batch{Items: items, Watermark: 100, Complete: true}, nil
		},
	}
}

// g3jBody hebt den ROHEN Payload eines Markers aus dem gerenderten Prompt —
// exakt das, was promptguard.Wrap zwischen ">\n" und "\n</untrusted_block"
// gesetzt hat, also byte-identisch mit dem, was der Arm in shown.text fuehrt.
func g3jBody(prompt string, addr a8Addr) string {
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
	return strings.TrimSpace(body)
}

// g3jEdge baut das NAHT-Zitat: das Ende des einen Chunks, unmittelbar gefolgt
// vom Anfang des naechsten. Es steht in keinem der beiden Chunks fuer sich und
// trotzdem woertlich im Material — genau der Fall, den ein Zwei-Wege-Schnitt
// als Halluzination lesen wuerde.
func g3jEdge(prompt string, a, b a8Addr) string {
	left, right := []rune(g3jBody(prompt, a)), []rune(g3jBody(prompt, b))
	if len(left) < 40 || len(right) < 40 {
		return ""
	}
	return string(left[len(left)-40:]) + "\n" + string(right[:40])
}

// g3jAnswer ist die Antwort des Stubs: eine Zeile, die das Tor passiert, und
// vier, die an G3 fallen — je eine pro Eimer.
func g3jAnswer(req a8Request) (string, int) {
	addrs := a8Addrs(req.User)
	if len(addrs) < 3 {
		return a8Answer(), http.StatusOK
	}
	c1, c2, c3 := addrs[0], addrs[1], addrs[2]
	line := func(quote string) map[string]any {
		// IMMER auf c1 adressiert: die Zeilen unterscheiden sich einzig darin,
		// WO ihr Zitat wirklich steht.
		return map[string]any{
			"claim": g3jClaim, "quote": quote,
			"block": c1.block, "chunk": c1.chunk, "kind": "finding",
		}
	}
	body1 := g3jBody(req.User, c1)
	return a8Answer(
		// kept — das Zitat steht im adressierten Chunk.
		line(firstSentence(body1)),
		// g3 → chunk — Nachbar-Chunk desselben Parts.
		line(firstSentence(g3jBody(req.User, c2))),
		// g3 → span — ueber die Naht zwischen Chunk 1 und Chunk 2.
		line(g3jEdge(req.User, c1, c2)),
		// g3 → part — fremdes Part desselben Calls.
		line(firstSentence(g3jBody(req.User, c3))),
		// g3 → none — nirgends.
		line(g3jInvented),
	), http.StatusOK
}

// firstSentence liefert den ersten Satz NACH der Message-Ueberschrift — lang
// genug fuer G2 (32 Runen) und woertlich aus dem Prompt.
func firstSentence(body string) string {
	if i := strings.Index(body, "\n\n"); i >= 0 {
		body = body[i+2:]
	}
	if i := strings.Index(body, ". "); i >= 0 {
		return body[:i+1]
	}
	return strings.TrimSpace(body)
}

// g3Journal ist die Ablesung dieser Welle. g1 und g3 stehen bewusst in
// DERSELBEN Abfrage wie die vier neuen Spalten (Review-Finding 6): die Frage
// "hat sich die Adressierung bewegt" ist nur beantwortbar, wenn das
// Adress-Tor (g1) und die Zitat-Pruefung (g3) nebeneinander ablesbar sind.
type g3Journal struct {
	calls    int
	kept     int
	rejected int
	rej      map[string]int
	g3       map[string]int
}

func g3Read(t *testing.T, pool *pgxpool.Pool, key string) g3Journal {
	t.Helper()
	var j g3Journal
	var g1, g2, g3, g4, g5, g6, g7, schema int
	var chunk, span, part, none int
	if err := pool.QueryRow(context.Background(), `
		SELECT calls, insights_kept, insights_rejected,
		       rej_g1, rej_g2, rej_g3, rej_g4, rej_g5, rej_g6, rej_g7, rej_schema,
		       rej_g3_chunk, rej_g3_span, rej_g3_part, rej_g3_none
		  FROM distill_run WHERE source_key = $1 ORDER BY started_at DESC LIMIT 1`, key).
		Scan(&j.calls, &j.kept, &j.rejected,
			&g1, &g2, &g3, &g4, &g5, &g6, &g7, &schema,
			&chunk, &span, &part, &none); err != nil {
		t.Fatalf("read the run row: %v", err)
	}
	j.rej = map[string]int{
		"g1": g1, "g2": g2, "g3": g3, "g4": g4,
		"g5": g5, "g6": g6, "g7": g7, "schema": schema,
	}
	j.g3 = map[string]int{"chunk": chunk, "span": span, "part": part, "none": none}
	return j
}

func TestDistillG3JournalDecomposition(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	key := distillSourceKey(dfLabel, dfScope, dfRoot)

	// SONDE 1 — die Zerlegung ueberlebt den Lauf. Fuenf angebotene Zeilen, vier
	// verschiedene G3-Ausgaenge, und das Journal benennt sie einzeln.
	t.Run("das Journal zerlegt rej_g3 nach dem Ort des Zitats", func(t *testing.T) {
		n6Reset(t, pool)
		stub := a8NewStub(t, g3jAnswer)
		s := a8Scheduler(pool, a8Config(), g3jSource(), a8Pool(stub.srv.URL))
		s.distillOnce(ctx, dfNoDemand)

		j := g3Read(t, pool, key)
		t.Logf("calls=%d kept=%d rejected=%d rej=%v g3=%v", j.calls, j.kept, j.rejected, j.rej, j.g3)

		if j.calls != 1 {
			t.Fatalf("calls = %d, want 1 — die Sonde misst sonst nicht den einen Call", j.calls)
		}
		if j.kept != 1 {
			t.Fatalf("insights_kept = %d, want 1 — die gute Zeile ist nicht durchgekommen", j.kept)
		}
		if j.rej["g3"] != 4 {
			t.Fatalf("rej_g3 = %d, want 4 (rej=%v) — die vier Zeilen fallen nicht alle an G3",
				j.rej["g3"], j.rej)
		}
		for _, k := range []string{"chunk", "span", "part", "none"} {
			if j.g3[k] != 1 {
				t.Errorf("rej_g3_%s = %d, want 1 — der Fall ist im Journal nicht zaehlbar (%v)",
					k, j.g3[k], j.g3)
			}
		}
	})

	// SONDE 2 — die Vollstaendigkeit, beide Ebenen. Ohne sie waere die
	// Zerlegung eine Auswahl: ein Ort, den niemand zaehlt, saehe im Betrieb aus
	// wie ein Ort, den es nicht gibt.
	t.Run("beide Gleichungen halten", func(t *testing.T) {
		n6Reset(t, pool)
		stub := a8NewStub(t, g3jAnswer)
		s := a8Scheduler(pool, a8Config(), g3jSource(), a8Pool(stub.srv.URL))
		s.distillOnce(ctx, dfNoDemand)

		j := g3Read(t, pool, key)
		if j.rejected == 0 {
			t.Fatal("insights_rejected = 0 — die Sonde hat nichts verworfen und misst nichts")
		}
		if got := n6Sum(j.rej); got != j.rejected {
			t.Fatalf("Histogramm-Summe %d != insights_rejected %d — die erste Zerlegung ist "+
				"unvollstaendig: %v", got, j.rejected, j.rej)
		}
		if got := n6Sum(j.g3); got != j.rej["g3"] {
			t.Fatalf("G3-Zerlegung %d != rej_g3 %d — die zweite Zerlegung ist unvollstaendig: %v",
				got, j.rej["g3"], j.g3)
		}
	})

	// SONDE 3 — g1 UND g3 in derselben Ablesung (Review-Finding 6). Der Befund
	// der naechsten Iteration lautet "hat die chunk-Bindung im Prompt die
	// Adressierung bewegt", und die Frage ist nur beantwortbar, wenn das
	// Adress-Tor neben der Zitat-Pruefung steht. Hier steht g1 auf 0, weil jede
	// Zeile eine Adresse DIESES Calls nennt — die Fehler liegen im Zitat.
	t.Run("g1 und g3 stehen in derselben Zeile", func(t *testing.T) {
		n6Reset(t, pool)
		stub := a8NewStub(t, g3jAnswer)
		s := a8Scheduler(pool, a8Config(), g3jSource(), a8Pool(stub.srv.URL))
		s.distillOnce(ctx, dfNoDemand)

		j := g3Read(t, pool, key)
		t.Logf("g1=%d g3=%d g3-Zerlegung=%v", j.rej["g1"], j.rej["g3"], j.g3)
		if j.rej["g1"] != 0 {
			t.Errorf("rej_g1 = %d, want 0 — die Sonde adressiert ausschliesslich Paare "+
				"dieses Calls", j.rej["g1"])
		}
		if j.rej["g3"] == 0 {
			t.Error("rej_g3 = 0 — ohne G3-Verwuerfe belegt die Ablesung nichts")
		}
	})

	// SONDE 4 — die Gegenprobe. Ein Lauf ohne G3-Verwurf schreibt eine
	// NULL-Zerlegung. Ohne sie wuerde ein Zaehler, der faelschlich mitzaehlt,
	// von den Sonden oben nicht gefangen.
	t.Run("ein sauberer Lauf schreibt eine Null-Zerlegung", func(t *testing.T) {
		n6Reset(t, pool)
		stub := a8NewStub(t, a8AnswerFromPrompt)
		s := a8Scheduler(pool, a8Config(), g3jSource(), a8Pool(stub.srv.URL))
		s.distillOnce(ctx, dfNoDemand)

		j := g3Read(t, pool, key)
		t.Logf("kept=%d rejected=%d g3=%v", j.kept, j.rejected, j.g3)
		if j.rej["g3"] != 0 {
			t.Fatalf("rej_g3 = %d, want 0 — die Sonde zitiert aus dem adressierten Chunk", j.rej["g3"])
		}
		if got := n6Sum(j.g3); got != 0 {
			t.Fatalf("G3-Zerlegung = %v bei null G3-Verwuerfen", j.g3)
		}
	})
}

// TestDistillG3JournalFixtureIsHonest ist die Fixture-Kontrolle: der Stub
// erfindet keine Adresse und kein Zitat, er hebt beides aus dem gerenderten
// Prompt. Faellt diese Sonde, misst die Sonde oben den Stub und nicht den Arm.
func TestDistillG3JournalFixtureIsHonest(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	n6Reset(t, pool)
	stub := a8NewStub(t, g3jAnswer)
	s := a8Scheduler(pool, a8Config(), g3jSource(), a8Pool(stub.srv.URL))
	s.distillOnce(ctx, dfNoDemand)

	seen := stub.seen()
	if len(seen) != 1 {
		t.Fatalf("der Stub sah %d Prompts, want 1", len(seen))
	}
	prompt := seen[0].User
	addrs := a8Addrs(prompt)
	if len(addrs) != 3 {
		t.Fatalf("der Prompt zeigt %d Adressen, want 3: %v", len(addrs), addrs)
	}
	// Zwei Chunks unter derselben block-Nummer, einer unter einer zweiten — das
	// ist die Lage, um die es in §15 geht.
	if addrs[0].block != addrs[1].block || addrs[0].block == addrs[2].block {
		t.Fatalf("Adress-Lage = %v, want zwei Chunks eines Parts plus ein zweites Part", addrs)
	}
	// Und jedes Zitat der Antwort steht woertlich im Prompt — ausser dem
	// erfundenen.
	answer, _ := g3jAnswer(a8Request{User: prompt})
	var parsed struct {
		Insights []struct {
			Quote string `json:"quote"`
		} `json:"insights"`
	}
	if err := json.Unmarshal([]byte(answer), &parsed); err != nil {
		t.Fatalf("die Stub-Antwort ist kein JSON: %v", err)
	}
	if len(parsed.Insights) != 5 {
		t.Fatalf("der Stub bot %d Zeilen, want 5", len(parsed.Insights))
	}
	for i, in := range parsed.Insights {
		if in.Quote == "" {
			t.Fatalf("Zeile %d traegt kein Zitat — g3jBody hat nichts gefunden", i)
		}
		inPrompt := strings.Contains(prompt, in.Quote)
		if i == 4 && inPrompt {
			t.Errorf("das erfundene Zitat steht im Prompt: %q", in.Quote)
		}
		// Das Naht-Zitat (i == 2) laeuft ueber die Wrapper-Markierung hinweg und
		// steht deshalb NICHT als zusammenhaengender String im Prompt — es steht
		// im MATERIAL, und genau das ist sein Punkt.
		if i < 2 && !inPrompt {
			t.Errorf("Zeile %d zitiert nicht aus dem Prompt: %q", i, in.Quote)
		}
	}
}
