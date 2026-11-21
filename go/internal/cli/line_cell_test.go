package cli

import (
	"encoding/json"
	"testing"
)

// The cell of a line: a tab, a carriage return or a line break would split the
// record, so each becomes a space; on a terminal the escape allowlist applies
// as well, in a pipe every other byte stays.
func TestLineCell(t *testing.T) {
	const in = "a\tb\r\nc\x1b[31mred\x07"
	if got, want := lineCell(in, false), "a b  c\x1b[31mred\x07"; got != want {
		t.Fatalf("pipe: lineCell = %q, want %q", got, want)
	}
	if got, want := lineCell(in, true), "a b  c[31mred"; got != want {
		t.Fatalf("tty: lineCell = %q, want %q", got, want)
	}
}

// One field's value on a terminal passes the escape allowlist; in a pipe it is
// verbatim, so a content written to a file comes back byte for byte.
func TestFieldTextOnATerminal(t *testing.T) {
	raw, _ := json.Marshal("line\x1b]0;title\x07\tnext\n")
	if got, want := fieldText(raw, false), "line\x1b]0;title\x07\tnext\n\n"; got != want {
		t.Fatalf("pipe: fieldText = %q, want %q", got, want)
	}
	if got, want := fieldText(raw, true), "line]0;title\tnext\n\n"; got != want {
		t.Fatalf("tty: fieldText = %q, want %q", got, want)
	}
	if got := fieldText(json.RawMessage(`null`), false); got != "null\n" {
		t.Fatalf("null: fieldText = %q, want %q", got, "null\n")
	}
}
