package goldset_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/goldset"
)

// Welle T06-3: die goldset-Leser teilen sich seit der Umstellung einen Kern mit
// goldbench.LoadCases — aber NICHT dessen zwei Strengen. Dieses Gegenstück zu
// internal/goldbench/loadcases_policy_t063_test.go hält die andere Hälfte der
// Politik fest: hier gibt es keinen Zeilendeckel, und eine Zeile aus
// Leerzeichen ist eine Leerzeile.

// TestReadJSONLHasNoLineCap ist die Gegenprobe zu Politik-Fixture (b): dieselbe
// 5-MiB-Zeile, an der goldbench.LoadCases abbricht, läuft in einer
// Slice-Datei durch. Der Deckel gehört dem Aufrufer, nicht dem Leser.
func TestReadJSONLHasNoLineCap(t *testing.T) {
	pad := strings.Repeat("p", 5*1024*1024)
	line := fmt.Sprintf(`{"slice":"g-ki","index":1,"query":%q,"query_sha256":"aa","origin":"llm-question"}`, pad)

	p := filepath.Join(t.TempDir(), "g-ki.jsonl")
	if err := os.WriteFile(p, []byte(line+"\n"), 0o600); err != nil {
		t.Fatalf("Fixture: %v", err)
	}

	cases, err := goldset.ReadJSONL(p)
	if err != nil {
		t.Fatalf("5-MiB-Zeile in einer Slice-Datei muss durchlaufen: %v", err)
	}
	if len(cases) != 1 {
		t.Fatalf("%d Fälle, erwartet 1", len(cases))
	}
	if len(cases[0].Query) != len(pad) {
		t.Errorf("Query kam mit %d Bytes zurück, erwartet %d", len(cases[0].Query), len(pad))
	}
}

// TestReadJSONLSkipsWhitespaceLine ist die Gegenprobe zu Politik-Fixture (a):
// die Zeile aus Leerzeichen, die goldbench als Fall-Fehler meldet, ist hier
// eine Leerzeile — und die Zeilennummern danach zählen sie trotzdem mit.
func TestReadJSONLSkipsWhitespaceLine(t *testing.T) {
	good := `{"slice":"g-ki","index":1,"query":"a","query_sha256":"aa","origin":"llm-question"}`
	p := filepath.Join(t.TempDir(), "g-ki.jsonl")
	if err := os.WriteFile(p, []byte(good+"\n   \n"+good+"\n"), 0o600); err != nil {
		t.Fatalf("Fixture: %v", err)
	}

	cases, err := goldset.ReadJSONL(p)
	if err != nil {
		t.Fatalf("Leerzeichen-Zeile wurde zum Fehler: %v", err)
	}
	if len(cases) != 2 {
		t.Errorf("%d Fälle, erwartet 2", len(cases))
	}
}

// TestReadJSONLErrorNamesTheLine hält den Fehlertext fest, den elf Fundstellen
// dieser Welle teilen: "<pfad>:<zeile>: <json-fehler>", 1-basiert und mit
// mitgezählten Leerzeilen.
func TestReadJSONLErrorNamesTheLine(t *testing.T) {
	good := `{"slice":"g-ki","index":1,"query":"a","query_sha256":"aa","origin":"llm-question"}`
	p := filepath.Join(t.TempDir(), "g-ki.jsonl")
	if err := os.WriteFile(p, []byte(good+"\n\n{kaputt\n"), 0o600); err != nil {
		t.Fatalf("Fixture: %v", err)
	}

	_, err := goldset.ReadJSONL(p)
	want := p + ":3: invalid character 'k' looking for beginning of object key string"
	if err == nil || err.Error() != want {
		t.Errorf("Text\n  ist      %v\n  erwartet %q", err, want)
	}
}
