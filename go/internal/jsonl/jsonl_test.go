package jsonl_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/jsonl"
)

type row struct {
	N int    `json:"n"`
	S string `json:"s"`
}

// write puts content into a temp file and returns its path.
func write(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return p
}

// collect runs EachReader over s and returns the line numbers it saw.
func collect(t *testing.T, s string, o ...jsonl.Opt) ([]int, []row, error) {
	t.Helper()
	var lines []int
	var vals []row
	err := jsonl.EachReader(strings.NewReader(s), "doc", func(n int, v row) error {
		lines = append(lines, n)
		vals = append(vals, v)
		return nil
	}, o...)
	return lines, vals, err
}

// TestLineErrorFormat pins both renderings of LineError. The named form is the
// one eleven call sites used to produce with fmt.Errorf("%s:%d: %w", …), the
// unnamed one is what goldset.AssertSheetBlind reads the line number out of.
func TestLineErrorFormat(t *testing.T) {
	inner := errors.New("boom")
	named := &jsonl.LineError{Name: "g-ki.jsonl", Line: 7, Err: inner}
	if got, want := named.Error(), "g-ki.jsonl:7: boom"; got != want {
		t.Errorf("named: %q, erwartet %q", got, want)
	}
	anon := &jsonl.LineError{Line: 3, Err: inner}
	if got, want := anon.Error(), "Zeile 3: boom"; got != want {
		t.Errorf("namenlos: %q, erwartet %q", got, want)
	}
	if !errors.Is(named, inner) {
		t.Error("LineError verliert den inneren Fehler — errors.Is bricht")
	}
}

// TestParseErrorCarriesLineAndName checks that a broken line is reported as a
// LineError with the 1-based number of THAT line.
func TestParseErrorCarriesLineAndName(t *testing.T) {
	_, _, err := collect(t, "{\"n\":1}\n\nnope\n")
	var le *jsonl.LineError
	if !errors.As(err, &le) {
		t.Fatalf("kein LineError: %v", err)
	}
	if le.Line != 3 || le.Name != "doc" {
		t.Errorf("LineError{Name:%q, Line:%d}, erwartet {doc, 3}", le.Name, le.Line)
	}
	if got, want := err.Error(), "doc:3: invalid character 'o' in literal null (expecting 'u')"; got != want {
		t.Errorf("Text %q, erwartet %q", got, want)
	}
}

// TestBlankLinesAreCountedNotParsed is the invariant every call site depends
// on: a skipped line still advances the number, so an error points at the line
// an editor shows.
func TestBlankLinesAreCountedNotParsed(t *testing.T) {
	lines, vals, err := collect(t, "\n\n{\"n\":3}\n\n{\"n\":5}\n")
	if err != nil {
		t.Fatalf("unerwartet: %v", err)
	}
	if want := []int{3, 5}; fmt.Sprint(lines) != fmt.Sprint(want) {
		t.Errorf("Zeilennummern %v, erwartet %v", lines, want)
	}
	for i, v := range vals {
		if v.N != lines[i] {
			t.Errorf("Zeile %d trug n=%d", lines[i], v.N)
		}
	}
}

// TestSkipBlankPolicies is politik-fixture (a) at the leaf: the default skips a
// whitespace-only line, SkipBlank(false) hands it to the parser and it fails.
// goldbench relies on the strict half.
func TestSkipBlankPolicies(t *testing.T) {
	const doc = "{\"n\":1}\n   \n{\"n\":3}\n"

	lines, _, err := collect(t, doc)
	if err != nil {
		t.Fatalf("Default sollte Leerzeichen-Zeilen überspringen: %v", err)
	}
	if want := []int{1, 3}; fmt.Sprint(lines) != fmt.Sprint(want) {
		t.Errorf("Default: Zeilen %v, erwartet %v", lines, want)
	}

	_, _, err = collect(t, doc, jsonl.SkipBlank(false))
	var le *jsonl.LineError
	if !errors.As(err, &le) {
		t.Fatalf("SkipBlank(false) sollte an der Leerzeichen-Zeile scheitern, bekam %v", err)
	}
	if got, want := err.Error(), "doc:2: unexpected end of JSON input"; got != want {
		t.Errorf("Text %q, erwartet %q", got, want)
	}
	// Die wirklich leere Zeile bleibt auch unter SkipBlank(false) übersprungen.
	if _, _, err := collect(t, "{\"n\":1}\n\n{\"n\":3}\n", jsonl.SkipBlank(false)); err != nil {
		t.Errorf("SkipBlank(false) darf die leere Zeile nicht parsen: %v", err)
	}
}

// TestMaxLine is politik-fixture (b) at the leaf: a line past the cap stops the
// read with ErrLineTooLong, and without a cap the same line goes through.
func TestMaxLine(t *testing.T) {
	long := "{\"s\":\"" + strings.Repeat("x", 200000) + "\"}"
	doc := "{\"n\":1}\n" + long + "\n"

	_, _, err := collect(t, doc, jsonl.MaxLine(4096))
	if !errors.Is(err, jsonl.ErrLineTooLong) {
		t.Fatalf("MaxLine(4096) sollte ErrLineTooLong liefern, bekam %v", err)
	}
	if got, want := err.Error(), "bufio.Scanner: token too long"; got != want {
		t.Errorf("Text %q, erwartet %q — goldbench druckt ihn wörtlich", got, want)
	}

	lines, _, err := collect(t, doc)
	if err != nil {
		t.Fatalf("ohne Deckel muss dieselbe Zeile durchlaufen: %v", err)
	}
	if want := []int{1, 2}; fmt.Sprint(lines) != fmt.Sprint(want) {
		t.Errorf("Zeilen %v, erwartet %v", lines, want)
	}

	// Der Deckel ist der Puffer, den ein bufio.Scanner bekommen hätte, und in
	// den musste der Zeilenumbruch mit hinein: eine Zeile von exakt n Bytes ist
	// schon zu lang, n-1 geht.
	exact := "{\"s\":\"xxxxxx\"}"
	if len(exact) != 14 {
		t.Fatalf("Fixture ist %d Bytes lang, der Test rechnet mit 14", len(exact))
	}
	if _, _, err := collect(t, exact+"\n", jsonl.MaxLine(15)); err != nil {
		t.Errorf("14-Byte-Zeile unter dem 15er-Deckel: %v", err)
	}
	if _, _, err := collect(t, exact+"\n", jsonl.MaxLine(14)); !errors.Is(err, jsonl.ErrLineTooLong) {
		t.Errorf("14-Byte-Zeile auf dem 14er-Deckel muss abbrechen, bekam %v", err)
	}
}

// TestMaxLineMatchesScanner ist die eigentliche Absicherung des Deckels: für
// jede Zeilenlänge um die Kante herum muss der Leser dieselbe Entscheidung
// treffen wie der bufio.Scanner, der bei goldbench.LoadCases vorher stand.
// Eine Kante um ein Byte daneben wäre genau das, was die Welle verbietet —
// aus einem Fehler würde ein Erfolg.
// Die Deckel-Grenzen decken beide Pfade in readLine ab: unter 4096 (der
// Default-Puffer eines bufio.Readers) entscheidet die Prüfung auf der fertig
// gelesenen Zeile, darüber die im Zusammensetz-Schleifchen. Eine Kante, die
// nur auf einem der beiden Pfade stimmt, ist keine Kante.
func TestMaxLineMatchesScanner(t *testing.T) {
	for _, max := range []int{16, 64, 4095, 4096, 4097, 9000} {
		for _, delta := range []int{-2, -1, 0, 1} {
			n := max + delta
			for _, tail := range []string{"\n", "", "\r\n"} {
				doc := strings.Repeat("x", n) + tail

				sc := bufio.NewScanner(strings.NewReader(doc))
				sc.Buffer(make([]byte, 0, 16), max)
				for sc.Scan() { //nolint:revive // nur sc.Err() interessiert hier
				}
				scannerRefused := sc.Err() != nil

				err := jsonl.EachReader(strings.NewReader(doc), "doc",
					func(_ int, _ json.RawMessage) error { return nil },
					jsonl.SkipBlank(false), jsonl.TrimCR(true), jsonl.MaxLine(max))
				readerRefused := errors.Is(err, jsonl.ErrLineTooLong)

				if scannerRefused != readerRefused {
					t.Errorf("Deckel %d, Länge %d, Abschluss %q: Scanner lehnt ab = %v, jsonl lehnt ab = %v (%v)",
						max, n, tail, scannerRefused, readerRefused, err)
				}
			}
		}
	}
}

// TestTrimCRPolicies hält die zweite Politik-Achse fest. Der Default gibt die
// Zeile heraus, wie strings.Split sie geliefert hätte — mitsamt einem
// terminalen CR; TrimCR(true) ist die bufio.ScanLines-Form. Sichtbar wird der
// Unterschied nur an einer KAPUTTEN Zeile, deren letztes Byte das CR ist:
// encoding/json beschwert sich dann verschieden.
func TestTrimCRPolicies(t *testing.T) {
	// Unabgeschlossener String, letztes Byte vor dem Umbruch ist das CR.
	const doc = "{\"n\":1}\n{\"s\":\"b\r\n"

	_, _, err := collect(t, doc)
	if got, want := err.Error(), "doc:2: invalid character '\\r' in string literal"; got != want {
		t.Errorf("Default (strings.Split-Form):\n  ist      %q\n  erwartet %q", got, want)
	}

	_, _, err = collect(t, doc, jsonl.TrimCR(true))
	if got, want := err.Error(), "doc:2: unexpected end of JSON input"; got != want {
		t.Errorf("TrimCR (ScanLines-Form):\n  ist      %q\n  erwartet %q", got, want)
	}
}

// TestErrLineTooLongIsBufioSentinel keeps the wording tied to its source: the
// call site that used a bufio.Scanner before still prints the scanner's text.
func TestErrLineTooLongIsBufioSentinel(t *testing.T) {
	if !errors.Is(jsonl.ErrLineTooLong, bufio.ErrTooLong) {
		t.Error("ErrLineTooLong ist nicht mehr bufio.ErrTooLong — goldbench-Text bricht")
	}
}

// TestDocumentShapes walks the boundary cases of splitting: no trailing
// newline, no content at all, and blank lines at both ends.
func TestDocumentShapes(t *testing.T) {
	cases := []struct {
		name  string
		doc   string
		lines []int
	}{
		{"leer", "", nil},
		{"nur Newline", "\n", nil},
		{"ohne Schluss-Newline", "{\"n\":1}", []int{1}},
		{"mit Schluss-Newline", "{\"n\":1}\n", []int{1}},
		{"Leerzeile am Ende", "{\"n\":1}\n\n", []int{1}},
		{"Leerzeile in der Mitte", "{\"n\":1}\n\n{\"n\":3}", []int{1, 3}},
		{"führende Leerzeilen", "\n\n{\"n\":3}\n", []int{3}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines, _, err := collect(t, c.doc)
			if err != nil {
				t.Fatalf("unerwartet: %v", err)
			}
			if fmt.Sprint(lines) != fmt.Sprint(c.lines) {
				t.Errorf("Zeilen %v, erwartet %v", lines, c.lines)
			}
		})
	}
}

// TestCRLF checks that a CRLF document reads the same under both terminator
// policies as long as its lines are well-formed — encoding/json treats a
// trailing CR as whitespace, and TrimSpace treats a CR-only line as blank.
func TestCRLF(t *testing.T) {
	const doc = "{\"n\":1,\"s\":\"a\"}\r\n\r\n{\"n\":3}\r\n"
	for _, o := range []struct {
		name string
		opt  []jsonl.Opt
	}{
		{"Default", nil},
		{"TrimCR", []jsonl.Opt{jsonl.TrimCR(true)}},
	} {
		t.Run(o.name, func(t *testing.T) {
			lines, vals, err := collect(t, doc, o.opt...)
			if err != nil {
				t.Fatalf("CRLF: %v", err)
			}
			if want := []int{1, 3}; fmt.Sprint(lines) != fmt.Sprint(want) {
				t.Errorf("Zeilen %v, erwartet %v", lines, want)
			}
			if vals[0].S != "a" {
				t.Errorf("Wert %q, erwartet \"a\"", vals[0].S)
			}
		})
	}
	// Unter der strengen Skip-Politik ist die reine CR-Zeile nur dann leer,
	// wenn das CR fällt — genau so hielt es der bufio.Scanner.
	if _, _, err := collect(t, "{\"n\":1}\r\n\r\n", jsonl.SkipBlank(false), jsonl.TrimCR(true)); err != nil {
		t.Errorf("die reine CR-Zeile muss unter TrimCR strikt leer sein: %v", err)
	}
}

// TestLongLineWithoutCap makes sure a line larger than the reader's buffer is
// assembled rather than truncated — the eleven uncapped call sites read whole
// files today and must keep doing so.
func TestLongLineWithoutCap(t *testing.T) {
	payload := strings.Repeat("y", 300000)
	_, vals, err := collect(t, "{\"s\":\""+payload+"\"}\n")
	if err != nil {
		t.Fatalf("lange Zeile ohne Deckel: %v", err)
	}
	if len(vals) != 1 || vals[0].S != payload {
		t.Errorf("Nutzlast kam mit %d Bytes zurück, erwartet %d", len(vals[0].S), len(payload))
	}
}

// TestCallbackErrorPassesThrough is what lets a call site keep its own wording:
// the error fn returns is returned unchanged, not wrapped in a LineError.
func TestCallbackErrorPassesThrough(t *testing.T) {
	sentinel := errors.New("Zeile 2 gefällt mir nicht")
	err := jsonl.EachReader(strings.NewReader("{\"n\":1}\n{\"n\":2}\n{\"n\":3}\n"), "doc",
		func(n int, v row) error {
			if v.N == 2 {
				return sentinel
			}
			return nil
		})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Callback-Fehler kam als %v zurück", err)
	}
	if err.Error() != sentinel.Error() {
		t.Errorf("Callback-Fehler umgekleidet: %q", err.Error())
	}
	var le *jsonl.LineError
	if errors.As(err, &le) {
		t.Error("Callback-Fehler darf kein LineError sein — Aufrufer unterscheiden daran")
	}
}

// TestCallbackStopsAtFirstError checks the read really stops.
func TestCallbackStopsAtFirstError(t *testing.T) {
	seen := 0
	err := jsonl.EachReader(strings.NewReader("1\n2\n3\n4\n"), "doc", func(n int, v int) error {
		seen++
		if v == 2 {
			return errors.New("stop")
		}
		return nil
	})
	if err == nil {
		t.Fatal("erwartet: Fehler")
	}
	if seen != 2 {
		t.Errorf("%d Zeilen verarbeitet, erwartet 2", seen)
	}
}

// TestAll covers the slice form, including the nil return the hand-written
// append loops produced for a document without a single row.
func TestAll(t *testing.T) {
	got, err := jsonl.All[row](write(t, "rows.jsonl", "{\"n\":1}\n{\"n\":2}\n"))
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 2 || got[1].N != 2 {
		t.Errorf("All lieferte %+v", got)
	}

	empty, err := jsonl.All[row](write(t, "empty.jsonl", "\n\n"))
	if err != nil {
		t.Fatalf("All auf leerem Dokument: %v", err)
	}
	if empty != nil {
		t.Errorf("All lieferte %#v, erwartet nil — die alten Schleifen gaben nil zurück", empty)
	}
}

// TestEachOpenError keeps the untouched os error: ReadJudgeJournal tells "file
// is absent" from "file is broken" with os.IsNotExist on exactly this value.
func TestEachOpenError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "weg.jsonl")
	err := jsonl.Each(missing, func(n int, v row) error { return nil })
	if !os.IsNotExist(err) {
		t.Fatalf("os.IsNotExist griff nicht auf %v", err)
	}
	var le *jsonl.LineError
	if errors.As(err, &le) {
		t.Error("ein Öffnungsfehler darf kein LineError sein")
	}
}

// TestEachReaderTakesAnyReader is the reason the core is not path-based: three
// call sites hold bytes or a gzip stream, not a file.
func TestEachReaderTakesAnyReader(t *testing.T) {
	var raw []map[string]json.RawMessage
	err := jsonl.EachReader(strings.NewReader("{\"a\":1}\n{\"b\":2}\n"), "",
		func(n int, v map[string]json.RawMessage) error {
			raw = append(raw, v)
			return nil
		})
	if err != nil {
		t.Fatalf("EachReader: %v", err)
	}
	if len(raw) != 2 {
		t.Fatalf("%d Zeilen, erwartet 2", len(raw))
	}
	if _, ok := raw[1]["b"]; !ok {
		t.Errorf("zweite Zeile ohne Feld b: %v", raw[1])
	}
}

// TestReadErrorSurfaces makes sure a broken reader is reported rather than
// mistaken for the end of the document.
func TestReadErrorSurfaces(t *testing.T) {
	boom := errors.New("Platte weg")
	r := errReader{after: []byte("{\"n\":1}\n"), err: boom}
	seen := 0
	err := jsonl.EachReader(&r, "doc", func(n int, v row) error {
		seen++
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Lesefehler kam als %v zurück", err)
	}
	if seen != 1 {
		t.Errorf("%d Zeilen vor dem Fehler, erwartet 1", seen)
	}
}

// errReader serves a prefix and then fails.
type errReader struct {
	after []byte
	err   error
}

func (r *errReader) Read(p []byte) (int, error) {
	if len(r.after) == 0 {
		return 0, r.err
	}
	n := copy(p, r.after)
	r.after = r.after[n:]
	return n, nil
}
