package goldbench

import (
	"encoding/json"
	"strings"
	"testing"
)

// Der Report-JSON ist die Persistenz-Schnittstelle der Bench-Kampagnen —
// jedes Top-Level-Feld muss im Marshal ankommen (ein stales Binary ohne
// Throughput-Wiring hat am 13.08. Reports ohne Durchsatz produziert).
func TestReportJSONFields(t *testing.T) {
	report := &Report{
		Env:  EnvStamp{Model: "test", MetricVersion: 2},
		Axes: map[string]AxisResult{},
		Throughput: Throughput{
			WallSeconds: 10, PromptTokens: 1000, CompletionTokens: 500,
			PromptTokPerSec: 100, CompletionTokPerSec: 50,
		},
	}
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"env", "axes", "composite", "throughput"} {
		if _, ok := m[key]; !ok {
			t.Errorf("Report-JSON ohne Top-Level-Feld %q", key)
		}
	}
	var tp map[string]any
	if err := json.Unmarshal(m["throughput"], &tp); err != nil {
		t.Fatalf("throughput unmarshal: %v", err)
	}
	for _, key := range []string{"wall_seconds", "prompt_tokens", "completion_tokens", "prompt_tok_per_sec", "completion_tok_per_sec"} {
		if _, ok := tp[key]; !ok {
			t.Errorf("throughput-Block ohne Feld %q", key)
		}
	}
}

// Die Markdown-Tabelle muss pro Zeile so viele Zellen tragen wie der Kopf —
// der CI95-Spalten-Drift (Kopf 6, Zeilen 7 Zellen) blieb sonst unsichtbar.
func TestMarkdownTableAligned(t *testing.T) {
	report := &Report{
		Env: EnvStamp{Model: "test"},
		Axes: map[string]AxisResult{
			"keywords": {N: 10, ParseRate: 1, PrimaryMetric: "capped_set_f1", PrimaryScore: 0.5, LabelQuality: "gold"},
		},
	}
	md := Markdown(report)
	var header, row string
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "| Achse") {
			header = line
		}
		if strings.HasPrefix(line, "| keywords") {
			row = line
		}
	}
	if header == "" || row == "" {
		t.Fatalf("Tabelle nicht gefunden:\n%s", md)
	}
	if hc, rc := strings.Count(header, "|"), strings.Count(row, "|"); hc != rc {
		t.Errorf("Tabellenkopf %d Trenner, Zeile %d — Spalten-Drift:\nKopf:  %s\nZeile: %s", hc, rc, header, row)
	}
}
