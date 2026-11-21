package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// StdoutIsTTY reports whether stdout is an interactive terminal (not piped or
// redirected). Mirrors ReadStdin's stdin probe via os.ModeCharDevice — stdlib
// only, no x/term dependency. Used to switch between human-readable rendering
// (TTY) and machine-readable JSON (pipe), so `ctx dream stats` stays scriptable.
func StdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// StdinIsTTY reports whether stdin is an interactive terminal (not piped or
// redirected). Mirrors StdoutIsTTY via os.ModeCharDevice. Used by the project
// detect/init flow: a mismatched .ctx-project file or a missing git identity
// prompts on a TTY, but MUST error (never block on a read) when piped (§4.3).
func StdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// PrintJSON writes raw JSON to stdout with optional pretty-printing.
// If the data is valid JSON, it pretty-prints it; otherwise writes raw.
func PrintJSON(data []byte) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, data, "", "  "); err != nil {
		// Not valid JSON, write raw
		_, _ = os.Stdout.Write(data)
		return
	}
	buf.WriteByte('\n')
	_, _ = os.Stdout.Write(buf.Bytes())
}

// renderOrJSON is the ONE place where the CLI decides between the two output
// contracts: piped/redirected stdout gets the server's JSON verbatim (the
// machine contract every script and `| jq` relies on), an interactive terminal
// gets render's human form. Every command that renders a response goes through
// here, so the decision cannot drift apart across commands — the shape used to
// stand hand-written at 38 places.
//
// render is only called on a TTY; its error is the command's error. A renderer
// that cannot decode the response prints the raw JSON itself and then returns
// the decoder's error — so a malformed answer ends the command non-zero on a
// terminal too, and the operator still sees the bytes that caused it. (The
// sentence here used to claim those renderers return nil; they do not, and
// never did.)
func renderOrJSON(resp []byte, render func(resp []byte) error) error {
	if !StdoutIsTTY() {
		PrintJSON(resp)
		return nil
	}
	return render(resp)
}

// lineCellBreaks are the bytes that would end a cell or a record of a
// tab-separated line.
var lineCellBreaks = strings.NewReplacer("\t", " ", "\r", " ", "\n", " ")

// lineCell is one cell of a line-per-record answer (`ctx search --lines`, the
// answer line of `ctx update`): a tab, a carriage return or a line break in
// the text would split the record, so each becomes a space. On a terminal the
// text passes the escape allowlist as well (titles are foreign text,
// sanitize.go); in a pipe every other byte stays as the server sent it.
func lineCell(s string, tty bool) string {
	s = lineCellBreaks.Replace(s)
	if tty {
		s = sanitizeTerminal(s)
	}
	return s
}

// PrintRaw writes raw bytes to stdout.
func PrintRaw(data []byte) {
	_, _ = os.Stdout.Write(data)
	// Ensure trailing newline
	if len(data) > 0 && data[len(data)-1] != '\n' {
		fmt.Println()
	}
}

// Errorf prints to stderr.
func Errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// ReadStdin reads all of stdin if it's piped (not a terminal).
func ReadStdin() (string, bool) {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return "", false
	}
	if (fi.Mode() & os.ModeCharDevice) != 0 {
		// Terminal, not piped
		return "", false
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", false
	}
	s := string(data)
	// Trim trailing newline like bash
	if len(s) > 0 && s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	return s, len(s) > 0
}
