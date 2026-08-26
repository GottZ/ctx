//go:build integration

package main

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// rawRow ist eine Fixture-Zeile, wie der Test sie unabhängig vom Werkzeug
// wieder aus der Datenbank liest — die Aggregation daraus passiert in Go, das
// Werkzeug aggregiert in SQL. Zwei Wege, ein Ergebnis (Gate (a)).
type rawRow struct {
	pipeline  string
	class     *string
	duration  *int64
	queueWait *int64
	prompt    *int64
	compl     *int64
	errStr    *string
	abort     *string
}

// seedFixture legt 48 Zeilen über drei Pipelines und zwei dispatch_classes an:
// NULL-queue_wait_ms, NULL-Tokens, eine Zeile ganz ohne Wire-Call
// (duration_ms NULL), Fehlerzeilen, dispatch_abort-Zeilen und GENAU zwei
// Zeilen mit cost_usd. Alle Zeilen liegen zwischen 2 h und 18 h vor DB-now().
func seedFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pipelines := []string{"dream-daily-synthesis", "cluster-label", "query-rerank"}
	classes := []string{"background", "interactive"}
	for i := range 48 {
		var (
			duration  = ptr(int64(500 + i*37))
			queueWait *int64
			prompt    *int64
			compl     *int64
			errStr    *string
			abort     *string
			cost      *float64
		)
		if i%5 != 0 {
			queueWait = ptr(int64(i * 13 % 400))
		}
		if i%4 != 0 {
			prompt = ptr(int64(100 + i))
		}
		if i%7 != 0 {
			compl = ptr(int64(10 + i*3))
		}
		if i%11 == 0 {
			errStr = ptr("boom")
		}
		if i%13 == 0 {
			abort = ptr("acquire_expired")
		}
		if i == 41 { // K9-Form: Ablehnung ohne physischen Call.
			duration = nil
			abort = ptr("queue_full")
		}
		if i == 5 {
			cost = ptr(0.01)
		}
		if i == 17 {
			cost = ptr(0.02)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_llm_log
			   (created_at, pipeline, model, host, duration_ms, error, prompt_tokens, completion_tokens,
			    cost_usd, queue_wait_ms, dispatch_class, dispatch_abort)
			 VALUES (now() - make_interval(mins => $1), $2, 'qwen', 'h', $3, $4, $5, $6, $7, $8, $9, $10)`,
			120+i*20, pipelines[i%3], duration, errStr, prompt, compl, cost, queueWait,
			classes[(i/3)%2], abort); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// readRaw liest die Fixture roh zurück — die unabhängige Quelle der Handzähler.
func readRaw(t *testing.T, pool *pgxpool.Pool, since, until time.Time) []rawRow {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT pipeline, dispatch_class, duration_ms, queue_wait_ms, prompt_tokens,
		        completion_tokens, error, dispatch_abort
		   FROM context_llm_log WHERE created_at >= $1 AND created_at < $2`, since, until)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []rawRow
	for rows.Next() {
		var r rawRow
		if err := rows.Scan(&r.pipeline, &r.class, &r.duration, &r.queueWait,
			&r.prompt, &r.compl, &r.errStr, &r.abort); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// percentileCont bildet percentile_cont in Go nach: lineare Interpolation über
// rn = p·(N−1) auf der sortierten Werteliste. Unabhängige Implementierung —
// das Werkzeug rechnet die Perzentile in SQL.
func percentileCont(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rn := p * float64(len(sorted)-1)
	lo := int(math.Floor(rn))
	hi := int(math.Ceil(rn))
	if lo == hi {
		return sorted[lo]
	}
	return sorted[lo] + (sorted[hi]-sorted[lo])*(rn-float64(lo))
}

// handAggregate zieht die Zähler in Go — ohne eine Zeile SQL des Werkzeugs.
func handAggregate(raw []rawRow, keyOf func(rawRow) string) map[string]*Bucket {
	out := map[string]*Bucket{}
	durs := map[string][]float64{}
	for _, r := range raw {
		k := keyOf(r)
		b := out[k]
		if b == nil {
			b = &Bucket{Key: k}
			out[k] = b
		}
		b.N++
		if r.duration != nil {
			b.WireSeconds += float64(*r.duration) / 1000
			wait := int64(0)
			if r.queueWait != nil {
				wait = *r.queueWait
			}
			b.OccupancySeconds += float64(*r.duration-wait) / 1000
			durs[k] = append(durs[k], float64(*r.duration))
		} else {
			b.DurationNull++
		}
		if r.queueWait == nil {
			b.QueueWaitNull++
		}
		if r.prompt != nil {
			b.PromptTokens += *r.prompt
		} else {
			b.PromptTokensNull++
		}
		if r.compl != nil {
			b.CompletionTokens += *r.compl
		} else {
			b.CompletionTokensNull++
		}
		if r.errStr != nil {
			b.Errors++
		}
		if r.abort != nil {
			b.DispatchAborts++
		}
	}
	for k, b := range out {
		d := durs[k]
		sort.Float64s(d)
		b.P50DurationMs = percentileCont(d, 0.5)
		b.P95DurationMs = percentileCont(d, 0.95)
		if b.N > 0 {
			b.ErrorRate = float64(b.Errors) / float64(b.N)
		}
	}
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// TestGoldenReportGegenHandzaehler ist Gate (a): der Report reproduziert für
// ein festes Fenster die unabhängig in Go gezogenen Zähler — je Pipeline und
// je dispatch_class. Anschließend läuft die Negativ-Probe: eine SQL-Variante
// OHNE den queue_wait_ms-Abzug liefert eine andere Belegungs-Summe.
func TestGoldenReportGegenHandzaehler(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedFixture(t, pool)

	var dbNow time.Time
	if err := pool.QueryRow(ctx, "SELECT now()").Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	since, until := dbNow.Add(-24*time.Hour), dbNow.Add(-time.Hour)

	rep, err := buildReport(ctx, pool, Options{Since: since, Until: until, ByClass: true})
	if err != nil {
		t.Fatalf("buildReport: %v", err)
	}
	if rep.RowsInWindow != 48 || rep.CountGate != 48 {
		t.Fatalf("Fenster hält %d Zeilen (Gate %d), erwartet 48/48", rep.RowsInWindow, rep.CountGate)
	}

	raw := readRaw(t, pool, since, until)
	if len(raw) != 48 {
		t.Fatalf("Rohzeilen=%d, erwartet 48", len(raw))
	}

	check := func(label string, got []Bucket, want map[string]*Bucket) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: %d Gruppen, erwartet %d", label, len(got), len(want))
		}
		for _, g := range got {
			w := want[g.Key]
			if w == nil {
				t.Fatalf("%s: unbekannte Gruppe %q", label, g.Key)
			}
			switch {
			case g.N != w.N:
				t.Fatalf("%s/%s n=%d, erwartet %d", label, g.Key, g.N, w.N)
			case !near(g.OccupancySeconds, w.OccupancySeconds):
				t.Fatalf("%s/%s belegung_s=%v, erwartet %v", label, g.Key, g.OccupancySeconds, w.OccupancySeconds)
			case !near(g.WireSeconds, w.WireSeconds):
				t.Fatalf("%s/%s wire_s=%v, erwartet %v", label, g.Key, g.WireSeconds, w.WireSeconds)
			case !near(g.P50DurationMs, w.P50DurationMs):
				t.Fatalf("%s/%s p50=%v, erwartet %v", label, g.Key, g.P50DurationMs, w.P50DurationMs)
			case !near(g.P95DurationMs, w.P95DurationMs):
				t.Fatalf("%s/%s p95=%v, erwartet %v", label, g.Key, g.P95DurationMs, w.P95DurationMs)
			case g.PromptTokens != w.PromptTokens || g.PromptTokensNull != w.PromptTokensNull:
				t.Fatalf("%s/%s prompt=%d(%d), erwartet %d(%d)", label, g.Key,
					g.PromptTokens, g.PromptTokensNull, w.PromptTokens, w.PromptTokensNull)
			case g.CompletionTokens != w.CompletionTokens || g.CompletionTokensNull != w.CompletionTokensNull:
				t.Fatalf("%s/%s compl=%d(%d), erwartet %d(%d)", label, g.Key,
					g.CompletionTokens, g.CompletionTokensNull, w.CompletionTokens, w.CompletionTokensNull)
			case g.Errors != w.Errors || !near(g.ErrorRate, w.ErrorRate):
				t.Fatalf("%s/%s fehler=%d/%v, erwartet %d/%v", label, g.Key, g.Errors, g.ErrorRate, w.Errors, w.ErrorRate)
			case g.DispatchAborts != w.DispatchAborts:
				t.Fatalf("%s/%s abort=%d, erwartet %d", label, g.Key, g.DispatchAborts, w.DispatchAborts)
			case g.DurationNull != w.DurationNull || g.QueueWaitNull != w.QueueWaitNull:
				t.Fatalf("%s/%s dur_null=%d qw_null=%d, erwartet %d/%d", label, g.Key,
					g.DurationNull, g.QueueWaitNull, w.DurationNull, w.QueueWaitNull)
			}
		}
	}
	check("pipeline", rep.Pipelines, handAggregate(raw, func(r rawRow) string { return r.pipeline }))
	check("dispatch_class", rep.Classes, handAggregate(raw, func(r rawRow) string {
		if r.class == nil {
			return "(null)"
		}
		return *r.class
	}))

	// Die Fixture muss die Formen wirklich tragen, sonst prüft der Golden-Test
	// eine Ideal-Welt (Gate-Selbstkontrolle).
	var nullQW, nullTok, aborts, errs, nullDur int64
	for _, b := range rep.Pipelines {
		nullQW += b.QueueWaitNull
		nullTok += b.PromptTokensNull + b.CompletionTokensNull
		aborts += b.DispatchAborts
		errs += b.Errors
		nullDur += b.DurationNull
	}
	if nullQW == 0 || nullTok == 0 || aborts == 0 || errs == 0 || nullDur == 0 {
		t.Fatalf("Fixture zu brav: qw_null=%d tok_null=%d abort=%d fehler=%d dur_null=%d",
			nullQW, nullTok, aborts, errs, nullDur)
	}

	// Gate (b) live: genau zwei Zeilen tragen cost_usd, und die Zeile nennt
	// die live gezählten Zahlen — nicht hartkodierte.
	const wantNote = "cost_usd: in 2 von 48 Zeilen gesetzt — nicht verwendet"
	if rep.CostUSDNote != wantNote {
		t.Fatalf("cost_usd-Zeile = %q, erwartet %q", rep.CostUSDNote, wantNote)
	}

	// NEGATIV-PROBE zum queue_wait-Abzug: dieselbe Abfrage, aber ohne den
	// Abzug — sie MUSS eine andere Belegungs-Summe liefern, sonst wäre der
	// Abzug wirkungslos und die Kennzahl beliebig.
	variantSQL := strings.Replace(pipelineBucketSQL, occupancyExpr, "(duration_ms)", 1)
	if variantSQL == pipelineBucketSQL {
		t.Fatal("Negativ-Probe konnte den Belegungs-Term nicht ersetzen")
	}
	variant, err := queryBuckets(ctx, pool, variantSQL, since, until)
	if err != nil {
		t.Fatalf("Variante ohne Abzug: %v", err)
	}
	var sumReal, sumVariant float64
	for _, b := range rep.Pipelines {
		sumReal += b.OccupancySeconds
	}
	for _, b := range variant {
		sumVariant += b.OccupancySeconds
	}
	if near(sumReal, sumVariant) {
		t.Fatalf("Negativ-Probe wirkungslos: mit Abzug %v == ohne Abzug %v", sumReal, sumVariant)
	}
	// Die Differenz ist per Konstruktion exakt Σ queue_wait_ms der Zeilen mit
	// gesetztem Wert UND gesetzter Dauer — das belegt, was der Abzug tut.
	var sumWait float64
	for _, r := range raw {
		if r.queueWait != nil && r.duration != nil {
			sumWait += float64(*r.queueWait) / 1000
		}
	}
	if !near(sumVariant-sumReal, sumWait) {
		t.Fatalf("Differenz %v, erwartet Σ queue_wait_ms = %v", sumVariant-sumReal, sumWait)
	}
	t.Logf("Belegung mit Abzug %.3f s, ohne Abzug %.3f s, Δ = Σ queue_wait %.3f s", sumReal, sumVariant, sumWait)
}

// TestInteractiveP95Vergleich ist Gate 6: p95 vorher 1 000 ms, im Fenster
// 1 600 ms ⇒ Faktor 1,6, Überschreitung markiert.
func TestInteractiveP95Vergleich(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var dbNow time.Time
	if err := pool.QueryRow(ctx, "SELECT now()").Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	// Fenster [now−3h, now−2h), Vorfenster [now−4h, now−3h).
	ins := func(ageMin int, class string, dur int64) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_llm_log (created_at, pipeline, model, host, duration_ms, queue_wait_ms, dispatch_class)
			 VALUES (now() - make_interval(mins => $1), 'web-chat', 'qwen', 'h', $2, 0, $3)`,
			ageMin, dur, class); err != nil {
			t.Fatal(err)
		}
	}
	for range 20 {
		ins(150, "interactive", 1600) // im Fenster
		ins(210, "interactive", 1000) // davor
		ins(150, "background", 9000)  // darf die Kennzahl nicht berühren
	}

	since, until := dbNow.Add(-3*time.Hour), dbNow.Add(-2*time.Hour)
	rep, err := buildReport(ctx, pool, Options{Since: since, Until: until, ByClass: true})
	if err != nil {
		t.Fatalf("buildReport: %v", err)
	}
	ip := rep.Interactive
	switch {
	case !near(ip.WindowMs, 1600):
		t.Fatalf("p95 im Fenster = %v, erwartet 1600", ip.WindowMs)
	case !near(ip.PriorMs, 1000):
		t.Fatalf("p95 davor = %v, erwartet 1000", ip.PriorMs)
	case !near(ip.Factor, 1.6):
		t.Fatalf("Faktor = %v, erwartet 1,6", ip.Factor)
	case !ip.Exceeded:
		t.Fatal("Faktor 1,6 > 1,5 muss als Überschreitung markiert sein")
	case ip.WindowN != 20 || ip.PriorN != 20:
		t.Fatalf("n = %d/%d, erwartet 20/20", ip.WindowN, ip.PriorN)
	}
	for _, want := range []string{"Faktor 1,60", "> 1,5 ⇒ Abbruchkriterium", "ÜBERSCHRITTEN"} {
		if !strings.Contains(ip.Note, want) {
			t.Fatalf("Note ohne %q: %s", want, ip.Note)
		}
	}
	var buf strings.Builder
	if err := renderTable(&buf, rep); err != nil {
		t.Fatal(err)
	}
	if err := checkFootnotes(buf.String()); err != nil {
		t.Fatalf("Pflicht-Fußzeilen fehlen im DB-gestützten Report: %v", err)
	}
}

// TestReadOnlyTxWeistSchreibenAb belegt den SELECT-only-Perimeter dort, wo er
// wirkt: in der Datenbank. Jede Abfrage des Werkzeugs läuft durch readOnly.
func TestReadOnlyTxWeistSchreibenAb(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	err := readOnly(ctx, pool, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, "CREATE TABLE armcost_write_probe (x int)")
		return e
	})
	if err == nil {
		t.Fatal("READ ONLY-Transaktion hat ein CREATE TABLE zugelassen")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("erwartet SQLSTATE 25006 (read_only_sql_transaction), bekam: %v", err)
	}
}
