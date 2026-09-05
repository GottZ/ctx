package goldbench

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/jsonl"
)

// Welle T06-3: LoadCases las die Fall-Dateien vor der Umstellung auf
// internal/jsonl mit einem bufio.Scanner und war dabei in ZWEI Punkten
// strenger als die elf übrigen JSONL-Leser des Baums. Beide Punkte sind hier
// festgenagelt, weil das Umstellen sie stillschweigend gelockert hätte — aus
// zwei Fehlern wären zwei Erfolge geworden, an einem Leser, dessen Strenge
// (Achsen- und Vollständigkeits-Prüfung) der Zweck ist.

// writeAxisFile legt <dir>/<axis>.jsonl mit dem gegebenen Inhalt an.
func writeAxisFile(t *testing.T, axis, content string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, axis+".jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("Fixture: %v", err)
	}
	return dir, path
}

// validCase ist eine Zeile, die alle drei Validierungen von LoadCases besteht.
func validCase(axis, id string) string {
	return fmt.Sprintf(`{"id":%q,"axis":%q,"input":{"q":"x"},"gold":{"a":"y"}}`, id, axis)
}

// oversizedCaseLine baut eine einzelne, syntaktisch einwandfreie Fall-Zeile
// von rund 5 MiB — jenseits des 4-MiB-Deckels, den LoadCases führt.
func oversizedCaseLine(axis string) string {
	pad := strings.Repeat("p", 5*1024*1024)
	return fmt.Sprintf(`{"id":"gross","axis":%q,"input":{"q":%q},"gold":{"a":"y"}}`, axis, pad) + "\n"
}

// TestLoadCasesWhitespaceLineStaysAnError ist Politik-Fixture (a): eine Zeile
// aus Leerzeichen ist KEINE leere Zeile. Die elf anderen Leser überspringen
// sie (TrimSpace), LoadCases gibt sie an den Parser — und der Fehlertext nennt
// die Zeilennummer, unter der sie in der Datei steht.
func TestLoadCasesWhitespaceLineStaysAnError(t *testing.T) {
	dir, path := writeAxisFile(t, "sensitivity",
		validCase("sensitivity", "a")+"\n   \n"+validCase("sensitivity", "b")+"\n")

	_, err := LoadCases(dir, "sensitivity")
	if err == nil {
		t.Fatal("Leerzeichen-Zeile lief durch — die Skip-Politik wurde gelockert")
	}
	want := fmt.Sprintf("goldbench: %s:2: unexpected end of JSON input", path)
	if err.Error() != want {
		t.Errorf("Text\n  ist      %q\n  erwartet %q", err.Error(), want)
	}
}

// TestLoadCasesBlankLineStillSkipped grenzt (a) ab: die WIRKLICH leere Zeile
// war schon immer erlaubt und bleibt es.
func TestLoadCasesBlankLineStillSkipped(t *testing.T) {
	dir, _ := writeAxisFile(t, "sensitivity",
		validCase("sensitivity", "a")+"\n\n"+validCase("sensitivity", "b")+"\n")

	cases, err := LoadCases(dir, "sensitivity")
	if err != nil {
		t.Fatalf("leere Zeile ist ein Fehler geworden: %v", err)
	}
	if len(cases) != 2 {
		t.Errorf("%d Fälle, erwartet 2", len(cases))
	}
}

// TestLoadCasesLineCapStaysAtFourMiB ist Politik-Fixture (b): eine 5-MiB-Zeile
// bricht ab, so wie der 4-MiB-Puffer des Scanners es tat, und der Text ist der
// des Scanners geblieben.
func TestLoadCasesLineCapStaysAtFourMiB(t *testing.T) {
	dir, path := writeAxisFile(t, "links", oversizedCaseLine("links"))

	_, err := LoadCases(dir, "links")
	if err == nil {
		t.Fatal("5-MiB-Zeile lief durch — der Zeilendeckel ist weg")
	}
	if !errors.Is(err, jsonl.ErrLineTooLong) {
		t.Errorf("Fehler ist nicht ErrLineTooLong: %v", err)
	}
	want := fmt.Sprintf("goldbench: scan %s: bufio.Scanner: token too long", path)
	if err.Error() != want {
		t.Errorf("Text\n  ist      %q\n  erwartet %q", err.Error(), want)
	}
}

// caseLineOfLength baut eine gültige Fall-Zeile von exakt n Bytes.
func caseLineOfLength(t *testing.T, axis string, n int) string {
	t.Helper()
	prefix := fmt.Sprintf(`{"id":"gross","axis":%q,"input":{"q":"`, axis)
	const suffix = `"},"gold":{"a":"y"}}`
	pad := n - len(prefix) - len(suffix)
	if pad < 0 {
		t.Fatalf("n=%d ist kleiner als das Gerüst (%d Bytes)", n, len(prefix)+len(suffix))
	}
	line := prefix + strings.Repeat("p", pad) + suffix
	if len(line) != n {
		t.Fatalf("Fixture ist %d Bytes, erwartet %d", len(line), n)
	}
	return line
}

// TestLoadCasesCapEdgeMatchesScanner nagelt die KANTE des Deckels fest, nicht
// nur seine Existenz. Der bufio.Scanner, der hier vorher stand, bekam
// caseLineMaxBytes als Puffergröße, und in diesen Puffer musste der
// Zeilenumbruch mit hinein — eine Zeile von exakt caseLineMaxBytes Bytes war
// ihm also schon zu lang. Läge die neue Kante ein Byte weiter, würde aus genau
// diesem Fall still ein Erfolg.
func TestLoadCasesCapEdgeMatchesScanner(t *testing.T) {
	t.Run("exakt auf dem Deckel bricht ab", func(t *testing.T) {
		dir, _ := writeAxisFile(t, "links", caseLineOfLength(t, "links", caseLineMaxBytes)+"\n")
		_, err := LoadCases(dir, "links")
		if !errors.Is(err, jsonl.ErrLineTooLong) {
			t.Errorf("Zeile von %d Bytes muss abbrechen, bekam %v", caseLineMaxBytes, err)
		}
	})
	t.Run("ein Byte darunter laeuft durch", func(t *testing.T) {
		dir, _ := writeAxisFile(t, "links", caseLineOfLength(t, "links", caseLineMaxBytes-1)+"\n")
		cases, err := LoadCases(dir, "links")
		if err != nil {
			t.Fatalf("Zeile von %d Bytes: %v", caseLineMaxBytes-1, err)
		}
		if len(cases) != 1 {
			t.Errorf("%d Fälle, erwartet 1", len(cases))
		}
	})
}

// TestLoadCasesUnderCapPasses zeigt, dass der Deckel und nicht die Dateigröße
// entscheidet: dieselbe Nutzlast auf zwei Zeilen verteilt läuft durch.
func TestLoadCasesUnderCapPasses(t *testing.T) {
	pad := strings.Repeat("p", 3*1024*1024)
	line := func(id string) string {
		return fmt.Sprintf(`{"id":%q,"axis":"links","input":{"q":%q},"gold":{"a":"y"}}`, id, pad)
	}
	dir, _ := writeAxisFile(t, "links", line("a")+"\n"+line("b")+"\n")

	cases, err := LoadCases(dir, "links")
	if err != nil {
		t.Fatalf("zwei Zeilen unter dem Deckel: %v", err)
	}
	if len(cases) != 2 {
		t.Errorf("%d Fälle, erwartet 2", len(cases))
	}
}

// TestLoadCasesValidationTextsUnchanged hält die drei Fehlertexte fest, die
// LoadCases selbst formuliert — sie laufen jetzt durch einen Callback und
// dürfen dabei keine Einkleidung dazubekommen.
func TestLoadCasesValidationTextsUnchanged(t *testing.T) {
	t.Run("falsche Achse", func(t *testing.T) {
		dir, path := writeAxisFile(t, "links", validCase("sensitivity", "a")+"\n")
		_, err := LoadCases(dir, "links")
		want := fmt.Sprintf("goldbench: %s:1: axis %q erwartet, %q gefunden", path, "links", "sensitivity")
		if err == nil || err.Error() != want {
			t.Errorf("Text\n  ist      %v\n  erwartet %q", err, want)
		}
	})
	t.Run("unvollständiger Fall", func(t *testing.T) {
		dir, path := writeAxisFile(t, "links",
			validCase("links", "a")+"\n"+`{"id":"b","axis":"links","input":{"q":"x"}}`+"\n")
		_, err := LoadCases(dir, "links")
		want := fmt.Sprintf("goldbench: %s:2: unvollständiger Fall (id/input/gold)", path)
		if err == nil || err.Error() != want {
			t.Errorf("Text\n  ist      %v\n  erwartet %q", err, want)
		}
	})
	t.Run("kaputtes JSON", func(t *testing.T) {
		dir, path := writeAxisFile(t, "links", validCase("links", "a")+"\n{nope\n")
		_, err := LoadCases(dir, "links")
		want := fmt.Sprintf("goldbench: %s:2: invalid character 'n' looking for beginning of object key string", path)
		if err == nil || err.Error() != want {
			t.Errorf("Text\n  ist      %v\n  erwartet %q", err, want)
		}
	})
	t.Run("Datei fehlt", func(t *testing.T) {
		dir := t.TempDir()
		_, err := LoadCases(dir, "weg")
		if err == nil || !strings.HasPrefix(err.Error(), "goldbench: open ") {
			t.Errorf("Öffnungsfehler ist %v, erwartet Präfix \"goldbench: open \"", err)
		}
	})
}
