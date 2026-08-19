package goldbench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// testDataDir ist das echte Gold-Daten-Verzeichnis relativ zum Paket.
func testDataDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "..", "..", "bench", "goldbench", "data")
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("gold-daten nicht verfügbar: %v", err)
	}
	return dir
}

// fakeChatServer beantwortet jeden Chat-Call mit einer kanonisch gültigen
// Antwort der jeweils erkannten Achse (Erkennung über den System-Prompt).
func fakeChatServer(t *testing.T) *httptest.Server {
	t.Helper()
	docRe := regexp.MustCompile(`Doc \d+ \[`)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) != 2 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		system, user := req.Messages[0].Content, req.Messages[1].Content

		var answer string
		switch {
		case strings.Contains(system, "temporal reference extractor"):
			answer = `{"dates":[{"date":"2026-07-25","source":"explicit"}],"directions":[],"false_positives":[]}`
		case strings.Contains(system, "temporal reference resolver"):
			answer = `{"dates":[{"ref":"letzten Montag","date":"2026-08-10","end":null,"dir":"past"}],"query":"q"}`
		case strings.Contains(system, "extract conceptual keywords"):
			answer = `["flash attention","kv cache","prompt eviction","ollama","qwen"]`
		case strings.Contains(system, "assign topical tags"):
			answer = `{"tags":["postgresql","pgvector","embedding","retrieval"]}`
		case strings.Contains(system, "write the title"):
			// Denk-Block vor der Antwort: der Client strippt ihn (ThinkStripped),
			// der Dump-v2-Test sieht das Signal je Slot.
			answer = "<think>kurz nachgedacht</think>" + `{"title":"Kanonischer Test-Titel"}`
		case strings.Contains(system, "Classify relationships"):
			answer = `[]`
		case strings.Contains(system, "recurring pattern"):
			answer = `{"verdict":"recurrent","pattern":"parallel","confidence":0.9}`
		case strings.Contains(system, "Sicherheits-Klassifizierer"):
			answer = `{"answer": false}`
		case strings.Contains(system, "You name clusters"):
			answer = `{"label":"Kanonisches Test-Label"}`
		case strings.Contains(system, "Rate how well each document"):
			n := len(docRe.FindAllString(user, -1))
			scores := make([]string, n)
			for i := range scores {
				scores[i] = "5"
			}
			answer = "[" + strings.Join(scores, ",") + "]"
		case strings.Contains(system, "fact extraction engine"):
			answer = `Die Quelle beantwortet die Frage [1].`
		case strings.Contains(system, "query translator"):
			answer = `restore database backup plan`
		default:
			t.Errorf("fake server: unbekannter system-prompt: %.80q", system)
			http.Error(w, "unknown axis", http.StatusBadRequest)
			return
		}

		resp := map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}},
			"usage": map[string]any{
				"prompt_tokens": 100 + len(user)/4, "completion_tokens": 7,
				"completion_tokens_details": map[string]any{"reasoning_tokens": 3},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// TestRunEndToEnd fährt den Runner mit n=2 pro Achse gegen den Fake-Server:
// echte Gold-Daten, echte Prompt-Builder, echte Parser — jede Achse muss
// parse_rate 1.0 erreichen und ohne Transport-Fehler durchlaufen.
func TestRunEndToEnd(t *testing.T) {
	srv := fakeChatServer(t)
	defer srv.Close()

	cfg := Config{
		DataDir:     testDataDir(t),
		Endpoint:    srv.URL,
		Model:       "fake-model",
		N:           2,
		Concurrency: 4,
		Seed:        20260812,
		TimeoutSec:  30,
		Verbose:     true,
	}
	report, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Axes) != len(Axes) {
		t.Fatalf("achsen im report: %d, erwartet %d", len(report.Axes), len(Axes))
	}
	for axis, res := range report.Axes {
		if res.N != 2 {
			t.Errorf("%s: n=%d, erwartet 2", axis, res.N)
		}
		if res.ParseRate != 1.0 {
			t.Errorf("%s: parse_rate=%v, erwartet 1.0", axis, res.ParseRate)
		}
		if res.TransportErrors != 0 {
			t.Errorf("%s: transport_errors=%d", axis, res.TransportErrors)
		}
		if len(res.PerCase) != 2 {
			t.Errorf("%s: per_case=%d, erwartet 2 (verbose)", axis, len(res.PerCase))
		}
	}
	if report.Env.DatasetSHA256 == "" || report.Env.Timestamp == "" {
		t.Error("env-stamp unvollständig")
	}
	// cluster-label: kanonisches Label besteht den Constraint immer.
	if got := report.Axes["cluster-label"].PrimaryScore; got != 1.0 {
		t.Errorf("cluster-label constraint_pass=%v, erwartet 1.0", got)
	}
}

// TestRunDryRun validiert den HTTP-freien Smoke: alle Fälle laden, Prompts
// bauen, Report mit parse_rate 0 auf jeder Achse.
func TestRunDryRun(t *testing.T) {
	cfg := Config{
		DataDir: testDataDir(t),
		DryRun:  true,
		Seed:    20260812,
	}
	report, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run (dry): %v", err)
	}
	total := 0
	for axis, res := range report.Axes {
		total += res.N
		if res.ParseRate != 0 {
			t.Errorf("%s: dry-run parse_rate=%v, erwartet 0", axis, res.ParseRate)
		}
	}
	if total != 1127 {
		t.Errorf("dry-run fälle gesamt=%d, erwartet 1127", total)
	}
}

// TestRunUnknownAxis stellt sicher, dass eine unbekannte Achse hart failt.
func TestRunUnknownAxis(t *testing.T) {
	cfg := Config{DataDir: testDataDir(t), DryRun: true, Axes: []string{"nope"}}
	if _, err := Run(context.Background(), cfg); err == nil {
		t.Fatal("unbekannte Achse muss einen Fehler liefern")
	}
}

// TestReportRendering prüft Markdown- und JSON-Ausgabe am Dry-Run-Report.
func TestReportRendering(t *testing.T) {
	cfg := Config{DataDir: testDataDir(t), DryRun: true, Seed: 1, N: 1,
		Axes: []string{"translate", "rerank"}}
	report, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	md := Markdown(report)
	for _, want := range []string{"| translate |", "| rerank |", "Composite:"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown ohne %q:\n%s", want, md)
		}
	}
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "r.json")
	if err := WriteJSON(report, jsonPath); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var back Report
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("report-json nicht parsebar: %v", err)
	}
	if fmt.Sprintf("%.6f", back.Composite) != fmt.Sprintf("%.6f", report.Composite) {
		t.Errorf("composite drift: %v vs %v", back.Composite, report.Composite)
	}
}
