//go:build integration

// Gate C4-1 (Befund N-6 aus reports/bau/c3-3-re-pilot.md §10): das
// Reject-Histogramm und der Gruppen-Verkleinerungs-Zaehler der Call-Planung,
// gelesen aus dem JOURNAL statt aus einer Log-Zeile.
//
// WARUM DAS JOURNAL UND NICHT DER LOG-LEVEL. Die Stufenzaehler verlassen
// distillOneCall als slog.Debug, und cmd/ctxd/main.go pinnt den Handler auf
// LevelInfo — im Betrieb sind sie damit unbeobachtbar. Ein Env-Schalter auf den
// Log-Level haette sie sichtbar gemacht, aber nicht ZAEHLBAR: die Debug-Zeile
// fuehrt kein run_id (distill_extract.go:1169-1171), also laesst sich ein
// Verwurf keinem Lauf und keinem Wasserzeichen-Bereich zuordnen, und die
// Auswertung einer Prompt-Iteration waere ein Zeitstempel-Abgleich ueber
// nebenlaeufige Quellen. Die Zahl gehoert in die Zeile, die der Lauf ohnehin
// schreibt.
//
// Die Sonden hier stehen auf drei Aussagen:
//
//  1. Das Histogramm ZERLEGT insights_rejected nach Toren — verschiedene
//     Verwurfsgruende landen in verschiedenen Spalten.
//  2. Die Zerlegung ist VOLLSTAENDIG: sum(rej_g1..rej_g7) + rej_schema ist
//     exakt insights_rejected. Das ist keine Konvention, sondern folgt aus
//     distillDecode (offered = len(lines)) und distillOneCall
//     (res.rejected += offered - len(kept)).
//  3. call_groups_shrunk zaehlt, WIE OFT die C3-1-Call-Planung eine Gruppe
//     verkleinert hat — die Haelfte des Instruments, die der WARN-Pfad des
//     Rune-Meters nicht abdeckt.
//
//	go test -tags=integration ./internal/events/ -run TestDistillRejectHistogramN6 -count=1 -v
package events

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// n6TightBlockRunes ist der Cap, unter dem die Call-Planung eine Gruppe
// verkleinern MUSS: gross genug, dass der Rune-Meter nicht schon vor dem ersten
// Call erschoepft ist, klein genug, dass room() unter rows_per_call (5) faellt.
// Der Wert ist am Fixture gemessen (Sonde 3 nennt beide Fehlrichtungen im
// Fehlertext), nicht aus der Rahmen-Arithmetik abgeleitet — der gerenderte
// Rahmen haengt an Wurzelname, Manifest und Zeitstempel des Laufs.
const n6TightBlockRunes = 1800

// n6Reset stellt den Ausgangszustand HERSTELLBAR her: a8Truncate raeumt Journal
// und Dedup-Ledger, aber NICHT den geschriebenen Insight-Block — und der ist der
// Carry, aus dem der Rune-Meter seinen Startwert bildet
// (distillNewRuneMeter/distillNextInsightRunes). Ohne diese Zeile haengt Sonde 3
// an der Reihenfolge der Subtests: gemessen am roten Lauf stand der Meter bei
// einem Carry von 1 schon vor dem ersten Call auf "block is full".
func n6Reset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	a8Truncate(t, pool)
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM context_blocks WHERE category = $1`, dfConfig().Distill.Category); err != nil {
		t.Fatalf("clear the arm's own blocks: %v", err)
	}
}

// n6Journal ist die Ablesung, um die es in dieser Welle geht: die neun Zahlen,
// die eine Prompt-Iteration quantitativ auswertbar machen.
type n6Journal struct {
	calls    int
	kept     int
	rejected int
	rej      map[string]int
	shrunk   int
	outcome  string
}

// n6Read liest die juengste Lauf-Zeile einer Quelle.
//
// ROT-FORM: gegen den unveraenderten Baum antwortet genau dieses Statement mit
// SQLSTATE 42703 (undefined_column) — die Spalten gibt es nicht, und das ist
// der Beleg, dass die Zaehler im Betrieb nicht beobachtbar sind.
func n6Read(t *testing.T, pool *pgxpool.Pool, key string) n6Journal {
	t.Helper()
	var j n6Journal
	var g1, g2, g3, g4, g5, g6, g7, schema int
	if err := pool.QueryRow(context.Background(), `
		SELECT calls, insights_kept, insights_rejected, outcome,
		       rej_g1, rej_g2, rej_g3, rej_g4, rej_g5, rej_g6, rej_g7, rej_schema,
		       call_groups_shrunk
		  FROM distill_run WHERE source_key = $1 ORDER BY started_at DESC LIMIT 1`, key).
		Scan(&j.calls, &j.kept, &j.rejected, &j.outcome,
			&g1, &g2, &g3, &g4, &g5, &g6, &g7, &schema, &j.shrunk); err != nil {
		t.Fatalf("read the run row: %v", err)
	}
	j.rej = map[string]int{
		"g1": g1, "g2": g2, "g3": g3, "g4": g4,
		"g5": g5, "g6": g6, "g7": g7, "schema": schema,
	}
	return j
}

// n6Sum ist die Summe des Histogramms.
func n6Sum(rej map[string]int) int {
	total := 0
	for _, v := range rej {
		total += v
	}
	return total
}

// n6MixedAnswer antwortet mit VIER Zeilen, die vier verschiedene Ausgaenge
// nehmen — der Punkt der Sonde ist, dass sie sich im Journal unterscheiden
// lassen:
//
//   - eine gute Zeile, die das Tor passiert (kept),
//   - eine Zeile auf eine (block, chunk)-Adresse, die dieser Prompt nicht
//     zeigt  → G1,
//   - eine Zeile mit einem Zitat unter der Laengen-Schwelle → G2,
//   - eine Zeile ohne `kind` — die der Parser abweist → "schema".
//
// G1 wird VOR der Zitatlaenge geprueft (distillScreen), deshalb traegt die
// G1-Zeile bewusst ein gutes Zitat: sie darf nur an der Adresse scheitern.
func n6MixedAnswer(req a8Request) (string, int) {
	addrs := a8Addrs(req.User)
	if len(addrs) == 0 {
		return a8Answer(), http.StatusOK
	}
	a := addrs[0]
	good := a8QuoteFrom(req.User, a)
	return a8Answer(
		map[string]any{
			"claim": a8Claim, "quote": good,
			"block": a.block, "chunk": a.chunk, "kind": "finding",
		},
		map[string]any{
			"claim": a8Claim, "quote": good,
			"block": "kein-block-dieses-prompts", "chunk": a.chunk, "kind": "finding",
		},
		map[string]any{
			"claim": a8Claim, "quote": "zu kurz",
			"block": a.block, "chunk": a.chunk, "kind": "finding",
		},
		map[string]any{
			"claim": a8Claim, "quote": good,
			"block": a.block, "chunk": a.chunk,
		},
	), http.StatusOK
}

func TestDistillRejectHistogramN6(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	key := distillSourceKey(dfLabel, dfScope, dfRoot)

	// SONDE 1 — die Zerlegung. Vier angebotene Zeilen, vier verschiedene
	// Ausgaenge, und das Journal benennt sie einzeln.
	t.Run("das Journal zerlegt insights_rejected nach Toren", func(t *testing.T) {
		n6Reset(t, pool)
		stub := a8NewStub(t, n6MixedAnswer)
		s := a8Scheduler(pool, a8Config(), a8Source([]string{a8Block1}), a8Pool(stub.srv.URL))
		s.distillOnce(ctx, dfNoDemand)

		j := n6Read(t, pool, key)
		t.Logf("calls=%d kept=%d rejected=%d rej=%v shrunk=%d outcome=%s",
			j.calls, j.kept, j.rejected, j.rej, j.shrunk, j.outcome)

		if j.calls != 1 {
			t.Fatalf("calls = %d, want 1 — die Sonde misst sonst nicht den einen Call", j.calls)
		}
		if j.kept != 1 {
			t.Fatalf("insights_kept = %d, want 1", j.kept)
		}
		want := map[string]int{"g1": 1, "g2": 1, "schema": 1}
		for k, n := range want {
			if j.rej[k] != n {
				t.Errorf("rej_%s = %d, want %d — der Verwurfsgrund ist nicht zaehlbar", k, j.rej[k], n)
			}
		}
		// Und die uebrigen Tore stehen auf null: ein Histogramm, das alles in
		// einen Eimer wirft, waere keine Zerlegung.
		for _, k := range []string{"g3", "g4", "g5", "g6", "g7"} {
			if j.rej[k] != 0 {
				t.Errorf("rej_%s = %d, want 0 — die Zeilen dieser Sonde treffen dieses Tor nicht",
					k, j.rej[k])
			}
		}
	})

	// SONDE 2 — die Vollstaendigkeit. Ohne sie waere das Histogramm eine
	// Auswahl und keine Messung: ein Verwurfsgrund, den niemand zaehlt, sieht
	// im Betrieb aus wie ein Grund, den es nicht gibt.
	t.Run("das Histogramm summiert sich auf insights_rejected", func(t *testing.T) {
		n6Reset(t, pool)
		stub := a8NewStub(t, n6MixedAnswer)
		s := a8Scheduler(pool, a8Config(), a8Source([]string{a8Block1, a8Block2}), a8Pool(stub.srv.URL))
		s.distillOnce(ctx, dfNoDemand)

		j := n6Read(t, pool, key)
		t.Logf("rejected=%d, Histogramm-Summe=%d, rej=%v", j.rejected, n6Sum(j.rej), j.rej)
		if j.rejected == 0 {
			t.Fatal("insights_rejected = 0 — die Sonde hat nichts verworfen und misst nichts")
		}
		if got := n6Sum(j.rej); got != j.rejected {
			t.Fatalf("Histogramm-Summe %d != insights_rejected %d — die Zerlegung ist unvollstaendig",
				got, j.rejected)
		}
	})

	// SONDE 3 — die zweite Haelfte des Instruments. Der Rune-Meter loggt per
	// WARN, DASS er bremst; wie oft er eine Gruppe VERKLEINERT hat, sagte
	// bisher nur eine Debug-Zeile.
	//
	// max_block_runes ist GEMESSEN und nicht geschaetzt: der leere Block
	// rendert einen Rahmen, und eine Einsicht kostet mindestens
	// distillMinInsightRunes (156). Der Wert unten laesst room() echt zwischen
	// 1 und rows_per_call (5) fallen — bei zu kleinem Wert waere der Meter
	// schon vor dem ersten Call erschoepft (calls=0), bei zu grossem gaebe es
	// nichts zu verkleinern.
	t.Run("call_groups_shrunk zaehlt die verkleinerten Gruppen", func(t *testing.T) {
		n6Reset(t, pool)
		cfg := a8Config()
		cfg.Distill.MaxBlockRunes = n6TightBlockRunes
		stub := a8NewStub(t, a8AnswerFromPrompt)
		s := a8Scheduler(pool, cfg, a8Source([]string{a8Block1, a8Block2}), a8Pool(stub.srv.URL))
		s.distillOnce(ctx, dfNoDemand)

		j := n6Read(t, pool, key)
		t.Logf("max_block_runes=%d -> calls=%d kept=%d shrunk=%d outcome=%s",
			cfg.Distill.MaxBlockRunes, j.calls, j.kept, j.shrunk, j.outcome)
		if j.calls == 0 {
			t.Fatalf("calls = 0 — max_block_runes = %d ist zu eng, der Meter bremst schon vor "+
				"dem ersten Call und die Sonde misst die falsche Bremse", n6TightBlockRunes)
		}
		if j.shrunk == 0 {
			t.Fatalf("call_groups_shrunk = 0 bei %d Calls — entweder zaehlt der Zaehler nicht, "+
				"oder max_block_runes = %d laesst room() >= rows_per_call",
				j.calls, n6TightBlockRunes)
		}
	})

	// SONDE 4 — die Gegenprobe. Ein Lauf, in dem nichts verworfen wird, muss
	// ein NULL-Histogramm schreiben. Ohne sie wuerde ein Zaehler, der
	// faelschlich mitzaehlt, von den Sonden oben nicht gefangen.
	t.Run("ein sauberer Lauf schreibt ein Null-Histogramm", func(t *testing.T) {
		n6Reset(t, pool)
		stub := a8NewStub(t, a8AnswerFromPrompt)
		s := a8Scheduler(pool, a8Config(), a8Source([]string{a8Block1}), a8Pool(stub.srv.URL))
		s.distillOnce(ctx, dfNoDemand)

		j := n6Read(t, pool, key)
		if j.kept != 1 || j.rejected != 0 {
			t.Fatalf("kept/rejected = %d/%d, want 1/0", j.kept, j.rejected)
		}
		if got := n6Sum(j.rej); got != 0 {
			t.Fatalf("Histogramm-Summe = %d bei null Verwuerfen: %v", got, j.rej)
		}
		if j.shrunk != 0 {
			t.Fatalf("call_groups_shrunk = %d, obwohl der Cap (6000) nichts verkleinert", j.shrunk)
		}
	})
}
