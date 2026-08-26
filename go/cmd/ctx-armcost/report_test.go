package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// checkFootnotes ist die geteilte Prüfung der drei Pflicht-Fußzeilen aus
// design/05 §4.7 plus dem M-W7-Befund. Sie liefert einen FEHLER statt zu
// t.Fatal-en, damit dieselbe Prüfung als Negativ-Probe gegen eine Variante
// OHNE die jeweilige Zeile gefahren werden kann (Gate (c)).
func checkFootnotes(rendered string) error {
	for _, want := range []struct{ name, needle string }{
		{"fire-and-forget", "llmlog/llmlog.go:135-143"},
		{"fire-and-forget (Wortlaut)", "schreibt asynchron in einer eigenen"},
		{"nullInt", "llmlog/llmlog.go:192-197"},
		{"M-W7-Befund", "die Design-Formel zieht die Lease-Wartezeit dort ein zweites Mal ab"},
	} {
		if !strings.Contains(rendered, want.needle) {
			return fmt.Errorf("Fußzeile %q fehlt (gesucht: %q)", want.name, want.needle)
		}
	}
	return nil
}

// checkNoCostMetric prüft Gate (b): cost_usd darf im JSON an KEINER Stelle als
// Kennzahl auftauchen. Erlaubt ist genau ein Schlüssel — die Provenienz-Zeile.
func checkNoCostMetric(raw []byte) error {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	var walk func(path string, v any) error
	walk = func(path string, v any) error {
		switch t := v.(type) {
		case map[string]any:
			for k, sub := range t {
				if strings.Contains(strings.ToLower(k), "cost") && k != "cost_usd_note" {
					return fmt.Errorf("cost-Kennzahl im JSON: %s.%s", path, k)
				}
				if err := walk(path+"."+k, sub); err != nil {
					return err
				}
			}
		case []any:
			for i, sub := range t {
				if err := walk(fmt.Sprintf("%s[%d]", path, i), sub); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk("$", doc)
}

// sampleReport ist ein handgeschriebener Report ohne DB — er trägt genau die
// Formen, auf die die Gates zeigen.
func sampleReport() Report {
	since := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	until := since.Add(7 * 24 * time.Hour)
	rep := Report{
		GeneratedAt:  until,
		Since:        since,
		Until:        until,
		RowsInWindow: 44,
		CountGate:    44,
		Pipelines: []Bucket{
			{Key: "dream-daily-synthesis", N: 20, OccupancySeconds: 508.5, WireSeconds: 520.0,
				P50DurationMs: 25000, P95DurationMs: 31000, PromptTokens: 900, PromptTokensNull: 2,
				CompletionTokens: 13840, CompletionTokensNull: 0, Errors: 1, ErrorRate: 0.05,
				DispatchAborts: 0, DurationNull: 0, QueueWaitNull: 3},
		},
		CostUSDNote: fmt.Sprintf("cost_usd: in %d von %d Zeilen gesetzt — nicht verwendet", 2, 44),
		Footnotes:   footnotes(),
	}
	rep.Interactive = InteractiveP95{
		WindowMs: 1600, WindowN: 20, PriorMs: 1000, PriorN: 20,
		PriorSince: since.Add(-7 * 24 * time.Hour), Factor: 1.6, Threshold: abortThreshold, Exceeded: true,
	}
	rep.Interactive.Note = interactiveNote(rep.Interactive)
	return rep
}

// TestFootnotesPflichtUndNegativprobe fährt Gate (c): die Fußzeile zur
// fire-and-forget-Verzerrung steht im gerenderten Report — und eine Variante
// OHNE sie macht dieselbe Prüfung rot.
func TestFootnotesPflichtUndNegativprobe(t *testing.T) {
	rep := sampleReport()
	var buf bytes.Buffer
	if err := renderTable(&buf, rep); err != nil {
		t.Fatal(err)
	}
	if err := checkFootnotes(buf.String()); err != nil {
		t.Fatalf("Pflicht-Fußzeile fehlt im echten Report: %v", err)
	}

	// Negativ-Probe: dieselbe Render-Strecke, aber die fire-and-forget-Zeile
	// aus den Fußzeilen entfernt. Die Prüfung MUSS jetzt fehlschlagen —
	// sonst wäre Gate (c) wirkungslos.
	variant := rep
	variant.Footnotes = nil
	for _, f := range rep.Footnotes {
		if f != ffFootnote {
			variant.Footnotes = append(variant.Footnotes, f)
		}
	}
	if len(variant.Footnotes) != len(rep.Footnotes)-1 {
		t.Fatalf("Variante hat %d Fußzeilen, erwartet %d", len(variant.Footnotes), len(rep.Footnotes)-1)
	}
	var vbuf bytes.Buffer
	if err := renderTable(&vbuf, variant); err != nil {
		t.Fatal(err)
	}
	if err := checkFootnotes(vbuf.String()); err == nil {
		t.Fatal("Negativ-Probe wirkungslos: Variante ohne fire-and-forget-Fußzeile besteht die Prüfung")
	}
}

// TestCostUSDIstKeineKennzahl fährt Gate (b): kein JSON-Feld und keine
// Tabellenspalte führt cost_usd als Kennzahl; die Provenienz-Zeile trägt die
// live gezählten Zahlen. Die Negativ-Probe zeigt, dass der Prüfer greift.
func TestCostUSDIstKeineKennzahl(t *testing.T) {
	rep := sampleReport()
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkNoCostMetric(raw); err != nil {
		t.Fatalf("cost_usd als Kennzahl im Report: %v", err)
	}
	const want = "cost_usd: in 2 von 44 Zeilen gesetzt — nicht verwendet"
	if !strings.Contains(string(raw), want) {
		t.Fatalf("Provenienz-Zeile fehlt im JSON: %s", raw)
	}
	var buf bytes.Buffer
	if err := renderTable(&buf, rep); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("Provenienz-Zeile fehlt in der Tabelle:\n%s", buf.String())
	}
	// Die Kopfzeile der Tabelle darf keine cost-Spalte tragen.
	head := strings.SplitN(buf.String(), "\n", 6)
	for _, line := range head {
		if strings.Contains(line, "pipeline") && strings.Contains(strings.ToLower(line), "cost") {
			t.Fatalf("cost-Spalte in der Tabellen-Kopfzeile: %s", line)
		}
	}

	// Negativ-Probe des Prüfers selbst: ein Report-JSON, das cost_usd als
	// Bucket-Kennzahl führt, muss abgewiesen werden.
	bad := []byte(`{"pipelines":[{"key":"x","n":1,"cost_usd":0.42}],"cost_usd_note":"…"}`)
	if err := checkNoCostMetric(bad); err == nil {
		t.Fatal("Negativ-Probe wirkungslos: cost_usd als Bucket-Kennzahl wird nicht erkannt")
	}
}

// TestInteractiveNoteMarkierung fährt die Rendering-Seite von Gate 6:
// Faktor 1,6 und die Markierung „> 1,5 ⇒ Abbruchkriterium".
func TestInteractiveNoteMarkierung(t *testing.T) {
	rep := sampleReport()
	for _, want := range []string{"Faktor 1,60", "> 1,5 ⇒ Abbruchkriterium", "ÜBERSCHRITTEN"} {
		if !strings.Contains(rep.Interactive.Note, want) {
			t.Fatalf("Note ohne %q: %s", want, rep.Interactive.Note)
		}
	}
	ok := InteractiveP95{WindowMs: 1100, WindowN: 5, PriorMs: 1000, PriorN: 5, Threshold: abortThreshold, Factor: 1.1}
	note := interactiveNote(ok)
	for _, want := range []string{"Faktor 1,10", "im Rahmen", "> 1,5 ⇒ Abbruchkriterium"} {
		if !strings.Contains(note, want) {
			t.Fatalf("Note ohne %q: %s", want, note)
		}
	}
	none := interactiveNote(InteractiveP95{Threshold: abortThreshold})
	if !strings.Contains(none, "kein Vergleich") {
		t.Fatalf("fehlender Vorher-Wert muss als solcher benannt sein: %s", none)
	}
}

// TestOccupancyExprImSQL hält fest, dass der Belegungs-Term genau einmal und
// unverändert im fertigen Gruppierungs-SQL steht — die Integration-Probe
// ersetzt ihn dort, um die Variante ohne queue_wait-Abzug zu fahren.
func TestOccupancyExprImSQL(t *testing.T) {
	if n := strings.Count(pipelineBucketSQL, occupancyExpr); n != 1 {
		t.Fatalf("occupancyExpr steht %dx in pipelineBucketSQL, erwartet 1x", n)
	}
	if n := strings.Count(classBucketSQL, occupancyExpr); n != 1 {
		t.Fatalf("occupancyExpr steht %dx in classBucketSQL, erwartet 1x", n)
	}
	if !strings.Contains(occupancyExpr, "queue_wait_ms") {
		t.Fatalf("occupancyExpr zieht queue_wait_ms nicht ab: %s", occupancyExpr)
	}
}
